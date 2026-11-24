package apis

import (
	"fmt"
	"net/netip"
	"slices"
	"strconv"
)

// Scheduling fields of Pods and Nodes (docs/design-multinode-tenancy.md 3.4).

var taintEffects = []string{"NoSchedule", "PreferNoSchedule", "NoExecute"}

func validateScheduling(s *PodSpec, path string, errs *ErrorList) {
	for k, v := range s.NodeSelector {
		validateLabelKV(k, v, path+".nodeSelector", errs)
	}
	for i, t := range s.Tolerations {
		p := fmt.Sprintf("%s.tolerations[%d]", path, i)
		switch t.Operator {
		case "", "Equal":
			if t.Key == "" {
				errs.invalid(p+".operator", t.Operator, "operator must be Exists when key is empty, which means \"match all values and all keys\"")
			}
		case "Exists":
			if t.Value != "" {
				errs.invalid(p+".value", t.Value, "value must be empty when operator is Exists")
			}
		default:
			errs.oneOf(p+".operator", t.Operator, "Equal", "Exists")
		}
		if t.Effect != "" && !slices.Contains(taintEffects, t.Effect) {
			errs.oneOf(p+".effect", t.Effect, taintEffects...)
		}
		if t.TolerationSeconds != nil && t.Effect != "NoExecute" {
			errs.invalid(p+".effect", t.Effect, "effect must be NoExecute when tolerationSeconds is set")
		}
	}
	if s.Affinity == nil || s.Affinity.NodeAffinity == nil {
		return
	}
	na := s.Affinity.NodeAffinity
	ap := path + ".affinity.nodeAffinity"
	if r := na.RequiredDuringSchedulingIgnoredDuringExecution; r != nil {
		rp := ap + ".requiredDuringSchedulingIgnoredDuringExecution"
		if len(r.NodeSelectorTerms) == 0 {
			errs.required(rp + ".nodeSelectorTerms")
		}
		for i := range r.NodeSelectorTerms {
			validateNodeSelectorTerm(&r.NodeSelectorTerms[i], fmt.Sprintf("%s.nodeSelectorTerms[%d]", rp, i), errs)
		}
	}
	for i, pref := range na.PreferredDuringSchedulingIgnoredDuringExecution {
		pp := fmt.Sprintf("%s.preferredDuringSchedulingIgnoredDuringExecution[%d]", ap, i)
		if pref.Weight < 1 || pref.Weight > 100 {
			errs.invalid(pp+".weight", pref.Weight, "must be in the range 1-100")
		}
		validateNodeSelectorTerm(&pref.Preference, pp+".preference", errs)
	}
}

func validateLabelKV(k, v, path string, errs *ErrorList) {
	validateLabels(map[string]string{k: v}, path, errs)
}

func validateNodeSelectorTerm(t *NodeSelectorTerm, path string, errs *ErrorList) {
	for i, r := range t.MatchExpressions {
		validateNodeSelectorRequirement(r, fmt.Sprintf("%s.matchExpressions[%d]", path, i), errs)
	}
	for i, r := range t.MatchFields {
		p := fmt.Sprintf("%s.matchFields[%d]", path, i)
		if r.Key != "metadata.name" {
			errs.invalid(p+".key", r.Key, "metadata.name is the only supported field")
		}
		if (r.Operator != "In" && r.Operator != "NotIn") || len(r.Values) != 1 {
			errs.invalid(p, r.Operator, "matchFields must use In or NotIn with exactly one value")
		}
	}
}

func validateNodeSelectorRequirement(r NodeSelectorRequirement, path string, errs *ErrorList) {
	if r.Key == "" {
		errs.required(path + ".key")
	}
	switch r.Operator {
	case "In", "NotIn":
		if len(r.Values) == 0 {
			errs.required(path + ".values")
		}
	case "Exists", "DoesNotExist":
		if len(r.Values) > 0 {
			errs.forbidden(path+".values", "may not be specified when `operator` is 'Exists' or 'DoesNotExist'")
		}
	case "Gt", "Lt":
		if len(r.Values) != 1 {
			errs.required(path + ".values")
		} else if _, err := strconv.ParseInt(r.Values[0], 10, 64); err != nil {
			errs.invalid(path+".values[0]", r.Values[0], "must be an integer for Gt and Lt")
		}
	default:
		errs.oneOf(path+".operator", r.Operator, "In", "NotIn", "Exists", "DoesNotExist", "Gt", "Lt")
	}
}

// ValidateNode checks what an administrator may set on a Node.
func ValidateNode(n *Node) ErrorList {
	var errs ErrorList
	validateMeta(&n.Metadata, IsDNS1123Subdomain, nameSubdomain, &errs)
	if n.Spec.PodCIDR != "" {
		if p, err := netip.ParsePrefix(n.Spec.PodCIDR); err != nil || !p.Addr().Is4() {
			errs.invalid("spec.podCIDR", n.Spec.PodCIDR, "must be an IPv4 CIDR")
		}
	}
	seen := map[string]bool{}
	for i, t := range n.Spec.Taints {
		p := fmt.Sprintf("spec.taints[%d]", i)
		if t.Key == "" {
			errs.required(p + ".key")
		}
		validateLabelKV(t.Key, t.Value, p, &errs)
		if !slices.Contains(taintEffects, t.Effect) {
			errs.oneOf(p+".effect", t.Effect, taintEffects...)
		}
		if seen[t.Key+":"+t.Effect] {
			errs.duplicate(p, t.Key+":"+t.Effect)
		}
		seen[t.Key+":"+t.Effect] = true
	}
	return errs
}

// ValidateNodeUpdate keeps the pod range: Pods already have addresses from it.
func ValidateNodeUpdate(n, old *Node) ErrorList {
	errs := ValidateNode(n)
	if old.Spec.PodCIDR != "" && n.Spec.PodCIDR != old.Spec.PodCIDR {
		errs.forbidden("spec.podCIDR", "the pod range cannot change once set; Pods on the node have addresses from it")
	}
	return errs
}

// Tolerates reports whether the tolerations accept the taint.
func Tolerates(tols []Toleration, t Taint) bool {
	for _, tol := range tols {
		if tol.Effect != "" && tol.Effect != t.Effect {
			continue
		}
		if tol.Key == "" && tol.Operator == "Exists" {
			return true
		}
		if tol.Key != t.Key {
			continue
		}
		if tol.Operator == "Exists" || tol.Value == t.Value {
			return true
		}
	}
	return false
}

// TolerationSeconds is how long the tolerations let a Pod stay on a node
// with the NoExecute taint t: -1 forever, 0 not at all.
func TolerationSeconds(tols []Toleration, t Taint) int64 {
	best := int64(0)
	for _, tol := range tols {
		if !Tolerates([]Toleration{tol}, t) {
			continue
		}
		if tol.TolerationSeconds == nil {
			return -1
		}
		best = max(best, *tol.TolerationSeconds)
	}
	return best
}
