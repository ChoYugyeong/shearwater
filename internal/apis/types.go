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

	// stored only
	DNSPolicy         string            `json:"dnsPolicy,omitempty"`
	NodeSelector      map[string]string `json:"nodeSelector,omitempty"`
	Affinity          json.RawMessage   `json:"affinity,omitempty"`
	Tolerations       json.RawMessage   `json:"tolerations,omitempty"`
	PriorityClassName string            `json:"priorityClassName,omitempty"`
	SecurityContext   json.RawMessage   `json:"securityContext,omitempty"`

	// rejected when set
	InitContainers     json.RawMessage `json:"initContainers,omitempty"`
	HostNetwork        bool            `json:"hostNetwork,omitempty"`
	ServiceAccountName string          `json:"serviceAccountName,omitempty"`
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

	// stored only
	LivenessProbe   json.RawMessage `json:"livenessProbe,omitempty"`
	ReadinessProbe  json.RawMessage `json:"readinessProbe,omitempty"`
	StartupProbe    json.RawMessage `json:"startupProbe,omitempty"`
	SecurityContext json.RawMessage `json:"securityContext,omitempty"`

	// rejected when set
	EnvFrom json.RawMessage `json:"envFrom,omitempty"`
}

type ContainerPort struct {
	Name          string `json:"name,omitempty"`
	ContainerPort int32  `json:"containerPort"`
	Protocol      string `json:"protocol,omitempty"`
}

type EnvVar struct {
	Name      string          `json:"name"`
	Value     string          `json:"value,omitempty"`
	ValueFrom json.RawMessage `json:"valueFrom,omitempty"` // rejected when set
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
	PodCIDR string `json:"podCIDR,omitempty"`
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
