package apis

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
)

type ResourceQuota struct {
	TypeMeta
	Metadata ObjectMeta          `json:"metadata"`
	Spec     ResourceQuotaSpec   `json:"spec"`
	Status   ResourceQuotaStatus `json:"status,omitzero"`
}

func (o *ResourceQuota) Meta() *ObjectMeta { return &o.Metadata }

type ResourceQuotaSpec struct {
	Hard          map[string]Quantity `json:"hard,omitempty"`
	Scopes        []string            `json:"scopes,omitempty"`
	ScopeSelector json.RawMessage     `json:"scopeSelector,omitempty"`
}

type ResourceQuotaStatus struct {
	Hard map[string]Quantity `json:"hard,omitempty"`
	Used map[string]Quantity `json:"used,omitempty"`
}

// QuotaResourceSupported reports which quota limits Shearwater enforces.
func QuotaResourceSupported(key string) bool {
	switch key {
	case "pods", "services", "secrets", "persistentvolumeclaims", "requests.storage",
		"requests.cpu", "requests.memory", "limits.cpu", "limits.memory":
		return true
	}
	return strings.HasPrefix(key, "count/") && len(key) > len("count/")
}

func (o *ResourceQuota) StoredOnlyFields() []StoredField {
	var fields []StoredField
	for _, key := range slices.Sorted(maps.Keys(o.Spec.Hard)) {
		if !QuotaResourceSupported(key) {
			value, _ := json.Marshal(o.Spec.Hard[key])
			fields = append(fields, StoredField{Field: "spec.hard[" + key + "]", Value: string(value), Note: "this resource is not accounted for"})
		}
	}
	return fields
}

func (o *ResourceQuota) validate() ErrorList {
	var errs ErrorList
	validateMeta(o.Meta(), IsDNS1123Subdomain, nameSubdomain, &errs)
	if len(o.Spec.Scopes) != 0 {
		errs.unsupported("spec.scopes", "only namespace-wide resource quotas are supported")
	}
	if !rawEmpty(o.Spec.ScopeSelector) {
		errs.unsupported("spec.scopeSelector", "only namespace-wide resource quotas are supported")
	}
	for key, limit := range o.Spec.Hard {
		value, err := ParseQuantity(string(limit))
		field := "spec.hard[" + key + "]"
		if err != nil || value.Sign() < 0 {
			errs.invalid(field, string(limit), "must be a non-negative quantity")
			continue
		}
		if strings.HasPrefix(key, "count/") || key == "pods" || key == "services" || key == "secrets" || key == "persistentvolumeclaims" {
			if !value.IsInt() {
				errs.invalid(field, string(limit), "must be a whole number of objects")
			}
		}
	}
	return errs
}

func init() {
	Register(&Resource{
		Version: "v1", Plural: "resourcequotas", Singular: "resourcequota", Kind: "ResourceQuota", ShortNames: []string{"quota"},
		Namespaced: true, Create: true, Update: true, Delete: true, StatusSub: true,
		New:      func() Object { return &ResourceQuota{} },
		Prepare:  func(o Object) { o.(*ResourceQuota).Status = ResourceQuotaStatus{} },
		Validate: func(o Object) ErrorList { return o.(*ResourceQuota).validate() },
	})
}
