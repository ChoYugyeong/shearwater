// Package rbac evaluates Kubernetes roles and bindings.
package rbac

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"

	"shearwater/internal/admission"
	"shearwater/internal/apis"
	"shearwater/internal/auth"
	"shearwater/internal/store"
)

type Evaluator struct {
	store   *store.Store
	mu      sync.Mutex
	changed <-chan struct{}
	index   *index
}

func New(s *store.Store) *Evaluator { return &Evaluator{store: s} }

type subjectKey struct{ namespace, kind, name string }
type index struct {
	roles  map[store.Key][]apis.PolicyRule
	grants map[subjectKey][]apis.PolicyRule
}

func roleKey(namespace string, ref apis.RoleRef) store.Key {
	if ref.Kind == "ClusterRole" {
		namespace = ""
	}
	return store.Key{Group: apis.RBACGroup, Resource: strings.ToLower(ref.Kind) + "s", Namespace: namespace, Name: ref.Name}
}

func (e *Evaluator) load() (*index, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for {
		ch := e.store.Changed()
		if e.changed == ch {
			return e.index, nil
		}
		idx := &index{roles: map[store.Key][]apis.PolicyRule{}, grants: map[subjectKey][]apis.PolicyRule{}}
		for _, res := range []string{"roles", "clusterroles", "rolebindings", "clusterrolebindings"} {
			recs, _, err := e.store.List(apis.RBACGroup, res, "")
			if err != nil {
				return nil, err
			}
			for _, rec := range recs {
				if strings.HasSuffix(res, "roles") {
					var role apis.Role
					if err := json.Unmarshal(rec.Data, &role); err != nil {
						return nil, err
					}
					idx.roles[rec.Key] = role.Rules
					continue
				}
				var b apis.RoleBinding
				if err := json.Unmarshal(rec.Data, &b); err != nil {
					return nil, err
				}
				ns := rec.Key.Namespace
				rules := idx.roles[roleKey(ns, b.RoleRef)]
				for _, s := range b.Subjects {
					k := subjectKey{ns, s.Kind, s.Name}
					if s.Kind == "ServiceAccount" {
						saNS := s.Namespace
						if saNS == "" {
							saNS = ns
						}
						k.kind, k.name = "User", "system:serviceaccount:"+saNS+":"+s.Name
					}
					for _, rule := range rules {
						if ns == "" || len(rule.NonResourceURLs) == 0 {
							idx.grants[k] = append(idx.grants[k], rule)
						}
					}
				}
			}
		}
		select {
		case <-ch:
			continue
		default:
			e.index, e.changed = idx, ch
			return idx, nil
		}
	}
}

var selfRules = []apis.PolicyRule{
	{Verbs: []string{"create"}, APIGroups: []string{"authorization.k8s.io"}, Resources: []string{"selfsubjectaccessreviews", "selfsubjectrulesreviews"}},
	{Verbs: []string{"create"}, APIGroups: []string{"authentication.k8s.io"}, Resources: []string{"selfsubjectreviews"}},
	{Verbs: []string{"get"}, NonResourceURLs: []string{"/api", "/api/*", "/apis", "/apis/*", "/openapi/*", "/version"}},
}

func (idx *index) rules(id auth.Identity, ns string) []apis.PolicyRule {
	if id.IsAdmin() {
		return adminAll()
	}
	out := slices.Clone(selfRules)
	scopes := []string{""}
	if ns != "" {
		scopes = append(scopes, ns)
	}
	for _, scope := range scopes {
		out = append(out, idx.grants[subjectKey{scope, "User", id.Name}]...)
		for _, g := range id.Groups {
			out = append(out, idx.grants[subjectKey{scope, "Group", g}]...)
		}
	}
	return out
}

func (e *Evaluator) Rules(id auth.Identity, ns string) ([]apis.PolicyRule, error) {
	idx, err := e.load()
	if err != nil {
		return nil, err
	}
	return idx.rules(id, ns), nil
}

func matches(values []string, value string) bool {
	return slices.Contains(values, "*") || slices.Contains(values, value)
}

func permits(r apis.PolicyRule, a auth.Attributes) bool {
	if !matches(r.Verbs, a.Verb) {
		return false
	}
	if !a.ResourceRequest {
		for _, p := range r.NonResourceURLs {
			if p == a.Path || strings.HasSuffix(p, "*") && strings.HasPrefix(a.Path, strings.TrimSuffix(p, "*")) {
				return true
			}
		}
		return false
	}
	if !matches(r.APIGroups, a.APIGroup) || len(r.ResourceNames) > 0 && (a.Name == "" || !slices.Contains(r.ResourceNames, a.Name)) {
		return false
	}
	res := a.Resource
	if a.Subresource != "" {
		if slices.Contains(r.Resources, "*/"+a.Subresource) {
			return true
		}
		res += "/" + a.Subresource
	}
	return matches(r.Resources, res)
}

func allowed(rules []apis.PolicyRule, a auth.Attributes) bool {
	return slices.ContainsFunc(rules, func(r apis.PolicyRule) bool { return permits(r, a) })
}

func (e *Evaluator) Authorize(id auth.Identity, a auth.Attributes) (bool, string) {
	rules, err := e.Rules(id, a.Namespace)
	if err != nil {
		return false, "RBAC policies could not be read"
	}
	return allowed(rules, a), ""
}

// Check each atomic permission independently: different existing roles may
// cover different verbs or names in the requested rule. An unrestricted name
// maps to the empty name, which only an unrestricted grant can satisfy.
func covers(owned, requested []apis.PolicyRule) bool {
	for _, r := range requested {
		for _, v := range r.Verbs {
			for _, u := range r.NonResourceURLs {
				if !allowed(owned, auth.Attributes{Verb: v, Path: u}) {
					return false
				}
			}
			for _, g := range r.APIGroups {
				for _, resource := range r.Resources {
					res, sub, _ := strings.Cut(resource, "/")
					names := r.ResourceNames
					if len(names) == 0 {
						names = []string{""}
					}
					for _, name := range names {
						if !allowed(owned, auth.Attributes{ResourceRequest: true, Verb: v, APIGroup: g, Resource: res, Subresource: sub, Name: name}) {
							return false
						}
					}
				}
			}
		}
	}
	return true
}

func (e *Evaluator) Admit(req admission.Request) error {
	if req.Resource.Group != apis.RBACGroup || req.Identity.IsAdmin() {
		return nil
	}
	idx, err := e.load()
	if err != nil {
		return err
	}
	owned := idx.rules(req.Identity, req.Namespace)
	action := auth.Attributes{ResourceRequest: true, Verb: "escalate", APIGroup: apis.RBACGroup, Resource: req.Resource.Plural, Name: req.Name}
	var requested []apis.PolicyRule
	switch obj := req.Object.(type) {
	case *apis.Role:
		requested = obj.Rules
	case *apis.RoleBinding:
		key := roleKey(req.Namespace, obj.RoleRef)
		action.Verb, action.Resource, action.Name = "bind", key.Resource, key.Name
		if allowed(owned, action) {
			return nil
		}
		var ok bool
		requested, ok = idx.roles[key]
		if !ok {
			return admission.Forbid("RBAC_BIND", "create the referenced role first", "%s %q not found", key.Resource, key.Name)
		}
	default:
		return fmt.Errorf("unexpected RBAC object %T", req.Object)
	}
	if allowed(owned, action) || covers(owned, requested) {
		return nil
	}
	return admission.Forbid("RBAC_ESCALATION", "ask the administrator to grant the missing permissions", "user %q is attempting to grant RBAC permissions not currently held", req.Identity.Name)
}
