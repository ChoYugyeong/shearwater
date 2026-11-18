package operation

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"shearwater/internal/apis"
	"shearwater/internal/objects"
	"shearwater/internal/store"
)

// Engine operates a Deployment and its ClusterIP Service in Shearwater's own
// store. The ledger and the app objects share one SQLite database, so applying
// a plan and recording its receipt commit in the same transaction.
type Engine struct{}

func (Engine) Managed(addr netip.Addr) bool {
	return addr.Is4() && (apis.PodCIDR.Contains(addr) || apis.ServiceCIDR.Contains(addr))
}

func (Engine) Bind(tx *store.Tx, r *Record) (bool, error) {
	if _, e := tx.Get(key("namespaces", "", r.Spec.Namespace)); e != nil {
		return false, e
	}
	rec, e := tx.Get(key("deployments", r.Spec.Namespace, r.Spec.Name))
	if errors.Is(e, store.ErrNotFound) {
		return false, nil
	}
	if e != nil {
		return false, e
	}
	var d apis.Deployment
	if e = decode(rec, &d); e != nil {
		return false, e
	}
	if e = eligible(d); e != nil {
		return false, e
	}
	r.UID, r.Generation = d.Metadata.UID, d.Metadata.Generation
	if r.Route, e = discoverRoute(tx, *r, d); e != nil {
		return false, e
	}
	if r.Spec.Image == "" {
		r.Spec.Image = d.Spec.Template.Spec.Containers[0].Image
	}
	return true, nil
}

func (Engine) Reserve(tx *store.Tx, r Record) error {
	if r.Spec.Service != "" && r.Spec.Service != r.Spec.Name {
		return fail("INVALID", "a new app uses a same-named Service")
	}
	if _, e := tx.Get(key("services", r.Spec.Namespace, r.Spec.Name)); !errors.Is(e, store.ErrNotFound) {
		return fail("CONFLICT", "new app Service name is already present or unreadable")
	}
	return nil
}

func (Engine) Vacant(tx *store.Tx, r Record) error {
	for _, resource := range []string{"deployments", "services"} {
		if _, e := tx.Get(key(resource, r.Spec.Namespace, r.Spec.Name)); !errors.Is(e, store.ErrNotFound) {
			return fail("STALE", "unselected object already occupies app or Service name")
		}
	}
	return nil
}

func (Engine) Base(tx *store.Tx, r Record) (string, string, error) {
	d, _, e := current(tx, r)
	if e != nil {
		return "", "", e
	}
	if d.Metadata.Generation != r.Generation {
		return "", "", fail("STALE", "app was changed outside this operation; operator must reconnect")
	}
	if _, e = checkRoute(tx, r, d); e != nil {
		return "", "", e
	}
	return digest(d.Spec), d.Spec.Template.Spec.Containers[0].Image, nil
}

func (Engine) Apply(tx *store.Tx, r *Record, p Plan) error {
	var d apis.Deployment
	res := apis.Lookup("apps", "v1", "deployments")
	if p.Action == "deploy" {
		for _, resource := range []string{"deployments", "services"} {
			if _, e := tx.Get(key(resource, r.Spec.Namespace, r.Spec.Name)); !errors.Is(e, store.ErrNotFound) {
				return fail("STALE", "app or Service appeared after approval")
			}
		}
		d = newDeployment(*r, p)
		objects.Stamp(res, &d)
		if e := res.Validate(&d); len(e) > 0 {
			return fail("INVALID", fmt.Sprint(e))
		}
		if _, e := tx.Create(objects.Put(res, &d)); e != nil {
			return e
		}
		r.UID, r.Generation = d.Metadata.UID, d.Metadata.Generation
		route, e := createService(tx, *r, d)
		if e != nil {
			return e
		}
		r.Route = route
		return nil
	}
	old, rec, e := current(tx, *r)
	if e != nil {
		return e
	}
	if old.Metadata.Generation != p.BaseGeneration || digest(old.Spec) != p.BaseSpec {
		return fail("STALE", "app specification changed after plan approval")
	}
	if _, e = checkRoute(tx, *r, old); e != nil {
		return e
	}
	b, _ := json.Marshal(old)
	if e = json.Unmarshal(b, &d); e != nil {
		return e
	}
	d.Spec.Template.Spec.Containers[0].Image = p.Image
	if d.Spec.Template.Metadata.Annotations == nil {
		d.Spec.Template.Metadata.Annotations = map[string]string{}
	}
	d.Spec.Template.Metadata.Annotations[Marker] = p.Hash
	d.Metadata.Generation++
	if errs := res.Validate(&d); len(errs) > 0 {
		return fail("INVALID", fmt.Sprint(errs))
	}
	if errs := res.ValidateUpdate(&d, &old); len(errs) > 0 {
		return fail("INVALID", fmt.Sprint(errs))
	}
	if _, _, e = tx.Update(objects.Put(res, &d), rec.Rev); e != nil {
		return e
	}
	r.Generation = d.Metadata.Generation
	return nil
}

func (Engine) Observe(tx *store.Tx, r Record) (Observation, error) {
	var o Observation
	d, _, e := current(tx, r)
	if e != nil {
		return o, e
	}
	if d.Metadata.Generation != r.Generation {
		o.External = true
		return o, nil
	}
	service, e := checkRoute(tx, r, d)
	if e != nil {
		return o, e
	}
	records, e := tx.ListObjects(r.Spec.Namespace)
	if e != nil {
		return o, e
	}
	sets := map[string]apis.ReplicaSet{}
	allPods := map[string]apis.Pod{}
	var endpoints []apis.EndpointSlice
	for _, rec := range records {
		switch rec.Key.Resource {
		case "replicasets":
			var rs apis.ReplicaSet
			if e = decode(rec, &rs); e != nil {
				return o, e
			}
			if owner(rs.Metadata, "Deployment", r.Spec.Name, r.UID) {
				sets[rs.Metadata.UID] = rs
			}
		case "pods":
			var p apis.Pod
			if e = decode(rec, &p); e != nil {
				return o, e
			}
			allPods[p.Metadata.UID] = p
		case "endpointslices":
			var sl apis.EndpointSlice
			if e = decode(rec, &sl); e != nil {
				return o, e
			}
			if owner(sl.Metadata, "Service", service.Metadata.Name, service.Metadata.UID) {
				endpoints = append(endpoints, sl)
			}
		}
	}
	ports := map[string]int32{}
	newPods := map[string]apis.Pod{}
	for _, p := range allPods {
		if p.Metadata.DeletionTimestamp != nil || !matches(r.Route.Selector, p.Metadata.Labels) {
			continue
		}
		var own *apis.ReplicaSet
		for _, rs := range sets {
			if owner(p.Metadata, "ReplicaSet", rs.Metadata.Name, rs.Metadata.UID) {
				copy := rs
				own = &copy
				break
			}
		}
		if own == nil {
			return o, fail("FOREIGN_BACKEND", "Service selects a Pod not owned by this app")
		}
		if !templateEqual(own.Spec.Template, d.Spec.Template) || own.Metadata.DeletionTimestamp != nil {
			continue
		}
		if len(p.Spec.Containers) != 1 || p.Spec.Containers[0].Image != d.Spec.Template.Spec.Containers[0].Image || p.Metadata.Annotations[Marker] != d.Spec.Template.Metadata.Annotations[Marker] {
			return o, fail("STALE", "new ReplicaSet Pod does not match selected release")
		}
		if !ready(p) {
			continue
		}
		ip, e := netip.ParseAddr(p.Status.PodIP)
		if e != nil || !ip.Is4() || !apis.PodCIDR.Contains(ip) {
			return o, fail("INVALID", "Pod address is outside the managed Pod network")
		}
		cs := p.Status.ContainerStatuses[0]
		if cs.ContainerID == "" || cs.ImageID == "" {
			continue
		}
		if pinned(p.Spec.Containers[0].Image) {
			_, wanted, _ := strings.Cut(p.Spec.Containers[0].Image, "@")
			_, actual, _ := strings.Cut(cs.ImageID, "@")
			if wanted != actual {
				continue
			}
		}
		port, e := targetPort(r.Route.Port, p.Spec)
		if e != nil {
			return o, e
		}
		newPods[p.Metadata.UID] = p
		ports[p.Metadata.UID] = port
	}
	want := *d.Spec.Replicas
	o.Facts = append(o.Facts, fmt.Sprintf("goal generation=%d; ready new-release Pods=%d/%d", r.Generation, len(newPods), want))
	if int32(len(newPods)) != want || d.Status.ObservedGeneration != r.Generation || d.Status.UpdatedReplicas != want || d.Status.ReadyReplicas < want || d.Status.AvailableReplicas < want {
		return o, nil
	}
	covered := map[string]bool{}
	for _, sl := range endpoints {
		if sl.Metadata.DeletionTimestamp != nil || sl.AddressType != "IPv4" || sl.Metadata.Annotations[apis.ServiceGenerationAnnotation] != strconv.FormatInt(service.Metadata.Generation, 10) {
			continue
		}
		for _, ep := range sl.Endpoints {
			if ep.Conditions.Ready == nil || !*ep.Conditions.Ready || ep.Conditions.Terminating != nil && *ep.Conditions.Terminating {
				continue
			}
			ref := ep.TargetRef
			if ref == nil {
				return o, fail("FOREIGN_BACKEND", "endpoint has no Pod identity")
			}
			p, ok := newPods[ref.UID]
			if !ok {
				o.Facts = append(o.Facts, "Service still contains an older or unverified backend")
				return o, nil
			}
			if ref.Kind != "Pod" || ref.Namespace != r.Spec.Namespace || ref.Name != p.Metadata.Name || len(ep.Addresses) != 1 || ep.Addresses[0] != p.Status.PodIP {
				return o, fail("FOREIGN_BACKEND", "endpoint identity/address mismatch")
			}
			portOK := false
			for _, port := range sl.Ports {
				name := ""
				if port.Name != nil {
					name = *port.Name
				}
				if name == r.Route.Port.Name && port.Protocol != nil && *port.Protocol == "TCP" && port.Port != nil && *port.Port == ports[p.Metadata.UID] {
					portOK = true
				}
			}
			if portOK {
				covered[p.Metadata.UID] = true
			}
		}
	}
	if len(covered) != len(newPods) {
		o.Facts = append(o.Facts, "Service endpoints have not converged to every new-release Pod")
		return o, nil
	}
	var pods []apis.Pod
	for _, p := range newPods {
		pods = append(pods, p)
	}
	slices.SortFunc(pods, func(a, b apis.Pod) int { return strings.Compare(a.Metadata.UID, b.Metadata.UID) })
	// A normal heartbeat may change resourceVersion without changing the
	// release. Pin the meaningful route, generation, Pod/IP/container identities
	// instead, and compare them again after network I/O before saving proof.
	var identities []any
	for _, p := range pods {
		cs := p.Status.ContainerStatuses[0]
		identities = append(identities, []any{p.Metadata.UID, p.Status.PodIP, cs.ContainerID, cs.ImageID, ports[p.Metadata.UID]})
		o.Replicas = append(o.Replicas, Replica{UID: p.Metadata.UID, ContainerID: cs.ContainerID, ImageID: cs.ImageID, IP: p.Status.PodIP, Port: ports[p.Metadata.UID]})
	}
	o.Fingerprint = digest([]any{r.UID, r.Generation, d.Spec, r.Route, r.GoalChecks, identities})
	o.Image = d.Spec.Template.Spec.Containers[0].Image
	o.FrontIP, o.FrontPort = r.Route.IP, r.Route.Port.Port
	o.Ready = true
	return o, nil
}

func (Engine) Diagnose(tx *store.Tx, r Record, result *Result) error {
	d, _, e := current(tx, r)
	if e != nil {
		return e
	}
	records, e := tx.ListObjects(r.Spec.Namespace)
	if e != nil {
		return e
	}
	sets := map[string]string{}
	for _, rec := range records {
		if rec.Key.Resource == "replicasets" {
			var rs apis.ReplicaSet
			if e = decode(rec, &rs); e != nil {
				return e
			}
			if owner(rs.Metadata, "Deployment", r.Spec.Name, r.UID) {
				sets[rs.Metadata.UID] = rs.Metadata.Name
			}
		}
	}
	result.Facts = append(result.Facts, fmt.Sprintf("observed generation=%d; intended=%d; ready replicas=%d", d.Metadata.Generation, r.Generation, d.Status.ReadyReplicas))
	count := 0
	for _, rec := range records {
		if rec.Key.Resource != "pods" {
			continue
		}
		var p apis.Pod
		if e = decode(rec, &p); e != nil {
			return e
		}
		owned := false
		for uid, name := range sets {
			if owner(p.Metadata, "ReplicaSet", name, uid) {
				owned = true
			}
		}
		if !owned || count >= 8 {
			continue
		}
		count++
		for _, c := range p.Status.ContainerStatuses {
			reason := "running"
			if c.State.Waiting != nil {
				reason = c.State.Waiting.Reason
			}
			if c.State.Terminated != nil {
				reason = fmt.Sprintf("terminated exit=%d", c.State.Terminated.ExitCode)
			}
			if len(reason) > 80 {
				reason = reason[:80]
			}
			result.Facts = append(result.Facts, fmt.Sprintf("Pod %s: ready=%t restarts=%d state=%q", p.Metadata.Name, c.Ready, c.RestartCount, reason))
		}
	}
	return nil
}

func eligible(d apis.Deployment) error {
	if d.Metadata.DeletionTimestamp != nil || d.Spec.Paused || len(d.Spec.Template.Spec.Containers) != 1 || d.Spec.Replicas == nil || *d.Spec.Replicas < 1 || *d.Spec.Replicas > 16 {
		return fail("UNSUPPORTED", "operations support an active one-container Deployment with 1..16 replicas")
	}
	return nil
}

func current(tx *store.Tx, r Record) (apis.Deployment, store.Record, error) {
	var d apis.Deployment
	rec, e := tx.Get(key("deployments", r.Spec.Namespace, r.Spec.Name))
	if e != nil {
		return d, rec, e
	}
	if e = decode(rec, &d); e != nil {
		return d, rec, e
	}
	if r.UID == "" || d.Metadata.UID != r.UID {
		return d, rec, fail("STALE", "selected app identity was removed or replaced")
	}
	return d, rec, eligible(d)
}

func ready(p apis.Pod) bool {
	if p.Status.Phase != "Running" || len(p.Status.ContainerStatuses) != 1 || !p.Status.ContainerStatuses[0].Ready || p.Status.ContainerStatuses[0].State.Running == nil {
		return false
	}
	for _, c := range p.Status.Conditions {
		if c.Type == "Ready" && c.Status == "True" {
			return true
		}
	}
	return false
}

func newDeployment(r Record, p Plan) apis.Deployment {
	n := int32(1)
	labels := map[string]string{"app.kubernetes.io/name": r.Spec.Name, "shearwater.io/task": r.ID}
	probe, _ := json.Marshal(map[string]any{"tcpSocket": map[string]any{"port": r.Spec.Port}})
	return apis.Deployment{Metadata: apis.ObjectMeta{Name: r.Spec.Name, Namespace: r.Spec.Namespace}, Spec: apis.DeploymentSpec{Replicas: &n, Selector: &apis.LabelSelector{MatchLabels: labels}, Template: apis.PodTemplateSpec{Metadata: apis.ObjectMeta{Labels: labels, Annotations: map[string]string{Marker: p.Hash}}, Spec: apis.PodSpec{Containers: []apis.Container{{Name: "app", Image: p.Image, Ports: []apis.ContainerPort{{ContainerPort: r.Spec.Port}}, ReadinessProbe: probe}}}}}}
}
