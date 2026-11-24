package apis

import "encoding/json"

// Fields follow design 3.3. A field is one of: supported, stored only (kept, no
// effect yet), or rejected (present so validation can refuse it with a reason).
// Anything not declared here is an unknown field.

// ---- Pod ----

type Pod struct {
	TypeMeta
	Metadata ObjectMeta `json:"metadata"`
	Spec     PodSpec    `json:"spec"`
	Status   PodStatus  `json:"status,omitzero"`
}

func (o *Pod) Meta() *ObjectMeta { return &o.Metadata }

type PodSpec struct {
	Containers                    []Container `json:"containers" patchStrategy:"merge" patchMergeKey:"name"`
	RestartPolicy                 string      `json:"restartPolicy,omitempty"`
	TerminationGracePeriodSeconds *int64      `json:"terminationGracePeriodSeconds,omitempty"`
	NodeName                      string      `json:"nodeName,omitempty"`
	Volumes                       []Volume    `json:"volumes,omitempty" patchStrategy:"merge,retainKeys" patchMergeKey:"name"`

	// scheduling (docs/design-multinode-tenancy.md 3.4)
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
	Affinity     *Affinity         `json:"affinity,omitempty"`
	Tolerations  []Toleration      `json:"tolerations,omitempty"`

	// cluster DNS (docs/dns.md)
	DNSPolicy string        `json:"dnsPolicy,omitempty"`
	DNSConfig *PodDNSConfig `json:"dnsConfig,omitempty"`

	// stored only
	PriorityClassName string          `json:"priorityClassName,omitempty"`
	SecurityContext   json.RawMessage `json:"securityContext,omitempty"`

	// rejected when set
	InitContainers     json.RawMessage `json:"initContainers,omitempty"`
	HostNetwork        bool            `json:"hostNetwork,omitempty"`
	ServiceAccountName string          `json:"serviceAccountName,omitempty"`
}

// Affinity: node affinity is used for placement; Pod affinity and
// anti-affinity are stored only.
type Affinity struct {
	NodeAffinity    *NodeAffinity   `json:"nodeAffinity,omitempty"`
	PodAffinity     json.RawMessage `json:"podAffinity,omitempty"`
	PodAntiAffinity json.RawMessage `json:"podAntiAffinity,omitempty"`
}

type NodeAffinity struct {
	RequiredDuringSchedulingIgnoredDuringExecution  *NodeSelector             `json:"requiredDuringSchedulingIgnoredDuringExecution,omitempty"`
	PreferredDuringSchedulingIgnoredDuringExecution []PreferredSchedulingTerm `json:"preferredDuringSchedulingIgnoredDuringExecution,omitempty"`
}

// NodeSelector matches a node when any of its terms matches.
type NodeSelector struct {
	NodeSelectorTerms []NodeSelectorTerm `json:"nodeSelectorTerms"`
}

// NodeSelectorTerm matches when all of its requirements match.
type NodeSelectorTerm struct {
	MatchExpressions []NodeSelectorRequirement `json:"matchExpressions,omitempty"`
	MatchFields      []NodeSelectorRequirement `json:"matchFields,omitempty"`
}

type NodeSelectorRequirement struct {
	Key      string   `json:"key"`
	Operator string   `json:"operator"` // In, NotIn, Exists, DoesNotExist, Gt, Lt
	Values   []string `json:"values,omitempty"`
}

type PreferredSchedulingTerm struct {
	Weight     int32            `json:"weight"`
	Preference NodeSelectorTerm `json:"preference"`
}

type Toleration struct {
	Key               string `json:"key,omitempty"`
	Operator          string `json:"operator,omitempty"` // Exists or Equal (default)
	Value             string `json:"value,omitempty"`
	Effect            string `json:"effect,omitempty"` // NoSchedule, PreferNoSchedule, NoExecute; empty: all
	TolerationSeconds *int64 `json:"tolerationSeconds,omitempty"`
}

type Container struct {
	Name            string               `json:"name"`
	Image           string               `json:"image,omitempty"`
	Command         []string             `json:"command,omitempty"`
	Args            []string             `json:"args,omitempty"`
	WorkingDir      string               `json:"workingDir,omitempty"`
	Ports           []ContainerPort      `json:"ports,omitempty" patchStrategy:"merge" patchMergeKey:"containerPort"`
	Env             []EnvVar             `json:"env,omitempty" patchStrategy:"merge" patchMergeKey:"name"`
	Resources       ResourceRequirements `json:"resources"`
	VolumeMounts    []VolumeMount        `json:"volumeMounts,omitempty" patchStrategy:"merge" patchMergeKey:"mountPath"`
	ImagePullPolicy string               `json:"imagePullPolicy,omitempty"`

	// Probes retain their JSON representation for existing database compatibility;
	// ParseProbe validates and supplies execution defaults at the boundary.
	LivenessProbe   json.RawMessage `json:"livenessProbe,omitempty"`
	ReadinessProbe  json.RawMessage `json:"readinessProbe,omitempty"`
	StartupProbe    json.RawMessage `json:"startupProbe,omitempty"`
	SecurityContext json.RawMessage `json:"securityContext,omitempty"` // stored only

	// Only secretRef sources are implemented; configMapRef is rejected.
	EnvFrom []EnvFromSource `json:"envFrom,omitempty"`
}

type ContainerPort struct {
	Name          string `json:"name,omitempty"`
	ContainerPort int32  `json:"containerPort"`
	Protocol      string `json:"protocol,omitempty"`
}

type EnvVar struct {
	Name      string        `json:"name"`
	Value     string        `json:"value,omitempty"`
	ValueFrom *EnvVarSource `json:"valueFrom,omitempty"`
}

// EnvVarSource reads a variable from a Secret key. The other Kubernetes
// sources are kept as raw JSON only so validation can name them as unsupported.
type EnvVarSource struct {
	SecretKeyRef     *SecretKeySelector `json:"secretKeyRef,omitempty"`
	FieldRef         json.RawMessage    `json:"fieldRef,omitempty"`         // rejected
	ResourceFieldRef json.RawMessage    `json:"resourceFieldRef,omitempty"` // rejected
	ConfigMapKeyRef  json.RawMessage    `json:"configMapKeyRef,omitempty"`  // rejected
	FileKeyRef       json.RawMessage    `json:"fileKeyRef,omitempty"`       // rejected
}

type SecretKeySelector struct {
	Name     string `json:"name"`
	Key      string `json:"key"`
	Optional *bool  `json:"optional,omitempty"`
}

// EnvFromSource turns every key of a Secret into a variable.
type EnvFromSource struct {
	Prefix       string           `json:"prefix,omitempty"`
	SecretRef    *SecretEnvSource `json:"secretRef,omitempty"`
	ConfigMapRef json.RawMessage  `json:"configMapRef,omitempty"` // rejected
}

type SecretEnvSource struct {
	Name     string `json:"name"`
	Optional *bool  `json:"optional,omitempty"`
}

type ResourceRequirements struct {
	Limits   map[string]Quantity `json:"limits,omitempty"`
	Requests map[string]Quantity `json:"requests,omitempty"` // stored only
}

type VolumeMount struct {
	Name      string `json:"name"`
	MountPath string `json:"mountPath"`
	ReadOnly  bool   `json:"readOnly,omitempty"`
}

type Volume struct {
	Name     string                `json:"name"`
	EmptyDir *EmptyDirVolumeSource `json:"emptyDir,omitempty"`
	HostPath *HostPathVolumeSource `json:"hostPath,omitempty"`
}

type EmptyDirVolumeSource struct{}

type HostPathVolumeSource struct {
	Path string `json:"path"`
	Type string `json:"type,omitempty"`
}

type PodStatus struct {
	Phase             string            `json:"phase,omitempty"`
	Conditions        []PodCondition    `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
	Message           string            `json:"message,omitempty"`
	Reason            string            `json:"reason,omitempty"`
	HostIP            string            `json:"hostIP,omitempty"`
	PodIP             string            `json:"podIP,omitempty"`
	PodIPs            []PodIP           `json:"podIPs,omitempty" patchStrategy:"merge" patchMergeKey:"ip"`
	StartTime         *Time             `json:"startTime,omitempty"`
	ContainerStatuses []ContainerStatus `json:"containerStatuses,omitempty"`
}

type PodIP struct {
	IP string `json:"ip"`
}

type PodCondition struct {
	Type               string `json:"type"`
	Status             string `json:"status"`
	LastTransitionTime *Time  `json:"lastTransitionTime,omitempty"`
	Reason             string `json:"reason,omitempty"`
	Message            string `json:"message,omitempty"`
}

type ContainerStatus struct {
	Name         string         `json:"name"`
	State        ContainerState `json:"state,omitzero"`
	LastState    ContainerState `json:"lastState,omitzero"`
	Ready        bool           `json:"ready"`
	RestartCount int32          `json:"restartCount"`
	Image        string         `json:"image"`
	ImageID      string         `json:"imageID"`
	ContainerID  string         `json:"containerID,omitempty"`
	Started      *bool          `json:"started,omitempty"`
}

type ContainerState struct {
	Waiting    *ContainerStateWaiting    `json:"waiting,omitempty"`
	Running    *ContainerStateRunning    `json:"running,omitempty"`
	Terminated *ContainerStateTerminated `json:"terminated,omitempty"`
}

type ContainerStateWaiting struct {
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

type ContainerStateRunning struct {
	StartedAt *Time `json:"startedAt,omitempty"`
}

type ContainerStateTerminated struct {
	ExitCode    int32  `json:"exitCode"`
	Signal      int32  `json:"signal,omitempty"`
	Reason      string `json:"reason,omitempty"`
	Message     string `json:"message,omitempty"`
	StartedAt   *Time  `json:"startedAt,omitempty"`
	FinishedAt  *Time  `json:"finishedAt,omitempty"`
	ContainerID string `json:"containerID,omitempty"`
}

type PodTemplateSpec struct {
	Metadata ObjectMeta `json:"metadata,omitzero"`
	Spec     PodSpec    `json:"spec"`
}

// ---- Deployment ----

type Deployment struct {
	TypeMeta
	Metadata ObjectMeta       `json:"metadata"`
	Spec     DeploymentSpec   `json:"spec"`
	Status   DeploymentStatus `json:"status,omitzero"`
}

func (o *Deployment) Meta() *ObjectMeta { return &o.Metadata }

type DeploymentSpec struct {
	Replicas             *int32             `json:"replicas,omitempty"`
	Selector             *LabelSelector     `json:"selector"`
	Template             PodTemplateSpec    `json:"template"`
	Strategy             DeploymentStrategy `json:"strategy,omitzero" patchStrategy:"retainKeys"`
	RevisionHistoryLimit *int32             `json:"revisionHistoryLimit,omitempty"`
	Paused               bool               `json:"paused,omitempty"`

	// stored only
	MinReadySeconds         int32  `json:"minReadySeconds,omitempty"`
	ProgressDeadlineSeconds *int32 `json:"progressDeadlineSeconds,omitempty"`
}

type DeploymentStrategy struct {
	Type          string                   `json:"type,omitempty"`
	RollingUpdate *RollingUpdateDeployment `json:"rollingUpdate,omitempty"`
}

type RollingUpdateDeployment struct {
	MaxUnavailable *IntOrString `json:"maxUnavailable,omitempty"`
	MaxSurge       *IntOrString `json:"maxSurge,omitempty"`
}

type DeploymentStatus struct {
	ObservedGeneration  int64                 `json:"observedGeneration,omitempty"`
	Replicas            int32                 `json:"replicas,omitempty"`
	UpdatedReplicas     int32                 `json:"updatedReplicas,omitempty"`
	ReadyReplicas       int32                 `json:"readyReplicas,omitempty"`
	AvailableReplicas   int32                 `json:"availableReplicas,omitempty"`
	UnavailableReplicas int32                 `json:"unavailableReplicas,omitempty"`
	Conditions          []DeploymentCondition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

type DeploymentCondition struct {
	Type               string `json:"type"`
	Status             string `json:"status"`
	LastUpdateTime     *Time  `json:"lastUpdateTime,omitempty"`
	LastTransitionTime *Time  `json:"lastTransitionTime,omitempty"`
	Reason             string `json:"reason,omitempty"`
	Message            string `json:"message,omitempty"`
}

// ---- ReplicaSet ----

type ReplicaSet struct {
	TypeMeta
	Metadata ObjectMeta       `json:"metadata"`
	Spec     ReplicaSetSpec   `json:"spec"`
	Status   ReplicaSetStatus `json:"status,omitzero"`
}

func (o *ReplicaSet) Meta() *ObjectMeta { return &o.Metadata }

type ReplicaSetSpec struct {
	Replicas        *int32          `json:"replicas,omitempty"`
	Selector        *LabelSelector  `json:"selector"`
	Template        PodTemplateSpec `json:"template"`
	MinReadySeconds int32           `json:"minReadySeconds,omitempty"` // stored only
}

type ReplicaSetStatus struct {
	Replicas             int32 `json:"replicas"`
	FullyLabeledReplicas int32 `json:"fullyLabeledReplicas,omitempty"`
	ReadyReplicas        int32 `json:"readyReplicas,omitempty"`
	AvailableReplicas    int32 `json:"availableReplicas,omitempty"`
	ObservedGeneration   int64 `json:"observedGeneration,omitempty"`
}

// ---- Service ----

type Service struct {
	TypeMeta
	Metadata ObjectMeta    `json:"metadata"`
	Spec     ServiceSpec   `json:"spec"`
	Status   ServiceStatus `json:"status,omitzero"`
}

func (o *Service) Meta() *ObjectMeta { return &o.Metadata }

type ServiceSpec struct {
	Type      string            `json:"type,omitempty"`
	Selector  map[string]string `json:"selector,omitempty"`
	Ports     []ServicePort     `json:"ports,omitempty" patchStrategy:"merge" patchMergeKey:"port"`
	ClusterIP string            `json:"clusterIP,omitempty"`

	SessionAffinity string `json:"sessionAffinity,omitempty"` // stored only
}

type ServicePort struct {
	Name       string      `json:"name,omitempty"`
	Protocol   string      `json:"protocol,omitempty"`
	Port       int32       `json:"port"`
	TargetPort IntOrString `json:"targetPort,omitzero"`
}

type ServiceStatus struct {
	LoadBalancer json.RawMessage `json:"loadBalancer,omitempty"`
}

// ---- Secret ----

// Secret holds small values for containers. Like Kubernetes without an
// encryption provider, the values are stored base64-encoded, not encrypted:
// the database must stay readable by root only.
type Secret struct {
	TypeMeta
	Metadata   ObjectMeta        `json:"metadata"`
	SecretType string            `json:"type,omitempty"` // Go name avoids the Object.Type method
	Data       map[string][]byte `json:"data,omitempty"`
	StringData map[string]string `json:"stringData,omitempty"` // write-only: merged into data
	Immutable  *bool             `json:"immutable,omitempty"`
}

func (o *Secret) Meta() *ObjectMeta { return &o.Metadata }

const (
	SecretTypeOpaque = "Opaque"
	SecretTypeTLS    = "kubernetes.io/tls"
	// MaxSecretSize is the Kubernetes limit for the decoded data of a Secret.
	MaxSecretSize = 1 << 20
)

// ---- Namespace ----

type Namespace struct {
	TypeMeta
	Metadata ObjectMeta      `json:"metadata"`
	Spec     NamespaceSpec   `json:"spec,omitzero"`
	Status   NamespaceStatus `json:"status,omitzero"`
}

func (o *Namespace) Meta() *ObjectMeta { return &o.Metadata }

type NamespaceSpec struct {
	Finalizers []string `json:"finalizers,omitempty"`
}

type NamespaceStatus struct {
	Phase string `json:"phase,omitempty"`
}

// ---- Node (read only through the API; the node agent writes it from M2) ----

type Node struct {
	TypeMeta
	Metadata ObjectMeta `json:"metadata"`
	Spec     NodeSpec   `json:"spec,omitzero"`
	Status   NodeStatus `json:"status,omitzero"`
}

func (o *Node) Meta() *ObjectMeta { return &o.Metadata }

type NodeSpec struct {
	PodCIDR       string   `json:"podCIDR,omitempty"`
	PodCIDRs      []string `json:"podCIDRs,omitempty"`
	Unschedulable bool     `json:"unschedulable,omitempty"`
	Taints        []Taint  `json:"taints,omitempty"`
}

type Taint struct {
	Key       string `json:"key"`
	Value     string `json:"value,omitempty"`
	Effect    string `json:"effect"` // NoSchedule, PreferNoSchedule or NoExecute
	TimeAdded *Time  `json:"timeAdded,omitempty"`
}

type NodeStatus struct {
	Capacity    map[string]Quantity `json:"capacity,omitempty"`
	Allocatable map[string]Quantity `json:"allocatable,omitempty"`
	Conditions  []NodeCondition     `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
	Addresses   []NodeAddress       `json:"addresses,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
	NodeInfo    NodeSystemInfo      `json:"nodeInfo,omitzero"`
}

type NodeCondition struct {
	Type               string `json:"type"`
	Status             string `json:"status"`
	LastHeartbeatTime  *Time  `json:"lastHeartbeatTime,omitempty"`
	LastTransitionTime *Time  `json:"lastTransitionTime,omitempty"`
	Reason             string `json:"reason,omitempty"`
	Message            string `json:"message,omitempty"`
}

type NodeAddress struct {
	Type    string `json:"type"`
	Address string `json:"address"`
}

type NodeSystemInfo struct {
	KernelVersion           string `json:"kernelVersion,omitempty"`
	OSImage                 string `json:"osImage,omitempty"`
	ContainerRuntimeVersion string `json:"containerRuntimeVersion,omitempty"`
	KubeletVersion          string `json:"kubeletVersion,omitempty"`
	OperatingSystem         string `json:"operatingSystem,omitempty"`
	Architecture            string `json:"architecture,omitempty"`
}

// ---- Event (read only through the API) ----

type Event struct {
	TypeMeta
	Metadata       ObjectMeta      `json:"metadata"`
	InvolvedObject ObjectReference `json:"involvedObject"`
	Reason         string          `json:"reason,omitempty"`
	Message        string          `json:"message,omitempty"`
	Source         EventSource     `json:"source,omitzero"`
	FirstTimestamp *Time           `json:"firstTimestamp,omitempty"`
	LastTimestamp  *Time           `json:"lastTimestamp,omitempty"`
	Count          int32           `json:"count,omitempty"`
	EventType      string          `json:"type,omitempty"`
}

func (o *Event) Meta() *ObjectMeta { return &o.Metadata }

type EventSource struct {
	Component string `json:"component,omitempty"`
	Host      string `json:"host,omitempty"`
}

// ---- EndpointSlice (read only through the API) ----

type EndpointSlice struct {
	TypeMeta
	Metadata    ObjectMeta     `json:"metadata"`
	AddressType string         `json:"addressType"`
	Endpoints   []Endpoint     `json:"endpoints"`
	Ports       []EndpointPort `json:"ports"`
}

func (o *EndpointSlice) Meta() *ObjectMeta { return &o.Metadata }

type Endpoint struct {
	Addresses  []string           `json:"addresses"`
	Conditions EndpointConditions `json:"conditions,omitzero"`
	TargetRef  *ObjectReference   `json:"targetRef,omitempty"`
	NodeName   *string            `json:"nodeName,omitempty"`
}

type EndpointConditions struct {
	Ready       *bool `json:"ready,omitempty"`
	Serving     *bool `json:"serving,omitempty"`
	Terminating *bool `json:"terminating,omitempty"`
}

type EndpointPort struct {
	Name     *string `json:"name,omitempty"`
	Protocol *string `json:"protocol,omitempty"`
	Port     *int32  `json:"port,omitempty"`
}
