package apis

const (
	AuthorizationGroup  = "authorization.k8s.io"
	AuthenticationGroup = "authentication.k8s.io"
)

type ResourceAttributes struct {
	Namespace   string `json:"namespace,omitempty"`
	Verb        string `json:"verb,omitempty"`
	Group       string `json:"group,omitempty"`
	Version     string `json:"version,omitempty"`
	Resource    string `json:"resource,omitempty"`
	Subresource string `json:"subresource,omitempty"`
	Name        string `json:"name,omitempty"`
}

type NonResourceAttributes struct {
	Path string `json:"path,omitempty"`
	Verb string `json:"verb,omitempty"`
}

type SelfSubjectAccessReview struct {
	TypeMeta
	Metadata ObjectMeta `json:"metadata"`
	Spec     struct {
		ResourceAttributes    *ResourceAttributes    `json:"resourceAttributes,omitempty"`
		NonResourceAttributes *NonResourceAttributes `json:"nonResourceAttributes,omitempty"`
	} `json:"spec"`
	Status struct {
		Allowed bool   `json:"allowed"`
		Reason  string `json:"reason,omitempty"`
	} `json:"status"`
}

func (r *SelfSubjectAccessReview) Meta() *ObjectMeta { return &r.Metadata }

type SelfSubjectRulesReview struct {
	TypeMeta
	Metadata ObjectMeta `json:"metadata"`
	Spec     struct {
		Namespace string `json:"namespace"`
	} `json:"spec"`
	Status struct {
		ResourceRules    []PolicyRule `json:"resourceRules"`
		NonResourceRules []PolicyRule `json:"nonResourceRules"`
		Incomplete       bool         `json:"incomplete"`
		EvaluationError  string       `json:"evaluationError,omitempty"`
	} `json:"status"`
}

func (r *SelfSubjectRulesReview) Meta() *ObjectMeta { return &r.Metadata }

type SelfSubjectReview struct {
	TypeMeta
	Metadata ObjectMeta `json:"metadata"`
	Status   struct {
		UserInfo struct {
			Username string   `json:"username"`
			Groups   []string `json:"groups,omitempty"`
		} `json:"userInfo"`
	} `json:"status"`
}

func (r *SelfSubjectReview) Meta() *ObjectMeta { return &r.Metadata }

func init() {
	for _, r := range []*Resource{
		{Group: AuthorizationGroup, Plural: "selfsubjectaccessreviews", Singular: "selfsubjectaccessreview", Kind: "SelfSubjectAccessReview", New: func() Object { return &SelfSubjectAccessReview{} }},
		{Group: AuthorizationGroup, Plural: "selfsubjectrulesreviews", Singular: "selfsubjectrulesreview", Kind: "SelfSubjectRulesReview", New: func() Object { return &SelfSubjectRulesReview{} }},
		{Group: AuthenticationGroup, Plural: "selfsubjectreviews", Singular: "selfsubjectreview", Kind: "SelfSubjectReview", New: func() Object { return &SelfSubjectReview{} }},
	} {
		r.Version, r.Create = "v1", true
		Register(r)
	}
}
