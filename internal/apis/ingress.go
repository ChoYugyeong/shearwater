package apis

import (
	"fmt"
	"net/netip"
	"slices"
	"strings"
)

const (
	// DefaultIngressClassName is the built-in IngressClass handled by Shearwater's edge.
	DefaultIngressClassName = "shearwater"
	// IngressClassController is the controller identifier for Shearwater's IngressClass.
	IngressClassController = "shearwater.io/ingress"
	// DefaultIngressClassAnnotation marks an IngressClass as the cluster default.
	DefaultIngressClassAnnotation = "ingressclass.kubernetes.io/is-default-class"

	PathTypeExact                  = "Exact"
	PathTypePrefix                 = "Prefix"
	PathTypeImplementationSpecific = "ImplementationSpecific"
)

func init() {
	Register(&Resource{
		Group: "networking.k8s.io", Version: "v1", Plural: "ingresses", Singular: "ingress", Kind: "Ingress",
		ShortNames: []string{"ing"},
		Namespaced: true, Create: true, Update: true, Delete: true, StatusSub: true,
		New:            func() Object { return &Ingress{} },
		Prepare:        func(o Object) { o.(*Ingress).Status = IngressStatus{} },
		Default:        func(o Object) { DefaultIngress(o.(*Ingress)) },
		Validate:       func(o Object) ErrorList { return ValidateIngress(o.(*Ingress)) },
		ValidateUpdate: func(o, old Object) ErrorList { return ValidateIngressUpdate(o.(*Ingress), old.(*Ingress)) },
	})
	Register(&Resource{
		Group: "networking.k8s.io", Version: "v1", Plural: "ingressclasses", Singular: "ingressclass", Kind: "IngressClass",
		Namespaced: false, Create: true, Update: true, Delete: true,
		New:      func() Object { return &IngressClass{} },
		Validate: func(o Object) ErrorList { return ValidateIngressClass(o.(*IngressClass)) },
		ValidateUpdate: func(o, old Object) ErrorList {
			return ValidateIngressClassUpdate(o.(*IngressClass), old.(*IngressClass))
		},
	})
}

// ---- Ingress (networking.k8s.io/v1) ----

type Ingress struct {
	TypeMeta
	Metadata ObjectMeta    `json:"metadata"`
	Spec     IngressSpec   `json:"spec,omitzero"`
	Status   IngressStatus `json:"status,omitzero"`
}

func (o *Ingress) Meta() *ObjectMeta { return &o.Metadata }

type IngressSpec struct {
	IngressClassName *string         `json:"ingressClassName,omitempty"`
	DefaultBackend   *IngressBackend `json:"defaultBackend,omitempty"`
	TLS              []IngressTLS    `json:"tls,omitempty"`
	Rules            []IngressRule   `json:"rules,omitempty"`
}

type IngressTLS struct {
	Hosts      []string `json:"hosts,omitempty"`
	SecretName string   `json:"secretName,omitempty"`
}

type IngressRule struct {
	Host string                `json:"host,omitempty"`
	HTTP *HTTPIngressRuleValue `json:"http,omitempty"`
}

type HTTPIngressRuleValue struct {
	Paths []HTTPIngressPath `json:"paths"`
}

type HTTPIngressPath struct {
	Path     string         `json:"path,omitempty"`
	PathType *string        `json:"pathType"`
	Backend  IngressBackend `json:"backend"`
}

type IngressBackend struct {
	Service  *IngressServiceBackend     `json:"service,omitempty"`
	Resource *TypedLocalObjectReference `json:"resource,omitempty"`
}

type IngressServiceBackend struct {
	Name string             `json:"name"`
	Port ServiceBackendPort `json:"port,omitzero"`
}

type ServiceBackendPort struct {
	Name   string `json:"name,omitempty"`
	Number int32  `json:"number,omitempty"`
}

type TypedLocalObjectReference struct {
	APIGroup *string `json:"apiGroup,omitempty"`
	Kind     string  `json:"kind"`
	Name     string  `json:"name"`
}

type IngressStatus struct {
	LoadBalancer IngressLoadBalancerStatus `json:"loadBalancer,omitzero"`
}

type IngressLoadBalancerStatus struct {
	Ingress []IngressLoadBalancerIngress `json:"ingress,omitempty"`
}

type IngressLoadBalancerIngress struct {
	IP       string              `json:"ip,omitempty"`
	Hostname string              `json:"hostname,omitempty"`
	Ports    []IngressPortStatus `json:"ports,omitempty"`
}

type IngressPortStatus struct {
	Port     int32   `json:"port"`
	Protocol string  `json:"protocol"`
	Error    *string `json:"error,omitempty"`
}

// ---- IngressClass (networking.k8s.io/v1) ----

type IngressClass struct {
	TypeMeta
	Metadata ObjectMeta       `json:"metadata"`
	Spec     IngressClassSpec `json:"spec,omitzero"`
}

func (o *IngressClass) Meta() *ObjectMeta { return &o.Metadata }

type IngressClassSpec struct {
	Controller string                           `json:"controller,omitempty"`
	Parameters *IngressClassParametersReference `json:"parameters,omitempty"`
}

type IngressClassParametersReference struct {
	APIGroup  *string `json:"apiGroup,omitempty"`
	Kind      string  `json:"kind"`
	Name      string  `json:"name"`
	Scope     *string `json:"scope,omitempty"`
	Namespace *string `json:"namespace,omitempty"`
}

// ---- Defaults and validation ----

func DefaultIngress(ing *Ingress) {
	for i := range ing.Spec.Rules {
		if h := ing.Spec.Rules[i].HTTP; h != nil {
			for j := range h.Paths {
				if h.Paths[j].Path == "" && h.Paths[j].PathType != nil && *h.Paths[j].PathType == PathTypePrefix {
					h.Paths[j].Path = "/"
				}
			}
		}
	}
}

func ValidateIngress(ing *Ingress) ErrorList {
	var errs ErrorList
	validateMeta(&ing.Metadata, IsDNS1123Subdomain, nameSubdomain, &errs)
	s := &ing.Spec
	if s.IngressClassName != nil && !IsDNS1123Subdomain(*s.IngressClassName) {
		errs.invalid("spec.ingressClassName", *s.IngressClassName, "must be "+nameSubdomain)
	}
	if s.DefaultBackend == nil && len(s.Rules) == 0 {
		errs.invalid("spec", "", "either `defaultBackend` or `rules` must be specified")
	}
	if s.DefaultBackend != nil {
		validateIngressBackend(s.DefaultBackend, "spec.defaultBackend", &errs)
	}
	for i, rule := range s.Rules {
		rf := fmt.Sprintf("spec.rules[%d]", i)
		if rule.Host != "" {
			validateIngressHost(rule.Host, rf+".host", &errs)
		}
		if rule.HTTP != nil {
			if len(rule.HTTP.Paths) == 0 {
				errs.required(rf + ".http.paths")
			}
			for j, p := range rule.HTTP.Paths {
				pf := fmt.Sprintf("%s.http.paths[%d]", rf, j)
				validateIngressPath(p, pf, &errs)
			}
		}
	}
	for i, tls := range s.TLS {
		tf := fmt.Sprintf("spec.tls[%d]", i)
		for j, h := range tls.Hosts {
			validateIngressHost(h, fmt.Sprintf("%s.hosts[%d]", tf, j), &errs)
		}
		if tls.SecretName != "" && !IsDNS1123Subdomain(tls.SecretName) {
			errs.invalid(tf+".secretName", tls.SecretName, "must be "+nameSubdomain)
		}
	}
	return errs
}

func ValidateIngressUpdate(ing, _ *Ingress) ErrorList {
	return nil
}

func validateIngressHost(host, field string, errs *ErrorList) {
	if _, err := netip.ParseAddr(host); err == nil {
		errs.invalid(field, host, "must be a DNS name, not an IP address")
		return
	}
	if strings.Contains(host, ":") {
		errs.invalid(field, host, "must not include a port")
		return
	}
	if strings.HasPrefix(host, "*.") {
		rest := host[2:]
		if IsDNS1123Subdomain(rest) && strings.Contains(rest, ".") {
			errs.unsupported(field, "wildcard hosts are not supported; list concrete host names")
		} else {
			errs.invalid(field, host, "must be "+nameSubdomain)
		}
		return
	}
	if !IsDNS1123Subdomain(host) {
		errs.invalid(field, host, "must be "+nameSubdomain)
	}
}

func validateIngressPath(p HTTPIngressPath, field string, errs *ErrorList) {
	if p.PathType == nil {
		errs.required(field + ".pathType")
	} else {
		switch pt := *p.PathType; pt {
		case PathTypeExact, PathTypePrefix:
			if !strings.HasPrefix(p.Path, "/") {
				errs.invalid(field+".path", p.Path, "must be an absolute path starting with '/'")
			} else if invalidIngressPathSegments(p.Path) {
				errs.invalid(field+".path", p.Path, "must not contain '//', '/./' or '/../'")
			}
		case PathTypeImplementationSpecific:
			if p.Path != "" && !strings.HasPrefix(p.Path, "/") {
				errs.invalid(field+".path", p.Path, "must be an absolute path starting with '/'")
			}
		default:
			errs.oneOf(field+".pathType", pt, PathTypeExact, PathTypePrefix, PathTypeImplementationSpecific)
		}
	}
	validateIngressBackend(&p.Backend, field+".backend", errs)
}

func invalidIngressPathSegments(p string) bool {
	if strings.Contains(p, "//") {
		return true
	}
	for seg := range strings.SplitSeq(p, "/") {
		if seg == "." || seg == ".." {
			return true
		}
	}
	return false
}

func validateIngressBackend(b *IngressBackend, field string, errs *ErrorList) {
	if b.Resource != nil {
		errs.unsupported(field+".resource", "only service backends are supported")
		if b.Service != nil {
			errs.invalid(field, "", "cannot set both `service` and `resource`")
		}
		return
	}
	if b.Service == nil {
		errs.required(field + ".service")
		return
	}
	if b.Service.Name == "" {
		errs.required(field + ".service.name")
	} else if !IsDNS1035Label(b.Service.Name) {
		errs.invalid(field+".service.name", b.Service.Name, "must be "+nameDNS1035)
	}
	port := b.Service.Port
	hasName := port.Name != ""
	hasNum := port.Number != 0
	switch {
	case hasName && hasNum:
		errs.invalid(field+".service.port", "", "cannot set both `name` and `number`")
	case !hasName && !hasNum:
		errs.required(field + ".service.port")
	case hasNum && (port.Number < 1 || port.Number > 65535):
		errs.invalid(field+".service.port.number", port.Number, "must be between 1 and 65535")
	case hasName && !IsDNS1123Label(port.Name):
		errs.invalid(field+".service.port.name", port.Name, "must be "+nameLabel)
	}
}

func ValidateIngressClass(ic *IngressClass) ErrorList {
	var errs ErrorList
	validateMeta(&ic.Metadata, IsDNS1123Subdomain, nameSubdomain, &errs)
	c := ic.Spec.Controller
	if c == "" {
		errs.required("spec.controller")
	} else {
		prefix, path, ok := strings.Cut(c, "/")
		if len(c) > 250 || !ok || !IsDNS1123Subdomain(prefix) || path == "" || strings.ContainsAny(path, " \t\r\n") {
			errs.invalid("spec.controller", c, "must be a domain-prefixed path such as \"shearwater.io/ingress\"")
		}
	}
	if p := ic.Spec.Parameters; p != nil {
		if p.Kind == "" {
			errs.required("spec.parameters.kind")
		}
		if p.Name == "" {
			errs.required("spec.parameters.name")
		}
		if p.Scope != nil && !slices.Contains([]string{"Cluster", "Namespace"}, *p.Scope) {
			errs.oneOf("spec.parameters.scope", *p.Scope, "Cluster", "Namespace")
		}
		if p.Scope != nil && *p.Scope == "Namespace" && (p.Namespace == nil || *p.Namespace == "") {
			errs.required("spec.parameters.namespace")
		}
	}
	return errs
}

func ValidateIngressClassUpdate(ic, old *IngressClass) ErrorList {
	var errs ErrorList
	if ic.Spec.Controller != old.Spec.Controller {
		errs.forbidden("spec.controller", "field is immutable")
	}
	return errs
}
