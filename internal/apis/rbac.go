package apis

import (
	"encoding/json"
	"fmt"
	"strings"
)

const RBACGroup = "rbac.authorization.k8s.io"

type PolicyRule struct {
	Verbs           []string `json:"verbs"`
	APIGroups       []string `json:"apiGroups,omitempty"`
	Resources       []string `json:"resources,omitempty"`
	ResourceNames   []string `json:"resourceNames,omitempty"`
	NonResourceURLs []string `json:"nonResourceURLs,omitempty"`
}

type Role struct {
	TypeMeta
	Metadata        ObjectMeta      `json:"metadata"`
	Rules           []PolicyRule    `json:"rules"`
	AggregationRule json.RawMessage `json:"aggregationRule,omitempty"`
}

func (r *Role) Meta() *ObjectMeta { return &r.Metadata }

type ClusterRole = Role

type RoleRef struct {
	APIGroup string `json:"apiGroup"`
	Kind     string `json:"kind"`
	Name     string `json:"name"`
}

type Subject struct {
	Kind      string `json:"kind"`
	APIGroup  string `json:"apiGroup,omitempty"`
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
}

type RoleBinding struct {
	TypeMeta
	Metadata ObjectMeta `json:"metadata"`
	RoleRef  RoleRef    `json:"roleRef"`
	Subjects []Subject  `json:"subjects,omitempty"`
}

func (b *RoleBinding) Meta() *ObjectMeta { return &b.Metadata }

type ClusterRoleBinding = RoleBinding

func validRBACName(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.ContainsAny(s, "/%")
}

func validateRBAC(o Object, namespaced bool) ErrorList {
	var errs ErrorList
	validateMeta(o.Meta(), validRBACName, "a valid path segment", &errs)
	switch v := o.(type) {
	case *Role:
		if namespaced && len(v.AggregationRule) != 0 {
			errs.forbidden("aggregationRule", "only ClusterRoles support aggregation")
		}
		for i, rule := range v.Rules {
			f := fmt.Sprintf("rules[%d].", i)
			if len(rule.Verbs) == 0 {
				errs.required(f + "verbs")
			}
			if len(rule.NonResourceURLs) == 0 {
				if len(rule.APIGroups) == 0 {
					errs.required(f + "apiGroups")
				}
				if len(rule.Resources) == 0 {
					errs.required(f + "resources")
				}
				continue
			}
			if namespaced || len(rule.Resources)+len(rule.ResourceNames)+len(rule.APIGroups) > 0 {
				errs.invalid(f+"nonResourceURLs", rule.NonResourceURLs, "non-resource rules must be separate ClusterRole rules")
			}
			for _, u := range rule.NonResourceURLs {
				if u != "*" && (!strings.HasPrefix(u, "/") || strings.Contains(strings.TrimSuffix(u, "*"), "*")) {
					errs.invalid(f+"nonResourceURLs", u, "must be an absolute path with an optional trailing wildcard")
				}
			}
		}
	case *RoleBinding:
		ref := v.RoleRef
		if ref.APIGroup != RBACGroup {
			errs.oneOf("roleRef.apiGroup", ref.APIGroup, RBACGroup)
		}
		if ref.Kind != "ClusterRole" && !(namespaced && ref.Kind == "Role") {
			errs.invalid("roleRef.kind", ref.Kind, "invalid referenced role kind")
		}
		if !validRBACName(ref.Name) {
			errs.invalid("roleRef.name", ref.Name, "must be a valid path segment")
		}
		for i, s := range v.Subjects {
			f := fmt.Sprintf("subjects[%d].", i)
			if s.Name == "" {
				errs.required(f + "name")
			}
			switch s.Kind {
			case "ServiceAccount":
				if s.APIGroup != "" {
					errs.invalid(f+"apiGroup", s.APIGroup, "must be empty for a ServiceAccount")
				}
				if !namespaced && s.Namespace == "" {
					errs.required(f + "namespace")
				}
			case "User", "Group":
				if s.APIGroup != RBACGroup {
					errs.oneOf(f+"apiGroup", s.APIGroup, RBACGroup)
				}
			default:
				errs.oneOf(f+"kind", s.Kind, "User", "Group", "ServiceAccount")
			}
		}
	}
	return errs
}

func init() {
	for _, kind := range []string{"Role", "ClusterRole", "RoleBinding", "ClusterRoleBinding"} {
		namespaced := !strings.HasPrefix(kind, "Cluster")
		singular := strings.ToLower(kind)
		r := &Resource{Group: RBACGroup, Version: "v1", Kind: kind, Singular: singular, Plural: singular + "s",
			Namespaced: namespaced, Create: true, Update: true, Delete: true,
			New:      func() Object { return &Role{} },
			Validate: func(o Object) ErrorList { return validateRBAC(o, namespaced) },
		}
		if strings.HasSuffix(kind, "Binding") {
			r.New = func() Object { return &RoleBinding{} }
			r.Default = func(o Object) {
				b := o.(*RoleBinding)
				if b.RoleRef.APIGroup == "" {
					b.RoleRef.APIGroup = RBACGroup
				}
				for i, s := range b.Subjects {
					if s.APIGroup == "" && (s.Kind == "User" || s.Kind == "Group") {
						b.Subjects[i].APIGroup = RBACGroup
					}
				}
			}
			r.ValidateUpdate = func(o, old Object) ErrorList {
				var errs ErrorList
				if o.(*RoleBinding).RoleRef != old.(*RoleBinding).RoleRef {
					errs.invalid("roleRef", o.(*RoleBinding).RoleRef, "cannot change roleRef")
				}
				return errs
			}
		}
		Register(r)
	}
}
