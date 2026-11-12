package agent

import (
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"shearwater/internal/apis"
)

func (r *treeRow) describe(endpoints map[string]int) {
	switch o := r.object.(type) {
	case *apis.Deployment:
		r.detail, r.problem = describeDeployment(o)
	case *apis.ReplicaSet:
		r.detail, r.problem = describeReplicaSet(o, r.parent)
	case *apis.Pod:
		r.detail, r.problem = describePod(o)
	case *apis.Service:
		r.detail, r.problem = describeService(o, endpoints[o.Metadata.UID])
	}
	if r.object.Meta().DeletionTimestamp != nil {
		r.detail += " Terminating"
		r.problem = true
	}
}

func inspectReplicas(replicas *int32) int32 {
	// A missing value defaults to one. An explicit zero means scaled down.
	if replicas == nil {
		return 1
	}
	return *replicas
}

func describeDeployment(d *apis.Deployment) (string, bool) {
	desired := inspectReplicas(d.Spec.Replicas)
	detail := fmt.Sprintf(" %d/%d ready", d.Status.ReadyReplicas, desired)
	if rev := d.Metadata.Annotations[apis.DeploymentRevisionAnnotation]; rev != "" {
		// The rollout revision is scoped to this Deployment. The revision in
		// the header belongs to the store and covers every resource kind.
		detail += " rev=" + inspectValue(rev)
	}
	if containers := d.Spec.Template.Spec.Containers; len(containers) != 0 {
		images := make([]string, 0, len(containers))
		for _, c := range containers {
			images = append(images, inspectValue(c.Image))
		}
		detail += " image=" + strings.Join(images, ",")
	}
	if d.Spec.Paused {
		detail += " paused"
	}
	if missing := desired - d.Status.Replicas; missing > 0 {
		detail += fmt.Sprintf(" missing=%d", missing)
	}
	problem := d.Status.ReadyReplicas < desired || d.Status.Replicas < desired
	for _, c := range d.Status.Conditions {
		if c.Type == "Progressing" && c.Status == "False" || c.Type == "ReplicaFailure" && c.Status == "True" {
			problem = true
			reason := c.Reason
			if reason == "" {
				reason = "RolloutFailed"
			}
			detail += " " + inspectValue(reason)
		}
	}
	return detail, problem
}

func describeReplicaSet(rs *apis.ReplicaSet, parent *treeRow) (string, bool) {
	desired := inspectReplicas(rs.Spec.Replicas)
	detail := fmt.Sprintf(" %d/%d", rs.Status.ReadyReplicas, desired)
	if parent != nil {
		if d, ok := parent.object.(*apis.Deployment); ok {
			current, own := d.Metadata.Annotations[apis.DeploymentRevisionAnnotation], rs.Metadata.Annotations[apis.DeploymentRevisionAnnotation]
			if current != "" && own != "" && current != own {
				detail += " old"
			}
		}
	}
	if missing := desired - rs.Status.Replicas; missing > 0 {
		detail += fmt.Sprintf(" missing=%d", missing)
	}
	return detail, rs.Status.ReadyReplicas < desired || rs.Status.Replicas < desired
}

func inspectPodReady(p *apis.Pod) bool {
	// Pod Ready comes from the Pod condition. Running only says the Pod has
	// started, and a Pod already being removed should not count as ready.
	if p.Status.Phase != "Running" || p.Metadata.DeletionTimestamp != nil {
		return false
	}
	for _, c := range p.Status.Conditions {
		if c.Type == "Ready" {
			return c.Status == "True"
		}
	}
	return false
}

func describePod(p *apis.Pod) (string, bool) {
	phase := p.Status.Phase
	if phase == "" {
		phase = "Unknown"
	}
	var ready int
	var restarts int64
	var reasons []string
	for _, c := range p.Status.ContainerStatuses {
		restarts += int64(c.RestartCount)
		if c.Ready && c.State.Running != nil {
			ready++
		}
		if reason := inspectContainerProblem(c.State); reason != "" {
			if len(p.Spec.Containers) > 1 {
				reason = inspectValue(c.Name) + ":" + reason
			}
			reasons = append(reasons, reason)
		}
	}
	// A Succeeded Pod completed normally. A failed previous container
	// attempt is covered by the restart count when the container recovered.
	unready := p.Metadata.DeletionTimestamp == nil && phase != "Succeeded" && !inspectPodReady(p)
	if unready && len(reasons) == 0 {
		if reason := inspectPodReason(p); reason != "" && reason != phase {
			reasons = append(reasons, inspectValue(reason))
		} else if phase == "Running" {
			reasons = append(reasons, "NotReady")
		}
	}
	detail := fmt.Sprintf(" %s %d/%d", inspectValue(phase), ready, len(p.Spec.Containers))
	if len(reasons) != 0 {
		detail += " " + strings.Join(reasons, " ")
	}
	detail += fmt.Sprintf(" restarts=%d", restarts)
	if ip := p.Status.PodIP; ip != "" {
		detail += " " + inspectValue(ip)
	}
	return detail, unready || len(reasons) != 0
}

func inspectContainerProblem(state apis.ContainerState) string {
	if w := state.Waiting; w != nil {
		if w.Reason == "" {
			return "Waiting"
		}
		return inspectValue(w.Reason)
	}
	if t := state.Terminated; t != nil && (t.ExitCode != 0 || t.Signal != 0) {
		reason := t.Reason
		if reason == "" {
			reason = "Terminated"
		}
		detail := fmt.Sprintf("%s exit=%d", inspectValue(reason), t.ExitCode)
		if t.Signal != 0 {
			detail += fmt.Sprintf(" signal=%d", t.Signal)
		}
		return detail
	}
	return ""
}

func inspectPodReason(p *apis.Pod) string {
	if p.Status.Reason != "" {
		return p.Status.Reason
	}
	var readyReason string
	for _, c := range p.Status.Conditions {
		if c.Status == "True" || c.Reason == "" {
			continue
		}
		if c.Type == "PodScheduled" {
			return c.Reason // e.g. Unschedulable before any container starts
		}
		if c.Type == "Ready" {
			readyReason = c.Reason
		}
	}
	return readyReason
}

func describeService(s *apis.Service, endpoints int) (string, bool) {
	address := s.Spec.ClusterIP
	if address == "" {
		address = "<pending>"
	} else if address != "None" && len(s.Spec.Ports) != 0 {
		ports := make([]int32, 0, len(s.Spec.Ports))
		for _, p := range s.Spec.Ports {
			ports = append(ports, p.Port)
		}
		slices.Sort(ports)
		portStrings := make([]string, 0, len(ports))
		for _, port := range ports {
			portStrings = append(portStrings, strconv.FormatInt(int64(port), 10))
		}
		address = net.JoinHostPort(address, strings.Join(portStrings, ","))
	}
	detail := fmt.Sprintf(" %s -> %d endpoints", inspectValue(address), endpoints)
	if s.Metadata.DeletionTimestamp != nil {
		return detail, false // the caller marks every deleting object Terminating
	}
	if s.Spec.ClusterIP == "" {
		return detail + " NoClusterIP", true
	}
	// The controller only supplies endpoints for selected, routed Services.
	if s.Spec.ClusterIP != "None" && len(s.Spec.Selector) != 0 && endpoints == 0 {
		return detail + " NoEndpoints", true
	}
	return detail, false
}

func readyServiceEndpoints(services []apis.Service, endpointSlices []apis.EndpointSlice) map[string]int {
	byUID := make(map[string]*apis.Service)
	for i := range services {
		s := &services[i]
		if s.Metadata.UID != "" && s.Metadata.DeletionTimestamp == nil {
			byUID[s.Metadata.UID] = s
		}
	}
	addresses := make(map[string]map[string]struct{})
	for _, slice := range endpointSlices {
		if slice.Metadata.DeletionTimestamp != nil {
			continue
		}
		for _, ref := range slice.Metadata.OwnerReferences {
			s := byUID[ref.UID]
			if s == nil || !inspectOwnerMatches(&slice.Metadata, ref, &s.Metadata, s.TypeMeta) ||
				slice.Metadata.Labels[apis.ServiceNameLabel] != s.Metadata.Name {
				continue
			}
			// A Service edit can commit before the controller has updated its
			// slices. An older generation describes the previous Service spec.
			if gen := slice.Metadata.Annotations[apis.ServiceGenerationAnnotation]; gen != "" && gen != strconv.FormatInt(s.Metadata.Generation, 10) {
				continue
			}
			if addresses[ref.UID] == nil {
				addresses[ref.UID] = make(map[string]struct{})
			}
			for _, ep := range slice.Endpoints {
				if ep.Conditions.Ready == nil || !*ep.Conditions.Ready || ep.Conditions.Terminating != nil && *ep.Conditions.Terminating {
					continue
				}
				// Slices can be split by port group. Count each ready address
				// once for the Service, regardless of how many ports use it.
				for _, ip := range ep.Addresses {
					if ip != "" {
						addresses[ref.UID][ip] = struct{}{}
					}
				}
			}
		}
	}
	counts := make(map[string]int, len(addresses))
	for uid, ips := range addresses {
		counts[uid] = len(ips)
	}
	return counts
}

// Status reasons and annotations can contain arbitrary text. Quote them when
// needed so a reason cannot introduce extra rows or terminal control codes.
func inspectValue(s string) string {
	if strings.ContainsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || !unicode.IsPrint(r) }) {
		return strconv.Quote(s)
	}
	return s
}
