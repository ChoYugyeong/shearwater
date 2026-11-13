package agent

import (
	"fmt"
	"strings"

	"shearwater/internal/apis"
)

// CompactObject leaves environment variables and full manifests out of the
// default view. The caller can explicitly request complete JSON. Reuse the
// tree's readiness definitions so get/why/tree agree about the same Pod.
func CompactObject(object apis.Object) string {
	switch o := object.(type) {
	case *apis.Pod:
		detail, _ := describePod(o)
		return strings.TrimSpace(detail)
	case *apis.Deployment:
		detail, _ := describeDeployment(o)
		return strings.TrimSpace(detail)
	case *apis.ReplicaSet:
		detail, _ := describeReplicaSet(o, nil)
		return strings.TrimSpace(detail)
	case *apis.Service:
		var ports []string
		for _, p := range o.Spec.Ports {
			ports = append(ports, fmt.Sprintf("%d/%s", p.Port, p.Protocol))
		}
		return fmt.Sprintf("ip=%s ports=%s", inspectValue(o.Spec.ClusterIP), strings.Join(ports, ","))
	case *apis.Node:
		return inspectNodeState(*o)
	case *apis.Namespace:
		return inspectValue(o.Status.Phase)
	case *apis.Event:
		return fmt.Sprintf("%s %s count=%d", inspectValue(o.EventType), inspectValue(o.Reason), o.Count)
	case *apis.EndpointSlice:
		ready := 0
		for _, e := range o.Endpoints {
			if e.Conditions.Ready != nil && *e.Conditions.Ready {
				ready++
			}
		}
		return fmt.Sprintf("service=%s endpoints=%d ready=%d", inspectValue(o.Metadata.Labels["kubernetes.io/service-name"]), len(o.Endpoints), ready)
	default:
		return ""
	}
}

// Finding separates an observation from a suggested next check. An exit code
// alone cannot prove an out-of-memory kill, for example: advice is not proof.
type Finding struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Next    string `json:"next"`
}

func PodFindings(p *apis.Pod) []Finding {
	var findings []Finding
	if p.Metadata.DeletionTimestamp != nil {
		findings = append(findings, Finding{"Terminating", "The Pod has a deletion timestamp.", "Check its grace period and finalizers."})
	}
	if p.Spec.NodeName == "" {
		findings = append(findings, Finding{"Unscheduled", "The Pod has not been assigned to a node.", "Run sw status and sw doctor; check that the server has a containerd node agent."})
	}
	for _, cs := range p.Status.ContainerStatuses {
		if cs.State.Running != nil && !cs.Ready {
			findings = append(findings, Finding{"ContainerNotReady", cs.Name + ": process is running but is not ready for Service traffic.", "Check startup/readiness probe settings and Unhealthy events; a running process can still reject requests."})
		}
		if w := cs.State.Waiting; w != nil {
			next := "Read the event and runtime error below; use sw doctor to check the node."
			switch w.Reason {
			case "ErrImagePull", "ImagePullBackOff":
				next = "Check the image name/tag and registry connectivity. Private-registry credentials are not implemented yet."
			case "CrashLoopBackOff":
				next = "Read the exit code and current/previous logs. Check the application's command and configuration; exit 137 alone does not prove an out-of-memory kill."
			case "CreateContainerConfigError":
				next = "Check volume host paths/types, mounts and container settings against the reported error."
			case "CreatePodSandboxError":
				// Sandbox preparation includes both volumes and networking. A
				// wrong hostPath type fails here before a container is created;
				// the reason alone does not mean that the network is broken.
				next = "Check the reported volume host path/type or Pod network setup error. Run sw doctor for host prerequisites."
			case "RunContainerError":
				next = "Check the executable, command/args and mounts in the runtime error. A container that never started may have no application log."
			case "ContainerCreating":
				next = "Creation may still be in progress. Check recent events, then inspect this Pod again."
			}
			findings = append(findings, Finding{w.Reason, cs.Name + ": " + w.Message, next})
		}
		if ended := cs.State.Terminated; ended != nil {
			next := "The container finished successfully; check restartPolicy to see whether it should run again."
			if ended.ExitCode != 0 {
				next = "Read this attempt's logs and check the application's command/configuration. Exit status describes the result, not necessarily the root cause."
			}
			findings = append(findings, Finding{"ContainerTerminated", fmt.Sprintf("%s: exit=%d reason=%s", cs.Name, ended.ExitCode, ended.Reason), next})
		}
		if cs.RestartCount > 0 && cs.State.Running != nil {
			findings = append(findings, Finding{"Restarted", fmt.Sprintf("%s is running after %d restart(s).", cs.Name, cs.RestartCount), "Read the previous termination and previous logs; a running process does not prove application health."})
		}
	}
	if len(p.Status.ContainerStatuses) == 0 && p.Spec.NodeName != "" {
		findings = append(findings, Finding{"NoContainerStatus", "The node has not reported container state yet.", "Check events and sw doctor, then inspect this Pod again."})
	}
	if len(findings) == 0 {
		findings = append(findings, Finding{"NoFailureReported", "No failure is reported in current container status.", "Verify the application's HTTP/TCP response. Configured readiness/startup probes affect Ready; without a readiness probe a running container may still be unhealthy."})
	}
	return findings
}
