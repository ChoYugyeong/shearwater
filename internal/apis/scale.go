package apis

import (
	"cmp"
	"slices"
	"strings"
)

// Scale is the autoscaling/v1 view of a workload's replica counts and selector.
type Scale struct {
	TypeMeta
	Metadata ObjectMeta  `json:"metadata"`
	Spec     ScaleSpec   `json:"spec"`
	Status   ScaleStatus `json:"status"`
}

type ScaleSpec struct {
	Replicas int32 `json:"replicas"`
}

type ScaleStatus struct {
	Replicas int32  `json:"replicas"`
	Selector string `json:"selector,omitempty"`
}

func (s *Scale) Meta() *ObjectMeta { return &s.Metadata }

func (d *Deployment) Scale() (*Scale, error) {
	return newScale(&d.Metadata, d.Spec.Replicas, d.Status.Replicas, d.Spec.Selector)
}

func (d *Deployment) SetScaleReplicas(replicas int32) { d.Spec.Replicas = &replicas }

func (r *ReplicaSet) Scale() (*Scale, error) {
	return newScale(&r.Metadata, r.Spec.Replicas, r.Status.Replicas, r.Spec.Selector)
}

func (r *ReplicaSet) SetScaleReplicas(replicas int32) { r.Spec.Replicas = &replicas }

func newScale(meta *ObjectMeta, replicas *int32, observed int32, ls *LabelSelector) (*Scale, error) {
	selector, err := scaleSelector(ls)
	if err != nil {
		return nil, err
	}
	desired := int32(1) // the workload default for an omitted replica count
	if replicas != nil {
		desired = *replicas
	}
	return &Scale{
		TypeMeta: TypeMeta{Kind: ScaleResource.Kind, APIVersion: ScaleResource.APIVersion()},
		Metadata: ObjectMeta{
			Name: meta.Name, Namespace: meta.Namespace, UID: meta.UID,
			ResourceVersion: meta.ResourceVersion, CreationTimestamp: meta.CreationTimestamp,
		},
		Spec:   ScaleSpec{Replicas: desired},
		Status: ScaleStatus{Replicas: observed, Selector: selector},
	}, nil
}

// scaleSelector follows LabelSelectorAsSelector in Kubernetes v1.36.6:
// sort requirements by key, render matchLabels with =, and sort set values.
func scaleSelector(ls *LabelSelector) (string, error) {
	if ls == nil {
		return "", nil
	}
	if _, err := SelectorFrom(ls); err != nil {
		return "", err
	}
	type term struct{ key, text string }
	terms := make([]term, 0, len(ls.MatchLabels)+len(ls.MatchExpressions))
	for key, value := range ls.MatchLabels {
		terms = append(terms, term{key, key + "=" + value})
	}
	for _, expr := range ls.MatchExpressions {
		part := expr.Key
		switch expr.Operator {
		case OpIn, OpNotIn:
			values := slices.Clone(expr.Values)
			slices.Sort(values)
			part += " " + strings.ToLower(expr.Operator) + " (" + strings.Join(values, ",") + ")"
		case OpDoesNotExist:
			part = "!" + part
		}
		terms = append(terms, term{expr.Key, part})
	}
	slices.SortStableFunc(terms, func(a, b term) int { return cmp.Compare(a.key, b.key) })
	parts := make([]string, len(terms))
	for i, term := range terms {
		parts[i] = term.text
	}
	return strings.Join(parts, ","), nil
}

func ValidateScale(s *Scale) ErrorList {
	var errs ErrorList
	validateMeta(&s.Metadata, IsDNS1123Subdomain, nameSubdomain, &errs)
	if s.Spec.Replicas < 0 {
		errs.invalid("spec.replicas", s.Spec.Replicas, "must be 0 or more")
	}
	return errs
}
