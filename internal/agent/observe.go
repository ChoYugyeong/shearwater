package agent

// Observations are a small, versioned contract shared by the CLI, MCP and chat.
// They deliberately contain facts and fixed reason codes, not a model's diagnosis
// or arbitrary log/annotation text. Fetching detailed evidence is a separate act.
import (
	"cmp"
	"fmt"
	"slices"
	"time"

	"shearwater/internal/apis"
)

type ObserveOptions struct {
	Namespace string `json:"namespace,omitempty"`
	Resource  string `json:"resource,omitempty"`
	Limit     int    `json:"limit"`
	Offset    int    `json:"offset"`
	Revision  string `json:"revision,omitempty"`
}

type ObservationItem struct {
	Resource   string           `json:"resource"`
	Namespace  string           `json:"namespace"`
	UID        string           `json:"uid"`
	Revision   string           `json:"resourceVersion"`
	Generation int64            `json:"generation"`
	State      string           `json:"state"`
	Reasons    []string         `json:"reasons"`
	Counts     map[string]int64 `json:"counts,omitempty"`
}

type Observation struct {
	Format       string            `json:"format"`
	ObservedAt   time.Time         `json:"observedAt"`
	Revision     string            `json:"revision"`
	Scope        ObserveOptions    `json:"scope"`
	Total        int               `json:"total"`
	States       map[string]int    `json:"states"`
	Items        []ObservationItem `json:"items"`
	Next         *ObserveOptions   `json:"next,omitempty"`
	Reachability string            `json:"reachability"`
}

// Observe paginates a consistent API snapshot, not a live stream. The next page
// is usable only at the same store revision. Otherwise callers must start again;
// silently combining old and new pages can hide a replaced or failing workload.
func (s Snapshot) Observe(o ObserveOptions, now time.Time) (Observation, error) {
	if o.Limit < 1 || o.Limit > 100 || o.Offset < 0 {
		return Observation{}, fmt.Errorf("limit must be 1..100 and offset non-negative")
	}
	if o.Offset > 0 && o.Revision == "" {
		return Observation{}, fmt.Errorf("pagination requires the previous revision")
	}
	if o.Revision != "" && o.Revision != s.Revision {
		return Observation{}, fmt.Errorf("CONFLICT: observation revision changed; restart at offset 0")
	}
	r := Observation{Format: "shearwater.observation/v1", ObservedAt: now.UTC(), Revision: s.Revision,
		Scope: o, States: map[string]int{}, Items: []ObservationItem{}, Reachability: "not_checked"}
	var items []ObservationItem
	add := func(kind string, m *apis.ObjectMeta, state string, reasons []string, counts map[string]int64) {
		ref := kind + "/" + m.Name
		if o.Namespace != "" && m.Namespace != o.Namespace || o.Resource != "" && ref != o.Resource {
			return
		}
		if m.DeletionTimestamp != nil {
			state, reasons = "terminating", []string{"deletion_requested"}
		}
		if m.UID == "" || m.ResourceVersion == "" || m.Generation < 1 {
			state, reasons = "unknown", []string{"missing_identity_or_generation"}
		}
		items = append(items, ObservationItem{ref, m.Namespace, m.UID, m.ResourceVersion, m.Generation, state, reasons, counts})
	}
	for _, d := range s.Deployments {
		n := inspectReplicas(d.Spec.Replicas)
		counts := map[string]int64{"desired": int64(n), "current": int64(d.Status.Replicas), "updated": int64(d.Status.UpdatedReplicas), "ready": int64(d.Status.ReadyReplicas), "available": int64(d.Status.AvailableReplicas), "observedGeneration": d.Status.ObservedGeneration}
		state, reasons := "not_ready", []string{"rollout_incomplete"}
		switch {
		case d.Status.ObservedGeneration != d.Metadata.Generation:
			state, reasons = "unknown", []string{"controller_has_not_observed_current_generation"}
		case d.Spec.Paused:
			state, reasons = "unknown", []string{"deployment_paused"}
		case d.Status.Replicas == n && d.Status.UpdatedReplicas == n && d.Status.ReadyReplicas == n && d.Status.AvailableReplicas == n:
			state, reasons = "ready", []string{"controller_reports_rollout_complete"}
			if n == 0 {
				state, reasons = "scaled_down", []string{"desired_replicas_zero"}
			}
		}
		for _, c := range d.Status.Conditions {
			if c.Type == "Progressing" && c.Status == "False" || c.Type == "ReplicaFailure" && c.Status == "True" {
				state, reasons = "not_ready", []string{"controller_reports_failure"}
			}
		}
		add("deploy", &d.Metadata, state, reasons, counts)
	}
	for _, p := range s.Pods {
		state, reasons := "not_ready", []string{"pod_not_ready"}
		var restarts int64
		for _, c := range p.Status.ContainerStatuses {
			restarts += int64(c.RestartCount)
		}
		switch {
		case inspectPodReady(&p):
			state, reasons = "ready", []string{"pod_condition_ready"}
		case p.Status.Phase == "":
			state, reasons = "unknown", []string{"pod_status_not_reported"}
		case p.Status.Phase == "Succeeded":
			state, reasons = "completed", []string{"pod_completed"}
		case p.Status.Phase == "Failed":
			reasons = []string{"pod_failed"}
		}
		add("pod", &p.Metadata, state, reasons, map[string]int64{"restarts": restarts})
	}
	// Unlike the human tree's compatibility view, verification accepts only
	// slices explicitly tied to the current Service generation. Missing version
	// evidence must never turn an old routing result into a new success.
	var current []apis.EndpointSlice
	for _, e := range s.EndpointSlices {
		if e.Metadata.Annotations[apis.ServiceGenerationAnnotation] != "" {
			current = append(current, e)
		}
	}
	endpoints := readyServiceEndpoints(s.Services, current)
	for _, svc := range s.Services {
		state, reasons := "not_ready", []string{"no_ready_endpoints"}
		n := endpoints[svc.Metadata.UID]
		switch {
		case svc.Spec.ClusterIP == "" || svc.Spec.ClusterIP == "None" || len(svc.Spec.Selector) == 0:
			state, reasons = "unknown", []string{"routed_service_evidence_unavailable"}
		case n > 0:
			state, reasons = "ready", []string{"ready_endpoints_reported"}
		}
		add("svc", &svc.Metadata, state, reasons, map[string]int64{"readyEndpoints": int64(n)})
	}
	slices.SortFunc(items, func(a, b ObservationItem) int {
		return cmp.Or(cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Resource, b.Resource))
	})
	r.Total = len(items)
	for _, item := range items {
		r.States[item.State]++
	}
	if o.Offset > len(items) {
		return Observation{}, fmt.Errorf("offset exceeds result count")
	}
	end := min(len(items), o.Offset+o.Limit)
	r.Items = append(r.Items, items[o.Offset:end]...)
	if end < len(items) {
		next := o
		next.Offset = end
		next.Revision = s.Revision
		r.Next = &next
	}
	return r, nil
}

// Verification proves only this explicit predicate at one observed revision.
// It does not claim an HTTP request succeeded, or that readiness will persist.
// Pinning UID AND generation prevents a replacement or later rollout from
// satisfying an earlier goal just because it has the same human-readable name.
type ReadinessGoal struct {
	Namespace  string `json:"namespace"`
	Resource   string `json:"resource"`
	UID        string `json:"uid"`
	Generation int64  `json:"generation"`
}

type Verification struct {
	Format      string        `json:"format"`
	Goal        ReadinessGoal `json:"goal"`
	State       string        `json:"state"`
	Reason      string        `json:"reason"`
	Observation Observation   `json:"observation"`
}

func (s Snapshot) Verify(g ReadinessGoal, now time.Time) (Verification, error) {
	if g.Namespace == "" || g.Resource == "" || g.UID == "" || g.Generation < 1 {
		return Verification{}, fmt.Errorf("verification requires namespace, resource, uid and positive generation")
	}
	o, err := s.Observe(ObserveOptions{Namespace: g.Namespace, Resource: g.Resource, Limit: 1}, now)
	if err != nil {
		return Verification{}, err
	}
	v := Verification{Format: "shearwater.verification/v1", Goal: g, State: "unknown", Reason: "target_missing", Observation: o}
	if len(o.Items) == 0 {
		return v, nil
	}
	i := o.Items[0]
	switch {
	case i.UID != g.UID || i.Generation != g.Generation:
		v.State, v.Reason = "stale", "target_identity_or_generation_changed"
	case i.State == "ready":
		v.State, v.Reason = "met", "readiness_reported_at_observed_revision"
	case i.State == "unknown":
		v.Reason = "readiness_not_known"
	default:
		v.State, v.Reason = "not_met", i.State
	}
	return v, nil
}
