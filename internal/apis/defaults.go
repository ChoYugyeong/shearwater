package apis

import "strings"

// Defaults from design 3.6. Stored-only fields get no default.

func defaultPodSpec(s *PodSpec) {
	if s.RestartPolicy == "" {
		s.RestartPolicy = "Always"
	}
	if s.TerminationGracePeriodSeconds == nil {
		s.TerminationGracePeriodSeconds = ptr(int64(30))
	}
	for i := range s.Containers {
		c := &s.Containers[i]
		if c.ImagePullPolicy == "" {
			c.ImagePullPolicy = "IfNotPresent"
			if imageIsLatest(c.Image) {
				c.ImagePullPolicy = "Always"
			}
		}
		for j := range c.Ports {
			if c.Ports[j].Protocol == "" {
				c.Ports[j].Protocol = "TCP"
			}
		}
	}
}

// imageIsLatest reports whether the reference has no tag or the tag "latest"
// (and no digest).
func imageIsLatest(image string) bool {
	if strings.Contains(image, "@") {
		return false
	}
	last := image[strings.LastIndex(image, "/")+1:]
	_, tag, ok := strings.Cut(last, ":")
	return !ok || tag == "latest"
}

func DefaultPod(p *Pod) { defaultPodSpec(&p.Spec) }

func DefaultDeployment(d *Deployment) {
	s := &d.Spec
	if s.Replicas == nil {
		s.Replicas = ptr(int32(1))
	}
	if s.Strategy.Type == "" {
		s.Strategy.Type = "RollingUpdate"
	}
	if s.Strategy.Type == "RollingUpdate" {
		if s.Strategy.RollingUpdate == nil {
			s.Strategy.RollingUpdate = &RollingUpdateDeployment{}
		}
		ru := s.Strategy.RollingUpdate
		if ru.MaxSurge == nil {
			ru.MaxSurge = ptr(Str("25%"))
		}
		if ru.MaxUnavailable == nil {
			ru.MaxUnavailable = ptr(Str("25%"))
		}
	}
	if s.RevisionHistoryLimit == nil {
		s.RevisionHistoryLimit = ptr(int32(10))
	}
	defaultPodSpec(&s.Template.Spec)
}

func DefaultReplicaSet(r *ReplicaSet) {
	if r.Spec.Replicas == nil {
		r.Spec.Replicas = ptr(int32(1))
	}
	defaultPodSpec(&r.Spec.Template.Spec)
}

func DefaultService(s *Service) {
	if s.Spec.Type == "" {
		s.Spec.Type = "ClusterIP"
	}
	for i := range s.Spec.Ports {
		p := &s.Spec.Ports[i]
		if p.Protocol == "" {
			p.Protocol = "TCP"
		}
		if p.TargetPort.IsZero() {
			p.TargetPort = Int(p.Port)
		}
	}
}

func ptr[T any](v T) *T { return &v }
