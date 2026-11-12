/*
Copyright The Kubernetes Authors.
Copyright 2015 The Kubernetes Authors.
Copyright 2017 The Kubernetes Authors.
Copyright 2026 The Shearwater Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

Schema metadata is derived from Kubernetes v1.36.6 generated.proto files
and the adjacent core/v1 and apps/v1 types.go files:
https://github.com/kubernetes/kubernetes/tree/v1.36.6/staging/src/k8s.io

Modifications for shearwater (2026): field numbers were manually transcribed
and JSON names/pointer metadata adapted into the tables below for a limited
request decoder. Unsupported message fields are explicitly represented and
rejected when present. See NOTICE and THIRD_PARTY_NOTICES.md for the source
paths, original license and scope of this adaptation.
*/

package protobuf

// Field numbers, copied by hand from the generated.proto files of the
// Kubernetes v1.36.6 tag (staging/src/k8s.io/...):
//
//	api/core/v1/generated.proto                    Pod, PodSpec, Container, Volume, Service, Namespace, ...
//	api/apps/v1/generated.proto                    Deployment, DeploymentSpec, DeploymentStrategy, ...
//	apimachinery/pkg/apis/meta/v1/generated.proto  ObjectMeta, OwnerReference, LabelSelector, Time
//	apimachinery/pkg/runtime/generated.proto       Unknown, TypeMeta (the envelope, in protobuf.go)
//	apimachinery/pkg/util/intstr/generated.proto   IntOrString
//	apimachinery/pkg/api/resource/generated.proto  Quantity
//
// A number means something only inside its own message. Every field of a listed
// message is here, also those Shearwater has no use for: kubectl writes them
// with zero values, and a non-zero one must reach the unknown-field rule under
// its JSON name. "ptr" marks fields that are pointers in the Kubernetes Go
// types (types.go next to each .proto), where being present matters.
// A message field without a table (nonePtr, nones) is refused when it is on the wire.

type kind uint8

const (
	tString kind = iota
	tBool
	tInt
	tMsg
	tInline   // a message whose members belong to the enclosing JSON object
	tStrMap   // map<string, string>
	tQtyMap   // map<string, Quantity>
	tTime     // meta/v1 Time
	tIntOrStr // intstr.IntOrString
	tQuantity // resource.Quantity
	tSkip     // never taken from a request (status)
)

func (k kind) wire() int {
	if k == tBool || k == tInt {
		return wireVarint
	}
	return wireBytes
}

type field struct {
	name string // JSON name
	kind kind
	rep  bool // repeated
	ptr  bool
	msg  *message
}

type message struct {
	name   string
	fields map[int]field
}

func str(name string) field                { return field{name: name, kind: tString} }
func strPtr(name string) field             { return field{name: name, kind: tString, ptr: true} }
func strs(name string) field               { return field{name: name, kind: tString, rep: true} }
func boolean(name string) field            { return field{name: name, kind: tBool} }
func boolPtr(name string) field            { return field{name: name, kind: tBool, ptr: true} }
func integer(name string) field            { return field{name: name, kind: tInt} }
func intPtr(name string) field             { return field{name: name, kind: tInt, ptr: true} }
func msg(name string, m *message) field    { return field{name: name, kind: tMsg, msg: m} }
func msgPtr(name string, m *message) field { return field{name: name, kind: tMsg, msg: m, ptr: true} }
func msgs(name string, m *message) field   { return field{name: name, kind: tMsg, msg: m, rep: true} }
func nonePtr(name string) field            { return field{name: name, kind: tMsg, ptr: true} }
func nones(name string) field              { return field{name: name, kind: tMsg, rep: true} }
func strMap(name string) field             { return field{name: name, kind: tStrMap} }
func qtyMap(name string) field             { return field{name: name, kind: tQtyMap} }
func status() field                        { return field{name: "status", kind: tSkip} }

// roots are the bodies read at all, keyed by "apiVersion kind" from the envelope.
var roots = map[string]*message{
	"v1 Pod":             pod,
	"v1 Service":         service,
	"v1 Namespace":       namespace,
	"apps/v1 Deployment": deployment,
}

// ---- meta/v1 ----

var objectMeta = &message{"ObjectMeta", map[int]field{
	1: str("name"), 2: str("generateName"), 3: str("namespace"), 4: str("selfLink"), 5: str("uid"),
	6: str("resourceVersion"), 7: integer("generation"),
	8: {name: "creationTimestamp", kind: tTime}, 9: {name: "deletionTimestamp", kind: tTime},
	10: intPtr("deletionGracePeriodSeconds"), 11: strMap("labels"), 12: strMap("annotations"),
	13: msgs("ownerReferences", ownerReference), 14: strs("finalizers"), 17: nones("managedFields"),
}}

var ownerReference = &message{"OwnerReference", map[int]field{
	5: str("apiVersion"), 1: str("kind"), 3: str("name"), 4: str("uid"),
	6: boolPtr("controller"), 7: boolPtr("blockOwnerDeletion"),
}}

var labelSelector = &message{"LabelSelector", map[int]field{
	1: strMap("matchLabels"), 2: msgs("matchExpressions", labelSelectorRequirement),
}}

var labelSelectorRequirement = &message{"LabelSelectorRequirement", map[int]field{
	1: str("key"), 2: str("operator"), 3: strs("values"),
}}

// ---- core/v1 ----

var pod = &message{"Pod", map[int]field{1: msg("metadata", objectMeta), 2: msg("spec", podSpec), 3: status()}}

var podTemplateSpec = &message{"PodTemplateSpec", map[int]field{1: msg("metadata", objectMeta), 2: msg("spec", podSpec)}}

var podSpec = &message{"PodSpec", map[int]field{
	1: msgs("volumes", volume), 20: msgs("initContainers", container), 2: msgs("containers", container),
	34: nones("ephemeralContainers"), 3: str("restartPolicy"), 4: intPtr("terminationGracePeriodSeconds"),
	5: intPtr("activeDeadlineSeconds"), 6: str("dnsPolicy"), 7: strMap("nodeSelector"),
	8: str("serviceAccountName"), 9: str("serviceAccount"), 21: boolPtr("automountServiceAccountToken"),
	10: str("nodeName"), 11: boolean("hostNetwork"), 12: boolean("hostPID"), 13: boolean("hostIPC"),
	27: boolPtr("shareProcessNamespace"), 14: nonePtr("securityContext"), 15: nones("imagePullSecrets"),
	16: str("hostname"), 17: str("subdomain"), 18: nonePtr("affinity"), 19: str("schedulerName"),
	22: msgs("tolerations", toleration), 23: nones("hostAliases"), 24: str("priorityClassName"),
	25: intPtr("priority"), 26: nonePtr("dnsConfig"), 28: nones("readinessGates"), 29: strPtr("runtimeClassName"),
	30: boolPtr("enableServiceLinks"), 31: strPtr("preemptionPolicy"), 32: qtyMap("overhead"),
	33: nones("topologySpreadConstraints"), 35: boolPtr("setHostnameAsFQDN"), 36: nonePtr("os"),
	37: boolPtr("hostUsers"), 38: nones("schedulingGates"), 39: nones("resourceClaims"), 40: nonePtr("resources"),
	41: strPtr("hostnameOverride"), 43: nonePtr("schedulingGroup"),
}}

var container = &message{"Container", map[int]field{
	1: str("name"), 2: str("image"), 3: strs("command"), 4: strs("args"), 5: str("workingDir"),
	6: msgs("ports", containerPort), 19: nones("envFrom"), 7: msgs("env", envVar), 8: msg("resources", resourceRequirements),
	23: nones("resizePolicy"), 24: strPtr("restartPolicy"), 25: nones("restartPolicyRules"),
	9: msgs("volumeMounts", volumeMount), 21: nones("volumeDevices"),
	10: nonePtr("livenessProbe"), 11: nonePtr("readinessProbe"), 22: nonePtr("startupProbe"), 12: nonePtr("lifecycle"),
	13: str("terminationMessagePath"), 20: str("terminationMessagePolicy"), 14: str("imagePullPolicy"),
	15: nonePtr("securityContext"), 16: boolean("stdin"), 17: boolean("stdinOnce"), 18: boolean("tty"),
}}

var containerPort = &message{"ContainerPort", map[int]field{
	1: str("name"), 2: integer("hostPort"), 3: integer("containerPort"), 4: str("protocol"), 5: str("hostIP"),
}}

var envVar = &message{"EnvVar", map[int]field{1: str("name"), 2: str("value"), 3: nonePtr("valueFrom")}}

var resourceRequirements = &message{"ResourceRequirements", map[int]field{
	1: qtyMap("limits"), 2: qtyMap("requests"), 3: nones("claims"),
}}

var volumeMount = &message{"VolumeMount", map[int]field{
	1: str("name"), 2: boolean("readOnly"), 7: strPtr("recursiveReadOnly"), 3: str("mountPath"),
	4: str("subPath"), 5: strPtr("mountPropagation"), 6: str("subPathExpr"),
}}

var volume = &message{"Volume", map[int]field{1: str("name"), 2: {name: "volumeSource", kind: tInline, msg: volumeSource}}}

var volumeSource = &message{"VolumeSource", map[int]field{
	1: msgPtr("hostPath", hostPathVolumeSource), 2: msgPtr("emptyDir", emptyDirVolumeSource),
	3: nonePtr("gcePersistentDisk"), 4: nonePtr("awsElasticBlockStore"), 5: nonePtr("gitRepo"), 6: nonePtr("secret"),
	7: nonePtr("nfs"), 8: nonePtr("iscsi"), 9: nonePtr("glusterfs"), 10: nonePtr("persistentVolumeClaim"),
	11: nonePtr("rbd"), 12: nonePtr("flexVolume"), 13: nonePtr("cinder"), 14: nonePtr("cephfs"), 15: nonePtr("flocker"),
	16: nonePtr("downwardAPI"), 17: nonePtr("fc"), 18: nonePtr("azureFile"), 19: nonePtr("configMap"),
	20: nonePtr("vsphereVolume"), 21: nonePtr("quobyte"), 22: nonePtr("azureDisk"), 23: nonePtr("photonPersistentDisk"),
	26: nonePtr("projected"), 24: nonePtr("portworxVolume"), 25: nonePtr("scaleIO"), 27: nonePtr("storageos"),
	28: nonePtr("csi"), 29: nonePtr("ephemeral"), 30: nonePtr("image"),
}}

var emptyDirVolumeSource = &message{"EmptyDirVolumeSource", map[int]field{
	1: str("medium"), 2: {name: "sizeLimit", kind: tQuantity, ptr: true},
}}

var hostPathVolumeSource = &message{"HostPathVolumeSource", map[int]field{1: str("path"), 2: strPtr("type")}}

var toleration = &message{"Toleration", map[int]field{
	1: str("key"), 2: str("operator"), 3: str("value"), 4: str("effect"), 5: intPtr("tolerationSeconds"),
}}

var service = &message{"Service", map[int]field{1: msg("metadata", objectMeta), 2: msg("spec", serviceSpec), 3: status()}}

var serviceSpec = &message{"ServiceSpec", map[int]field{
	1: msgs("ports", servicePort), 2: strMap("selector"), 3: str("clusterIP"), 18: strs("clusterIPs"), 4: str("type"),
	5: strs("externalIPs"), 7: str("sessionAffinity"), 8: str("loadBalancerIP"), 9: strs("loadBalancerSourceRanges"),
	10: str("externalName"), 11: str("externalTrafficPolicy"), 12: integer("healthCheckNodePort"),
	13: boolean("publishNotReadyAddresses"), 14: nonePtr("sessionAffinityConfig"), 19: strs("ipFamilies"),
	17: strPtr("ipFamilyPolicy"), 20: boolPtr("allocateLoadBalancerNodePorts"), 21: strPtr("loadBalancerClass"),
	22: strPtr("internalTrafficPolicy"), 23: strPtr("trafficDistribution"),
}}

var servicePort = &message{"ServicePort", map[int]field{
	1: str("name"), 2: str("protocol"), 6: strPtr("appProtocol"), 3: integer("port"),
	4: {name: "targetPort", kind: tIntOrStr}, 5: integer("nodePort"),
}}

var namespace = &message{"Namespace", map[int]field{1: msg("metadata", objectMeta), 2: msg("spec", namespaceSpec), 3: status()}}

var namespaceSpec = &message{"NamespaceSpec", map[int]field{1: strs("finalizers")}}

// ---- apps/v1 ----

var deployment = &message{"Deployment", map[int]field{1: msg("metadata", objectMeta), 2: msg("spec", deploymentSpec), 3: status()}}

var deploymentSpec = &message{"DeploymentSpec", map[int]field{
	1: intPtr("replicas"), 2: msgPtr("selector", labelSelector), 3: msg("template", podTemplateSpec),
	4: msg("strategy", deploymentStrategy), 5: integer("minReadySeconds"), 6: intPtr("revisionHistoryLimit"),
	7: boolean("paused"), 9: intPtr("progressDeadlineSeconds"),
}}

var deploymentStrategy = &message{"DeploymentStrategy", map[int]field{
	1: str("type"), 2: msgPtr("rollingUpdate", rollingUpdateDeployment),
}}

var rollingUpdateDeployment = &message{"RollingUpdateDeployment", map[int]field{
	1: {name: "maxUnavailable", kind: tIntOrStr, ptr: true}, 2: {name: "maxSurge", kind: tIntOrStr, ptr: true},
}}
