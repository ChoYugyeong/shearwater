package apis

import (
	"encoding/json"
	"fmt"
)

// StoredField is a "stored only" field that is set on an object (design 3.3):
// Shearwater keeps it and does nothing with it. The API answers with a Warning
// header for each, so nobody waits for an effect that will not come.
type StoredField struct {
	Field string
	Value string // the value as JSON; lets an update warn only about what it changed
	Note  string // why it has no effect
}

func (f StoredField) Warning() string {
	return f.Field + ": stored, but has no effect: " + f.Note
}

const (
	noteScheduling = "there is one node and no scheduling rules are evaluated"
	noteProbe      = "probes are not run yet (planned for stage 3); Ready means the container is running"
	noteSecurity   = "containers run with the runtime's default security settings"
)

// StoredOnly lists the stored-only fields that are set on o. A field holding
// the value Kubernetes would default it to (dnsPolicy: ClusterFirst,
// sessionAffinity: None) is not listed: kubectl writes those on its own.
func StoredOnly(o Object) []StoredField {
	var out []StoredField
	add := func(field string, value any, note string) {
		b, _ := json.Marshal(value)
		out = append(out, StoredField{Field: field, Value: string(b), Note: note})
	}
	podSpec := func(s *PodSpec, path string) {
		if s.DNSPolicy != "" && s.DNSPolicy != "ClusterFirst" {
			add(path+".dnsPolicy", s.DNSPolicy, "there is no cluster DNS in stage 1")
		}
		if len(s.NodeSelector) > 0 {
			add(path+".nodeSelector", s.NodeSelector, noteScheduling)
		}
		if !rawEmpty(s.Affinity) {
			add(path+".affinity", s.Affinity, noteScheduling)
		}
		if !rawEmpty(s.Tolerations) {
			add(path+".tolerations", s.Tolerations, noteScheduling)
		}
		if s.PriorityClassName != "" {
			add(path+".priorityClassName", s.PriorityClassName, "there are no priority classes and no preemption")
		}
		if !rawEmpty(s.SecurityContext) {
			add(path+".securityContext", s.SecurityContext, noteSecurity)
		}
		for i := range s.Containers {
			c := &s.Containers[i]
			cp := fmt.Sprintf("%s.containers[%d]", path, i)
			if len(c.Resources.Requests) > 0 {
				add(cp+".resources.requests", c.Resources.Requests, "only limits are enforced; requests are not used for placement")
			}
			if !rawEmpty(c.LivenessProbe) {
				add(cp+".livenessProbe", c.LivenessProbe, noteProbe)
			}
			if !rawEmpty(c.ReadinessProbe) {
				add(cp+".readinessProbe", c.ReadinessProbe, noteProbe)
			}
			if !rawEmpty(c.StartupProbe) {
				add(cp+".startupProbe", c.StartupProbe, noteProbe)
			}
			if !rawEmpty(c.SecurityContext) {
				add(cp+".securityContext", c.SecurityContext, noteSecurity)
			}
		}
	}
	switch v := o.(type) {
	case *Pod:
		podSpec(&v.Spec, "spec")
	case *Deployment:
		if v.Spec.MinReadySeconds != 0 {
			add("spec.minReadySeconds", v.Spec.MinReadySeconds, "a pod counts as available as soon as it is ready")
		}
		if v.Spec.ProgressDeadlineSeconds != nil {
			add("spec.progressDeadlineSeconds", *v.Spec.ProgressDeadlineSeconds, "a stalled rollout is not marked as failed")
		}
		podSpec(&v.Spec.Template.Spec, "spec.template.spec")
	case *ReplicaSet:
		if v.Spec.MinReadySeconds != 0 {
			add("spec.minReadySeconds", v.Spec.MinReadySeconds, "a pod counts as available as soon as it is ready")
		}
		podSpec(&v.Spec.Template.Spec, "spec.template.spec")
	case *Service:
		if a := v.Spec.SessionAffinity; a != "" && a != "None" {
			add("spec.sessionAffinity", a, "the service proxy does not pin a client to one pod")
		}
		if v.Spec.ClusterIP == "None" {
			add("spec.clusterIP", "None", "a headless service needs cluster DNS, which stage 1 does not have")
		}
	}
	return out
}

// NewlyStored returns the warnings for stored-only fields that are set on obj
// and were not set to the same value on old (old may be nil).
func NewlyStored(obj, old Object) []string {
	had := map[string]string{}
	if old != nil {
		for _, f := range StoredOnly(old) {
			had[f.Field] = f.Value
		}
	}
	var out []string
	for _, f := range StoredOnly(obj) {
		if v, ok := had[f.Field]; !ok || v != f.Value {
			out = append(out, f.Warning())
		}
	}
	return out
}
