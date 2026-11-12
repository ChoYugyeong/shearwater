// Package agent renders the compact inspection views used by sw status and sw tree.
package agent

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"shearwater/internal/apis"
)

// Snapshot contains API lists with the same store-wide revision. An object's
// own resourceVersion can be older because it records that object's last edit.
type Snapshot struct {
	Revision       string
	Nodes          []apis.Node
	Pods           []apis.Pod
	Namespaces     []apis.Namespace
	Deployments    []apis.Deployment
	ReplicaSets    []apis.ReplicaSet
	Services       []apis.Service
	EndpointSlices []apis.EndpointSlice
}

// Summary is cluster-wide even when the tree below it is filtered.
func (s Snapshot) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "cluster shearwater rev=%s", s.Revision)
	nodes := slices.Clone(s.Nodes)
	slices.SortFunc(nodes, func(a, b apis.Node) int { return cmp.Compare(a.Metadata.Name, b.Metadata.Name) })
	if len(nodes) == 0 {
		b.WriteString(" node=<none>")
	}
	for _, node := range nodes {
		fmt.Fprintf(&b, " node=%s %s", node.Metadata.Name, inspectNodeState(node))
	}
	ready := 0
	for i := range s.Pods {
		if inspectPodReady(&s.Pods[i]) {
			ready++
		}
	}
	fmt.Fprintf(&b, " pods=%d/%d", ready, len(s.Pods))
	return b.String()
}

func inspectNodeState(node apis.Node) string {
	if node.Metadata.DeletionTimestamp != nil {
		return "Terminating"
	}
	for _, c := range node.Status.Conditions {
		if c.Type == "Ready" {
			switch c.Status {
			case "True":
				return "Ready"
			case "False":
				return "NotReady"
			}
			break
		}
	}
	return "Unknown"
}

type TreeOptions struct {
	Namespace string // empty shows all namespaces
	Root      string // empty shows all roots, otherwise a short kind/name
	Problems  bool
}

var (
	ErrTreeNotFound  = errors.New("not found")
	ErrTreeAmbiguous = errors.New("matches more than one namespace")
)

type treeRow struct {
	label    string
	detail   string
	problem  bool
	object   apis.Object // nil for the synthetic namespace heading
	parent   *treeRow
	children []*treeRow
}

// Tree returns the summary and the selected tree. Namespace names, then
// resource names within each kind, determine the output order.
func (s Snapshot) Tree(options TreeOptions) (string, error) {
	namespaces := make(map[string]*treeRow)
	namespaceRow := func(name string) *treeRow {
		if namespaces[name] == nil {
			namespaces[name] = &treeRow{label: "ns " + name}
		}
		return namespaces[name]
	}
	visible := func(namespace string) bool { return options.Namespace == "" || options.Namespace == namespace }
	for _, ns := range s.Namespaces {
		if visible(ns.Metadata.Name) {
			row := namespaceRow(ns.Metadata.Name)
			if ns.Metadata.DeletionTimestamp != nil || ns.Status.Phase == "Terminating" {
				row.detail, row.problem = " Terminating", true
			}
		}
	}

	var rows []*treeRow
	byUID := make(map[string]*treeRow)
	add := func(kind string, object apis.Object) {
		meta := object.Meta()
		if !visible(meta.Namespace) {
			return
		}
		row := &treeRow{label: kind + "/" + meta.Name, object: object}
		rows = append(rows, row)
		if meta.UID != "" {
			byUID[meta.UID] = row
		}
		namespaceRow(meta.Namespace)
	}
	for i := range s.Deployments {
		add("deploy", &s.Deployments[i])
	}
	for i := range s.ReplicaSets {
		add("rs", &s.ReplicaSets[i])
	}
	for i := range s.Pods {
		add("pod", &s.Pods[i])
	}
	for i := range s.Services {
		add("svc", &s.Services[i])
	}
	if options.Namespace != "" && namespaces[options.Namespace] == nil {
		return "", fmt.Errorf("namespace %q %w", options.Namespace, ErrTreeNotFound)
	}
	slices.SortFunc(rows, func(a, b *treeRow) int {
		return cmp.Or(cmp.Compare(a.object.Meta().Namespace, b.object.Meta().Namespace), cmp.Compare(a.label, b.label))
	})

	var root *treeRow
	endpoints := readyServiceEndpoints(s.Services, s.EndpointSlices)
	for _, row := range rows {
		ns := row.object.Meta().Namespace
		if row.label == options.Root {
			if root != nil {
				return "", fmt.Errorf("root %s %w (%s and %s)", options.Root, ErrTreeAmbiguous, root.object.Meta().Namespace, ns)
			}
			root = row
		}
		row.parent = row.owner(byUID)
		parent := row.parent
		if parent == nil {
			parent = namespaces[ns]
		}
		parent.children = append(parent.children, row)
		row.describe(endpoints)
	}

	if options.Root != "" {
		if root == nil {
			return "", fmt.Errorf("root %s %w", options.Root, ErrTreeNotFound)
		}
		// Resolve the root before pruning. A healthy root still exists when
		// --problems removes it from the printed tree.
		ns := root.object.Meta().Namespace
		namespaces[ns].children = []*treeRow{root}
		namespaces = map[string]*treeRow{ns: namespaces[ns]}
	}
	var b strings.Builder
	b.WriteString(s.Summary())
	b.WriteByte('\n')
	for _, ns := range slices.Sorted(maps.Keys(namespaces)) {
		row := namespaces[ns]
		if !options.Problems || row.keepProblems() {
			row.write(&b, 0)
		}
	}
	return b.String(), nil
}

func (r *treeRow) owner(byUID map[string]*treeRow) *treeRow {
	var fallback *treeRow
	for _, ref := range r.object.Meta().OwnerReferences {
		owner := byUID[ref.UID]
		// The UID is what distinguishes a deleted owner from a new object
		// with the same name. Also check the rest of the reference and scope.
		if owner == nil || !inspectOwnerMatches(r.object.Meta(), ref, owner.object.Meta(), *owner.object.Type()) {
			continue
		}
		if r.wouldCycle(owner) {
			continue
		}
		// One row gets one parent. Prefer the controller when both a
		// controller and another owner reference are present.
		if ref.Controller != nil && *ref.Controller {
			return owner
		}
		if fallback == nil {
			fallback = owner
		}
	}
	return fallback
}

func inspectOwnerMatches(child *apis.ObjectMeta, ref apis.OwnerReference, owner *apis.ObjectMeta, kind apis.TypeMeta) bool {
	return ref.UID == owner.UID && ref.Name == owner.Name && ref.Kind == kind.Kind &&
		ref.APIVersion == kind.APIVersion && child.Namespace == owner.Namespace
}

func (r *treeRow) wouldCycle(owner *treeRow) bool {
	// Users can set owner references themselves. Skip a link that would
	// close a cycle so every object remains reachable from its namespace.
	for p := owner; p != nil; p = p.parent {
		if p == r {
			return true
		}
	}
	return false
}

func (r *treeRow) keepProblems() bool {
	r.children = slices.DeleteFunc(r.children, func(child *treeRow) bool { return !child.keepProblems() })
	return r.problem || len(r.children) != 0
}

func (r *treeRow) write(b *strings.Builder, depth int) {
	b.WriteString(strings.Repeat("  ", depth))
	b.WriteString(r.label)
	b.WriteString(r.detail)
	b.WriteByte('\n')
	for _, child := range r.children {
		child.write(b, depth+1)
	}
}
