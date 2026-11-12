package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"shearwater/internal/apis"
	"shearwater/internal/controller"
	"shearwater/internal/store"
)

const firstServiceIP = "10.96.0.1"

func serviceBody(name, ip string) string {
	return fmt.Sprintf(`{"metadata":{"name":%q},"spec":{"clusterIP":%q,"ports":[{"port":80}]}}`, name, ip)
}

func checkServiceIP(t *testing.T, r reply, want string) string {
	t.Helper()
	uid, _ := r.at("metadata", "uid").(string)
	if uid == "" || r.at("spec", "clusterIP") != want {
		t.Fatalf("want Service with UID and ClusterIP %q, got %s", want, r.raw)
	}
	return uid
}

func checkServiceIPOwner(t *testing.T, st *store.Store, ip, want string) {
	t.Helper()
	ips, err := st.IPs("service")
	var owner string
	for uid, address := range ips {
		if address == ip {
			owner = uid
			break
		}
	}
	if err != nil || owner != want {
		t.Fatalf("owner of %s = %q, %v, want %q", ip, owner, err, want)
	}
}

func TestServiceIPAllocationInAPIOnlyMode(t *testing.T) {
	c := newClient(t)
	st := c.srv.Store
	before := c.rev()
	systemServices := "/api/v1/namespaces/kube-system/services"
	explicit := serviceBody("explicit", firstServiceIP)
	firstUID := checkServiceIP(t, c.want(c.do("POST", systemServices, explicit), 201), firstServiceIP)
	checkServiceIPOwner(t, st, firstServiceIP, firstUID)

	// The same name still gets AlreadyExists. A different name in another
	// namespace cannot claim the same IP because the allocator is global.
	c.want(c.do("POST", systemServices, explicit), 409)
	r := c.want(c.do("POST", services, serviceBody("conflict", firstServiceIP)), 422)
	if r.at("reason") != "Invalid" || !strings.Contains(r.raw, `"field":"spec.clusterIP"`) ||
		!strings.Contains(r.raw, "already allocated") || c.rev() != before+1 {
		t.Fatalf("an occupied IP must fail field validation with no revision, got %s", r.raw)
	}
	for _, ip := range []string{"bad-ip", "10.244.0.2", "10.96.0.0", "2001:db8::1"} {
		r := c.want(c.do("POST", services, serviceBody("invalid", ip)), 422)
		if !strings.Contains(r.raw, `"field":"spec.clusterIP"`) {
			t.Fatalf("invalid ClusterIP %q must have a field error, got %s", ip, r.raw)
		}
	}

	const secondIP = "10.96.0.2"
	r = c.want(c.do("POST", services, `{"metadata":{"name":"auto"},"spec":{"ports":[{"port":80}]}}`), 201)
	autoUID := checkServiceIP(t, r, secondIP)
	checkServiceIPOwner(t, st, secondIP, autoUID)
	checkServiceIP(t, c.want(c.do("GET", services+"/auto", ""), 200), secondIP)
	r = c.want(c.do("POST", services, serviceBody("headless", "None")), 201)
	checkServiceIP(t, r, "None")
	if !strings.Contains(r.header.Get("Warning"), "spec.clusterIP") {
		t.Fatalf("headless Service should still carry a stored-only warning, got %v", r.header)
	}
	checkServiceIPOwner(t, st, "None", "")

	changes, _, err := st.Changes("", "services", "", before, 10)
	if err != nil || len(changes) != 3 {
		t.Fatalf("Service changes = %v, %v, want exactly three successful creates", changes, err)
	}
	for i, ip := range []string{firstServiceIP, secondIP, "None"} {
		var svc apis.Service
		change := changes[i]
		if err := json.Unmarshal(change.Data, &svc); err != nil || change.Type != "ADDED" || svc.Spec.ClusterIP != ip {
			t.Fatalf("change %d must publish ADDED with ClusterIP %q, got %s %s (%v)", i, ip, change.Type, change.Data, err)
		}
	}
	rev := c.rev()
	c.want(c.do("PUT", services+"/auto", serviceBody("auto", "10.96.0.99")), 422)
	c.want(c.patch(services+"/auto", mergeType, `{"spec":{"clusterIP":null}}`), 422)
	if c.rev() != rev {
		t.Fatal("an attempt to change a ClusterIP advanced the revision")
	}
	checkServiceIPOwner(t, st, secondIP, autoUID)
}

func TestServiceIPDryRunAndImmediateDelete(t *testing.T) {
	c := newClient(t)
	st := c.srv.Store
	wake, before := st.Changed(), c.rev()
	for _, requested := range []string{"", firstServiceIP} {
		r := c.want(c.do("POST", services+"?dryRun=All", serviceBody("preview", requested)), 201)
		checkServiceIP(t, r, firstServiceIP)
		c.want(c.do("GET", services+"/preview", ""), 404)
		checkServiceIPOwner(t, st, firstServiceIP, "")
		if c.rev() != before || st.Changed() != wake {
			t.Fatal("dry-run create advanced the revision or woke watchers")
		}
	}
	oldUID := checkServiceIP(t, c.want(c.do("POST", services, serviceBody("live", "")), 201), firstServiceIP)
	wake, before = st.Changed(), c.rev()
	c.want(c.do("DELETE", services+"/live?dryRun=All", ""), 200)
	c.want(c.do("GET", services+"/live", ""), 200)
	checkServiceIPOwner(t, st, firstServiceIP, oldUID)
	if c.rev() != before || st.Changed() != wake {
		t.Fatal("dry-run delete advanced the revision or woke watchers")
	}
	c.want(c.do("DELETE", services+"/live", ""), 200)
	checkServiceIPOwner(t, st, firstServiceIP, "")

	// A stale listener identified by the old UID can distinguish even this
	// identical Service name and IP from the deleted object.
	newUID := checkServiceIP(t, c.want(c.do("POST", services, serviceBody("live", firstServiceIP)), 201), firstServiceIP)
	if newUID == oldUID {
		t.Fatal("recreating a Service must issue a different UID")
	}
	c.want(c.do("DELETE", services+"/live", fmt.Sprintf(`{"preconditions":{"uid":%q}}`, oldUID)), 409)
	checkServiceIPOwner(t, st, firstServiceIP, newUID)
}

func TestServiceIPSurvivesPendingDelete(t *testing.T) {
	for _, path := range []string{"user finalizer", "garbage collector"} {
		t.Run(path, func(t *testing.T) {
			c := newClient(t)
			st := c.srv.Store
			body, options := serviceBody("held", ""), ""
			if path == "user finalizer" {
				body = strings.Replace(body, `"name":"held"`, `"name":"held","finalizers":["example.com/hold"]`, 1)
			} else {
				c.srv.NodeName = "test-node"
				options = `{"propagationPolicy":"Foreground"}`
			}
			uid := checkServiceIP(t, c.want(c.do("POST", services, body), 201), firstServiceIP)
			r := c.want(c.do("DELETE", services+"/held", options), 200)
			if r.at("metadata", "deletionTimestamp") == nil {
				t.Fatalf("pending delete must mark the object, got %s", r.raw)
			}
			checkServiceIPOwner(t, st, firstServiceIP, uid)
			c.want(c.do("POST", services, serviceBody("reuse", firstServiceIP)), 422)
			if path == "user finalizer" {
				const clear = `{"metadata":{"finalizers":[]}}`
				c.want(c.patch(services+"/held?dryRun=All", mergeType, clear), 200)
				checkServiceIPOwner(t, st, firstServiceIP, uid)
				c.want(c.patch(services+"/held", mergeType, clear), 200)
			} else {
				m := controller.Manager{Store: st, NodeName: c.srv.NodeName}
				if err := m.Sync(); err != nil {
					t.Fatal(err)
				}
			}
			c.want(c.do("GET", services+"/held", ""), 404)
			checkServiceIPOwner(t, st, firstServiceIP, "")
			checkServiceIP(t, c.want(c.do("POST", services, serviceBody("reuse", firstServiceIP)), 201), firstServiceIP)
		})
	}
}

func TestServiceIPExhaustion(t *testing.T) {
	// Use two allocatable addresses so the error and recovery exercise the
	// full API path without creating tens of thousands of Services.
	cidr := apis.ServiceCIDR
	t.Cleanup(func() { apis.ServiceCIDR = cidr })
	apis.ServiceCIDR = netip.MustParsePrefix("10.96.0.0/30")
	c := newClient(t)
	checkServiceIP(t, c.want(c.do("POST", services, serviceBody("first", "")), 201), firstServiceIP)
	checkServiceIP(t, c.want(c.do("POST", services, serviceBody("second", "")), 201), "10.96.0.2")
	before := c.rev()
	r := c.want(c.do("POST", services, serviceBody("third", "")), 503)
	if r.at("reason") != "ServiceUnavailable" || !strings.Contains(r.raw, "IP_EXHAUSTED") || c.rev() != before {
		t.Fatalf("exhaustion must report pool capacity without an API write, got %s", r.raw)
	}
	c.want(c.do("DELETE", services+"/first", ""), 200)
	checkServiceIP(t, c.want(c.do("POST", services, serviceBody("third", "")), 201), firstServiceIP)
}

func TestConcurrentServiceIPCreates(t *testing.T) {
	// Independent Store handles have independent Go mutexes. The SQLite
	// writer transaction must still serialize both the reservation and create.
	path := filepath.Join(t.TempDir(), "state.db")
	var clients []*client
	for range 2 {
		st, err := store.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
		clients = append(clients, &client{t: t, srv: &Server{Store: st, Token: "secret"}})
	}
	if err := clients[0].srv.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	const n = 10
	const explicit = "10.96.1.50"
	replies, start := make(chan reply, n), make(chan struct{})
	for i := range n {
		go func() {
			request := ""
			if i < 2 {
				request = explicit
			}
			<-start
			replies <- clients[i%len(clients)].do("POST", services, serviceBody(fmt.Sprintf("svc-%d", i), request))
		}()
	}
	close(start)
	responses := make([]reply, 0, n)
	for range n {
		responses = append(responses, <-replies)
	}
	want := map[string]string{}
	taken := map[string]bool{}
	conflicts := 0
	for _, r := range responses {
		if r.code == 422 {
			conflicts++
			if !strings.Contains(r.raw, explicit) {
				t.Fatalf("only explicit IP requests can conflict, got %s", r.raw)
			}
			continue
		}
		clients[0].want(r, 201)
		ip, _ := r.at("spec", "clusterIP").(string)
		uid := checkServiceIP(t, r, ip)
		if ip == "" || taken[ip] {
			t.Fatalf("successful create got a missing or duplicate IP, %s", r.raw)
		}
		taken[ip], want[uid] = true, ip
	}
	if conflicts != 1 || !taken[explicit] {
		t.Fatalf("expected one winner and one conflict for %s, got %v (%d conflicts)", explicit, taken, conflicts)
	}
	got, err := clients[0].srv.Store.IPs("service")
	if err != nil || !maps.Equal(got, want) {
		t.Fatalf("committed reservations = %v, %v, want %v", got, err, want)
	}
}

// legacyService writes the same object M4 stored, before ClusterIP allocation.
func legacyService(c *client, ns, name, ip string) store.Record {
	c.t.Helper()
	res := apis.Lookup("", "v1", "services")
	svc := &apis.Service{
		Metadata: apis.ObjectMeta{Name: name, Namespace: ns},
		Spec:     apis.ServiceSpec{ClusterIP: ip, Ports: []apis.ServicePort{{Port: 80}}},
	}
	stamp(res, svc)
	var rec store.Record
	if err := c.srv.Store.Write(false, func(tx *store.Tx) (err error) {
		rec, err = tx.Create(put(res, svc))
		return err
	}); err != nil {
		c.t.Fatal(err)
	}
	return rec
}

func TestReconcileServiceIPsReservesLegacyExplicitBeforeBlank(t *testing.T) {
	c := newClient(t)
	st := c.srv.Store
	blank := legacyService(c, "default", "a-unassigned", "")
	headless := legacyService(c, "default", "headless", "None")
	explicit := legacyService(c, "kube-system", "z-explicit", firstServiceIP)
	broadcast := legacyService(c, "kube-system", "z-upper-bound", "10.96.255.255")
	before := c.rev()
	if err := ReconcileServiceIPs(st); err != nil {
		t.Fatal(err)
	}
	checkServiceIPOwner(t, st, firstServiceIP, explicit.UID)
	checkServiceIPOwner(t, st, "10.96.0.2", blank.UID)
	checkServiceIPOwner(t, st, "10.96.255.255", broadcast.UID)
	checkServiceIPOwner(t, st, "None", "")
	r := c.want(c.do("GET", services+"/a-unassigned", ""), 200)
	if uid := checkServiceIP(t, r, "10.96.0.2"); uid != blank.UID || r.at("metadata", "generation") != float64(blank.Generation+1) {
		t.Fatalf("backfill must preserve UID and increment generation, got %s", r.raw)
	}
	for _, rec := range []store.Record{explicit, headless, broadcast} {
		got, err := st.Get(rec.Key)
		if err != nil || got.Rev != rec.Rev || string(got.Data) != string(rec.Data) {
			t.Fatalf("startup rewrote %s: %s, %v", rec.Key, got.Data, err)
		}
	}
	changes, _, err := st.Changes("", "services", "", before, 10)
	if err != nil || len(changes) != 1 || changes[0].Key != blank.Key || changes[0].Type != "MODIFIED" {
		t.Fatalf("upgrade must publish only the modified blank Service, got %v, %v", changes, err)
	}
	wake := st.Changed()
	if err := ReconcileServiceIPs(st); err != nil || c.rev() != before+1 || st.Changed() != wake {
		t.Fatalf("reconciling twice must be a no-op, got %v", err)
	}
}

func TestReconcileServiceIPsRejectsDuplicateLegacyExplicit(t *testing.T) {
	c := newClient(t)
	st := c.srv.Store
	legacyService(c, "default", "a-free", "10.96.0.7")
	a := legacyService(c, "default", "b-conflict", firstServiceIP)
	b := legacyService(c, "kube-system", "c-conflict", firstServiceIP)
	legacyService(c, "default", "z-blank", "")
	wake, before := st.Changed(), c.rev()
	err := ReconcileServiceIPs(st)
	if err == nil || !strings.Contains(err.Error(), firstServiceIP) ||
		!strings.Contains(err.Error(), a.Key.String()) || !strings.Contains(err.Error(), b.Key.String()) {
		t.Fatalf("duplicate legacy IP error must identify both Services and the address, got %v", err)
	}
	ips, err := st.IPs("service")
	if err != nil || len(ips) != 0 || c.rev() != before || st.Changed() != wake {
		t.Fatalf("duplicate legacy IPs must leave the database untouched, got IPs %v and error %v", ips, err)
	}
}

func TestReconcileServiceIPsRollsBackOnExistingReservation(t *testing.T) {
	c := newClient(t)
	st := c.srv.Store
	legacyService(c, "default", "a-free", firstServiceIP)
	legacyService(c, "default", "b-occupied", "10.96.0.2")
	if _, err := st.AllocIP("pod", "test-pod", slices.Values([]string{"10.96.0.2"})); err != nil {
		t.Fatal(err)
	}
	before := c.rev()
	if err := ReconcileServiceIPs(st); !errors.Is(err, store.ErrIPAllocated) {
		t.Fatalf("cannot reserve an IP already owned by a Pod, got %v", err)
	}
	if ips, err := st.IPs("service"); err != nil || len(ips) != 0 || c.rev() != before {
		t.Fatalf("reservation of the first Service must roll back, got %v, %v", ips, err)
	}
}
