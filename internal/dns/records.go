package dns

import (
	"cmp"
	"encoding/json"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"shearwater/internal/apis"
	"shearwater/internal/store"
)

const (
	// ClusterDomain is the DNS suffix for all cluster-local records.
	ClusterDomain = "cluster.local."
	// DefaultTTL is the short TTL used on every cluster DNS answer (design 4.1).
	DefaultTTL uint32 = 5
)

// clusterPodSupernet is the cluster-wide pod CIDR (design 3.3). Reverse lookups
// inside this range or ServiceCIDR stay local instead of leaking upstream.
var clusterPodSupernet = netip.MustParsePrefix("10.244.0.0/16")

// Table is an immutable snapshot of cluster DNS records rebuilt on store
// changes. Queries read only from this table and never touch the store.
type Table struct {
	a     map[string][]netip.Addr
	srv   map[string][]SRVRecord
	ptr   map[string]string
	known map[string]bool
}

// NewEmptyTable returns an empty cluster record table.
func NewEmptyTable() *Table {
	t := &Table{
		a:     map[string][]netip.Addr{},
		srv:   map[string][]SRVRecord{},
		ptr:   map[string]string{},
		known: map[string]bool{},
	}
	t.markKnown("cluster.local.")
	t.markKnown("svc.cluster.local.")
	t.markKnown("pod.cluster.local.")
	return t
}

func (t *Table) markKnown(fqdn string) {
	t.known[strings.ToLower(fqdn)] = true
}

// BuildTable reads Services, EndpointSlices and Pods through store.Reader and
// builds a fresh Table. Works identically on the control plane and on workers.
func BuildTable(r store.Reader) (*Table, error) {
	svcRecs, _, err := r.List("", "services", "")
	if err != nil {
		return nil, fmt.Errorf("list services: %w", err)
	}
	sliceRecs, _, err := r.List("discovery.k8s.io", "endpointslices", "")
	if err != nil {
		return nil, fmt.Errorf("list endpointslices: %w", err)
	}
	podRecs, _, err := r.List("", "pods", "")
	if err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}
	services := make([]apis.Service, 0, len(svcRecs))
	for _, rec := range svcRecs {
		var s apis.Service
		if json.Unmarshal(rec.Data, &s) == nil {
			services = append(services, s)
		}
	}
	epSlices := make([]apis.EndpointSlice, 0, len(sliceRecs))
	for _, rec := range sliceRecs {
		var es apis.EndpointSlice
		if json.Unmarshal(rec.Data, &es) == nil {
			epSlices = append(epSlices, es)
		}
	}
	pods := make([]apis.Pod, 0, len(podRecs))
	for _, rec := range podRecs {
		var p apis.Pod
		if json.Unmarshal(rec.Data, &p) == nil {
			pods = append(pods, p)
		}
	}
	return BuildTableFrom(services, epSlices, pods), nil
}

type headlessEndpoint struct {
	ip    netip.Addr
	ports map[string]uint16 // port name + "/" + lower(proto) -> port number
}

// BuildTableFrom constructs a Table directly from API objects.
func BuildTableFrom(services []apis.Service, epSlices []apis.EndpointSlice, pods []apis.Pod) *Table {
	t := NewEmptyTable()

	// Index ready IPv4 endpoints per Service (ns/name) from EndpointSlices.
	bySvc := map[string][]headlessEndpoint{}
	for _, es := range epSlices {
		if es.AddressType != "" && es.AddressType != "IPv4" {
			continue
		}
		svcName := es.Metadata.Labels[apis.ServiceNameLabel]
		if svcName == "" || es.Metadata.Namespace == "" {
			continue
		}
		portMap := map[string]uint16{}
		for _, ep := range es.Ports {
			if ep.Port == nil || *ep.Port < 1 || *ep.Port > 65535 {
				continue
			}
			pname := ""
			if ep.Name != nil {
				pname = strings.ToLower(*ep.Name)
			}
			proto := "tcp"
			if ep.Protocol != nil && *ep.Protocol != "" {
				proto = strings.ToLower(*ep.Protocol)
			}
			portMap[pname+"/"+proto] = uint16(*ep.Port)
		}
		key := strings.ToLower(es.Metadata.Namespace) + "/" + strings.ToLower(svcName)
		for _, ep := range es.Endpoints {
			if ep.Conditions.Ready != nil && !*ep.Conditions.Ready {
				continue
			}
			if ep.Conditions.Terminating != nil && *ep.Conditions.Terminating {
				continue
			}
			for _, raw := range ep.Addresses {
				ip, err := netip.ParseAddr(raw)
				if err != nil || !ip.Is4() {
					continue
				}
				bySvc[key] = append(bySvc[key], headlessEndpoint{ip: ip, ports: portMap})
			}
		}
	}

	// Services: ClusterIP A + PTR, headless ready pod A, and named-port SRV.
	for _, svc := range services {
		ns := strings.ToLower(svc.Metadata.Namespace)
		name := strings.ToLower(svc.Metadata.Name)
		if ns == "" || name == "" {
			continue
		}
		fqdn := name + "." + ns + ".svc." + ClusterDomain
		t.markKnown(ns + ".svc." + ClusterDomain)
		t.markKnown(fqdn)

		if svc.Spec.ClusterIP == "None" {
			eps := bySvc[ns+"/"+name]
			seenIP := map[netip.Addr]bool{}
			var addrs []netip.Addr
			for _, ep := range eps {
				if !seenIP[ep.ip] {
					seenIP[ep.ip] = true
					addrs = append(addrs, ep.ip)
				}
			}
			slices.SortFunc(addrs, func(a, b netip.Addr) int { return a.Compare(b) })
			if len(addrs) > 0 {
				t.a[fqdn] = addrs
			}
			for _, p := range svc.Spec.Ports {
				if p.Name == "" || p.Port < 1 || p.Port > 65535 {
					continue
				}
				pname := strings.ToLower(p.Name)
				proto := strings.ToLower(p.Protocol)
				if proto == "" {
					proto = "tcp"
				}
				srvName := "_" + pname + "._" + proto + "." + fqdn
				t.markKnown("_" + proto + "." + fqdn)
				t.markKnown(srvName)
				var srvs []SRVRecord
				seenSRV := map[string]bool{}
				for _, ep := range eps {
					port := uint16(p.Port)
					if resolved, ok := ep.ports[pname+"/"+proto]; ok {
						port = resolved
					}
					target := podFQDN(ep.ip, ns)
					k := fmt.Sprintf("%s:%d", target, port)
					if seenSRV[k] {
						continue
					}
					seenSRV[k] = true
					srvs = append(srvs, SRVRecord{Priority: 0, Weight: 100, Port: port, Target: target})
				}
				slices.SortFunc(srvs, func(a, b SRVRecord) int {
					if c := cmp.Compare(a.Target, b.Target); c != 0 {
						return c
					}
					return cmp.Compare(a.Port, b.Port)
				})
				if len(srvs) > 0 {
					t.srv[srvName] = srvs
				}
			}
			continue
		}

		ip, err := netip.ParseAddr(svc.Spec.ClusterIP)
		if err != nil || !ip.Is4() {
			continue
		}
		t.a[fqdn] = []netip.Addr{ip}
		t.ptr[reverseIPv4(ip)] = fqdn
		for _, p := range svc.Spec.Ports {
			if p.Name == "" || p.Port < 1 || p.Port > 65535 {
				continue
			}
			pname := strings.ToLower(p.Name)
			proto := strings.ToLower(p.Protocol)
			if proto == "" {
				proto = "tcp"
			}
			srvName := "_" + pname + "._" + proto + "." + fqdn
			t.markKnown("_" + proto + "." + fqdn)
			t.markKnown(srvName)
			t.srv[srvName] = append(t.srv[srvName], SRVRecord{
				Priority: 0,
				Weight:   100,
				Port:     uint16(p.Port),
				Target:   fqdn,
			})
		}
	}

	// Pods: <a-b-c-d>.<ns>.pod.cluster.local A and PTR.
	for _, pod := range pods {
		if pod.Status.Phase == "Succeeded" || pod.Status.Phase == "Failed" {
			continue
		}
		ns := strings.ToLower(pod.Metadata.Namespace)
		if ns == "" {
			continue
		}
		ip, err := netip.ParseAddr(pod.Status.PodIP)
		if err != nil || !ip.Is4() {
			continue
		}
		fqdn := podFQDN(ip, ns)
		t.markKnown(ns + ".pod." + ClusterDomain)
		t.markKnown(fqdn)
		t.a[fqdn] = []netip.Addr{ip}
		rev := reverseIPv4(ip)
		if _, exists := t.ptr[rev]; !exists {
			t.ptr[rev] = fqdn
		}
	}

	return t
}

func podFQDN(ip netip.Addr, ns string) string {
	return strings.ReplaceAll(ip.String(), ".", "-") + "." + ns + ".pod." + ClusterDomain
}

func reverseIPv4(ip netip.Addr) string {
	a := ip.As4()
	return fmt.Sprintf("%d.%d.%d.%d.in-addr.arpa.", a[3], a[2], a[1], a[0])
}

func parseReverseIPv4(fqdn string) (netip.Addr, bool) {
	trimmed, ok := strings.CutSuffix(strings.ToLower(fqdn), ".in-addr.arpa.")
	if !ok {
		return netip.Addr{}, false
	}
	parts := strings.Split(trimmed, ".")
	if len(parts) != 4 {
		return netip.Addr{}, false
	}
	ip, err := netip.ParseAddr(parts[3] + "." + parts[2] + "." + parts[1] + "." + parts[0])
	if err != nil || !ip.Is4() {
		return netip.Addr{}, false
	}
	return ip, true
}

// Lookup answers one DNS question against the cluster table.
// handled is false when the query is outside cluster.local (and not a cluster
// reverse lookup) and should be forwarded to the host's upstream resolvers.
func (t *Table) Lookup(q Question) (answers []Resource, rcode uint8, handled bool) {
	name := CanonicalName(q.Name)

	// Cluster domain: always authoritative, never forwarded upstream.
	if name == ClusterDomain || strings.HasSuffix(name, "."+ClusterDomain) {
		if q.Class != ClassIN && q.Class != ClassANY {
			return nil, RcodeNoError, true
		}
		if !t.known[name] {
			return nil, RcodeNXDomain, true
		}
		switch q.Type {
		case TypeA:
			for _, ip := range t.a[name] {
				answers = append(answers, Resource{Name: q.Name, Type: TypeA, Class: ClassIN, TTL: DefaultTTL, A: ip})
			}
		case TypeSRV:
			for _, s := range t.srv[name] {
				cp := s
				answers = append(answers, Resource{Name: q.Name, Type: TypeSRV, Class: ClassIN, TTL: DefaultTTL, SRV: &cp})
			}
		case TypeANY:
			for _, ip := range t.a[name] {
				answers = append(answers, Resource{Name: q.Name, Type: TypeA, Class: ClassIN, TTL: DefaultTTL, A: ip})
			}
			for _, s := range t.srv[name] {
				cp := s
				answers = append(answers, Resource{Name: q.Name, Type: TypeSRV, Class: ClassIN, TTL: DefaultTTL, SRV: &cp})
			}
		case TypeAAAA:
			// Existing cluster name has no IPv6 records: empty NOERROR (NODATA).
		}
		return answers, RcodeNoError, true
	}

	// Reverse lookups (PTR) for cluster Service and Pod IPs.
	if ip, ok := parseReverseIPv4(name); ok {
		if target, found := t.ptr[name]; found {
			if q.Type == TypePTR || q.Type == TypeANY {
				answers = append(answers, Resource{Name: q.Name, Type: TypePTR, Class: ClassIN, TTL: DefaultTTL, PTR: target})
			}
			return answers, RcodeNoError, true
		}
		if apis.ServiceCIDR.Contains(ip) || clusterPodSupernet.Contains(ip) {
			return nil, RcodeNXDomain, true
		}
	}

	return nil, RcodeNoError, false
}
