package controller

import (
	"encoding/json"
	"maps"
	"slices"
	"testing"

	"shearwater/internal/apis"
	"shearwater/internal/objects"
	"shearwater/internal/store"
)

var testServiceResource = apis.Lookup("", "v1", "services")

func endpointService(name string, ports ...apis.ServicePort) *apis.Service {
	return &apis.Service{
		Metadata: apis.ObjectMeta{Name: name, Namespace: "default"},
		Spec: apis.ServiceSpec{
			ClusterIP: "10.96.0.2", Selector: map[string]string{"app": "web"}, Ports: ports,
		},
	}
}

func createEndpointPod(t *testing.T, m Manager, namespace, name, ip string, ports ...apis.ContainerPort) *apis.Pod {
	t.Helper()
	pod := &apis.Pod{
		Metadata: apis.ObjectMeta{Name: name, Namespace: namespace, Labels: map[string]string{"app": "web"}},
		Spec:     apis.PodSpec{NodeName: m.NodeName, Containers: []apis.Container{{Name: "app", Ports: ports}}},
	}
	objects.Stamp(podResource, pod)
	pod.Status = apis.PodStatus{Phase: "Running", PodIP: ip, Conditions: []apis.PodCondition{{Type: "Ready", Status: "True"}}}
	if err := m.Store.Write(false, func(tx *store.Tx) error {
		_, err := tx.Create(objects.Put(podResource, pod))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return pod
}

func writeEndpointTestObject(t *testing.T, m Manager, res *apis.Resource, obj apis.Object) {
	t.Helper()
	if err := m.Store.Write(false, func(tx *store.Tx) error {
		_, _, err := tx.Update(objects.Put(res, obj), 0)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func listEndpointSlices(t *testing.T, m Manager) []apis.EndpointSlice {
	t.Helper()
	rows, _, err := m.Store.List("discovery.k8s.io", "endpointslices", "")
	if err != nil {
		t.Fatal(err)
	}
	got := make([]apis.EndpointSlice, len(rows))
	for i, rec := range rows {
		if err := json.Unmarshal(rec.Data, &got[i]); err != nil {
			t.Fatal(err)
		}
	}
	return got
}

func TestEndpointSlicesGroupByResolvedPorts(t *testing.T) {
	m := testManager(t)
	svc := endpointService("web",
		apis.ServicePort{Name: "web", Port: 80, TargetPort: apis.Str("http")},
		apis.ServicePort{Name: "metrics", Port: 91, TargetPort: apis.Str("prom")},
	)
	create(t, m, testServiceResource, svc)
	metrics := apis.ContainerPort{Name: "prom", ContainerPort: 9090}
	web := apis.ContainerPort{Name: "http", ContainerPort: 8080}
	wrongProtocol := apis.ContainerPort{Name: "http", ContainerPort: 5353, Protocol: "UDP"}
	// Create in reverse name order to exercise deterministic endpoint ordering.
	createEndpointPod(t, m, "default", "b", "10.244.0.3", web, metrics)
	a := createEndpointPod(t, m, "default", "a", "10.244.0.2", metrics, wrongProtocol)
	a.Spec.Containers = append(a.Spec.Containers, apis.Container{Name: "web", Ports: []apis.ContainerPort{web}})
	writeEndpointTestObject(t, m, podResource, a)
	createEndpointPod(t, m, "default", "c", "10.244.0.4", apis.ContainerPort{Name: "http", ContainerPort: 8081}, metrics)
	createEndpointPod(t, m, "default", "d", "10.244.0.5", wrongProtocol, metrics)
	createEndpointPod(t, m, "default", "e", "10.244.0.6", web, apis.ContainerPort{Name: "prom", ContainerPort: 9091})

	sync(t, m, 1)
	got := listEndpointSlices(t, m)
	wantIPs := map[[2]int32][]string{
		{8080, 9090}: {"10.244.0.2", "10.244.0.3"},
		{8081, 9090}: {"10.244.0.4"},
		{0, 9090}:    {"10.244.0.5"},
		{8080, 9091}: {"10.244.0.6"},
	}
	if len(got) != len(wantIPs) {
		t.Fatalf("want four port groups, got %+v", got)
	}
	for _, slice := range got {
		ports := make(map[string]int32)
		for _, port := range slice.Ports {
			if port.Name == nil || port.Protocol == nil || *port.Protocol != "TCP" || port.Port == nil {
				t.Fatalf("invalid EndpointPort: %+v", port)
			}
			ports[*port.Name] = *port.Port
		}
		var ips []string
		for _, endpoint := range slice.Endpoints {
			ips = append(ips, endpoint.Addresses...)
		}
		vector := [2]int32{ports["web"], ports["metrics"]}
		wantPorts := map[string]int32{"metrics": vector[1]}
		if vector[0] != 0 {
			wantPorts["web"] = vector[0]
		}
		if !maps.Equal(ports, wantPorts) || len(slice.Ports) != len(wantPorts) || *slice.Ports[0].Name != "metrics" {
			t.Errorf("ports for %s = %v, want %v in name order", slice.Metadata.Name, ports, wantPorts)
		}
		if !slices.Equal(ips, wantIPs[vector]) {
			t.Errorf("endpoints for ports %v = %v, want %v", ports, ips, wantIPs[vector])
		}
		delete(wantIPs, vector)
	}
	if len(wantIPs) != 0 {
		t.Errorf("missing port groups: %v", wantIPs)
	}
}

func TestEndpointSliceEligibilityAndProxyMetadata(t *testing.T) {
	m := testManager(t)
	svc := endpointService("web", apis.ServicePort{Port: 80})
	svc.Spec.Selector["enabled"] = ""
	create(t, m, testServiceResource, svc)
	headless := endpointService("headless", apis.ServicePort{Port: 80})
	headless.Spec.ClusterIP = "None"
	create(t, m, testServiceResource, headless)
	selectorless := endpointService("selectorless", apis.ServicePort{Port: 80})
	selectorless.Spec.Selector = map[string]string{}
	create(t, m, testServiceResource, selectorless)

	for _, tc := range []struct {
		name, namespace, ip string
		mutate              func(*apis.Pod)
	}{
		{"ready", "default", "10.244.0.2", nil},
		{"other-namespace", "other", "10.244.0.3", nil},
		{"missing-label", "default", "10.244.0.4", func(p *apis.Pod) { delete(p.Metadata.Labels, "enabled") }},
		{"wrong-label", "default", "10.244.0.5", func(p *apis.Pod) { p.Metadata.Labels["app"] = "other" }},
		{"unready", "default", "10.244.0.6", func(p *apis.Pod) { p.Status.Conditions[0].Status = "False" }},
		{"no-ready-condition", "default", "10.244.0.7", func(p *apis.Pod) { p.Status.Conditions = nil }},
		{"terminating", "default", "10.244.0.8", func(p *apis.Pod) { p.Metadata.DeletionTimestamp = apis.Now() }},
		{"missing-ip", "default", "", nil},
		{"invalid-ip", "default", "bad-ip", nil},
		{"ipv6", "default", "fd00::1", nil},
		{"mapped-ip", "default", "::ffff:10.244.0.9", nil},
		{"outside-cidr", "default", "10.244.1.2", nil},
		{"network", "default", "10.244.0.0", nil},
		{"gateway", "default", "10.244.0.1", nil},
		{"broadcast", "default", "10.244.0.255", nil},
	} {
		p := createEndpointPod(t, m, tc.namespace, tc.name, tc.ip)
		p.Metadata.Labels["enabled"] = ""
		if tc.mutate != nil {
			tc.mutate(p)
		}
		writeEndpointTestObject(t, m, podResource, p)
	}
	sync(t, m, 1)
	got := listEndpointSlices(t, m)
	if len(got) != 1 || len(got[0].Endpoints) != 1 || len(got[0].Ports) != 1 {
		t.Fatalf("want one slice with one endpoint and port, got %+v", got)
	}
	slice := got[0]
	if slice.APIVersion != "discovery.k8s.io/v1" || slice.Kind != "EndpointSlice" || slice.AddressType != "IPv4" ||
		slice.Metadata.Namespace != "default" || slice.Metadata.Labels["kubernetes.io/service-name"] != "web" ||
		slice.Metadata.Labels["endpointslice.kubernetes.io/managed-by"] != "shearwater" ||
		slice.Metadata.Annotations["shearwater.io/service-generation"] != "1" {
		t.Errorf("slice metadata: %+v", slice)
	}
	if refs := slice.Metadata.OwnerReferences; len(refs) != 1 || refs[0].APIVersion != "v1" || refs[0].Kind != "Service" ||
		refs[0].Name != "web" || refs[0].UID != svc.Metadata.UID || refs[0].Controller == nil || !*refs[0].Controller {
		t.Errorf("Service owner reference: %+v", refs)
	}
	if port := slice.Ports[0]; port.Name == nil || *port.Name != "" || port.Port == nil || *port.Port != 80 {
		t.Errorf("unnamed port with default target: %+v", port)
	}
	ep := slice.Endpoints[0]
	if !slices.Equal(ep.Addresses, []string{"10.244.0.2"}) || ep.Conditions.Ready == nil || !*ep.Conditions.Ready ||
		ep.TargetRef == nil || ep.TargetRef.Name != "ready" || ep.TargetRef.Kind != "Pod" || ep.TargetRef.Namespace != "default" ||
		ep.NodeName == nil || *ep.NodeName != m.NodeName {
		t.Errorf("endpoint for the ready Pod: %+v", ep)
	}
}

func TestEndpointSlicesRetireStaleGroupsOnServiceChanges(t *testing.T) {
	m := testManager(t)
	svc := endpointService("web", apis.ServicePort{Port: 80, TargetPort: apis.Str("http")})
	create(t, m, testServiceResource, svc)
	first := createEndpointPod(t, m, "default", "a", "10.244.0.2", apis.ContainerPort{Name: "http", ContainerPort: 8080})
	first.Metadata.Labels["track"] = "stable"
	writeEndpointTestObject(t, m, podResource, first)
	createEndpointPod(t, m, "default", "b", "10.244.0.3") // no resolved port
	sync(t, m, 1)
	initial := listEndpointSlices(t, m)
	if len(initial) != 2 {
		t.Fatalf("want resolved and unresolved groups, got %+v", initial)
	}
	var keep apis.EndpointSlice
	for _, slice := range initial {
		if len(slice.Ports) == 1 {
			keep = slice
		} else if len(slice.Endpoints) != 1 || slice.Endpoints[0].TargetRef.Name != "b" {
			t.Fatalf("unresolved Pod must have its own group: %+v", slice)
		}
	}
	if keep.Metadata.UID == "" {
		t.Fatal("missing resolved group")
	}

	// A Service spec change retires one group and updates the retained
	// group's generation even though its target and endpoint are identical.
	svc.Spec.Selector["track"] = "stable"
	svc.Metadata.Generation++
	writeEndpointTestObject(t, m, testServiceResource, svc)
	sync(t, m, 1)
	got := listEndpointSlices(t, m)
	if len(got) != 1 || got[0].Metadata.UID != keep.Metadata.UID ||
		got[0].Metadata.Annotations["shearwater.io/service-generation"] != "2" {
		t.Fatalf("selector change should keep one current group: %+v", got)
	}

	svc.Spec.Ports[0].TargetPort = apis.Int(9000)
	svc.Metadata.Generation++
	writeEndpointTestObject(t, m, testServiceResource, svc)
	sync(t, m, 1)
	got = listEndpointSlices(t, m)
	if len(got) != 1 || got[0].Metadata.Name == keep.Metadata.Name || len(got[0].Ports) != 1 ||
		got[0].Ports[0].Port == nil || *got[0].Ports[0].Port != 9000 || got[0].Metadata.Annotations["shearwater.io/service-generation"] != "3" {
		t.Fatalf("target change should replace the stale group: %+v", got)
	}

	first.Metadata.DeletionTimestamp = apis.Now()
	writeEndpointTestObject(t, m, podResource, first)
	sync(t, m, 1)
	got = listEndpointSlices(t, m)
	if len(got) != 1 || got[0].Endpoints == nil || len(got[0].Endpoints) != 0 || len(got[0].Ports) != 0 {
		t.Fatalf("want an empty slice after the last endpoint terminates, got %+v", got)
	}

	foreign := &apis.EndpointSlice{Metadata: apis.ObjectMeta{Name: "external", Namespace: "default", Labels: map[string]string{
		"kubernetes.io/service-name": "web", "endpointslice.kubernetes.io/managed-by": "external",
	}}}
	create(t, m, endpointSliceResource, foreign)
	svc.Spec.Selector = nil
	svc.Metadata.Generation++
	writeEndpointTestObject(t, m, testServiceResource, svc)
	sync(t, m, 1)
	if got := listEndpointSlices(t, m); len(got) != 1 || got[0].Metadata.UID != foreign.Metadata.UID {
		t.Fatalf("only the externally managed slice should survive selector removal: %+v", got)
	}
}

func TestEndpointSliceSettledPassDoesNotAdvanceRevision(t *testing.T) {
	m := testManager(t)
	create(t, m, testServiceResource, endpointService("web", apis.ServicePort{Port: 80}))
	pod := createEndpointPod(t, m, "default", "web", "10.244.0.2")
	sync(t, m, 1)
	// The Pod's resourceVersion is intentionally absent from TargetRef.
	// Status changes unrelated to endpoint selection leave the slice alone.
	pod.Status.Message = "unrelated status update"
	writeEndpointTestObject(t, m, podResource, pod)
	before, err := m.Store.Rev()
	if err != nil {
		t.Fatal(err)
	}
	sync(t, m, 3)
	after, err := m.Store.Rev()
	if err != nil || after != before {
		t.Fatalf("settled controller revision %d -> %d, error %v", before, after, err)
	}
}

func TestEndpointSliceWithdrawsPodWhenGCStartsDeletion(t *testing.T) {
	m := testManager(t)
	create(t, m, testServiceResource, endpointService("web", apis.ServicePort{Port: 80}))
	owner := endpointService("owner", apis.ServicePort{Port: 80})
	owner.Spec.Selector = nil
	create(t, m, testServiceResource, owner)
	pod := createEndpointPod(t, m, "default", "web", "10.244.0.2")
	pod.Metadata.OwnerReferences = []apis.OwnerReference{{APIVersion: "v1", Kind: "Service", Name: "owner", UID: owner.Metadata.UID}}
	writeEndpointTestObject(t, m, podResource, pod)
	sync(t, m, 1)
	if got := listEndpointSlices(t, m); len(got) != 1 || len(got[0].Endpoints) != 1 {
		t.Fatalf("want one live endpoint before deleting the Pod owner: %+v", got)
	}
	if err := m.Store.Write(false, func(tx *store.Tx) error {
		_, err := tx.Delete(objects.Key(testServiceResource, "default", "owner"), 0, objects.Encoder(owner))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	sync(t, m, 1)
	if got := pods(t, m); len(got) != 1 || got[0].Metadata.DeletionTimestamp == nil {
		t.Fatalf("GC should have marked the Pod terminating: %+v", got)
	}
	if got := listEndpointSlices(t, m); len(got) != 1 || len(got[0].Endpoints) != 0 {
		t.Fatalf("the same pass should withdraw the Pod's endpoint: %+v", got)
	}
}
