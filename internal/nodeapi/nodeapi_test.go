package nodeapi

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"shearwater/internal/apis"
	"shearwater/internal/auth"
	"shearwater/internal/objects"
	"shearwater/internal/pki"
	"shearwater/internal/store"
)

type cluster struct {
	st     *store.Store
	srv    *httptest.Server
	ca     []byte
	hash   string
	joinTk string
	api    *Server
}

func newCluster(t *testing.T) *cluster {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.InitCredentials(); err != nil {
		t.Fatal(err)
	}
	mat, err := pki.Ensure(filepath.Join(dir, "etc"))
	if err != nil {
		t.Fatal(err)
	}
	a := &auth.Authenticator{AdminToken: mat.Token, Credentials: st}
	api := &Server{Store: st, Auth: a, ConfigDir: filepath.Join(dir, "etc"),
		ClusterCIDR: netip.MustParsePrefix("10.244.0.0/16"), ServiceCIDR: netip.MustParsePrefix("10.96.0.0/16"),
		ReservedCIDR: netip.MustParsePrefix("10.244.0.0/24"), Live: NewLiveness(), Logf: t.Logf}
	srv := httptest.NewUnstartedServer(api)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{mat.Server}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	hash, err := pki.CAHash(mat.CACert)
	if err != nil {
		t.Fatal(err)
	}
	tok, id, h, _ := auth.NewToken(auth.KindJoin)
	if err := st.PutCredential(store.Credential{ID: id, Kind: auth.KindJoin, Name: "test", Hash: h, Expires: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	return &cluster{st: st, srv: srv, ca: mat.CACert, hash: hash, joinTk: tok, api: api}
}

func (c *cluster) join(t *testing.T, name, ip string) (*Client, JoinResponse) {
	t.Helper()
	ctx := context.Background()
	ca, err := FetchCA(ctx, c.srv.URL, c.hash)
	if err != nil {
		t.Fatal(err)
	}
	_, csr, err := pki.NewNodeKey(name)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := Join(ctx, c.srv.URL, c.joinTk, ca, JoinRequest{NodeName: name, InternalIP: ip, CSR: string(csr)})
	if err != nil {
		t.Fatal(err)
	}
	cl, err := NewClient(c.srv.URL, resp.NodeToken, ca, netip.MustParsePrefix(resp.PodCIDR))
	if err != nil {
		t.Fatal(err)
	}
	return cl, resp
}

var podsRes = apis.Lookup("", "v1", "pods")

func (c *cluster) createPod(t *testing.T, name, node string, secret string) *apis.Pod {
	t.Helper()
	p := &apis.Pod{Metadata: apis.ObjectMeta{Name: name, Namespace: "default"},
		Spec: apis.PodSpec{NodeName: node, Containers: []apis.Container{{Name: "c", Image: "busybox"}}}}
	if secret != "" {
		p.Spec.Containers[0].EnvFrom = []apis.EnvFromSource{{SecretRef: &apis.SecretEnvSource{Name: secret}}}
	}
	objects.Stamp(podsRes, p)
	if err := c.st.Write(false, func(tx *store.Tx) error {
		_, err := tx.Create(objects.Put(podsRes, p))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestJoinChecksTheCAAndHandsOutRanges(t *testing.T) {
	c := newCluster(t)
	if _, err := FetchCA(context.Background(), c.srv.URL, "sha256:00"); err == nil {
		t.Fatal("a wrong CA hash must stop the join")
	}
	_, r1 := c.join(t, "worker-1", "192.0.2.11")
	_, r2 := c.join(t, "worker-2", "192.0.2.12")
	if r1.PodCIDR != "10.244.1.0/24" || r2.PodCIDR != "10.244.2.0/24" {
		t.Fatalf("pod ranges: %s %s", r1.PodCIDR, r2.PodCIDR)
	}
	// While worker-1 is heard from, nobody can take its name.
	_, csr, _ := pki.NewNodeKey("worker-1")
	_, err := Join(context.Background(), c.srv.URL, c.joinTk, c.ca, JoinRequest{NodeName: "worker-1", InternalIP: "192.0.2.99", CSR: string(csr)})
	var e *Error
	if !errors.As(err, &e) || e.Code != codeExists {
		t.Fatalf("taking a live node's name: %v", err)
	}
	// After it went silent, joining again keeps its range.
	c.api.Live.Forget("worker-1")
	c.api.Live.Now = func() time.Time { return time.Now().Add(time.Hour) }
	_, again := c.join(t, "worker-1", "192.0.2.11")
	if again.PodCIDR != r1.PodCIDR {
		t.Fatalf("rejoin moved the pod range: %s -> %s", r1.PodCIDR, again.PodCIDR)
	}
	rec, err := c.st.Get(store.Key{Resource: "nodes", Name: "worker-2"})
	if err != nil {
		t.Fatal(err)
	}
	var n apis.Node
	json.Unmarshal(rec.Data, &n)
	if n.Metadata.Annotations[InternalIPAnnotation] != "192.0.2.12" || n.Spec.PodCIDR != "10.244.2.0/24" {
		t.Fatalf("node object: %+v", n)
	}
	// The serving certificate names the node and is signed by the cluster CA.
	if _, err := pki.PinnedTLS(c.ca, pki.NodeName+"worker-2"); err != nil {
		t.Fatal(err)
	}
	// A join token is not a node credential.
	bad, _ := NewClient(c.srv.URL, c.joinTk, c.ca, netip.Prefix{})
	if _, _, err := bad.List("", "pods", ""); err == nil {
		t.Fatal("a join token read the cluster state")
	}
}

func TestNodeRestriction(t *testing.T) {
	c := newCluster(t)
	w1, _ := c.join(t, "worker-1", "192.0.2.11")
	c.join(t, "worker-2", "192.0.2.12")
	mine := c.createPod(t, "mine", "worker-1", "app-secret")
	c.createPod(t, "theirs", "worker-2", "other-secret")
	secretRes := apis.Lookup("", "v1", "secrets")
	for _, name := range []string{"app-secret", "other-secret"} {
		s := &apis.Secret{Metadata: apis.ObjectMeta{Name: name, Namespace: "default"}}
		objects.Stamp(secretRes, s)
		c.st.Write(false, func(tx *store.Tx) error { _, err := tx.Create(objects.Put(secretRes, s)); return err })
	}

	recs, _, err := w1.List("", "pods", "")
	if err != nil || len(recs) != 2 {
		t.Fatalf("list pods: %d %v", len(recs), err)
	}
	if _, err := w1.Get(store.Key{Resource: "secrets", Namespace: "default", Name: "app-secret"}); err != nil {
		t.Fatalf("a Secret its own Pod uses: %v", err)
	}
	if _, err := w1.Get(store.Key{Resource: "secrets", Namespace: "default", Name: "other-secret"}); err == nil {
		t.Fatal("read a Secret only another node's Pod uses")
	}
	if _, _, err := w1.List("", "secrets", "default"); err == nil {
		t.Fatal("listed Secrets")
	}

	// Update its own Pod's status in a transaction, with an event.
	err = w1.Transact(func(tx store.Txn) error {
		rec, err := tx.Get(objects.Key(podsRes, "default", "mine"))
		if err != nil {
			return err
		}
		var p apis.Pod
		json.Unmarshal(rec.Data, &p)
		p.Status.Phase = "Running"
		if _, _, err := tx.Update(objects.Put(podsRes, &p), rec.Rev); err != nil {
			return err
		}
		return objects.PodEvent(tx, &p, time.Now(), "start/c", "", "Started", "Started container c", "shearwater-node", "Normal")
	})
	if err != nil {
		t.Fatal(err)
	}
	rec, _ := c.st.Get(objects.Key(podsRes, "default", "mine"))
	var got apis.Pod
	json.Unmarshal(rec.Data, &got)
	if got.Status.Phase != "Running" || got.Metadata.UID != mine.Metadata.UID {
		t.Fatalf("status not written: %+v", got.Status)
	}
	events, _, _ := c.st.List("", "events", "default")
	if len(events) != 1 {
		t.Fatalf("events: %d", len(events))
	}

	// Another node's Pod, rebinding, creating Pods and other kinds are refused.
	for name, fn := range map[string]func(tx store.Txn) error{
		"other node's pod": func(tx store.Txn) error {
			rec, err := tx.Get(objects.Key(podsRes, "default", "theirs"))
			if err != nil {
				return err
			}
			var p apis.Pod
			json.Unmarshal(rec.Data, &p)
			p.Status.Phase = "Failed"
			_, _, err = tx.Update(objects.Put(podsRes, &p), rec.Rev)
			return err
		},
		"rebind own pod": func(tx store.Txn) error {
			rec, _ := tx.Get(objects.Key(podsRes, "default", "mine"))
			var p apis.Pod
			json.Unmarshal(rec.Data, &p)
			p.Spec.NodeName = "worker-2"
			_, _, err := tx.Update(objects.Put(podsRes, &p), rec.Rev)
			return err
		},
		"create pod": func(tx store.Txn) error {
			p := &apis.Pod{Metadata: apis.ObjectMeta{Name: "new", Namespace: "default"}, Spec: apis.PodSpec{NodeName: "worker-1"}}
			objects.Stamp(podsRes, p)
			_, err := tx.Create(objects.Put(podsRes, p))
			return err
		},
		"other node object": func(tx store.Txn) error {
			rec, _ := tx.Get(store.Key{Resource: "nodes", Name: "worker-2"})
			var n apis.Node
			json.Unmarshal(rec.Data, &n)
			n.Metadata.Labels["x"] = "y"
			_, _, err := tx.Update(objects.Put(nodesRes, &n), rec.Rev)
			return err
		},
	} {
		var e *Error
		if err := w1.Transact(fn); !errors.As(err, &e) || e.Code != codeForbidden {
			t.Errorf("%s: %v", name, err)
		}
	}

	// Its own Node: the pod range and the administrator's settings stay.
	err = w1.Transact(func(tx store.Txn) error {
		rec, _ := tx.Get(store.Key{Resource: "nodes", Name: "worker-1"})
		var n apis.Node
		json.Unmarshal(rec.Data, &n)
		n.Spec.Unschedulable = true
		n.Status.Conditions = []apis.NodeCondition{{Type: "Ready", Status: "True"}}
		_, _, err := tx.Update(objects.Put(nodesRes, &n), rec.Rev)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	rec, _ = c.st.Get(store.Key{Resource: "nodes", Name: "worker-1"})
	var n apis.Node
	json.Unmarshal(rec.Data, &n)
	if n.Spec.Unschedulable || len(n.Status.Conditions) != 1 {
		t.Fatalf("node write: unschedulable=%v conditions=%v", n.Spec.Unschedulable, n.Status.Conditions)
	}
}

func TestAddressesStayInTheNodeRange(t *testing.T) {
	c := newCluster(t)
	w1, r1 := c.join(t, "worker-1", "192.0.2.11")
	w2, _ := c.join(t, "worker-2", "192.0.2.12")
	ip, err := w1.AllocIP("pod", "uid-1", slices.Values([]string{"10.244.1.2", "10.244.1.3"}))
	if err != nil || ip != "10.244.1.2" {
		t.Fatalf("alloc: %q %v", ip, err)
	}
	if _, err := w1.AllocIP("pod", "uid-2", slices.Values([]string{"10.244.2.2"})); err == nil {
		t.Fatal("allocated from another node's range")
	}
	if mine, _ := w1.IPs("pod"); len(mine) != 1 {
		t.Fatalf("own allocations: %v", mine)
	}
	if theirs, _ := w2.IPs("pod"); len(theirs) != 0 {
		t.Fatalf("worker-2 sees worker-1's allocations as its own: %v", theirs)
	}
	if err := w2.ReleaseIP("uid-1"); err == nil {
		t.Fatal("released another node's address")
	}
	if err := w1.ReleaseIP("uid-1"); err != nil {
		t.Fatal(err)
	}
	_ = r1
}

func TestChangesAndWait(t *testing.T) {
	c := newCluster(t)
	w1, _ := c.join(t, "worker-1", "192.0.2.11")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w1.Run(ctx)
	// Wait for the first wake-up (the client starts at revision 0).
	select {
	case <-w1.Changed():
	case <-time.After(10 * time.Second):
		t.Fatal("no first wake-up")
	}
	changed := w1.Changed()
	c.createPod(t, "p", "worker-1", "")
	select {
	case <-changed:
	case <-time.After(10 * time.Second):
		t.Fatal("a new Pod did not wake the worker")
	}
	ch, next, err := w1.Changes("", "pods", "", 0, 100)
	if err != nil || len(ch) != 1 || ch[0].Type != "ADDED" || next < ch[0].Rev {
		t.Fatalf("changes: %+v next=%d %v", ch, next, err)
	}
	if last, ok := c.api.Live.LastSeen("worker-1"); !ok || time.Since(last) > time.Minute {
		t.Fatal("the node API did not record the node as alive")
	}
	if _, err := w1.ServiceNetwork(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = http.MethodGet
}
