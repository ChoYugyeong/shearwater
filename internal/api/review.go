package api

import (
	"net/http"

	"shearwater/internal/apis"
	"shearwater/internal/auth"
)

type rulesResolver interface {
	Rules(auth.Identity, string) ([]apis.PolicyRule, error)
}

func (s *Server) review(w http.ResponseWriter, r *http.Request, res *apis.Resource, ri reqInfo) error {
	if r.Method != http.MethodPost || ri.name != "" || ri.sub != "" {
		return errMethod(r.Method)
	}
	body, err := readBody(r)
	if err != nil {
		return err
	}
	obj, warnings, err := decodeObject(r.URL.Query(), res, body)
	if err != nil {
		return err
	}
	*obj.Type() = apis.TypeMeta{Kind: res.Kind, APIVersion: res.APIVersion()}
	switch review := obj.(type) {
	case *apis.SelfSubjectReview:
		review.Status.UserInfo.Username = ri.id.Name
		review.Status.UserInfo.Groups = ri.id.Groups
	case *apis.SelfSubjectAccessReview:
		attr, non := review.Spec.ResourceAttributes, review.Spec.NonResourceAttributes
		if (attr == nil) == (non == nil) {
			return errBadRequest("exactly one of spec.resourceAttributes and spec.nonResourceAttributes must be specified").withHint("use kubectl auth can-i VERB RESOURCE")
		}
		var a auth.Attributes
		if attr != nil {
			a = auth.Attributes{ResourceRequest: true, Namespace: attr.Namespace, Verb: attr.Verb, APIGroup: attr.Group, APIVersion: attr.Version, Resource: attr.Resource, Subresource: attr.Subresource, Name: attr.Name}
		} else {
			a = auth.Attributes{Verb: non.Verb, Path: non.Path}
		}
		review.Status.Allowed = ri.id.IsAdmin()
		review.Status.Reason = ""
		if !review.Status.Allowed && s.Authorizer != nil {
			review.Status.Allowed, review.Status.Reason = s.Authorizer.Authorize(ri.id, a)
		}
	case *apis.SelfSubjectRulesReview:
		if review.Spec.Namespace == "" {
			return errBadRequest("spec.namespace: Required value").withHint("pass the namespace to kubectl auth can-i --list -n NAME")
		}
		review.Status.ResourceRules = []apis.PolicyRule{}
		review.Status.NonResourceRules = []apis.PolicyRule{}
		review.Status.Incomplete = false
		review.Status.EvaluationError = ""
		if resolver, ok := s.Authorizer.(rulesResolver); ok {
			rules, err := resolver.Rules(ri.id, review.Spec.Namespace)
			if err != nil {
				return err
			}
			for _, rule := range rules {
				if len(rule.NonResourceURLs) > 0 {
					review.Status.NonResourceRules = append(review.Status.NonResourceRules, rule)
				} else {
					review.Status.ResourceRules = append(review.Status.ResourceRules, rule)
				}
			}
		} else {
			review.Status.Incomplete = true
			review.Status.EvaluationError = "the configured authorizer cannot enumerate rules"
		}
	}
	warn(w, warnings)
	writeJSON(w, http.StatusCreated, obj)
	return nil
}
