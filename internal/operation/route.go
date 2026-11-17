package operation

import (
	"errors"
	"maps"
	"net/netip"
	"reflect"

	"shearwater/internal/apis"
	"shearwater/internal/objects"
	"shearwater/internal/store"
)

func matches(selector, labels map[string]string) bool {
	if len(selector) == 0 {
		return false
	}
	for k, v := range selector {
		if actual, exists := labels[k]; !exists || actual != v {
			return false
		}
	}
	return true
}
func owner(meta apis.ObjectMeta, kind, name, uid string) bool {
	for _, o := range meta.OwnerReferences {
		if o.Kind == kind && o.Name == name && o.UID == uid && uid != "" && o.Controller != nil && *o.Controller {
			return true
		}
	}
	return false
}
func routeFrom(s apis.Service, port int32) (*Route, error) {
	ip, e := netip.ParseAddr(s.Spec.ClusterIP)
	if e != nil || !ip.Is4() || !apis.ServiceCIDR.Contains(ip) || s.Spec.Type != "ClusterIP" || len(s.Spec.Selector) == 0 || s.Metadata.DeletionTimestamp != nil {
		return nil, fail("UNSUPPORTED", "select an active internal TCP Service")
	}
	var ports []apis.ServicePort
	for _, p := range s.Spec.Ports {
		if p.Protocol == "TCP" && (port == 0 || p.Port == port) {
			ports = append(ports, p)
		}
	}
	if len(ports) != 1 {
		return nil, fail("AMBIGUOUS", "select one TCP Service port explicitly")
	}
	return &Route{Name: s.Metadata.Name, UID: s.Metadata.UID, IP: ip.String(), Selector: maps.Clone(s.Spec.Selector), Port: ports[0]}, nil
}
func targetPort(port apis.ServicePort, spec apis.PodSpec) (int32, error) {
	if !port.TargetPort.IsString {
		n := port.TargetPort.Int
		if n == 0 {
			n = port.Port
		}
		if n > 0 && n <= 65535 {
			return n, nil
		}
	}
	for _, c := range spec.Containers {
		for _, p := range c.Ports {
			if port.TargetPort.IsString && p.Name == port.TargetPort.Str && (p.Protocol == "" || p.Protocol == "TCP") {
				return p.ContainerPort, nil
			}
		}
	}
	return 0, fail("INVALID", "selected targetPort does not resolve to a container TCP port")
}
func discoverRoute(tx *store.Tx, r Record, d apis.Deployment) (*Route, error) {
	records, e := tx.ListObjects(r.Spec.Namespace)
	if e != nil {
		return nil, e
	}
	var found []*Route
	for _, rec := range records {
		if rec.Key.Group != "" || rec.Key.Resource != "services" || r.Spec.Service != "" && rec.Key.Name != r.Spec.Service {
			continue
		}
		var s apis.Service
		if e = decode(rec, &s); e != nil {
			return nil, e
		}
		if !matches(s.Spec.Selector, d.Spec.Template.Metadata.Labels) {
			continue
		}
		route, e := routeFrom(s, r.Spec.Port)
		if e != nil {
			return nil, e
		}
		if _, e = targetPort(route.Port, d.Spec.Template.Spec); e != nil {
			return nil, e
		}
		found = append(found, route)
	}
	if len(found) != 1 {
		return nil, fail("AMBIGUOUS", "select one matching Service explicitly")
	}
	return found[0], nil
}
func checkRoute(tx *store.Tx, r Record, d apis.Deployment) (apis.Service, error) {
	var s apis.Service
	if r.Route == nil {
		return s, fail("STALE", "no bound Service")
	}
	rec, e := tx.Get(key("services", r.Spec.Namespace, r.Route.Name))
	if e != nil {
		return s, e
	}
	if e = decode(rec, &s); e != nil {
		return s, e
	}
	actual, e := routeFrom(s, r.Route.Port.Port)
	if e != nil {
		return s, e
	}
	if !reflect.DeepEqual(actual, r.Route) || !matches(actual.Selector, d.Spec.Template.Metadata.Labels) {
		return s, fail("STALE", "bound Service identity or routing changed")
	}
	_, e = targetPort(actual.Port, d.Spec.Template.Spec)
	return s, e
}
func createService(tx *store.Tx, r Record, d apis.Deployment) (*Route, error) {
	s := apis.Service{Metadata: apis.ObjectMeta{Name: r.Spec.Name, Namespace: r.Spec.Namespace}, Spec: apis.ServiceSpec{Selector: maps.Clone(d.Spec.Template.Metadata.Labels), Ports: []apis.ServicePort{{Port: r.Spec.Port, TargetPort: apis.IntOrString{Int: r.Spec.Port}, Protocol: "TCP"}}}}
	res := apis.Lookup("", "v1", "services")
	objects.Stamp(res, &s)
	if _, e := tx.Get(key("services", r.Spec.Namespace, r.Spec.Name)); !errors.Is(e, store.ErrNotFound) {
		return nil, fail("CONFLICT", "Service already exists")
	}
	ip, e := tx.AllocIP("service", s.Metadata.UID, "", func(yield func(string) bool) {
		cidr := apis.ServiceCIDR.Masked()
		for ip := cidr.Addr().Next(); cidr.Contains(ip) && cidr.Contains(ip.Next()); ip = ip.Next() {
			if !yield(ip.String()) {
				return
			}
		}
	})
	if e != nil {
		return nil, e
	}
	s.Spec.ClusterIP = ip
	if errs := res.Validate(&s); len(errs) > 0 {
		return nil, fail("INVALID", "generated Service validation failed")
	}
	if _, e = tx.Create(objects.Put(res, &s)); e != nil {
		return nil, e
	}
	return routeFrom(s, r.Spec.Port)
}

func templateEqual(a, b apis.PodTemplateSpec) bool {
	// The controller adds one selector label. It does not identify the release
	// by image alone: changes in probes, env or the per-plan marker also matter.
	a.Metadata.Labels = maps.Clone(a.Metadata.Labels)
	b.Metadata.Labels = maps.Clone(b.Metadata.Labels)
	delete(a.Metadata.Labels, "pod-template-hash")
	delete(b.Metadata.Labels, "pod-template-hash")
	return reflect.DeepEqual(a, b)
}
