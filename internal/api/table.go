package api

import (
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"shearwater/internal/apis"
)

// Table output (design 3.2): when kubectl asks for
// "application/json;as=Table;g=meta.k8s.io;v=v1" the server sends the columns
// (READY, STATUS, AGE, ...) instead of the objects, and kubectl prints them.
// The columns and cells follow the Kubernetes printers for each kind.

type tableColumn struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Format      string `json:"format"`
	Description string `json:"description"`
	Priority    int32  `json:"priority"`
}

type tableRow struct {
	Cells  []any           `json:"cells"`
	Object json.RawMessage `json:"object,omitempty"`
}

type table struct {
	Kind       string `json:"kind"`
	APIVersion string `json:"apiVersion"`
	Metadata   struct {
		ResourceVersion string `json:"resourceVersion,omitempty"`
	} `json:"metadata"`
	ColumnDefinitions []tableColumn `json:"columnDefinitions,omitempty"`
	Rows              []tableRow    `json:"rows"`
}

// tableOpts is how a request wants its Table: nil means plain JSON.
type tableOpts struct {
	includeObject string // Metadata (default), Object or None
}

// negotiate reads the Accept header. Protobuf and anything else the server
// cannot produce is skipped in favour of JSON, as long as the client lists JSON
// at all or lists nothing.
func negotiate(r *http.Request) (*tableOpts, *apiError) {
	unsupported := ""
	for part := range strings.SplitSeq(r.Header.Get("Accept"), ",") {
		mt, params, err := mime.ParseMediaType(strings.TrimSpace(part))
		if err != nil || (mt != "application/json" && mt != "*/*" && mt != "application/*") {
			continue
		}
		switch as := params["as"]; {
		case as == "":
			return nil, nil
		case as == "Table" && params["g"] == "meta.k8s.io" && params["v"] == "v1":
			opts := &tableOpts{includeObject: r.URL.Query().Get("includeObject")}
			switch opts.includeObject {
			case "":
				opts.includeObject = "Metadata"
			case "Metadata", "Object", "None":
			default:
				return nil, errBadRequest("includeObject must be Metadata, Object or None, got %q", opts.includeObject)
			}
			return opts, nil
		default:
			unsupported = part
		}
	}
	if unsupported != "" {
		return nil, newErr(http.StatusNotAcceptable, "NotAcceptable", "UNSUPPORTED", "never",
			"the server answers with JSON objects or as=Table;g=meta.k8s.io;v=v1, not %q", strings.TrimSpace(unsupported))
	}
	return nil, nil
}

func col(name, typ string, priority int32, description string) tableColumn {
	format := ""
	if name == "Name" {
		format = "name"
	}
	return tableColumn{Name: name, Type: typ, Format: format, Description: description, Priority: priority}
}

var (
	nameCol = col("Name", "string", 0, "Name of the object")
	ageCol  = col("Age", "string", 0, "Time since the object was created")
)

var tableColumns = map[string][]tableColumn{
	"Pod": {nameCol,
		col("Ready", "string", 0, "Containers that are running, out of all containers. In stage 1 there are no probes, so ready means running."),
		col("Status", "string", 0, "Phase of the pod, or the reason a container is waiting or has terminated"),
		col("Restarts", "string", 0, "Number of times the containers have been restarted"),
		ageCol,
		col("IP", "string", 1, "IP address of the pod"),
		col("Node", "string", 1, "Node the pod runs on"),
		col("Nominated Node", "string", 1, "Always <none>: there is no preemption"),
		col("Readiness Gates", "string", 1, "Always <none>: readiness gates are not supported")},
	"Deployment": {nameCol,
		col("Ready", "string", 0, "Ready pods out of the desired number"),
		col("Up-to-date", "string", 0, "Pods that run the current template"),
		col("Available", "string", 0, "Pods that are available"),
		ageCol,
		col("Containers", "string", 1, "Names of the containers"),
		col("Images", "string", 1, "Images of the containers"),
		col("Selector", "string", 1, "Label selector for the pods")},
	"ReplicaSet": {nameCol,
		col("Desired", "integer", 0, "Desired number of pods"),
		col("Current", "integer", 0, "Pods that exist"),
		col("Ready", "integer", 0, "Pods that are ready"),
		ageCol,
		col("Containers", "string", 1, "Names of the containers"),
		col("Images", "string", 1, "Images of the containers"),
		col("Selector", "string", 1, "Label selector for the pods")},
	"Service": {nameCol,
		col("Type", "string", 0, "Type of the service"),
		col("Cluster-IP", "string", 0, "Virtual IP of the service"),
		col("External-IP", "string", 0, "Always <none>: only ClusterIP services exist"),
		col("Port(s)", "string", 0, "Ports of the service"),
		ageCol,
		col("Selector", "string", 1, "Label selector for the pods behind the service")},
	"Node": {nameCol,
		col("Status", "string", 0, "Ready or NotReady"),
		col("Roles", "string", 0, "Roles from node-role.kubernetes.io/ labels"),
		ageCol,
		col("Version", "string", 0, "Version of the node agent"),
		col("Internal-IP", "string", 1, "Internal address of the node"),
		col("External-IP", "string", 1, "External address of the node"),
		col("OS-Image", "string", 1, "Operating system of the node"),
		col("Kernel-Version", "string", 1, "Kernel of the node"),
		col("Container-Runtime", "string", 1, "Container runtime of the node")},
	"Namespace": {nameCol, col("Status", "string", 0, "Phase of the namespace"), ageCol},
	"Event": {
		col("Last Seen", "string", 0, "Time since the event last happened"),
		col("Type", "string", 0, "Normal or Warning"),
		col("Reason", "string", 0, "Short reason for the event"),
		col("Object", "string", 0, "Object the event is about"),
		col("Message", "string", 0, "What happened"),
		col("First Seen", "string", 1, "Time since the event first happened"),
		col("Count", "string", 1, "Number of times the event happened"),
		nameColP1()},
	"EndpointSlice": {nameCol,
		col("AddressType", "string", 0, "Type of the addresses"),
		col("Ports", "string", 0, "Ports of the endpoints"),
		col("Endpoints", "string", 0, "Addresses of the endpoints"),
		ageCol},
}

func nameColP1() tableColumn {
	c := nameCol
	c.Priority = 1
	return c
}

// humanDuration is Kubernetes' duration.HumanDuration: "45s", "5m12s", "3h", "2d4h".
func humanDuration(d time.Duration) string {
	seconds := int(d.Seconds())
	switch {
	case seconds < -1:
		return "<invalid>"
	case seconds < 0:
		return "0s"
	case seconds < 120:
		return fmt.Sprintf("%ds", seconds)
	}
	two := func(a int, ua string, b int, ub string) string {
		if b == 0 {
			return fmt.Sprintf("%d%s", a, ua)
		}
		return fmt.Sprintf("%d%s%d%s", a, ua, b, ub)
	}
	minutes := int(d / time.Minute)
	hours := int(d / time.Hour)
	switch {
	case minutes < 10:
		return two(minutes, "m", seconds%60, "s")
	case minutes < 180:
		return fmt.Sprintf("%dm", minutes)
	case hours < 8:
		return two(hours, "h", minutes%60, "m")
	case hours < 48:
		return fmt.Sprintf("%dh", hours)
	case hours < 24*8:
		return two(hours/24, "d", hours%24, "h")
	case hours < 24*365*2:
		return fmt.Sprintf("%dd", hours/24)
	case hours < 24*365*8:
		return two(hours/24/365, "y", (hours/24)%365, "d")
	}
	return fmt.Sprintf("%dy", hours/24/365)
}

func since(t *apis.Time, now time.Time) string {
	if t == nil || t.IsZero() {
		return "<unknown>"
	}
	return humanDuration(now.Sub(t.Time))
}

func orNone(s string) string {
	if s == "" {
		return "<none>"
	}
	return s
}

func formatSelector(ls *apis.LabelSelector) string {
	if ls == nil {
		return "<none>"
	}
	keys := make([]string, 0, len(ls.MatchLabels))
	for k := range ls.MatchLabels {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, k+"="+ls.MatchLabels[k])
	}
	for _, e := range ls.MatchExpressions {
		switch e.Operator {
		case apis.OpIn:
			parts = append(parts, e.Key+" in ("+strings.Join(e.Values, ",")+")")
		case apis.OpNotIn:
			parts = append(parts, e.Key+" notin ("+strings.Join(e.Values, ",")+")")
		case apis.OpExists:
			parts = append(parts, e.Key)
		case apis.OpDoesNotExist:
			parts = append(parts, "!"+e.Key)
		}
	}
	return orNone(strings.Join(parts, ","))
}

func containerColumns(s *apis.PodSpec) (names, images string) {
	var n, i []string
	for _, c := range s.Containers {
		n = append(n, c.Name)
		i = append(i, c.Image)
	}
	return strings.Join(n, ","), strings.Join(i, ",")
}

func podCells(p *apis.Pod, now time.Time) []any {
	reason := p.Status.Phase
	if p.Status.Reason != "" {
		reason = p.Status.Reason
	}
	restarts, ready, hasRunning := 0, 0, false
	for i := len(p.Status.ContainerStatuses) - 1; i >= 0; i-- {
		cs := &p.Status.ContainerStatuses[i]
		restarts += int(cs.RestartCount)
		switch w, t := cs.State.Waiting, cs.State.Terminated; {
		case w != nil && w.Reason != "":
			reason = w.Reason
		case t != nil && t.Reason != "":
			reason = t.Reason
		case t != nil && t.Signal != 0:
			reason = fmt.Sprintf("Signal:%d", t.Signal)
		case t != nil:
			reason = fmt.Sprintf("ExitCode:%d", t.ExitCode)
		case cs.Ready && cs.State.Running != nil:
			hasRunning = true
			ready++
		}
	}
	if reason == "Completed" && hasRunning {
		reason = "NotReady"
		for _, c := range p.Status.Conditions {
			if c.Type == "Ready" && c.Status == "True" {
				reason = "Running"
			}
		}
	}
	if p.Metadata.DeletionTimestamp != nil {
		reason = "Terminating"
	}
	return []any{p.Metadata.Name, fmt.Sprintf("%d/%d", ready, len(p.Spec.Containers)), reason, strconv.Itoa(restarts),
		since(p.Metadata.CreationTimestamp, now), orNone(p.Status.PodIP), orNone(p.Spec.NodeName), "<none>", "<none>"}
}

func servicePorts(s *apis.Service) string {
	var parts []string
	for _, p := range s.Spec.Ports {
		parts = append(parts, fmt.Sprintf("%d/%s", p.Port, p.Protocol))
	}
	return orNone(strings.Join(parts, ","))
}

func nodeCells(n *apis.Node, now time.Time) []any {
	status := "Unknown"
	for _, c := range n.Status.Conditions {
		if c.Type == "Ready" {
			status = "NotReady"
			if c.Status == "True" {
				status = "Ready"
			}
		}
	}
	var roles []string
	for k := range n.Metadata.Labels {
		if role, ok := strings.CutPrefix(k, "node-role.kubernetes.io/"); ok && role != "" {
			roles = append(roles, role)
		}
	}
	slices.Sort(roles)
	addr := map[string]string{}
	for _, a := range n.Status.Addresses {
		if addr[a.Type] == "" {
			addr[a.Type] = a.Address
		}
	}
	i := n.Status.NodeInfo
	return []any{n.Metadata.Name, status, orNone(strings.Join(roles, ",")), since(n.Metadata.CreationTimestamp, now), i.KubeletVersion,
		orNone(addr["InternalIP"]), orNone(addr["ExternalIP"]), orNone(i.OSImage), orNone(i.KernelVersion), orNone(i.ContainerRuntimeVersion)}
}

func sliceCells(e *apis.EndpointSlice, now time.Time) []any {
	var ports, addrs []string
	for _, p := range e.Ports {
		if p.Port != nil {
			ports = append(ports, strconv.Itoa(int(*p.Port)))
		}
	}
	for _, ep := range e.Endpoints {
		addrs = append(addrs, ep.Addresses...)
	}
	if more := len(addrs) - 3; more > 0 {
		addrs = append(addrs[:3], fmt.Sprintf("+ %d more...", more))
	}
	unset := func(l []string) string {
		if len(l) == 0 {
			return "<unset>"
		}
		return strings.Join(l, ",")
	}
	return []any{e.Metadata.Name, e.AddressType, unset(ports), unset(addrs), since(e.Metadata.CreationTimestamp, now)}
}

// cells returns one row for obj, in the order of tableColumns[kind].
func cells(obj apis.Object, now time.Time) []any {
	m := obj.Meta()
	age := since(m.CreationTimestamp, now)
	switch o := obj.(type) {
	case *apis.Pod:
		return podCells(o, now)
	case *apis.Deployment:
		desired := int32(0)
		if o.Spec.Replicas != nil {
			desired = *o.Spec.Replicas
		}
		names, images := containerColumns(&o.Spec.Template.Spec)
		return []any{m.Name, fmt.Sprintf("%d/%d", o.Status.ReadyReplicas, desired), o.Status.UpdatedReplicas, o.Status.AvailableReplicas,
			age, names, images, formatSelector(o.Spec.Selector)}
	case *apis.ReplicaSet:
		desired := int32(0)
		if o.Spec.Replicas != nil {
			desired = *o.Spec.Replicas
		}
		names, images := containerColumns(&o.Spec.Template.Spec)
		return []any{m.Name, desired, o.Status.Replicas, o.Status.ReadyReplicas, age, names, images, formatSelector(o.Spec.Selector)}
	case *apis.Service:
		return []any{m.Name, o.Spec.Type, orNone(o.Spec.ClusterIP), "<none>", servicePorts(o), age,
			formatSelector(&apis.LabelSelector{MatchLabels: o.Spec.Selector})}
	case *apis.Node:
		return nodeCells(o, now)
	case *apis.Namespace:
		return []any{m.Name, o.Status.Phase, age}
	case *apis.Event:
		last := o.LastTimestamp
		if last == nil {
			last = o.FirstTimestamp
		}
		return []any{since(last, now), o.EventType, o.Reason, strings.ToLower(o.InvolvedObject.Kind) + "/" + o.InvolvedObject.Name, o.Message,
			since(o.FirstTimestamp, now), strconv.Itoa(int(o.Count)), m.Name}
	case *apis.EndpointSlice:
		return sliceCells(o, now)
	}
	return []any{m.Name, age}
}

// tableRowFor builds the row for one stored object.
func tableRowFor(res *apis.Resource, data []byte, opts *tableOpts, now time.Time) (tableRow, error) {
	obj := res.New()
	if err := json.Unmarshal(data, obj); err != nil {
		return tableRow{}, fmt.Errorf("stored %s is not readable: %v", res.Kind, err)
	}
	row := tableRow{Cells: cells(obj, now)}
	switch opts.includeObject {
	case "Object":
		row.Object = data
	case "Metadata":
		var part struct {
			Metadata json.RawMessage `json:"metadata"`
		}
		json.Unmarshal(data, &part)
		row.Object = json.RawMessage(`{"kind":"PartialObjectMetadata","apiVersion":"meta.k8s.io/v1","metadata":` + string(part.Metadata) + `}`)
	}
	return row, nil
}

// newTable starts a Table for res. Watch events after the first leave the columns out.
func newTable(res *apis.Resource, rev string, withColumns bool) *table {
	t := &table{Kind: "Table", APIVersion: "meta.k8s.io/v1", Rows: []tableRow{}}
	t.Metadata.ResourceVersion = rev
	if withColumns {
		t.ColumnDefinitions = tableColumns[res.Kind]
	}
	return t
}
