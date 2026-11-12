package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/netip"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"shearwater/internal/apis"
	"shearwater/internal/objects"
	"shearwater/internal/store"
)

func record(t *testing.T, key store.Key, obj apis.Object) store.Record {
	t.Helper()
	data, err := json.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	return store.Record{Key: key, UID: obj.Meta().UID, Generation: obj.Meta().Generation, Data: data}
}

func TestRoutesUseCurrentOwnerAndPortGroup(t *testing.T) {
	svc := &apis.Service{
		Metadata: apis.ObjectMeta{Name: "web", Namespace: "default", UID: "service-2", Generation: 3},
		Spec: apis.ServiceSpec{Type: "ClusterIP", ClusterIP: "10.96.0.10", Selector: map[string]string{"app": "web"},
			Ports: []apis.ServicePort{{Name: "web", Protocol: "TCP", Port: 80}, {Name: "metrics", Protocol: "TCP", Port: 90}}},
	}
	sr := record(t, store.Key{Resource: "services", Namespace: "default", Name: "web"}, svc)
	es := func(owner, generation, ip, portName string, target int32, ready bool) store.Record {
		return record(t, store.Key{Group: "discovery.k8s.io", Resource: "endpointslices", Namespace: "default"}, &apis.EndpointSlice{
			Metadata: apis.ObjectMeta{
				Labels:          map[string]string{apis.ServiceNameLabel: "web", apis.EndpointSliceManagedByLabel: "shearwater"},
				Annotations:     map[string]string{apis.ServiceGenerationAnnotation: generation},
				OwnerReferences: []apis.OwnerReference{{APIVersion: "v1", Kind: "Service", Name: "web", UID: owner}},
			},
			AddressType: "IPv4",
			Ports:       []apis.EndpointPort{{Name: &portName, Port: &target}},
			Endpoints: []apis.Endpoint{{
				Addresses: []string{ip}, Conditions: apis.EndpointConditions{Ready: &ready},
				TargetRef: &apis.ObjectReference{Kind: "Pod", APIVersion: "v1", Namespace: "default", Name: "pod", UID: "pod-1"},
			}},
		})
	}
	routes, err := buildRoutes([]store.Record{sr}, []store.Record{
		es("service-2", "3", "10.244.0.5", "web", 8080, true),
		es("service-2", "3", "10.244.0.6", "web", 8081, true),
		es("service-2", "3", "10.244.0.5", "metrics", 9090, true),
		es("service-1", "3", "10.244.0.7", "web", 8080, true),
		es("service-2", "2", "10.244.0.8", "web", 8080, true),
		es("service-2", "3", "10.244.0.9", "web", 8080, false),
		es("service-2", "3", "127.0.0.1", "web", 8080, true),
	})
	if err != nil {
		t.Fatal(err)
	}
	for address, want := range map[string][]netip.AddrPort{
		"10.96.0.10:80": {netip.MustParseAddrPort("10.244.0.5:8080"), netip.MustParseAddrPort("10.244.0.6:8081")},
		"10.96.0.10:90": {netip.MustParseAddrPort("10.244.0.5:9090")},
	} {
		var got []netip.AddrPort
		for _, target := range routes[netip.MustParseAddrPort(address)].backends {
			got = append(got, target.address)
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s routes to %v, want %v", address, got, want)
		}
	}
}

func TestBackendGuardRejectsStalePod(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	res := apis.Lookup("", "v1", "pods")
	pod := &apis.Pod{
		Metadata: apis.ObjectMeta{Namespace: "default", Name: "blue", Labels: map[string]string{"app": "blue"}},
	}
	objects.Stamp(res, pod)
	pod.Status = apis.PodStatus{PodIP: "10.244.0.5", Phase: "Running",
		Conditions: []apis.PodCondition{{Type: "Ready", Status: "True"}}}
	b := backend{address: netip.MustParseAddrPort("10.244.0.5:8080"),
		pod: objects.Key(res, pod.Metadata.Namespace, pod.Metadata.Name), uid: pod.Metadata.UID}
	if err := st.Write(false, func(tx *store.Tx) error {
		_, err := tx.Create(objects.Put(res, pod))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	m := Manager{Store: st}
	r := &route{selector: map[string]string{"app": "blue"}}
	if m.currentPod(t.Context(), r, b) {
		t.Fatal("Pod status claimed an IP without a reservation")
	}
	if err := st.Write(false, func(tx *store.Tx) error {
		_, err := tx.AllocIP("pod", pod.Metadata.UID, pod.Status.PodIP, nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !m.currentPod(t.Context(), r, b) {
		t.Fatal("eligible Pod was rejected")
	}
	if m.currentPod(t.Context(), &route{selector: map[string]string{"app": "green"}}, b) {
		t.Fatal("Pod with changed labels was accepted by the old selector")
	}
	stale := b
	stale.uid = "previous-pod"
	if m.currentPod(t.Context(), r, stale) {
		t.Fatal("a reused Pod IP was accepted for the old UID")
	}
	pod.Metadata.DeletionTimestamp = apis.Now()
	if err := st.Write(false, func(tx *store.Tx) error {
		_, _, err := tx.Update(objects.Put(res, pod), 0)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if m.currentPod(t.Context(), r, b) {
		t.Fatal("terminating Pod was accepted")
	}
}

var loopback = netip.MustParseAddrPort("127.0.0.1:0")

func testBackend(t *testing.T, reply string) *net.TCPListener {
	t.Helper()
	ln, err := net.ListenTCP("tcp4", net.TCPAddrFromAddrPort(loopback))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.AcceptTCP()
			if err != nil {
				return
			}
			conn.SetDeadline(time.Now().Add(3 * time.Second))
			// The backend will only reply after it sees a write half-close.
			if data, err := io.ReadAll(conn); err == nil {
				fmt.Fprintf(conn, "%s:%s", reply, data)
			}
			conn.Close()
		}
	}()
	return ln
}

func exchange(t *testing.T, f *forwarder, want string) {
	t.Helper()
	c, err := net.DialTCP("tcp4", nil, f.ln.Addr().(*net.TCPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.WriteString(c, "hello"); err != nil {
		t.Fatal(err)
	}
	if err := c.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(c)
	if err != nil || string(got) != want {
		t.Fatalf("proxy reply %q, %v, want %q", got, err, want)
	}
}

func TestTCPRoundRobinAndDialFallback(t *testing.T) {
	a := testBackend(t, "a")
	b := testBackend(t, "b")
	targets := []backend{{address: a.Addr().(*net.TCPAddr).AddrPort()}, {address: b.Addr().(*net.TCPAddr).AddrPort()}}
	f, err := listen(t.Context(), loopback, route{backends: targets}, func(*route) bool { return true }, func(context.Context, *route, backend) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	defer f.stop()
	exchange(t, f, "a:hello")
	exchange(t, f, "b:hello")
	a.Close()
	exchange(t, f, "b:hello") // The next rotation starts at the closed port.
}

func TestTCPStopClosesOpenStreams(t *testing.T) {
	upstream, err := net.ListenTCP("tcp4", net.TCPAddrFromAddrPort(loopback))
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()
	f, err := listen(t.Context(), loopback, route{backends: []backend{{address: upstream.Addr().(*net.TCPAddr).AddrPort()}}},
		func(*route) bool { return true }, func(context.Context, *route, backend) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	defer f.stop()
	c, err := net.DialTCP("tcp4", nil, f.ln.Addr().(*net.TCPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	upstream.SetDeadline(time.Now().Add(3 * time.Second))
	target, err := upstream.AcceptTCP()
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	target.SetDeadline(time.Now().Add(3 * time.Second))
	c.SetDeadline(time.Now().Add(3 * time.Second))
	f.stop()
	for _, conn := range []*net.TCPConn{c, target} {
		if _, err := conn.Read(make([]byte, 1)); err != io.EOF {
			t.Fatalf("stream was not closed after listener shutdown: %v", err)
		}
	}
}

func TestDialCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if conn := dialBackend(ctx, []backend{{address: loopback}}, 0, func(context.Context, backend) bool { return true }); conn != nil {
		conn.Close()
		t.Fatal("canceled dial made a connection")
	}
}
