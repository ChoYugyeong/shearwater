package netpol

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"reflect"
	"sync"
	"testing"
	"time"

	"shearwater/internal/apis"
	"shearwater/internal/runtime"
	"shearwater/internal/store"
)

func makePod(ns, name, ip string, labels map[string]string, ports ...apis.ContainerPort) apis.Pod {
	return apis.Pod{
		Metadata: apis.ObjectMeta{Namespace: ns, Name: name, Labels: labels},
		Spec: apis.PodSpec{
			Containers: []apis.Container{{Name: "app", Image: "img:1", Ports: ports}},
		},
		Status: apis.PodStatus{Phase: "Running", PodIP: ip},
	}
}

func makeNS(name string, labels map[string]string) apis.Namespace {
	return apis.Namespace{
		Metadata: apis.ObjectMeta{Name: name, Labels: labels},
	}
}

func portInt(n int32) *apis.IntOrString {
	v := apis.Int(n)
	return &v
}

func portStr(s string) *apis.IntOrString {
	v := apis.Str(s)
	return &v
}

func i32(n int32) *int32 { return &n }

func TestCompileNoPolicies(t *testing.T) {
	cidr := netip.MustParsePrefix("10.244.0.0/24")
	pods := []apis.Pod{makePod("default", "web", "10.244.0.2", map[string]string{"app": "web"})}
	plan := Compile(pods, []apis.Namespace{makeNS("default", nil)}, nil, cidr)
	if plan.HasPolicies || len(plan.Pods) != 0 {
		t.Fatalf("expected empty plan with HasPolicies=false, got %+v", plan)
	}
}

// Kubernetes doc example: Default deny all ingress traffic.
func TestCompileDefaultDenyIngress(t *testing.T) {
	cidr := netip.MustParsePrefix("10.244.0.0/24")
	pods := []apis.Pod{
		makePod("default", "a", "10.244.0.2", map[string]string{"app": "a"}),
		makePod("other", "b", "10.244.0.3", map[string]string{"app": "b"}),
		makePod("default", "remote", "10.244.1.2", map[string]string{"app": "a"}),
	}
	np := apis.NetworkPolicy{
		Metadata: apis.ObjectMeta{Namespace: "default", Name: "default-deny-ingress"},
		Spec:     apis.NetworkPolicySpec{PodSelector: apis.LabelSelector{}, PolicyTypes: []string{apis.PolicyTypeIngress}},
	}
	plan := Compile(pods, []apis.Namespace{makeNS("default", nil), makeNS("other", nil)}, []apis.NetworkPolicy{np}, cidr)
	if !plan.HasPolicies || len(plan.Pods) != 1 {
		t.Fatalf("want 1 isolated local pod in default ns, got %+v", plan)
	}
	p := plan.Pods[0]
	if p.IP != netip.MustParseAddr("10.244.0.2") || !p.IngressIsolated || len(p.Ingress) != 0 || p.EgressIsolated {
		t.Fatalf("unexpected pod policy: %+v", p)
	}
}

// Kubernetes doc example: Allow ingress from same namespace pods (and across nodes).
func TestCompileAllowFromSameNamespaceAndMultiNode(t *testing.T) {
	cidr := netip.MustParsePrefix("10.244.0.0/24")
	pods := []apis.Pod{
		makePod("default", "db", "10.244.0.2", map[string]string{"role": "db"}),
		makePod("default", "front-local", "10.244.0.3", map[string]string{"role": "frontend"}),
		makePod("default", "front-remote", "10.244.1.7", map[string]string{"role": "frontend"}),
		makePod("other", "front-other-ns", "10.244.0.4", map[string]string{"role": "frontend"}),
		// Terminated pod must be ignored.
		{
			Metadata: apis.ObjectMeta{Namespace: "default", Name: "front-done", Labels: map[string]string{"role": "frontend"}},
			Status:   apis.PodStatus{Phase: "Succeeded", PodIP: "10.244.0.9"},
		},
	}
	np := apis.NetworkPolicy{
		Metadata: apis.ObjectMeta{Namespace: "default", Name: "allow-frontend"},
		Spec: apis.NetworkPolicySpec{
			PodSelector: apis.LabelSelector{MatchLabels: map[string]string{"role": "db"}},
			Ingress: []apis.NetworkPolicyIngressRule{{
				From:  []apis.NetworkPolicyPeer{{PodSelector: &apis.LabelSelector{MatchLabels: map[string]string{"role": "frontend"}}}},
				Ports: []apis.NetworkPolicyPort{{Protocol: "TCP", Port: portInt(5432)}},
			}},
		},
	}
	plan := Compile(pods, []apis.Namespace{makeNS("default", nil), makeNS("other", nil)}, []apis.NetworkPolicy{np}, cidr)
	if len(plan.Pods) != 1 {
		t.Fatalf("want 1 isolated pod (db), got %+v", plan.Pods)
	}
	want := []runtime.NetpolRule{
		{Peer: netip.MustParsePrefix("10.244.0.3/32"), Protocol: "TCP", Port: 5432},
		{Peer: netip.MustParsePrefix("10.244.1.7/32"), Protocol: "TCP", Port: 5432},
	}
	if !reflect.DeepEqual(plan.Pods[0].Ingress, want) {
		t.Fatalf("ingress rules = %+v, want %+v", plan.Pods[0].Ingress, want)
	}
}

// Kubernetes doc example: namespaceSelector + podSelector AND (single item) vs OR (two items).
func TestCompileNamespaceAndPodSelectorANDvsOR(t *testing.T) {
	cidr := netip.MustParsePrefix("10.244.0.0/24")
	namespaces := []apis.Namespace{
		makeNS("app", map[string]string{"team": "core"}),
		makeNS("prod", map[string]string{"env": "prod"}),
		makeNS("staging", map[string]string{"env": "staging"}),
	}
	pods := []apis.Pod{
		makePod("app", "target", "10.244.0.2", map[string]string{"app": "target"}),
		makePod("app", "same-ns-client", "10.244.0.3", map[string]string{"role": "client"}),
		makePod("prod", "prod-client", "10.244.0.4", map[string]string{"role": "client"}),
		makePod("prod", "prod-other", "10.244.0.5", map[string]string{"role": "batch"}),
		makePod("staging", "staging-client", "10.244.0.6", map[string]string{"role": "client"}),
	}

	// AND: single peer with both namespaceSelector and podSelector -> only prod-client (10.244.0.4).
	andNP := apis.NetworkPolicy{
		Metadata: apis.ObjectMeta{Namespace: "app", Name: "and-policy"},
		Spec: apis.NetworkPolicySpec{
			PodSelector: apis.LabelSelector{MatchLabels: map[string]string{"app": "target"}},
			Ingress: []apis.NetworkPolicyIngressRule{{
				From: []apis.NetworkPolicyPeer{{
					NamespaceSelector: &apis.LabelSelector{MatchLabels: map[string]string{"env": "prod"}},
					PodSelector:       &apis.LabelSelector{MatchLabels: map[string]string{"role": "client"}},
				}},
			}},
		},
	}
	andPlan := Compile(pods, namespaces, []apis.NetworkPolicy{andNP}, cidr)
	wantAND := []runtime.NetpolRule{
		{Peer: netip.MustParsePrefix("10.244.0.4/32")},
	}
	if len(andPlan.Pods) != 1 || !reflect.DeepEqual(andPlan.Pods[0].Ingress, wantAND) {
		t.Fatalf("AND rules = %+v, want %+v", andPlan.Pods, wantAND)
	}

	// OR: two separate peers -> all pods in prod (10.244.0.4, 10.244.0.5) OR role=client in policy's own ns "app" (10.244.0.3).
	orNP := apis.NetworkPolicy{
		Metadata: apis.ObjectMeta{Namespace: "app", Name: "or-policy"},
		Spec: apis.NetworkPolicySpec{
			PodSelector: apis.LabelSelector{MatchLabels: map[string]string{"app": "target"}},
			Ingress: []apis.NetworkPolicyIngressRule{{
				From: []apis.NetworkPolicyPeer{
					{NamespaceSelector: &apis.LabelSelector{MatchLabels: map[string]string{"env": "prod"}}},
					{PodSelector: &apis.LabelSelector{MatchLabels: map[string]string{"role": "client"}}},
				},
			}},
		},
	}
	orPlan := Compile(pods, namespaces, []apis.NetworkPolicy{orNP}, cidr)
	wantOR := []runtime.NetpolRule{
		{Peer: netip.MustParsePrefix("10.244.0.3/32")},
		{Peer: netip.MustParsePrefix("10.244.0.4/32")},
		{Peer: netip.MustParsePrefix("10.244.0.5/32")},
	}
	if len(orPlan.Pods) != 1 || !reflect.DeepEqual(orPlan.Pods[0].Ingress, wantOR) {
		t.Fatalf("OR rules = %+v, want %+v", orPlan.Pods, wantOR)
	}
}

// Kubernetes doc example: ipBlock with except (172.17.0.0/16 except 172.17.1.0/24).
func TestCompileIPBlockExcept(t *testing.T) {
	cidr := netip.MustParsePrefix("10.244.0.0/24")
	pods := []apis.Pod{makePod("default", "db", "10.244.0.2", map[string]string{"role": "db"})}
	np := apis.NetworkPolicy{
		Metadata: apis.ObjectMeta{Namespace: "default", Name: "ipblock"},
		Spec: apis.NetworkPolicySpec{
			PodSelector: apis.LabelSelector{MatchLabels: map[string]string{"role": "db"}},
			Ingress: []apis.NetworkPolicyIngressRule{{
				From: []apis.NetworkPolicyPeer{{
					IPBlock: &apis.IPBlock{
						CIDR:   "172.17.0.0/16",
						Except: []string{"172.17.1.0/24"},
					},
				}},
				Ports: []apis.NetworkPolicyPort{{Protocol: "TCP", Port: portInt(6379)}},
			}},
		},
	}
	plan := Compile(pods, []apis.Namespace{makeNS("default", nil)}, []apis.NetworkPolicy{np}, cidr)
	if len(plan.Pods) != 1 {
		t.Fatalf("want 1 pod, got %+v", plan.Pods)
	}
	rules := plan.Pods[0].Ingress
	wantCIDRs := []string{
		"172.17.0.0/24",
		"172.17.2.0/23",
		"172.17.4.0/22",
		"172.17.8.0/21",
		"172.17.16.0/20",
		"172.17.32.0/19",
		"172.17.64.0/18",
		"172.17.128.0/17",
	}
	if len(rules) != len(wantCIDRs) {
		t.Fatalf("got %d rules (%+v), want %d", len(rules), rules, len(wantCIDRs))
	}
	for i, w := range wantCIDRs {
		if rules[i].Peer.String() != w || rules[i].Protocol != "TCP" || rules[i].Port != 6379 {
			t.Errorf("rule[%d] = %+v, want %s TCP 6379", i, rules[i], w)
		}
	}
	// Verify membership invariant: 172.17.1.5 excluded, 172.17.0.5 and 172.17.2.5 included.
	contains := func(ipStr string) bool {
		ip := netip.MustParseAddr(ipStr)
		for _, r := range rules {
			if r.Peer.Contains(ip) {
				return true
			}
		}
		return false
	}
	if contains("172.17.1.5") || !contains("172.17.0.5") || !contains("172.17.2.5") || !contains("172.17.255.254") || contains("172.18.0.1") {
		t.Fatal("ipBlock except prefix subtraction violated membership invariant")
	}
}

// Named ports resolve against the destination Pod's container ports (both ingress and egress).
func TestCompileNamedPortsAndEndPort(t *testing.T) {
	cidr := netip.MustParsePrefix("10.244.0.0/24")
	pods := []apis.Pod{
		makePod("default", "client", "10.244.0.2", map[string]string{"app": "client"}),
		makePod("default", "api-a", "10.244.0.3", map[string]string{"app": "api"},
			apis.ContainerPort{Name: "http", ContainerPort: 8080, Protocol: "TCP"},
			apis.ContainerPort{Name: "metrics", ContainerPort: 9090, Protocol: "TCP"},
		),
		makePod("default", "api-b", "10.244.1.4", map[string]string{"app": "api"},
			apis.ContainerPort{Name: "http", ContainerPort: 8000, Protocol: "TCP"},
		),
		makePod("default", "api-udp", "10.244.0.5", map[string]string{"app": "api"},
			apis.ContainerPort{Name: "http", ContainerPort: 9999, Protocol: "UDP"}, // wrong protocol
		),
	}

	// Ingress policy on api pods using named port "http" + port range 32000-32768.
	ingNP := apis.NetworkPolicy{
		Metadata: apis.ObjectMeta{Namespace: "default", Name: "api-ingress"},
		Spec: apis.NetworkPolicySpec{
			PodSelector: apis.LabelSelector{MatchLabels: map[string]string{"app": "api"}},
			PolicyTypes: []string{apis.PolicyTypeIngress},
			Ingress: []apis.NetworkPolicyIngressRule{{
				From: []apis.NetworkPolicyPeer{{PodSelector: &apis.LabelSelector{MatchLabels: map[string]string{"app": "client"}}}},
				Ports: []apis.NetworkPolicyPort{
					{Protocol: "TCP", Port: portStr("http")},
					{Protocol: "TCP", Port: portInt(32000), EndPort: i32(32768)},
				},
			}},
		},
	}

	// Egress policy on client pod using named port "http" to app=api.
	egNP := apis.NetworkPolicy{
		Metadata: apis.ObjectMeta{Namespace: "default", Name: "client-egress"},
		Spec: apis.NetworkPolicySpec{
			PodSelector: apis.LabelSelector{MatchLabels: map[string]string{"app": "client"}},
			PolicyTypes: []string{apis.PolicyTypeEgress},
			Egress: []apis.NetworkPolicyEgressRule{{
				To:    []apis.NetworkPolicyPeer{{PodSelector: &apis.LabelSelector{MatchLabels: map[string]string{"app": "api"}}}},
				Ports: []apis.NetworkPolicyPort{{Protocol: "TCP", Port: portStr("http")}},
			}},
		},
	}

	plan := Compile(pods, []apis.Namespace{makeNS("default", nil)}, []apis.NetworkPolicy{ingNP, egNP}, cidr)
	byIP := map[netip.Addr]runtime.PodNetpol{}
	for _, p := range plan.Pods {
		byIP[p.IP] = p
	}

	// client (10.244.0.2): egress resolves "http"/TCP to api-a:8080 and api-b:8000, skipping api-udp.
	wantClientEgress := []runtime.NetpolRule{
		{Peer: netip.MustParsePrefix("10.244.0.3/32"), Protocol: "TCP", Port: 8080},
		{Peer: netip.MustParsePrefix("10.244.1.4/32"), Protocol: "TCP", Port: 8000},
	}
	if !reflect.DeepEqual(byIP[netip.MustParseAddr("10.244.0.2")].Egress, wantClientEgress) {
		t.Fatalf("client egress = %+v, want %+v", byIP[netip.MustParseAddr("10.244.0.2")].Egress, wantClientEgress)
	}

	// api-a (10.244.0.3): ingress resolves "http"/TCP to 8080 plus range 32000:32768.
	wantAPIA := []runtime.NetpolRule{
		{Peer: netip.MustParsePrefix("10.244.0.2/32"), Protocol: "TCP", Port: 8080},
		{Peer: netip.MustParsePrefix("10.244.0.2/32"), Protocol: "TCP", Port: 32000, EndPort: 32768},
	}
	if !reflect.DeepEqual(byIP[netip.MustParseAddr("10.244.0.3")].Ingress, wantAPIA) {
		t.Fatalf("api-a ingress = %+v, want %+v", byIP[netip.MustParseAddr("10.244.0.3")].Ingress, wantAPIA)
	}

	// api-udp (10.244.0.5): "http" is UDP only so TCP "http" does not resolve, only 32000:32768 matches.
	wantAPIUDP := []runtime.NetpolRule{
		{Peer: netip.MustParsePrefix("10.244.0.2/32"), Protocol: "TCP", Port: 32000, EndPort: 32768},
	}
	if !reflect.DeepEqual(byIP[netip.MustParseAddr("10.244.0.5")].Ingress, wantAPIUDP) {
		t.Fatalf("api-udp ingress = %+v, want %+v", byIP[netip.MustParseAddr("10.244.0.5")].Ingress, wantAPIUDP)
	}
}

// Kubernetes doc example: Egress to DNS on UDP 53 (empty to + UDP 53) and union with default-deny egress.
func TestCompileEgressToDNSAndPolicyUnion(t *testing.T) {
	cidr := netip.MustParsePrefix("10.244.0.0/24")
	pods := []apis.Pod{
		makePod("default", "worker", "10.244.0.2", map[string]string{"app": "worker"}),
		makePod("kube-system", "dns", "10.244.0.10", map[string]string{"k8s-app": "kube-dns"}),
	}
	denyEgress := apis.NetworkPolicy{
		Metadata: apis.ObjectMeta{Namespace: "default", Name: "default-deny-egress"},
		Spec: apis.NetworkPolicySpec{
			PodSelector: apis.LabelSelector{},
			PolicyTypes: []string{apis.PolicyTypeEgress},
		},
	}
	allowDNS := apis.NetworkPolicy{
		Metadata: apis.ObjectMeta{Namespace: "default", Name: "allow-dns"},
		Spec: apis.NetworkPolicySpec{
			PodSelector: apis.LabelSelector{MatchLabels: map[string]string{"app": "worker"}},
			PolicyTypes: []string{apis.PolicyTypeEgress},
			Egress: []apis.NetworkPolicyEgressRule{
				{
					// Empty to = all destinations on UDP 53.
					Ports: []apis.NetworkPolicyPort{{Protocol: "UDP", Port: portInt(53)}},
				},
				{
					// Also test synthetic kubernetes.io/metadata.name namespace label on kube-system.
					To:    []apis.NetworkPolicyPeer{{NamespaceSelector: &apis.LabelSelector{MatchLabels: map[string]string{namespaceNameLabel: "kube-system"}}}},
					Ports: []apis.NetworkPolicyPort{{Protocol: "TCP", Port: portInt(53)}},
				},
			},
		},
	}
	plan := Compile(pods, []apis.Namespace{makeNS("default", nil), makeNS("kube-system", nil)}, []apis.NetworkPolicy{denyEgress, allowDNS}, cidr)
	if len(plan.Pods) != 1 {
		t.Fatalf("want 1 isolated pod (worker), got %+v", plan.Pods)
	}
	p := plan.Pods[0]
	if p.IngressIsolated || !p.EgressIsolated {
		t.Fatalf("expected egress-only isolation, got %+v", p)
	}
	want := []runtime.NetpolRule{
		{Peer: netip.MustParsePrefix("0.0.0.0/0"), Protocol: "UDP", Port: 53},
		{Peer: netip.MustParsePrefix("10.244.0.10/32"), Protocol: "TCP", Port: 53},
	}
	if !reflect.DeepEqual(p.Egress, want) {
		t.Fatalf("egress rules = %+v, want %+v", p.Egress, want)
	}
}

// Empty ingress rule ({}) allows all ingress sources and ports.
func TestCompileAllowAllIngress(t *testing.T) {
	cidr := netip.MustParsePrefix("10.244.0.0/24")
	pods := []apis.Pod{makePod("default", "web", "10.244.0.2", map[string]string{"app": "web"})}
	np := apis.NetworkPolicy{
		Metadata: apis.ObjectMeta{Namespace: "default", Name: "allow-all"},
		Spec: apis.NetworkPolicySpec{
			PodSelector: apis.LabelSelector{},
			Ingress:     []apis.NetworkPolicyIngressRule{{}},
			PolicyTypes: []string{apis.PolicyTypeIngress},
		},
	}
	plan := Compile(pods, []apis.Namespace{makeNS("default", nil)}, []apis.NetworkPolicy{np}, cidr)
	want := []runtime.NetpolRule{{Peer: netip.MustParsePrefix("0.0.0.0/0")}}
	if len(plan.Pods) != 1 || !reflect.DeepEqual(plan.Pods[0].Ingress, want) {
		t.Fatalf("allow-all ingress = %+v, want %+v", plan.Pods, want)
	}
}

// ---- Manager tests ----

type fakeReader struct {
	mu      sync.Mutex
	records map[string][]store.Record
	ch      chan struct{}
}

func newFakeReader() *fakeReader {
	return &fakeReader{
		records: map[string][]store.Record{},
		ch:      make(chan struct{}),
	}
}

func (f *fakeReader) set(group, resource string, objs ...any) {
	f.mu.Lock()
	var recs []store.Record
	for _, o := range objs {
		b, _ := json.Marshal(o)
		recs = append(recs, store.Record{Data: b})
	}
	f.records[group+"/"+resource] = recs
	old := f.ch
	f.ch = make(chan struct{})
	f.mu.Unlock()
	close(old)
}

func (f *fakeReader) Get(store.Key) (store.Record, error) { return store.Record{}, store.ErrNotFound }

func (f *fakeReader) List(group, resource, _ string) ([]store.Record, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.Record(nil), f.records[group+"/"+resource]...), 1, nil
}

func (f *fakeReader) Changed() <-chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ch
}

type fakeDataPath struct {
	mu     sync.Mutex
	plans  []runtime.NetpolPlan
	failN  int
	notify chan struct{}
}

func (d *fakeDataPath) Reconcile(_ context.Context, p runtime.NetpolPlan) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.failN > 0 {
		d.failN--
		return errors.New("transient iptables error")
	}
	d.plans = append(d.plans, p)
	select {
	case d.notify <- struct{}{}:
	default:
	}
	return nil
}

func TestManagerReconcilesOnChangeAndBacksOffOnError(t *testing.T) {
	r := newFakeReader()
	r.set("", "namespaces", makeNS("default", nil))
	r.set("", "pods", makePod("default", "web", "10.244.0.2", map[string]string{"app": "web"}))

	dp := &fakeDataPath{failN: 1, notify: make(chan struct{}, 8)}
	var logged []string
	var logMu sync.Mutex

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	m := &Manager{
		Store:        r,
		PodCIDR:      netip.MustParsePrefix("10.244.0.0/24"),
		DataPath:     dp,
		RefreshEvery: time.Hour,
		MinBackoff:   10 * time.Millisecond,
		MaxBackoff:   50 * time.Millisecond,
		Logf: func(format string, args ...any) {
			logMu.Lock()
			logged = append(logged, format)
			logMu.Unlock()
		},
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		m.Run(ctx)
	}()

	// First sync fails once, logs, backs off 10ms, then succeeds with HasPolicies=false.
	select {
	case <-dp.notify:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for initial reconcile after backoff")
	}

	// Add a default-deny NetworkPolicy; Changed() must trigger immediate re-plan.
	r.set("networking.k8s.io", "networkpolicies", apis.NetworkPolicy{
		Metadata: apis.ObjectMeta{Namespace: "default", Name: "deny"},
		Spec:     apis.NetworkPolicySpec{PodSelector: apis.LabelSelector{}, PolicyTypes: []string{apis.PolicyTypeIngress}},
	})
	select {
	case <-dp.notify:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for reconcile on store change")
	}

	cancel()
	<-done

	dp.mu.Lock()
	defer dp.mu.Unlock()
	if len(dp.plans) < 2 {
		t.Fatalf("expected at least 2 reconciled plans, got %d", len(dp.plans))
	}
	if dp.plans[0].HasPolicies {
		t.Fatalf("first plan should have HasPolicies=false, got %+v", dp.plans[0])
	}
	last := dp.plans[len(dp.plans)-1]
	if !last.HasPolicies || len(last.Pods) != 1 || !last.Pods[0].IngressIsolated {
		t.Fatalf("latest plan did not reflect new NetworkPolicy: %+v", last)
	}
	logMu.Lock()
	defer logMu.Unlock()
	if len(logged) == 0 {
		t.Fatal("expected transient error to be logged")
	}
}
