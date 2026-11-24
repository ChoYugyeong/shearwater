// Package podsecurity enforces namespace Pod Security labels at admission.
package podsecurity

import (
	"encoding/json"
	"fmt"
	"slices"

	"shearwater/internal/admission"
	"shearwater/internal/apis"
	"shearwater/internal/store"
)

const EnforceLabel = "pod-security.kubernetes.io/enforce"

type securityContext struct {
	Privileged               bool   `json:"privileged"`
	RunAsNonRoot             *bool  `json:"runAsNonRoot"`
	RunAsUser                *int64 `json:"runAsUser"`
	AllowPrivilegeEscalation *bool  `json:"allowPrivilegeEscalation"`
	SeccompProfile           *struct {
		Type string `json:"type"`
	} `json:"seccompProfile"`
	Capabilities struct {
		Add  []string `json:"add"`
		Drop []string `json:"drop"`
	} `json:"capabilities"`
}

func parse(raw json.RawMessage) (s securityContext, err error) {
	if len(raw) != 0 {
		err = json.Unmarshal(raw, &s)
	}
	return
}

func check(level string, spec *apis.PodSpec) error {
	if level == "" || level == "privileged" {
		return nil
	}
	if level != "baseline" && level != "restricted" {
		return fmt.Errorf("unknown enforcement level %q", level)
	}
	if spec.HostNetwork {
		return fmt.Errorf("hostNetwork must be false")
	}
	for _, volume := range spec.Volumes {
		if volume.HostPath != nil {
			return fmt.Errorf("hostPath volume %q is forbidden", volume.Name)
		}
	}
	pod, err := parse(spec.SecurityContext)
	if err != nil {
		return fmt.Errorf("invalid pod securityContext: %w", err)
	}
	if pod.SeccompProfile != nil && pod.SeccompProfile.Type != "RuntimeDefault" && pod.SeccompProfile.Type != "Localhost" {
		return fmt.Errorf("pod seccompProfile must be RuntimeDefault or Localhost")
	}
	if level == "restricted" && ((pod.RunAsNonRoot != nil && !*pod.RunAsNonRoot) || (pod.RunAsUser != nil && *pod.RunAsUser == 0)) {
		return fmt.Errorf("pod must run as non-root")
	}
	for _, c := range spec.Containers {
		ctx, err := parse(c.SecurityContext)
		if err != nil {
			return fmt.Errorf("container %q: invalid securityContext: %w", c.Name, err)
		}
		if ctx.Privileged {
			return fmt.Errorf("container %q must not be privileged", c.Name)
		}
		if ctx.SeccompProfile != nil && ctx.SeccompProfile.Type != "RuntimeDefault" && ctx.SeccompProfile.Type != "Localhost" {
			return fmt.Errorf("container %q seccompProfile must be RuntimeDefault or Localhost", c.Name)
		}
		for _, cap := range ctx.Capabilities.Add {
			if !baselineCapability(cap) || (level == "restricted" && cap != "NET_BIND_SERVICE") {
				return fmt.Errorf("container %q must not add capability %s", c.Name, cap)
			}
		}
		if level == "restricted" {
			nonRoot := ctx.RunAsNonRoot
			if nonRoot == nil {
				nonRoot = pod.RunAsNonRoot
			}
			if nonRoot == nil || !*nonRoot || (ctx.RunAsUser != nil && *ctx.RunAsUser == 0) {
				return fmt.Errorf("container %q must run as non-root", c.Name)
			}
			if ctx.AllowPrivilegeEscalation == nil || *ctx.AllowPrivilegeEscalation {
				return fmt.Errorf("container %q must set allowPrivilegeEscalation=false", c.Name)
			}
			if ctx.SeccompProfile == nil && pod.SeccompProfile == nil {
				return fmt.Errorf("container %q must specify a seccompProfile", c.Name)
			}
			if !slices.Contains(ctx.Capabilities.Drop, "ALL") {
				return fmt.Errorf("container %q must drop ALL capabilities", c.Name)
			}
		}
	}
	return nil
}

func baselineCapability(cap string) bool {
	switch cap {
	case "AUDIT_WRITE", "CHOWN", "DAC_OVERRIDE", "FOWNER", "FSETID", "KILL", "MKNOD", "NET_BIND_SERVICE",
		"SETFCAP", "SETGID", "SETPCAP", "SETUID", "SYS_CHROOT":
		return true
	}
	return false
}

// Admit checks templates too, so a workload owner sees the refusal immediately.
func Admit(r admission.Request) error {
	var spec *apis.PodSpec
	switch obj := r.Object.(type) {
	case *apis.Pod:
		spec = &obj.Spec
	case *apis.Deployment:
		spec = &obj.Spec.Template.Spec
	case *apis.ReplicaSet:
		spec = &obj.Spec.Template.Spec
	default:
		return nil
	}
	rec, err := r.Tx.Get(store.Key{Resource: "namespaces", Name: r.Namespace})
	if err != nil {
		return err
	}
	var ns apis.Namespace
	if err := json.Unmarshal(rec.Data, &ns); err != nil {
		return err
	}
	level := ns.Metadata.Labels[EnforceLabel]
	if err := check(level, spec); err != nil {
		return admission.Forbid("POD_SECURITY", "Adjust the Pod securityContext or volume configuration to satisfy namespace policy.", "violates PodSecurity %q: %s", level+":latest", err)
	}
	return nil
}
