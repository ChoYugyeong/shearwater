package protobuf

// Wire schemas for kubectl's typed RBAC, quota and identity clients.
func init() {
	rule := &message{"PolicyRule", map[int]field{
		1: strs("verbs"), 2: strs("apiGroups"), 3: strs("resources"), 4: strs("resourceNames"), 5: strs("nonResourceURLs"),
	}}
	aggregation := &message{"AggregationRule", map[int]field{1: msgs("clusterRoleSelectors", labelSelector)}}
	subject := &message{"Subject", map[int]field{1: str("kind"), 2: str("apiGroup"), 3: str("name"), 4: str("namespace")}}
	ref := &message{"RoleRef", map[int]field{1: str("apiGroup"), 2: str("kind"), 3: str("name")}}
	const rbac = "rbac.authorization.k8s.io/v1 "
	roots[rbac+"Role"] = &message{"Role", map[int]field{1: msg("metadata", objectMeta), 2: msgs("rules", rule)}}
	roots[rbac+"ClusterRole"] = &message{"ClusterRole", map[int]field{
		1: msg("metadata", objectMeta), 2: msgs("rules", rule), 3: msgPtr("aggregationRule", aggregation),
	}}
	for _, kind := range []string{"RoleBinding", "ClusterRoleBinding"} {
		roots[rbac+kind] = &message{kind, map[int]field{1: msg("metadata", objectMeta), 2: msgs("subjects", subject), 3: msg("roleRef", ref)}}
	}
	scopeRequirement := &message{"ScopedResourceSelectorRequirement", map[int]field{
		1: str("scopeName"), 2: str("operator"), 3: strs("values"),
	}}
	scopes := &message{"ScopeSelector", map[int]field{1: msgs("matchExpressions", scopeRequirement)}}
	quota := &message{"ResourceQuotaSpec", map[int]field{1: qtyMap("hard"), 2: strs("scopes"), 3: msgPtr("scopeSelector", scopes)}}
	roots["v1 ResourceQuota"] = &message{"ResourceQuota", map[int]field{1: msg("metadata", objectMeta), 2: msg("spec", quota), 3: status()}}

	resource := &message{"ResourceAttributes", map[int]field{
		1: str("namespace"), 2: str("verb"), 3: str("group"), 4: str("version"),
		5: str("resource"), 6: str("subresource"), 7: str("name"),
	}}
	url := &message{"NonResourceAttributes", map[int]field{1: str("path"), 2: str("verb")}}
	access := &message{"SelfSubjectAccessReviewSpec", map[int]field{1: msgPtr("resourceAttributes", resource), 2: msgPtr("nonResourceAttributes", url)}}
	rules := &message{"SelfSubjectRulesReviewSpec", map[int]field{1: str("namespace")}}
	const authorization = "authorization.k8s.io/v1 "
	roots[authorization+"SelfSubjectAccessReview"] = &message{"SelfSubjectAccessReview", map[int]field{1: msg("metadata", objectMeta), 2: msg("spec", access), 3: status()}}
	roots[authorization+"SelfSubjectRulesReview"] = &message{"SelfSubjectRulesReview", map[int]field{1: msg("metadata", objectMeta), 2: msg("spec", rules), 3: status()}}
	roots["authentication.k8s.io/v1 SelfSubjectReview"] = &message{"SelfSubjectReview", map[int]field{1: msg("metadata", objectMeta), 2: status()}}
}
