package proxy

import (
	"cmp"
	"encoding/json"
	"fmt"
	"net/netip"
	"slices"
	"strconv"

	"shearwater/internal/apis"
	"shearwater/internal/store"
)

type route struct {
	service    store.Key
	uid        string
	generation int64
	selector   map[string]string // immutable Service selector at this generation
	backends   []backend
}

type backend struct {
	address netip.AddrPort
	pod     store.Key
	uid     string
}

// snapshot converts API objects into one TCP listener per Service port. The
// lists may come from different revisions. An EndpointSlice is usable only
// when its owner UID and Service generation agree with the Service we read.
func snapshot(st *store.Store) (map[netip.AddrPort]route, error) {
	services, _, err := st.List("", "services", "")
	if err != nil {
		return nil, err
	}
	endpointSlices, _, err := st.List("discovery.k8s.io", "endpointslices", "")
	if err != nil {
		return nil, err
	}
	return buildRoutes(services, endpointSlices)
}

func buildRoutes(services, endpointSlices []store.Record) (map[netip.AddrPort]route, error) {
	type serviceRoutes struct {
		uid        string
		generation int64
		byPort     map[string]netip.AddrPort
	}
	routes := make(map[netip.AddrPort]route)
	byService := make(map[store.Key]serviceRoutes)
	for _, rec := range services {
		if rec.Deleting {
			continue
		}
		var svc apis.Service
		if err := json.Unmarshal(rec.Data, &svc); err != nil {
			return nil, fmt.Errorf("read %s: %w", rec.Key, err)
		}
		ip, err := netip.ParseAddr(svc.Spec.ClusterIP)
		if err != nil || !ip.Is4() || !apis.ServiceCIDR.Contains(ip) || svc.Spec.Type != "ClusterIP" {
			continue
		}
		sr := serviceRoutes{uid: rec.UID, generation: rec.Generation, byPort: make(map[string]netip.AddrPort)}
		for _, p := range svc.Spec.Ports {
			if p.Protocol != "TCP" || p.Port < 1 || p.Port > 65535 {
				continue
			}
			address := netip.AddrPortFrom(ip, uint16(p.Port))
			if _, exists := routes[address]; exists {
				return nil, fmt.Errorf("more than one Service port claims %s", address)
			}
			routes[address] = route{service: rec.Key, uid: rec.UID, generation: rec.Generation, selector: svc.Spec.Selector}
			sr.byPort[p.Name] = address
		}
		// Selectorless Services still have an IP and a listener, but this
		// server never supplies endpoints for them.
		if len(svc.Spec.Selector) != 0 {
			byService[rec.Key] = sr
		}
	}
	for _, rec := range endpointSlices {
		if rec.Deleting {
			continue
		}
		var es apis.EndpointSlice
		if err := json.Unmarshal(rec.Data, &es); err != nil {
			return nil, fmt.Errorf("read %s: %w", rec.Key, err)
		}
		if es.AddressType != "IPv4" || es.Metadata.Labels[apis.EndpointSliceManagedByLabel] != apis.EndpointSliceManager {
			continue
		}
		name := es.Metadata.Labels[apis.ServiceNameLabel]
		sr, ok := byService[store.Key{Resource: "services", Namespace: rec.Key.Namespace, Name: name}]
		if !ok || es.Metadata.Annotations[apis.ServiceGenerationAnnotation] != strconv.FormatInt(sr.generation, 10) ||
			!sliceOwnedBy(es.Metadata.OwnerReferences, name, sr.uid) {
			continue
		}
		for _, p := range es.Ports {
			portName := ""
			if p.Name != nil {
				portName = *p.Name
			}
			address, ok := sr.byPort[portName]
			if !ok || p.Port == nil || *p.Port < 1 || *p.Port > 65535 || (p.Protocol != nil && *p.Protocol != "TCP") {
				continue
			}
			r := routes[address]
			for _, ep := range es.Endpoints {
				if ep.Conditions.Ready == nil || !*ep.Conditions.Ready || (ep.Conditions.Terminating != nil && *ep.Conditions.Terminating) {
					continue
				}
				ref := ep.TargetRef
				if ref == nil || ref.Kind != "Pod" || ref.APIVersion != "v1" || ref.Namespace != rec.Key.Namespace ||
					ref.Name == "" || ref.UID == "" {
					continue
				}
				for _, rawIP := range ep.Addresses {
					ip, err := netip.ParseAddr(rawIP)
					// The proxy runs on the host. It must dial only usable
					// Pod IPs, never host, gateway or external addresses.
					if err == nil && apis.UsablePodIP(ip) {
						r.backends = append(r.backends, backend{
							address: netip.AddrPortFrom(ip, uint16(*p.Port)),
							pod:     store.Key{Resource: "pods", Namespace: ref.Namespace, Name: ref.Name},
							uid:     ref.UID,
						})
					}
				}
			}
			routes[address] = r
		}
	}
	for address, r := range routes {
		slices.SortFunc(r.backends, func(a, b backend) int {
			if n := a.address.Compare(b.address); n != 0 {
				return n
			}
			return cmp.Compare(a.uid, b.uid)
		})
		r.backends = slices.Compact(r.backends)
		routes[address] = r
	}
	return routes, nil
}

func sliceOwnedBy(refs []apis.OwnerReference, name, uid string) bool {
	for _, ref := range refs {
		if ref.Kind == "Service" && ref.APIVersion == "v1" && ref.Name == name && ref.UID == uid {
			return true
		}
	}
	return false
}
