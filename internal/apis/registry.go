package apis

// Resource describes one API resource: where it lives, what may be done to it,
// and how its objects are prepared and checked.
type Resource struct {
	Group, Version string
	Plural         string
	Singular       string
	Kind           string
	ShortNames     []string
	Categories     []string
	Namespaced     bool

	Create, Update, Delete bool // through the API; get and list always work
	StatusSub              bool // has /status
	ScaleSub               bool // has /scale

	// AnnotationsMoveGeneration: metadata.generation goes up when the
	// annotations change, not only when the spec does. Kubernetes does this for
	// Deployments, because a Deployment's annotations are copied to its
	// ReplicaSets, so a controller has to see the change. Codex checked this
	// against PrepareForUpdate in pkg/registry/apps/deployment/strategy.go at
	// Kubernetes v1.36.6 (2026-11-11). The other kinds were not checked.
	AnnotationsMoveGeneration bool

	// Fields usable in fieldSelector besides metadata.name and metadata.namespace.
	Fields []string

	New            func() Object
	Prepare        func(Object) // set the initial status of a new object
	Default        func(Object)
	Validate       func(Object) ErrorList
	ValidateUpdate func(obj, old Object) ErrorList
}

func (r *Resource) APIVersion() string {
	if r.Group == "" {
		return r.Version
	}
	return r.Group + "/" + r.Version
}

func (r *Resource) SelectableFields() []string {
	base := []string{"metadata.name"}
	if r.Namespaced {
		base = append(base, "metadata.namespace")
	}
	return append(base, r.Fields...)
}

var all = []string{"all"}

// ScaleResource describes the representation served by workload /scale routes.
var ScaleResource = &Resource{
	Group: "autoscaling", Version: "v1", Kind: "Scale",
	New: func() Object { return &Scale{} },
}

var Resources = []*Resource{
	{
		Version: "v1", Plural: "namespaces", Singular: "namespace", Kind: "Namespace", ShortNames: []string{"ns"},
		Create: true, Delete: true,
		New:      func() Object { return &Namespace{} },
		Prepare:  func(o Object) { o.(*Namespace).Status = NamespaceStatus{Phase: "Active"} },
		Validate: func(o Object) ErrorList { return ValidateNamespace(o.(*Namespace)) },
	},
	{
		Version: "v1", Plural: "nodes", Singular: "node", Kind: "Node", ShortNames: []string{"no"},
		New: func() Object { return &Node{} },
	},
	{
		Version: "v1", Plural: "pods", Singular: "pod", Kind: "Pod", ShortNames: []string{"po"}, Categories: all,
		Namespaced: true, Create: true, Update: true, Delete: true, StatusSub: true,
		Fields:         []string{"spec.nodeName", "status.phase"},
		New:            func() Object { return &Pod{} },
		Prepare:        func(o Object) { o.(*Pod).Status = PodStatus{Phase: "Pending"} },
		Default:        func(o Object) { DefaultPod(o.(*Pod)) },
		Validate:       func(o Object) ErrorList { return ValidatePod(o.(*Pod)) },
		ValidateUpdate: func(o, old Object) ErrorList { return ValidatePodUpdate(o.(*Pod), old.(*Pod)) },
	},
	{
		Version: "v1", Plural: "services", Singular: "service", Kind: "Service", ShortNames: []string{"svc"}, Categories: all,
		Namespaced: true, Create: true, Update: true, Delete: true,
		New:            func() Object { return &Service{} },
		Prepare:        func(o Object) { o.(*Service).Status = ServiceStatus{} },
		Default:        func(o Object) { DefaultService(o.(*Service)) },
		Validate:       func(o Object) ErrorList { return ValidateService(o.(*Service)) },
		ValidateUpdate: func(o, old Object) ErrorList { return ValidateServiceUpdate(o.(*Service), old.(*Service)) },
	},
	{
		Version: "v1", Plural: "events", Singular: "event", Kind: "Event", ShortNames: []string{"ev"},
		Namespaced: true,
		Fields: []string{"involvedObject.kind", "involvedObject.namespace", "involvedObject.name", "involvedObject.uid",
			"involvedObject.apiVersion", "involvedObject.resourceVersion", "involvedObject.fieldPath"},
		New: func() Object { return &Event{} },
	},
	{
		Group: "apps", Version: "v1", Plural: "deployments", Singular: "deployment", Kind: "Deployment",
		ShortNames: []string{"deploy"}, Categories: all,
		Namespaced: true, Create: true, Update: true, Delete: true, StatusSub: true, ScaleSub: true,
		AnnotationsMoveGeneration: true,
		New:                       func() Object { return &Deployment{} },
		Prepare:                   func(o Object) { o.(*Deployment).Status = DeploymentStatus{} },
		Default:                   func(o Object) { DefaultDeployment(o.(*Deployment)) },
		Validate:                  func(o Object) ErrorList { return ValidateDeployment(o.(*Deployment)) },
		ValidateUpdate:            func(o, old Object) ErrorList { return ValidateDeploymentUpdate(o.(*Deployment), old.(*Deployment)) },
	},
	{
		Group: "apps", Version: "v1", Plural: "replicasets", Singular: "replicaset", Kind: "ReplicaSet",
		ShortNames: []string{"rs"}, Categories: all,
		Namespaced: true, Create: true, Update: true, Delete: true, StatusSub: true, ScaleSub: true,
		New:            func() Object { return &ReplicaSet{} },
		Prepare:        func(o Object) { o.(*ReplicaSet).Status = ReplicaSetStatus{} },
		Default:        func(o Object) { DefaultReplicaSet(o.(*ReplicaSet)) },
		Validate:       func(o Object) ErrorList { return ValidateReplicaSet(o.(*ReplicaSet)) },
		ValidateUpdate: func(o, old Object) ErrorList { return ValidateReplicaSetUpdate(o.(*ReplicaSet), old.(*ReplicaSet)) },
	},
	{
		Group: "discovery.k8s.io", Version: "v1", Plural: "endpointslices", Singular: "endpointslice", Kind: "EndpointSlice",
		Namespaced: true,
		New:        func() Object { return &EndpointSlice{} },
	},
}

func Lookup(group, version, plural string) *Resource {
	for _, r := range Resources {
		if r.Group == group && r.Version == version && r.Plural == plural {
			return r
		}
	}
	return nil
}
