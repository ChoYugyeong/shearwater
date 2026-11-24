package controller

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"shearwater/internal/apis"
	"shearwater/internal/objects"
	"shearwater/internal/store"
)

// The scheduler places each pending Pod on one Node (design-multinode-tenancy
// 3.4). Filters say which nodes can take it; scores pick among those. Both
// follow the Kubernetes default plugins that Shearwater can express:
// NodeUnschedulable, TaintToleration, NodeAffinity, NodeResourcesFit,
// NodeResourcesBalancedAllocation (as "least allocated"), and spreading the
// Pods of one owner.

// maxPodsPerNode is the Kubernetes default.
const maxPodsPerNode = 110

// usage is what the Pods bound to a node request.
type usage struct {
	milliCPU, memory int64
	pods             int
	owners           map[string]int // controller UID -> Pods of it on the node
}

type candidate struct {
	node  *apis.Node
	score int64
}

func nodeReady(n *apis.Node) bool {
	if n.Metadata.DeletionTimestamp != nil {
		return false
	}
	for _, c := range n.Status.Conditions {
		if c.Type == "Ready" {
			return c.Status == "True"
		}
	}
	return false
}

// podRequests sums the containers' requests. A container with a limit and no
// request requests its limit, as in Kubernetes.
func podRequests(p *apis.Pod) (milliCPU, memory int64) {
	for _, c := range p.Spec.Containers {
		pick := func(name string) apis.Quantity {
			if q, ok := c.Resources.Requests[name]; ok {
				return q
			}
			return c.Resources.Limits[name]
		}
		if q := pick("cpu"); q != "" {
			if v, err := q.MilliValue(); err == nil {
				milliCPU += v
			}
		}
		if q := pick("memory"); q != "" {
			if v, err := q.Value(); err == nil {
				memory += v
			}
		}
	}
	return milliCPU, memory
}

func allocatable(n *apis.Node) (milliCPU, memory int64, pods int) {
	milliCPU, memory, pods = -1, -1, maxPodsPerNode
	if q, ok := n.Status.Allocatable["cpu"]; ok {
		if v, err := q.MilliValue(); err == nil {
			milliCPU = v
		}
	}
	if q, ok := n.Status.Allocatable["memory"]; ok {
		if v, err := q.Value(); err == nil {
			memory = v
		}
	}
	if q, ok := n.Status.Allocatable["pods"]; ok {
		if v, err := strconv.Atoi(string(q)); err == nil {
			pods = v
		}
	}
	return
}

// matchRequirement evaluates one node selector requirement on labels.
func matchRequirement(r apis.NodeSelectorRequirement, labels map[string]string) bool {
	v, has := labels[r.Key]
	switch r.Operator {
	case "In":
		return has && slices.Contains(r.Values, v)
	case "NotIn":
		return !has || !slices.Contains(r.Values, v)
	case "Exists":
		return has
	case "DoesNotExist":
		return !has
	case "Gt", "Lt":
		if !has || len(r.Values) != 1 {
			return false
		}
		a, err1 := strconv.ParseInt(v, 10, 64)
		b, err2 := strconv.ParseInt(r.Values[0], 10, 64)
		if err1 != nil || err2 != nil {
			return false
		}
		if r.Operator == "Gt" {
			return a > b
		}
		return a < b
	}
	return false
}

func matchTerm(t apis.NodeSelectorTerm, n *apis.Node) bool {
	if len(t.MatchExpressions) == 0 && len(t.MatchFields) == 0 {
		return false // an empty term matches nothing (Kubernetes)
	}
	for _, r := range t.MatchExpressions {
		if !matchRequirement(r, n.Metadata.Labels) {
			return false
		}
	}
	for _, r := range t.MatchFields {
		if !matchRequirement(apis.NodeSelectorRequirement{Key: "name", Operator: r.Operator, Values: r.Values},
			map[string]string{"name": n.Metadata.Name}) {
			return false
		}
	}
	return true
}

// matchesNode applies nodeSelector and required node affinity.
func matchesNode(p *apis.Pod, n *apis.Node) bool {
	for k, v := range p.Spec.NodeSelector {
		if n.Metadata.Labels[k] != v {
			return false
		}
	}
	if a := p.Spec.Affinity; a != nil && a.NodeAffinity != nil && a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution != nil {
		for _, t := range a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
			if matchTerm(t, n) {
				return true
			}
		}
		return false
	}
	return true
}

// unschedulableTaint is how Kubernetes expresses a cordoned node.
var unschedulableTaint = apis.Taint{Key: "node.kubernetes.io/unschedulable", Effect: "NoSchedule"}

// filter returns "" when the node can take the Pod, otherwise the reason in
// the words Kubernetes uses in "0/3 nodes are available: ...".
func (m Manager) filter(p *apis.Pod, n *apis.Node, u *usage, cpu, mem int64) string {
	if !nodeReady(n) {
		return "node(s) were not ready"
	}
	if n.Spec.Unschedulable && !apis.Tolerates(p.Spec.Tolerations, unschedulableTaint) {
		return "node(s) were unschedulable"
	}
	for _, t := range n.Spec.Taints {
		if (t.Effect == "NoSchedule" || t.Effect == "NoExecute") && !apis.Tolerates(p.Spec.Tolerations, t) {
			return fmt.Sprintf("node(s) had untolerated taint {%s: %s}", t.Key, t.Value)
		}
	}
	if !matchesNode(p, n) {
		return "node(s) didn't match Pod's node affinity/selector"
	}
	allocCPU, allocMem, allocPods := allocatable(n)
	if u.pods+1 > allocPods {
		return "Too many pods"
	}
	if allocCPU >= 0 && cpu > 0 && u.milliCPU+cpu > allocCPU {
		return "Insufficient cpu"
	}
	if allocMem >= 0 && mem > 0 && u.memory+mem > allocMem {
		return "Insufficient memory"
	}
	if m.VolumeFilter != nil {
		if why := m.VolumeFilter(p, n); why != "" {
			return why
		}
	}
	return ""
}

// score ranks a feasible node: emptier nodes first, then fewer Pods of the
// same owner, then preferred node affinity; PreferNoSchedule taints count against.
func score(p *apis.Pod, n *apis.Node, u *usage, cpu, mem int64, owner string) int64 {
	allocCPU, allocMem, _ := allocatable(n)
	free := func(alloc, used, req int64) int64 {
		if alloc <= 0 {
			return 50
		}
		return max(0, (alloc-used-req)*100/alloc)
	}
	s := (free(allocCPU, u.milliCPU, cpu) + free(allocMem, u.memory, mem)) / 2
	if owner != "" {
		s += 100 / int64(1+u.owners[owner]) // spread one owner's Pods
	}
	if a := p.Spec.Affinity; a != nil && a.NodeAffinity != nil {
		var total, got int64
		for _, pref := range a.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution {
			total += int64(pref.Weight)
			if matchTerm(pref.Preference, n) {
				got += int64(pref.Weight)
			}
		}
		if total > 0 {
			s += got * 100 / total
		}
	}
	for _, t := range n.Spec.Taints {
		if t.Effect == "PreferNoSchedule" && !apis.Tolerates(p.Spec.Tolerations, t) {
			s -= 50
		}
	}
	return s
}

func ownerUID(p *apis.Pod) string {
	if ref, ok := controllerRef(&p.Metadata); ok {
		return ref.UID
	}
	return ""
}

// unschedulableMessage builds "0/3 nodes are available: 1 Insufficient cpu, 2 ...".
func unschedulableMessage(total int, reasons map[string]int) string {
	if total == 0 {
		return "0/0 nodes are available: no nodes have joined the cluster"
	}
	var parts []string
	for _, k := range slices.Collect(maps.Keys(reasons)) {
		parts = append(parts, fmt.Sprintf("%d %s", reasons[k], k))
	}
	slices.Sort(parts) // Kubernetes sorts the "N reason" strings
	return fmt.Sprintf("0/%d nodes are available: %s.", total, strings.Join(parts, ", "))
}

func (m Manager) schedule(tx *store.Tx, v view, now time.Time) error {
	use := map[string]*usage{}
	for _, n := range v.nodes {
		use[n.Metadata.Name] = &usage{owners: map[string]int{}}
	}
	for _, i := range v.pods {
		p := i.obj.(*apis.Pod)
		u := use[p.Spec.NodeName]
		if u == nil || i.rec.Deleting || !nonTerminal(p) {
			continue
		}
		cpu, mem := podRequests(p)
		u.milliCPU += cpu
		u.memory += mem
		u.pods++
		if o := ownerUID(p); o != "" {
			u.owners[o]++
		}
	}
	nodes := slices.Clone(v.nodes)
	slices.SortFunc(nodes, func(a, b *apis.Node) int { return strings.Compare(a.Metadata.Name, b.Metadata.Name) })

	for _, i := range v.pods {
		p := i.obj.(*apis.Pod)
		if i.rec.Deleting || p.Spec.NodeName != "" || !active(p) {
			continue
		}
		cpu, mem := podRequests(p)
		owner := ownerUID(p)
		reasons := map[string]int{}
		var best *candidate
		for _, n := range nodes {
			u := use[n.Metadata.Name]
			if why := m.filter(p, n, u, cpu, mem); why != "" {
				reasons[why]++
				continue
			}
			if s := score(p, n, u, cpu, mem, owner); best == nil || s > best.score {
				best = &candidate{node: n, score: s}
			}
		}
		c := apis.PodCondition{Type: "PodScheduled", Status: "False", Reason: "Unschedulable",
			Message: unschedulableMessage(len(nodes), reasons), LastTransitionTime: &apis.Time{Time: now}}
		if best != nil {
			p.Spec.NodeName = best.node.Metadata.Name
			p.Metadata.Generation++
			c.Status, c.Reason, c.Message = "True", "", ""
			u := use[best.node.Metadata.Name]
			u.milliCPU += cpu
			u.memory += mem
			u.pods++
			if owner != "" {
				u.owners[owner]++
			}
		}
		found, same := false, false
		for j := range p.Status.Conditions {
			old := &p.Status.Conditions[j]
			if old.Type == c.Type {
				same = old.Status == c.Status && old.Reason == c.Reason && old.Message == c.Message
				if old.Status == c.Status {
					c.LastTransitionTime = old.LastTransitionTime
				}
				*old, found = c, true
				break
			}
		}
		if !found {
			p.Status.Conditions = append(p.Status.Conditions, c)
		}
		if same && best == nil {
			continue // still unschedulable for the same reasons: write nothing
		}
		if err := i.update(tx); err != nil {
			return err
		}
		if best != nil {
			message := fmt.Sprintf("Successfully assigned %s/%s to %s", p.Metadata.Namespace, p.Metadata.Name, best.node.Metadata.Name)
			if err := objects.PodEvent(tx, p, now, "schedule", "", "Scheduled", message, "shearwater-scheduler", "Normal"); err != nil {
				return err
			}
		} else if err := objects.PodEvent(tx, p, now, "schedule-failed", "", "FailedScheduling", c.Message, "shearwater-scheduler", "Warning"); err != nil {
			return err
		}
	}
	return nil
}
