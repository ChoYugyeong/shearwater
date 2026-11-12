package controller

import (
	"cmp"
	"crypto/sha256"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"shearwater/internal/apis"
	"shearwater/internal/objects"
	"shearwater/internal/store"
)

var endpointSliceResource = apis.Lookup("discovery.k8s.io", "v1", "endpointslices")

func reconcileEndpointSlices(tx *store.Tx, v view) error {
	if len(v.services) == 0 && len(v.endpointSlices) == 0 {
		return nil
	}
	podsByNamespace := make(map[string][]*item)
	for _, pod := range v.pods {
		ns := pod.rec.Key.Namespace
		podsByNamespace[ns] = append(podsByNamespace[ns], pod)
	}
	existing := make(map[store.Key]*item, len(v.endpointSlices))
	for _, slice := range v.endpointSlices {
		existing[slice.rec.Key] = slice
	}
	for _, owner := range v.services {
		svc := owner.obj.(*apis.Service)
		if owner.rec.Deleting || svc.Spec.Type != "ClusterIP" || svc.Spec.ClusterIP == "None" || len(svc.Spec.Selector) == 0 {
			continue
		}
		groups := desiredEndpointSlices(svc, podsByNamespace[owner.rec.Key.Namespace])
		for _, vector := range slices.Sorted(maps.Keys(groups)) {
			// Keep the name stable while endpoints join or leave a port group.
			// The Service UID gives a recreated Service its own set of names.
			digest := sha256.Sum256([]byte(owner.rec.UID + "\n" + vector))
			name := fmt.Sprintf("%s-%x", childPrefix(owner.rec.Key.Name, 30), digest[:16])
			key := objects.Key(endpointSliceResource, owner.rec.Key.Namespace, name)
			if err := writeEndpointSlice(tx, key, owner, existing[key], groups[vector]); err != nil {
				return err
			}
			delete(existing, key)
		}
	}
	// Retire obsolete groups in the same transaction as the new groups so a
	// reader gets a complete set of slices for the reconciled Service spec.
	for _, stale := range v.endpointSlices {
		if existing[stale.rec.Key] == nil || stale.obj.Meta().Labels[apis.EndpointSliceManagedByLabel] != apis.EndpointSliceManager {
			continue
		}
		if _, err := tx.Delete(stale.rec.Key, stale.rec.Rev, objects.Encoder(stale.obj)); err != nil {
			return err
		}
	}
	return nil
}

func writeEndpointSlice(tx *store.Tx, key store.Key, owner, current *item, want *apis.EndpointSlice) error {
	want.AddressType = "IPv4"
	if current == nil {
		want.Metadata = apis.ObjectMeta{Name: key.Name, Namespace: key.Namespace, Labels: make(map[string]string)}
		objects.Stamp(endpointSliceResource, want)
	} else {
		if current.obj.Meta().Labels[apis.EndpointSliceManagedByLabel] != apis.EndpointSliceManager {
			return fmt.Errorf("EndpointSlice %s is managed by another controller", key)
		}
		want.TypeMeta = *current.obj.Type()
		want.Metadata = *current.obj.Meta()
	}
	want.Metadata.Labels[apis.ServiceNameLabel] = owner.rec.Key.Name
	want.Metadata.Labels[apis.EndpointSliceManagedByLabel] = apis.EndpointSliceManager
	want.Metadata.OwnerReferences = []apis.OwnerReference{ownerRef(owner)}
	// The proxy can see a new Service spec before this transaction runs. The
	// owner UID and this generation identify the exact Service spec we used.
	annotate(want.Meta(), apis.ServiceGenerationAnnotation, strconv.FormatInt(owner.obj.Meta().Generation, 10))
	put := objects.Put(endpointSliceResource, want)
	if current == nil {
		_, err := tx.Create(put)
		return err
	}
	// Update compares the encoded content at the current revision. Stable
	// ordering and references keep a settled slice out of the change log.
	_, _, err := tx.Update(put, current.rec.Rev)
	return err
}

func desiredEndpointSlices(svc *apis.Service, pods []*item) map[string]*apis.EndpointSlice {
	// A Service selector contains only equality matches, including the case
	// of a label whose value is the empty string. SelectorFrom handles that.
	selector, _ := apis.SelectorFrom(&apis.LabelSelector{MatchLabels: svc.Spec.Selector})
	ports := slices.Clone(svc.Spec.Ports)
	slices.SortFunc(ports, func(a, b apis.ServicePort) int { return cmp.Compare(a.Name, b.Name) })
	groups := make(map[string]*apis.EndpointSlice)
	// load reads Pods in namespace/name order. Appending in that order keeps
	// the endpoints stable even if an unrelated object changes in the store.
	for _, candidate := range pods {
		pod := candidate.obj.(*apis.Pod)
		if candidate.rec.Deleting || !active(pod) || !podReady(pod) || !selector.Matches(pod.Metadata.Labels) {
			continue
		}
		ip, err := netip.ParseAddr(pod.Status.PodIP)
		// The node reserves the network address, bridge gateway and broadcast
		// address. Eligible Pod IPs come from the addresses between them.
		if err != nil || !apis.UsablePodIP(ip) {
			continue
		}
		resolved, vector := endpointPorts(pod, ports)
		group := groups[vector]
		if group == nil {
			group = &apis.EndpointSlice{Ports: resolved}
			groups[vector] = group
		}
		endpoint := apis.Endpoint{
			Addresses:  []string{ip.String()},
			Conditions: apis.EndpointConditions{Ready: new(true), Serving: new(true), Terminating: new(false)},
			TargetRef: &apis.ObjectReference{
				APIVersion: "v1", Kind: "Pod", Namespace: candidate.rec.Key.Namespace, Name: candidate.rec.Key.Name, UID: candidate.rec.UID,
			},
		}
		if pod.Spec.NodeName != "" {
			endpoint.NodeName = &pod.Spec.NodeName
		}
		group.Endpoints = append(group.Endpoints, endpoint)
	}
	if len(groups) == 0 {
		// Keep an empty slice so API clients can see the selected Service has
		// zero eligible endpoints, and retire every former port group.
		groups[""] = &apis.EndpointSlice{Ports: []apis.EndpointPort{}, Endpoints: []apis.Endpoint{}}
	}
	return groups
}

// A slice's ports apply to every endpoint in it. The vector records the
// resolved (ServicePort name, protocol, target number) tuples in name order.
// Missing named targets omit their tuple, so the Pod can still serve its
// other ports and joins only Pods with the same set of resolved ports.
func endpointPorts(pod *apis.Pod, ports []apis.ServicePort) ([]apis.EndpointPort, string) {
	resolved := make([]apis.EndpointPort, 0, len(ports))
	var vector strings.Builder
	for _, port := range ports {
		protocol := portProtocol(port.Protocol)
		target := endpointTargetPort(pod, port, protocol)
		if target < 1 || target > 65535 {
			continue
		}
		// The proxy joins on ServicePort.Name. An unnamed ServicePort has
		// an explicit empty name here. Port carries the Pod's target number.
		resolved = append(resolved, apis.EndpointPort{Name: new(port.Name), Protocol: &protocol, Port: &target})
		fmt.Fprintf(&vector, "%q/%q/%d;", port.Name, protocol, target)
	}
	return resolved, vector.String()
}

func endpointTargetPort(pod *apis.Pod, port apis.ServicePort, protocol string) int32 {
	target := port.TargetPort
	if target.IsZero() {
		return port.Port
	}
	if !target.IsString {
		return target.Int
	}
	// A named target can be declared by any container in the Pod. Match
	// the protocol as well as the name, in container declaration order.
	for _, container := range pod.Spec.Containers {
		for _, cp := range container.Ports {
			if cp.Name == target.Str && portProtocol(cp.Protocol) == protocol && cp.ContainerPort >= 1 && cp.ContainerPort <= 65535 {
				return cp.ContainerPort
			}
		}
	}
	return 0
}

func portProtocol(protocol string) string {
	if protocol == "" {
		return "TCP"
	}
	return protocol
}
