package apis

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"
)

// ServiceCIDR is the range ClusterIPs come from (design 7).
var ServiceCIDR = netip.MustParsePrefix("10.96.0.0/16")

// PodCIDR is the range pod addresses come from (design 7). The first address
// is the bridge's.
var PodCIDR = netip.MustParsePrefix("10.244.0.0/24")

const MaxGraceSeconds = int64(math.MaxInt64) / int64(time.Second)

// FieldError is one problem with one field. Unsupported marks a field that
// Kubernetes has but Shearwater refuses on purpose (the "rejected" grade).
type FieldError struct {
	Reason      string // FieldValueRequired, FieldValueInvalid, ...
	Field       string
	Detail      string
	Unsupported bool
}

func (e FieldError) String() string { return e.Field + ": " + e.Detail }

type ErrorList []FieldError

func (l *ErrorList) required(field string) {
	*l = append(*l, FieldError{Reason: "FieldValueRequired", Field: field, Detail: "Required value"})
}

func (l *ErrorList) invalid(field string, value any, why string) {
	*l = append(*l, FieldError{Reason: "FieldValueInvalid", Field: field, Detail: fmt.Sprintf("Invalid value: %s: %s", quote(value), why)})
}

func (l *ErrorList) oneOf(field, value string, allowed ...string) {
	*l = append(*l, FieldError{Reason: "FieldValueNotSupported", Field: field,
		Detail: fmt.Sprintf("Unsupported value: %q: supported values: %s", value, strings.Join(allowed, ", "))})
}

func (l *ErrorList) duplicate(field string, value any) {
	*l = append(*l, FieldError{Reason: "FieldValueDuplicate", Field: field, Detail: "Duplicate value: " + quote(value)})
}

func (l *ErrorList) forbidden(field, why string) {
	*l = append(*l, FieldError{Reason: "FieldValueForbidden", Field: field, Detail: "Forbidden: " + why})
}

// unsupported records a rejected-grade field: what is not supported and what to use instead.
func (l *ErrorList) unsupported(field, why string) {
	*l = append(*l, FieldError{Reason: "FieldValueForbidden", Field: field, Detail: "Not supported by Shearwater: " + why, Unsupported: true})
}

func quote(v any) string {
	if s, ok := v.(string); ok {
		return strconv.Quote(s)
	}
	return fmt.Sprint(v)
}

const (
	nameSubdomain = "a lowercase RFC 1123 subdomain: lowercase letters, digits, '-' or '.', starting and ending with a letter or digit"
	nameLabel     = "a lowercase RFC 1123 label: lowercase letters, digits or '-', starting and ending with a letter or digit, at most 63 characters"
	nameDNS1035   = "an RFC 1035 label: lowercase letters, digits or '-', starting with a letter, at most 63 characters"
)

func validateLabels(labels map[string]string, field string, errs *ErrorList) {
	for k, v := range labels {
		if !IsQualifiedName(k) {
			errs.invalid(field, k, "label key must be [prefix/]name with letters, digits, '-', '_' or '.'")
		}
		if !IsLabelValue(v) {
			errs.invalid(field, v, "label value must be empty or at most 63 letters, digits, '-', '_' or '.', starting and ending with a letter or digit")
		}
	}
}

func validateMeta(m *ObjectMeta, nameOK func(string) bool, nameRule string, errs *ErrorList) {
	switch {
	case m.Name == "":
		errs.required("metadata.name")
	case !nameOK(m.Name):
		errs.invalid("metadata.name", m.Name, "must be "+nameRule)
	}
	validateLabels(m.Labels, "metadata.labels", errs)
	for k := range m.Annotations {
		if !IsQualifiedName(k) {
			errs.invalid("metadata.annotations", k, "annotation key must be [prefix/]name")
		}
	}
	for i, o := range m.OwnerReferences {
		f := fmt.Sprintf("metadata.ownerReferences[%d]", i)
		if o.APIVersion == "" {
			errs.required(f + ".apiVersion")
		}
		if o.Kind == "" {
			errs.required(f + ".kind")
		}
		if o.Name == "" {
			errs.required(f + ".name")
		}
		if o.UID == "" {
			errs.required(f + ".uid")
		}
	}
	for i, f := range m.Finalizers {
		if !IsQualifiedName(f) {
			errs.invalid(fmt.Sprintf("metadata.finalizers[%d]", i), f, "must be [prefix/]name")
		}
	}
}

func validatePodSpec(s *PodSpec, path string, errs *ErrorList) {
	if !rawEmpty(s.InitContainers) {
		errs.unsupported(path+".initContainers", "init containers are not implemented; run the setup step inside the main container's command")
	}
	if s.HostNetwork {
		errs.unsupported(path+".hostNetwork", "pods always get their own network namespace; remove hostNetwork")
	}
	if s.ServiceAccountName != "" {
		errs.unsupported(path+".serviceAccountName", "there are no service accounts; remove the field")
	}
	if !slices.Contains([]string{"Always", "OnFailure", "Never"}, s.RestartPolicy) {
		errs.oneOf(path+".restartPolicy", s.RestartPolicy, "Always", "OnFailure", "Never")
	}
	if g := s.TerminationGracePeriodSeconds; g != nil && (*g < 0 || *g > MaxGraceSeconds) {
		errs.invalid(path+".terminationGracePeriodSeconds", *g, fmt.Sprintf("must be between 0 and %d", MaxGraceSeconds))
	}
	if s.NodeName != "" && !IsDNS1123Subdomain(s.NodeName) {
		errs.invalid(path+".nodeName", s.NodeName, "must be "+nameSubdomain)
	}

	volumes := map[string]bool{}
	for i, v := range s.Volumes {
		f := fmt.Sprintf("%s.volumes[%d]", path, i)
		switch {
		case v.Name == "":
			errs.required(f + ".name")
		case !IsDNS1123Label(v.Name):
			errs.invalid(f+".name", v.Name, "must be "+nameLabel)
		case volumes[v.Name]:
			errs.duplicate(f+".name", v.Name)
		}
		volumes[v.Name] = true
		switch {
		case v.EmptyDir != nil && v.HostPath != nil:
			errs.forbidden(f, "a volume has exactly one source; both emptyDir and hostPath are set")
		case v.EmptyDir == nil && v.HostPath == nil:
			errs.unsupported(f, "only emptyDir and hostPath volumes are implemented (no configMap, secret or persistentVolumeClaim)")
		case v.HostPath != nil && !strings.HasPrefix(v.HostPath.Path, "/"):
			errs.invalid(f+".hostPath.path", v.HostPath.Path, "must be an absolute path")
		}
		if v.HostPath != nil {
			switch v.HostPath.Type {
			case "", "Directory", "DirectoryOrCreate", "File", "FileOrCreate", "Socket", "CharDevice", "BlockDevice":
			default:
				errs.oneOf(f+".hostPath.type", v.HostPath.Type, "", "Directory", "DirectoryOrCreate", "File", "FileOrCreate", "Socket", "CharDevice", "BlockDevice")
			}
		}
	}

	if len(s.Containers) == 0 {
		errs.required(path + ".containers")
	}
	names := map[string]bool{}
	for i := range s.Containers {
		c := &s.Containers[i]
		f := fmt.Sprintf("%s.containers[%d]", path, i)
		switch {
		case c.Name == "":
			errs.required(f + ".name")
		case !IsDNS1123Label(c.Name):
			errs.invalid(f+".name", c.Name, "must be "+nameLabel)
		case names[c.Name]:
			errs.duplicate(f+".name", c.Name)
		}
		names[c.Name] = true
		if c.Image == "" {
			errs.required(f + ".image")
		} else if strings.ContainsAny(c.Image, " \t\n") {
			errs.invalid(f+".image", c.Image, "must not contain whitespace")
		}
		if !slices.Contains([]string{"Always", "IfNotPresent", "Never"}, c.ImagePullPolicy) {
			errs.oneOf(f+".imagePullPolicy", c.ImagePullPolicy, "Always", "IfNotPresent", "Never")
		}
		if !rawEmpty(c.EnvFrom) {
			errs.unsupported(f+".envFrom", "there are no ConfigMaps or Secrets; list the variables under env with literal values")
		}
		for j, e := range c.Env {
			ef := fmt.Sprintf("%s.env[%d]", f, j)
			if e.Name == "" {
				errs.required(ef + ".name")
			}
			if !rawEmpty(e.ValueFrom) {
				errs.unsupported(ef+".valueFrom", "there are no ConfigMaps, Secrets or downward API; use a literal value")
			}
		}
		for j, p := range c.Ports {
			pf := fmt.Sprintf("%s.ports[%d]", f, j)
			if p.ContainerPort < 1 || p.ContainerPort > 65535 {
				errs.invalid(pf+".containerPort", p.ContainerPort, "must be between 1 and 65535")
			}
			if !slices.Contains([]string{"TCP", "UDP", "SCTP"}, p.Protocol) {
				errs.oneOf(pf+".protocol", p.Protocol, "TCP", "UDP", "SCTP")
			}
			if p.Name != "" && !IsDNS1123Label(p.Name) {
				errs.invalid(pf+".name", p.Name, "must be "+nameLabel)
			}
		}
		for name := range c.Resources.Limits {
			if name != "cpu" && name != "memory" {
				errs.unsupported(f+".resources.limits."+name, "only cpu and memory limits are enforced")
			}
		}
		for j, m := range c.VolumeMounts {
			mf := fmt.Sprintf("%s.volumeMounts[%d]", f, j)
			switch {
			case m.Name == "":
				errs.required(mf + ".name")
			case !volumes[m.Name]:
				errs.invalid(mf+".name", m.Name, "no volume with this name in spec.volumes")
			}
			if !strings.HasPrefix(m.MountPath, "/") {
				errs.invalid(mf+".mountPath", m.MountPath, "must be an absolute path")
			}
		}
	}
}

func ValidatePod(p *Pod) ErrorList {
	var errs ErrorList
	validateMeta(&p.Metadata, IsDNS1123Subdomain, nameSubdomain, &errs)
	validatePodSpec(&p.Spec, "spec", &errs)
	return errs
}

// ValidatePodUpdate allows changing container images and nothing else in the
// spec, as Kubernetes does. (The scheduler and node agent write through the
// store, not through this check.)
func ValidatePodUpdate(p, old *Pod) ErrorList {
	var errs ErrorList
	a := p.Spec
	a.Containers = slices.Clone(p.Spec.Containers)
	if len(a.Containers) == len(old.Spec.Containers) {
		for i := range a.Containers {
			a.Containers[i].Image = old.Spec.Containers[i].Image
		}
	}
	na, _ := json.Marshal(a)
	nb, _ := json.Marshal(old.Spec)
	if !bytes.Equal(na, nb) {
		errs.forbidden("spec", "a pod's spec cannot change after creation, except spec.containers[*].image; delete and recreate the pod, or use a Deployment")
	}
	return errs
}

func validateTemplate(sel *LabelSelector, t *PodTemplateSpec, errs *ErrorList) {
	validateLabels(t.Metadata.Labels, "spec.template.metadata.labels", errs)
	validatePodSpec(&t.Spec, "spec.template.spec", errs)
	if t.Spec.RestartPolicy != "Always" && t.Spec.RestartPolicy != "" {
		errs.oneOf("spec.template.spec.restartPolicy", t.Spec.RestartPolicy, "Always")
	}
	if sel == nil || len(sel.MatchLabels)+len(sel.MatchExpressions) == 0 {
		errs.required("spec.selector")
		return
	}
	s, err := SelectorFrom(sel)
	if err != nil {
		errs.invalid("spec.selector", "", err.Error())
	} else if !s.Matches(t.Metadata.Labels) {
		errs.invalid("spec.template.metadata.labels", fmt.Sprint(t.Metadata.Labels), "`selector` does not match template `labels`")
	}
}

func validatePercentOrInt(v *IntOrString, field string, errs *ErrorList) (zero bool) {
	if v == nil {
		return false
	}
	if !v.IsString {
		if v.Int < 0 {
			errs.invalid(field, v.Int, "must be 0 or more")
		}
		return v.Int == 0
	}
	n, err := strconv.Atoi(strings.TrimSuffix(v.Str, "%"))
	if err != nil || n < 0 || !strings.HasSuffix(v.Str, "%") {
		errs.invalid(field, v.Str, "must be a non-negative integer or a percentage such as \"25%\"")
		return false
	}
	return n == 0
}

func ValidateDeployment(d *Deployment) ErrorList {
	var errs ErrorList
	validateMeta(&d.Metadata, IsDNS1123Subdomain, nameSubdomain, &errs)
	s := &d.Spec
	if s.Replicas != nil && *s.Replicas < 0 {
		errs.invalid("spec.replicas", *s.Replicas, "must be 0 or more")
	}
	if s.RevisionHistoryLimit != nil && *s.RevisionHistoryLimit < 0 {
		errs.invalid("spec.revisionHistoryLimit", *s.RevisionHistoryLimit, "must be 0 or more")
	}
	switch s.Strategy.Type {
	case "RollingUpdate":
		if ru := s.Strategy.RollingUpdate; ru != nil {
			z1 := validatePercentOrInt(ru.MaxSurge, "spec.strategy.rollingUpdate.maxSurge", &errs)
			z2 := validatePercentOrInt(ru.MaxUnavailable, "spec.strategy.rollingUpdate.maxUnavailable", &errs)
			if z1 && z2 {
				errs.invalid("spec.strategy.rollingUpdate.maxUnavailable", ru.MaxUnavailable.String(), "may not be 0 when maxSurge is 0")
			}
		}
	case "Recreate":
		if s.Strategy.RollingUpdate != nil {
			errs.forbidden("spec.strategy.rollingUpdate", "may not be set when the strategy type is Recreate")
		}
	default:
		errs.oneOf("spec.strategy.type", s.Strategy.Type, "RollingUpdate", "Recreate")
	}
	validateTemplate(s.Selector, &s.Template, &errs)
	return errs
}

func selectorChanged(a, b *LabelSelector) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return !bytes.Equal(x, y)
}

func ValidateDeploymentUpdate(d, old *Deployment) ErrorList {
	var errs ErrorList
	if selectorChanged(d.Spec.Selector, old.Spec.Selector) {
		errs.forbidden("spec.selector", "the selector cannot change after creation; create a new Deployment under another name")
	}
	return errs
}

func ValidateReplicaSet(r *ReplicaSet) ErrorList {
	var errs ErrorList
	validateMeta(&r.Metadata, IsDNS1123Subdomain, nameSubdomain, &errs)
	if r.Spec.Replicas != nil && *r.Spec.Replicas < 0 {
		errs.invalid("spec.replicas", *r.Spec.Replicas, "must be 0 or more")
	}
	validateTemplate(r.Spec.Selector, &r.Spec.Template, &errs)
	return errs
}

func ValidateReplicaSetUpdate(r, old *ReplicaSet) ErrorList {
	var errs ErrorList
	if selectorChanged(r.Spec.Selector, old.Spec.Selector) {
		errs.forbidden("spec.selector", "the selector cannot change after creation")
	}
	return errs
}

// ValidateServiceClusterIP checks the address independently of the rest of the
// Service. Startup reconciliation uses it to validate the IPs already stored
// by M4 without revalidating unrelated fields.
func ValidateServiceClusterIP(ip string) ErrorList {
	if ip == "" || ip == "None" {
		return nil
	}
	var errs ErrorList
	a, err := netip.ParseAddr(ip)
	switch {
	case err != nil:
		errs.invalid("spec.clusterIP", ip, "must be an IPv4 address, \"None\" or empty")
	case !ServiceCIDR.Contains(a) || a == ServiceCIDR.Masked().Addr():
		errs.invalid("spec.clusterIP", ip, "must be inside the service range "+ServiceCIDR.String())
	}
	return errs
}

func ValidateService(s *Service) ErrorList {
	var errs ErrorList
	validateMeta(&s.Metadata, IsDNS1035Label, nameDNS1035, &errs)
	switch s.Spec.Type {
	case "ClusterIP":
	case "NodePort", "LoadBalancer", "ExternalName":
		errs.unsupported("spec.type", "only ClusterIP services are implemented (NodePort is planned for stage 3); use type ClusterIP")
	default:
		errs.oneOf("spec.type", s.Spec.Type, "ClusterIP")
	}
	validateLabels(s.Spec.Selector, "spec.selector", &errs)
	errs = append(errs, ValidateServiceClusterIP(s.Spec.ClusterIP)...)
	if len(s.Spec.Ports) == 0 {
		errs.required("spec.ports")
	}
	names, ports := map[string]bool{}, map[int32]bool{}
	for i, p := range s.Spec.Ports {
		f := fmt.Sprintf("spec.ports[%d]", i)
		if p.Port < 1 || p.Port > 65535 {
			errs.invalid(f+".port", p.Port, "must be between 1 and 65535")
		} else if ports[p.Port] {
			errs.duplicate(f+".port", p.Port)
		}
		ports[p.Port] = true
		switch {
		case p.Name == "" && len(s.Spec.Ports) > 1:
			errs.required(f + ".name")
		case p.Name != "" && !IsDNS1123Label(p.Name):
			errs.invalid(f+".name", p.Name, "must be "+nameLabel)
		case p.Name != "" && names[p.Name]:
			errs.duplicate(f+".name", p.Name)
		}
		names[p.Name] = true
		switch p.Protocol {
		case "TCP":
		case "UDP", "SCTP":
			errs.unsupported(f+".protocol", "the service proxy forwards TCP only")
		default:
			errs.oneOf(f+".protocol", p.Protocol, "TCP")
		}
		if t := p.TargetPort; t.IsString {
			if !IsDNS1123Label(t.Str) {
				errs.invalid(f+".targetPort", t.Str, "must be a port number or a container port name")
			}
		} else if t.Int < 1 || t.Int > 65535 {
			errs.invalid(f+".targetPort", t.Int, "must be between 1 and 65535")
		}
	}
	return errs
}

func ValidateServiceUpdate(s, old *Service) ErrorList {
	var errs ErrorList
	// Creation and startup backfill are the only writers of ClusterIP.
	// Neither PUT nor PATCH can bypass the reservation for a legacy blank IP.
	if s.Spec.ClusterIP != old.Spec.ClusterIP {
		errs.forbidden("spec.clusterIP", "the cluster IP cannot change after it is assigned")
	}
	return errs
}

func ValidateNamespace(n *Namespace) ErrorList {
	var errs ErrorList
	validateMeta(&n.Metadata, IsDNS1123Label, nameLabel, &errs)
	return errs
}
