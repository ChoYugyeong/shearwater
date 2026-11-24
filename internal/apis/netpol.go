package apis

import (
	"fmt"
	"net/netip"
	"slices"
)

const (
	PolicyTypeIngress = "Ingress"
	PolicyTypeEgress  = "Egress"
)

func init() {
	Register(&Resource{
		Group: "networking.k8s.io", Version: "v1", Plural: "networkpolicies", Singular: "networkpolicy", Kind: "NetworkPolicy",
		ShortNames: []string{"netpol"},
		Namespaced: true, Create: true, Update: true, Delete: true,
		New:      func() Object { return &NetworkPolicy{} },
		Default:  func(o Object) { DefaultNetworkPolicy(o.(*NetworkPolicy)) },
		Validate: func(o Object) ErrorList { return ValidateNetworkPolicy(o.(*NetworkPolicy)) },
		ValidateUpdate: func(o, old Object) ErrorList {
			return ValidateNetworkPolicyUpdate(o.(*NetworkPolicy), old.(*NetworkPolicy))
		},
	})
}

// ---- NetworkPolicy (networking.k8s.io/v1) ----

type NetworkPolicy struct {
	TypeMeta
	Metadata ObjectMeta        `json:"metadata"`
	Spec     NetworkPolicySpec `json:"spec,omitzero"`
}

func (o *NetworkPolicy) Meta() *ObjectMeta { return &o.Metadata }

type NetworkPolicySpec struct {
	PodSelector LabelSelector              `json:"podSelector"`
	Ingress     []NetworkPolicyIngressRule `json:"ingress,omitempty"`
	Egress      []NetworkPolicyEgressRule  `json:"egress,omitempty"`
	PolicyTypes []string                   `json:"policyTypes,omitempty"`
}

type NetworkPolicyIngressRule struct {
	Ports []NetworkPolicyPort `json:"ports,omitempty"`
	From  []NetworkPolicyPeer `json:"from,omitempty"`
}

type NetworkPolicyEgressRule struct {
	Ports []NetworkPolicyPort `json:"ports,omitempty"`
	To    []NetworkPolicyPeer `json:"to,omitempty"`
}

type NetworkPolicyPort struct {
	Protocol string       `json:"protocol,omitempty"`
	Port     *IntOrString `json:"port,omitempty"`
	EndPort  *int32       `json:"endPort,omitempty"`
}

type NetworkPolicyPeer struct {
	PodSelector       *LabelSelector `json:"podSelector,omitempty"`
	NamespaceSelector *LabelSelector `json:"namespaceSelector,omitempty"`
	IPBlock           *IPBlock       `json:"ipBlock,omitempty"`
}

type IPBlock struct {
	CIDR   string   `json:"cidr"`
	Except []string `json:"except,omitempty"`
}

// ---- Defaults and validation ----

// DefaultNetworkPolicy applies Kubernetes NetworkPolicy defaults:
// policyTypes defaults to [Ingress], plus Egress whenever egress rules exist;
// each port's protocol defaults to TCP.
func DefaultNetworkPolicy(np *NetworkPolicy) {
	if len(np.Spec.PolicyTypes) == 0 {
		np.Spec.PolicyTypes = []string{PolicyTypeIngress}
		if len(np.Spec.Egress) > 0 {
			np.Spec.PolicyTypes = append(np.Spec.PolicyTypes, PolicyTypeEgress)
		}
	}
	for i := range np.Spec.Ingress {
		for j := range np.Spec.Ingress[i].Ports {
			if np.Spec.Ingress[i].Ports[j].Protocol == "" {
				np.Spec.Ingress[i].Ports[j].Protocol = "TCP"
			}
		}
	}
	for i := range np.Spec.Egress {
		for j := range np.Spec.Egress[i].Ports {
			if np.Spec.Egress[i].Ports[j].Protocol == "" {
				np.Spec.Egress[i].Ports[j].Protocol = "TCP"
			}
		}
	}
}

func ValidateNetworkPolicy(np *NetworkPolicy) ErrorList {
	var errs ErrorList
	validateMeta(&np.Metadata, IsDNS1123Subdomain, nameSubdomain, &errs)
	if _, err := SelectorFrom(&np.Spec.PodSelector); err != nil {
		errs.invalid("spec.podSelector", "", err.Error())
	}

	seenTypes := map[string]bool{}
	for i, pt := range np.Spec.PolicyTypes {
		f := fmt.Sprintf("spec.policyTypes[%d]", i)
		if !slices.Contains([]string{PolicyTypeIngress, PolicyTypeEgress}, pt) {
			errs.oneOf(f, pt, PolicyTypeIngress, PolicyTypeEgress)
			continue
		}
		if seenTypes[pt] {
			errs.duplicate(f, pt)
		}
		seenTypes[pt] = true
	}

	for i, rule := range np.Spec.Ingress {
		rf := fmt.Sprintf("spec.ingress[%d]", i)
		for j, p := range rule.Ports {
			validateNetworkPolicyPort(p, fmt.Sprintf("%s.ports[%d]", rf, j), &errs)
		}
		for j, peer := range rule.From {
			validateNetworkPolicyPeer(peer, fmt.Sprintf("%s.from[%d]", rf, j), &errs)
		}
	}
	for i, rule := range np.Spec.Egress {
		rf := fmt.Sprintf("spec.egress[%d]", i)
		for j, p := range rule.Ports {
			validateNetworkPolicyPort(p, fmt.Sprintf("%s.ports[%d]", rf, j), &errs)
		}
		for j, peer := range rule.To {
			validateNetworkPolicyPeer(peer, fmt.Sprintf("%s.to[%d]", rf, j), &errs)
		}
	}
	return errs
}

func ValidateNetworkPolicyUpdate(_, _ *NetworkPolicy) ErrorList {
	return nil
}

func validateNetworkPolicyPort(p NetworkPolicyPort, field string, errs *ErrorList) {
	switch p.Protocol {
	case "TCP", "UDP":
	case "SCTP":
		errs.unsupported(field+".protocol", "only TCP and UDP protocols are supported")
	default:
		errs.oneOf(field+".protocol", p.Protocol, "TCP", "UDP")
	}

	if p.Port != nil {
		if p.Port.IsString {
			if !IsDNS1123Label(p.Port.Str) {
				errs.invalid(field+".port", p.Port.Str, "must be "+nameLabel)
			}
			if p.EndPort != nil {
				errs.invalid(field+".endPort", *p.EndPort, "may not be specified when `port` is a named port")
			}
		} else {
			if p.Port.Int < 1 || p.Port.Int > 65535 {
				errs.invalid(field+".port", p.Port.Int, "must be between 1 and 65535")
			}
			if p.EndPort != nil {
				if *p.EndPort < 1 || *p.EndPort > 65535 {
					errs.invalid(field+".endPort", *p.EndPort, "must be between 1 and 65535")
				} else if *p.EndPort < p.Port.Int {
					errs.invalid(field+".endPort", *p.EndPort, "must be greater than or equal to `port`")
				}
			}
		}
	} else if p.EndPort != nil {
		errs.invalid(field+".endPort", *p.EndPort, "may not be specified when `port` is not specified")
	}
}

func validateNetworkPolicyPeer(peer NetworkPolicyPeer, field string, errs *ErrorList) {
	if peer.PodSelector == nil && peer.NamespaceSelector == nil && peer.IPBlock == nil {
		errs.invalid(field, "", "must specify at least one of `podSelector`, `namespaceSelector` or `ipBlock`")
		return
	}
	if peer.IPBlock != nil {
		if peer.PodSelector != nil || peer.NamespaceSelector != nil {
			errs.invalid(field+".ipBlock", "", "may not be combined with `podSelector` or `namespaceSelector`")
		}
		validateIPBlock(peer.IPBlock, field+".ipBlock", errs)
	}
	if peer.PodSelector != nil {
		if _, err := SelectorFrom(peer.PodSelector); err != nil {
			errs.invalid(field+".podSelector", "", err.Error())
		}
	}
	if peer.NamespaceSelector != nil {
		if _, err := SelectorFrom(peer.NamespaceSelector); err != nil {
			errs.invalid(field+".namespaceSelector", "", err.Error())
		}
	}
}

func validateIPBlock(b *IPBlock, field string, errs *ErrorList) {
	if b.CIDR == "" {
		errs.required(field + ".cidr")
		return
	}
	cidr, err := netip.ParsePrefix(b.CIDR)
	if err != nil {
		errs.invalid(field+".cidr", b.CIDR, "must be a valid IPv4 CIDR")
		return
	}
	if !cidr.Addr().Is4() {
		errs.unsupported(field+".cidr", "only IPv4 CIDRs are supported")
		return
	}
	cidr = cidr.Masked()
	for i, raw := range b.Except {
		ef := fmt.Sprintf("%s.except[%d]", field, i)
		ex, err := netip.ParsePrefix(raw)
		if err != nil {
			errs.invalid(ef, raw, "must be a valid IPv4 CIDR")
			continue
		}
		if !ex.Addr().Is4() {
			errs.unsupported(ef, "only IPv4 CIDRs are supported")
			continue
		}
		ex = ex.Masked()
		if !cidr.Contains(ex.Addr()) || ex.Bits() <= cidr.Bits() {
			errs.invalid(ef, raw, "must be a strict subset of `cidr` "+cidr.String())
		}
	}
}
