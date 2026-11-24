// Package netpol compiles Kubernetes networking.k8s.io/v1 NetworkPolicy objects
// into node-local allow lists and reconciles them against the node's data path.
package netpol

import (
	"cmp"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"maps"
	"net/netip"
	"slices"

	"shearwater/internal/apis"
	"shearwater/internal/runtime"
	"shearwater/internal/store"
)

const namespaceNameLabel = "kubernetes.io/metadata.name"

var anyIPv4 = netip.MustParsePrefix("0.0.0.0/0")

// CompileFromStore reads Pods, Namespaces and NetworkPolicies through
// store.Reader and compiles the node-local NetworkPolicy plan for podCIDR.
func CompileFromStore(r store.Reader, podCIDR netip.Prefix) (runtime.NetpolPlan, error) {
	npRecs, _, err := r.List("networking.k8s.io", "networkpolicies", "")
	if err != nil {
		return runtime.NetpolPlan{}, fmt.Errorf("list networkpolicies: %w", err)
	}
	if len(npRecs) == 0 {
		return runtime.NetpolPlan{HasPolicies: false}, nil
	}
	podRecs, _, err := r.List("", "pods", "")
	if err != nil {
		return runtime.NetpolPlan{}, fmt.Errorf("list pods: %w", err)
	}
	nsRecs, _, err := r.List("", "namespaces", "")
	if err != nil {
		return runtime.NetpolPlan{}, fmt.Errorf("list namespaces: %w", err)
	}

	policies := make([]apis.NetworkPolicy, 0, len(npRecs))
	for _, rec := range npRecs {
		var np apis.NetworkPolicy
		if json.Unmarshal(rec.Data, &np) == nil {
			policies = append(policies, np)
		}
	}
	pods := make([]apis.Pod, 0, len(podRecs))
	for _, rec := range podRecs {
		var p apis.Pod
		if json.Unmarshal(rec.Data, &p) == nil {
			pods = append(pods, p)
		}
	}
	namespaces := make([]apis.Namespace, 0, len(nsRecs))
	for _, rec := range nsRecs {
		var ns apis.Namespace
		if json.Unmarshal(rec.Data, &ns) == nil {
			namespaces = append(namespaces, ns)
		}
	}
	return Compile(pods, namespaces, policies, podCIDR), nil
}

type activePod struct {
	pod    apis.Pod
	ip     netip.Addr
	prefix netip.Prefix
}

type compiledPolicy struct {
	np         apis.NetworkPolicy
	podSel     apis.Selector
	hasIngress bool
	hasEgress  bool
}

type resolvedPort struct {
	protocol string
	port     uint16
	endPort  uint16
}

// Compile computes the per-local-Pod ingress and egress allow lists for one
// node's podCIDR using Kubernetes NetworkPolicy semantics:
//   - A Pod selected by no policy of a given type is unrestricted for that type.
//   - Selected Pods allow only the union of matching rules across selecting policies.
//   - An empty from/to list matches all IPv4 peers; an empty ports list matches all ports.
//   - Named ports resolve against the destination Pod's container ports.
//   - Peers on other nodes are matched by their IPv4 Pod IP.
func Compile(pods []apis.Pod, namespaces []apis.Namespace, policies []apis.NetworkPolicy, podCIDR netip.Prefix) runtime.NetpolPlan {
	if len(policies) == 0 {
		return runtime.NetpolPlan{HasPolicies: false}
	}
	plan := runtime.NetpolPlan{HasPolicies: true}
	if !podCIDR.IsValid() || !podCIDR.Addr().Is4() {
		return plan
	}
	cidr := podCIDR.Masked()
	netAddr := cidr.Addr()
	gateway := netAddr.Next()

	nsLabels := make(map[string]map[string]string, len(namespaces))
	for _, ns := range namespaces {
		lbl := maps.Clone(ns.Metadata.Labels)
		if lbl == nil {
			lbl = map[string]string{}
		}
		if _, ok := lbl[namespaceNameLabel]; !ok && ns.Metadata.Name != "" {
			lbl[namespaceNameLabel] = ns.Metadata.Name
		}
		nsLabels[ns.Metadata.Name] = lbl
	}
	labelsForNS := func(name string) map[string]string {
		if lbl, ok := nsLabels[name]; ok {
			return lbl
		}
		if name == "" {
			return nil
		}
		return map[string]string{namespaceNameLabel: name}
	}

	var allPods []activePod
	var localPods []activePod
	for _, p := range pods {
		if p.Status.Phase == "Succeeded" || p.Status.Phase == "Failed" {
			continue
		}
		ip, err := netip.ParseAddr(p.Status.PodIP)
		if err != nil || !ip.Is4() {
			continue
		}
		ap := activePod{pod: p, ip: ip, prefix: netip.PrefixFrom(ip, 32)}
		allPods = append(allPods, ap)
		if cidr.Contains(ip) && ip != netAddr && ip != gateway {
			localPods = append(localPods, ap)
		}
	}
	slices.SortFunc(allPods, func(a, b activePod) int { return a.ip.Compare(b.ip) })
	slices.SortFunc(localPods, func(a, b activePod) int { return a.ip.Compare(b.ip) })

	byNS := make(map[string][]compiledPolicy)
	for _, np := range policies {
		sel, err := apis.SelectorFrom(&np.Spec.PodSelector)
		if err != nil {
			continue
		}
		in, eg := effectivePolicyTypes(np.Spec)
		byNS[np.Metadata.Namespace] = append(byNS[np.Metadata.Namespace], compiledPolicy{
			np:         np,
			podSel:     sel,
			hasIngress: in,
			hasEgress:  eg,
		})
	}

	for _, lp := range localPods {
		nsPolicies := byNS[lp.pod.Metadata.Namespace]
		if len(nsPolicies) == 0 {
			continue
		}
		pp := runtime.PodNetpol{IP: lp.ip}
		for _, cp := range nsPolicies {
			if !cp.podSel.Matches(lp.pod.Metadata.Labels) {
				continue
			}
			if cp.hasIngress {
				pp.IngressIsolated = true
				for _, rule := range cp.np.Spec.Ingress {
					pp.Ingress = append(pp.Ingress, compileIngressRule(rule, cp.np.Metadata.Namespace, &lp.pod, allPods, labelsForNS)...)
				}
			}
			if cp.hasEgress {
				pp.EgressIsolated = true
				for _, rule := range cp.np.Spec.Egress {
					pp.Egress = append(pp.Egress, compileEgressRule(rule, cp.np.Metadata.Namespace, allPods, labelsForNS)...)
				}
			}
		}
		if !pp.IngressIsolated && !pp.EgressIsolated {
			continue
		}
		pp.Ingress = canonicalRules(pp.Ingress)
		pp.Egress = canonicalRules(pp.Egress)
		plan.Pods = append(plan.Pods, pp)
	}
	return plan
}

func effectivePolicyTypes(spec apis.NetworkPolicySpec) (ingress, egress bool) {
	if len(spec.PolicyTypes) == 0 {
		return true, len(spec.Egress) > 0
	}
	for _, pt := range spec.PolicyTypes {
		switch pt {
		case apis.PolicyTypeIngress:
			ingress = true
		case apis.PolicyTypeEgress:
			egress = true
		}
	}
	return ingress, egress
}

func compileIngressRule(
	rule apis.NetworkPolicyIngressRule,
	policyNS string,
	dstPod *apis.Pod,
	allPods []activePod,
	labelsForNS func(string) map[string]string,
) []runtime.NetpolRule {
	ports := resolvePortsForPod(rule.Ports, dstPod)
	if len(ports) == 0 {
		return nil
	}
	var sources []netip.Prefix
	if len(rule.From) == 0 {
		sources = []netip.Prefix{anyIPv4}
	} else {
		for _, peer := range rule.From {
			if peer.IPBlock != nil {
				sources = append(sources, expandIPBlock(peer.IPBlock)...)
				continue
			}
			for _, ap := range matchPeerPods(peer, policyNS, allPods, labelsForNS) {
				sources = append(sources, ap.prefix)
			}
		}
	}
	if len(sources) == 0 {
		return nil
	}
	out := make([]runtime.NetpolRule, 0, len(sources)*len(ports))
	for _, src := range sources {
		for _, rp := range ports {
			out = append(out, runtime.NetpolRule{
				Peer:     src,
				Protocol: rp.protocol,
				Port:     rp.port,
				EndPort:  rp.endPort,
			})
		}
	}
	return out
}

func compileEgressRule(
	rule apis.NetworkPolicyEgressRule,
	policyNS string,
	allPods []activePod,
	labelsForNS func(string) map[string]string,
) []runtime.NetpolRule {
	var out []runtime.NetpolRule
	if len(rule.To) == 0 {
		if len(rule.Ports) == 0 {
			return []runtime.NetpolRule{{Peer: anyIPv4}}
		}
		for _, p := range rule.Ports {
			if p.Port != nil && p.Port.IsString {
				for _, dp := range allPods {
					for _, rp := range resolvePortsForPod([]apis.NetworkPolicyPort{p}, &dp.pod) {
						out = append(out, runtime.NetpolRule{
							Peer:     dp.prefix,
							Protocol: rp.protocol,
							Port:     rp.port,
							EndPort:  rp.endPort,
						})
					}
				}
				continue
			}
			for _, rp := range resolvePortsForPod([]apis.NetworkPolicyPort{p}, nil) {
				out = append(out, runtime.NetpolRule{
					Peer:     anyIPv4,
					Protocol: rp.protocol,
					Port:     rp.port,
					EndPort:  rp.endPort,
				})
			}
		}
		return out
	}

	for _, peer := range rule.To {
		if peer.IPBlock != nil {
			cidrs := expandIPBlock(peer.IPBlock)
			if len(cidrs) == 0 {
				continue
			}
			if len(rule.Ports) == 0 {
				for _, c := range cidrs {
					out = append(out, runtime.NetpolRule{Peer: c})
				}
				continue
			}
			for _, p := range rule.Ports {
				if p.Port != nil && p.Port.IsString {
					for _, dp := range allPods {
						if !prefixListContains(cidrs, dp.ip) {
							continue
						}
						for _, rp := range resolvePortsForPod([]apis.NetworkPolicyPort{p}, &dp.pod) {
							out = append(out, runtime.NetpolRule{
								Peer:     dp.prefix,
								Protocol: rp.protocol,
								Port:     rp.port,
								EndPort:  rp.endPort,
							})
						}
					}
					continue
				}
				for _, rp := range resolvePortsForPod([]apis.NetworkPolicyPort{p}, nil) {
					for _, c := range cidrs {
						out = append(out, runtime.NetpolRule{
							Peer:     c,
							Protocol: rp.protocol,
							Port:     rp.port,
							EndPort:  rp.endPort,
						})
					}
				}
			}
			continue
		}

		for _, dp := range matchPeerPods(peer, policyNS, allPods, labelsForNS) {
			for _, rp := range resolvePortsForPod(rule.Ports, &dp.pod) {
				out = append(out, runtime.NetpolRule{
					Peer:     dp.prefix,
					Protocol: rp.protocol,
					Port:     rp.port,
					EndPort:  rp.endPort,
				})
			}
		}
	}
	return out
}

func resolvePortsForPod(ports []apis.NetworkPolicyPort, dstPod *apis.Pod) []resolvedPort {
	if len(ports) == 0 {
		return []resolvedPort{{}}
	}
	var out []resolvedPort
	for _, p := range ports {
		proto := p.Protocol
		if proto == "" {
			proto = "TCP"
		}
		if proto != "TCP" && proto != "UDP" {
			continue
		}
		if p.Port == nil {
			out = append(out, resolvedPort{protocol: proto})
			continue
		}
		if !p.Port.IsString {
			if p.Port.Int < 1 || p.Port.Int > 65535 {
				continue
			}
			var endPort uint16
			if p.EndPort != nil && *p.EndPort > p.Port.Int && *p.EndPort <= 65535 {
				endPort = uint16(*p.EndPort)
			}
			out = append(out, resolvedPort{
				protocol: proto,
				port:     uint16(p.Port.Int),
				endPort:  endPort,
			})
			continue
		}
		if dstPod == nil {
			continue
		}
		for _, c := range dstPod.Spec.Containers {
			for _, cp := range c.Ports {
				cpProto := cp.Protocol
				if cpProto == "" {
					cpProto = "TCP"
				}
				if cp.Name == p.Port.Str && cpProto == proto && cp.ContainerPort >= 1 && cp.ContainerPort <= 65535 {
					out = append(out, resolvedPort{
						protocol: proto,
						port:     uint16(cp.ContainerPort),
					})
				}
			}
		}
	}
	return out
}

func matchPeerPods(
	peer apis.NetworkPolicyPeer,
	policyNS string,
	allPods []activePod,
	labelsForNS func(string) map[string]string,
) []activePod {
	if peer.PodSelector == nil && peer.NamespaceSelector == nil {
		return nil
	}
	var podSel, nsSel apis.Selector
	var err error
	if peer.PodSelector != nil {
		if podSel, err = apis.SelectorFrom(peer.PodSelector); err != nil {
			return nil
		}
	}
	if peer.NamespaceSelector != nil {
		if nsSel, err = apis.SelectorFrom(peer.NamespaceSelector); err != nil {
			return nil
		}
	}
	var out []activePod
	for _, ap := range allPods {
		if peer.NamespaceSelector != nil {
			if !nsSel.Matches(labelsForNS(ap.pod.Metadata.Namespace)) {
				continue
			}
		} else if ap.pod.Metadata.Namespace != policyNS {
			continue
		}
		if peer.PodSelector != nil && !podSel.Matches(ap.pod.Metadata.Labels) {
			continue
		}
		out = append(out, ap)
	}
	return out
}

// expandIPBlock subtracts all Except prefixes from CIDR and returns the
// remaining disjoint canonical IPv4 prefixes in sorted order.
func expandIPBlock(b *apis.IPBlock) []netip.Prefix {
	cidr, err := netip.ParsePrefix(b.CIDR)
	if err != nil || !cidr.Addr().Is4() {
		return nil
	}
	remaining := []netip.Prefix{cidr.Masked()}
	for _, raw := range b.Except {
		ex, err := netip.ParsePrefix(raw)
		if err != nil || !ex.Addr().Is4() {
			continue
		}
		ex = ex.Masked()
		var next []netip.Prefix
		for _, p := range remaining {
			next = append(next, subtractPrefix(p, ex)...)
		}
		remaining = next
	}
	slices.SortFunc(remaining, comparePrefix)
	return remaining
}

func subtractPrefix(base, ex netip.Prefix) []netip.Prefix {
	if !base.Overlaps(ex) {
		return []netip.Prefix{base}
	}
	if ex.Bits() <= base.Bits() {
		return nil
	}
	bits := base.Bits()
	a := base.Masked().Addr().As4()
	v := binary.BigEndian.Uint32(a[:])
	left := netip.PrefixFrom(addrFromU32(v), bits+1)
	right := netip.PrefixFrom(addrFromU32(v|(uint32(1)<<(31-bits))), bits+1)
	return append(subtractPrefix(left, ex), subtractPrefix(right, ex)...)
}

func addrFromU32(v uint32) netip.Addr {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	return netip.AddrFrom4(b)
}

func prefixListContains(prefixes []netip.Prefix, ip netip.Addr) bool {
	for _, p := range prefixes {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

func comparePrefix(a, b netip.Prefix) int {
	if c := a.Addr().Compare(b.Addr()); c != 0 {
		return c
	}
	return cmp.Compare(a.Bits(), b.Bits())
}

func canonicalRules(rules []runtime.NetpolRule) []runtime.NetpolRule {
	if len(rules) == 0 {
		return nil
	}
	for i := range rules {
		rules[i].Peer = rules[i].Peer.Masked()
		if rules[i].EndPort == rules[i].Port {
			rules[i].EndPort = 0
		}
	}
	slices.SortFunc(rules, func(a, b runtime.NetpolRule) int {
		if c := comparePrefix(a.Peer, b.Peer); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Protocol, b.Protocol); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Port, b.Port); c != 0 {
			return c
		}
		return cmp.Compare(a.EndPort, b.EndPort)
	})
	return slices.Compact(rules)
}
