package ha

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"shearwater/internal/store"
)

func fastTimers(t *testing.T) {
	t.Helper()
	old := []time.Duration{BeatEvery, LeaseFor, FailAfter, VoteAfter, SyncTimeout}
	BeatEvery, LeaseFor, FailAfter, VoteAfter, SyncTimeout = 50*time.Millisecond, 250*time.Millisecond,
		300*time.Millisecond, 350*time.Millisecond, 150*time.Millisecond
	t.Cleanup(func() {
		BeatEvery, LeaseFor, FailAfter, VoteAfter, SyncTimeout = old[0], old[1], old[2], old[3], old[4]
	})
}

func TestWitnessRules(t *testing.T) {
	fastTimers(t)
	now := time.Unix(1000, 0)
	w := &Witness{Path: filepath.Join(t.TempDir(), "w.json"), Token: "t", Now: func() time.Time { return now }}
	call := func(op string, r WitnessRequest) WitnessReply {
		t.Helper()
		reply, err := w.handle(op, r)
		if err != nil {
			t.Fatal(err)
		}
		return reply
	}
	if r := call("vote", WitnessRequest{Epoch: 1, Candidate: "b"}); r.OK {
		t.Fatal("a fresh witness voted for a member it never saw in sync")
	}
	if r := call("heartbeat", WitnessRequest{Epoch: 1, Leader: "a"}); !r.OK || r.State.Leader != "a" {
		t.Fatalf("first heartbeat: %+v", r)
	}
	if r := call("resync", WitnessRequest{Epoch: 1, Leader: "b", Member: "b"}); r.OK {
		t.Fatal("a non-leader changed the sync set")
	}
	if r := call("resync", WitnessRequest{Epoch: 1, Leader: "a", Member: "b"}); !r.OK || len(r.State.InSync) != 2 {
		t.Fatalf("resync: %+v", r)
	}
	// The leader was just heard: no vote for the standby yet.
	now = now.Add(VoteAfter / 2)
	if r := call("vote", WitnessRequest{Epoch: 2, Candidate: "b"}); r.OK || !strings.Contains(r.Reason, "heard") {
		t.Fatalf("vote while the leader is alive: %+v", r)
	}
	now = now.Add(VoteAfter)
	if r := call("vote", WitnessRequest{Epoch: 2, Candidate: "b"}); !r.OK || r.State.Leader != "b" || len(r.State.InSync) != 1 {
		t.Fatalf("vote after silence: %+v", r)
	}
	// One vote per epoch, and the old leader's epoch is over.
	if r := call("vote", WitnessRequest{Epoch: 2, Candidate: "a"}); r.OK {
		t.Fatal("two votes in one epoch")
	}
	if r := call("heartbeat", WitnessRequest{Epoch: 1, Leader: "a"}); r.OK {
		t.Fatal("the old leader's heartbeat was accepted")
	}
	if r := call("vote", WitnessRequest{Epoch: 3, Candidate: "a"}); r.OK {
		t.Fatal("an out-of-sync member was elected")
	}
	// The state survives a restart, and a restarted witness waits before voting.
	w2 := &Witness{Path: w.Path, Token: "t", Now: func() time.Time { return now }}
	st, err := w2.State()
	if err != nil || st.Epoch != 2 || st.Leader != "b" {
		t.Fatalf("after restart: %+v %v", st, err)
	}
	reply, _ := w2.handle("resync", WitnessRequest{Epoch: 2, Leader: "b", Member: "a"})
	if !reply.OK {
		t.Fatal(reply.Reason)
	}
	if reply, _ := w2.handle("vote", WitnessRequest{Epoch: 3, Candidate: "a"}); reply.OK {
		t.Fatal("a restarted witness voted before hearing the leader")
	}
	// The leader of the current epoch itself may run again at once.
	if reply, _ := w2.handle("vote", WitnessRequest{Epoch: 3, Candidate: "b"}); !reply.OK {
		t.Fatalf("the restarting leader: %s", reply.Reason)
	}
}

// node is one control plane in the test: a store and, when leading, an HTTP
// server for its standby.
type node struct {
	name, dir string
	st        *store.Store
	leader    *Leader
	srv       *httptest.Server
	ln        net.Listener
	addr      string
	stop      context.CancelFunc // the leader's loops
}

type cluster struct {
	t       *testing.T
	token   string
	witness *httptest.Server
	nodes   map[string]*node
	mu      sync.Mutex
	logs    []string
}

func (c *cluster) logf(format string, a ...any) {
	c.mu.Lock()
	c.logs = append(c.logs, fmt.Sprintf(format, a...))
	c.mu.Unlock()
}

func newCluster(t *testing.T) *cluster {
	c := &cluster{t: t, token: NewToken(), nodes: map[string]*node{}}
	w := &Witness{Path: filepath.Join(t.TempDir(), "witness-state.json"), Token: c.token}
	c.witness = httptest.NewServer(w)
	t.Cleanup(c.witness.Close)
	for _, name := range []string{"a", "b"} {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		c.nodes[name] = &node{name: name, dir: t.TempDir(), ln: ln, addr: ln.Addr().String()}
	}
	return c
}

func (c *cluster) config(self, role string, epoch int64) Config {
	return Config{Self: self, Role: role, Epoch: epoch, Token: c.token, Witness: c.witness.URL,
		Members: []Member{{"a", "http://" + c.nodes["a"].addr}, {"b", "http://" + c.nodes["b"].addr}}}
}

func (c *cluster) witnessClient() *WitnessClient {
	return &WitnessClient{URL: c.witness.URL, Token: c.token, Client: http.DefaultClient}
}

// lead makes n the leader at epoch, serving on its listener.
func (c *cluster) lead(n *node, epoch int64, stepped chan<- string) {
	c.t.Helper()
	var err error
	if n.st == nil {
		if n.st, err = store.Open(filepath.Join(n.dir, "state.db")); err != nil {
			c.t.Fatal(err)
		}
	}
	n.leader = &Leader{Store: n.st, Config: c.config(n.name, RoleLeader, epoch), DataDir: n.dir, Witness: c.witnessClient(),
		Logf: c.logf, StepDown: func(r string) { stepped <- n.name + ": " + r }}
	ctx, cancel := context.WithCancel(context.Background())
	c.t.Cleanup(cancel)
	n.stop = cancel
	if err := n.leader.Start(ctx); err != nil {
		c.t.Fatal(err)
	}
	if n.ln == nil {
		if n.ln, err = net.Listen("tcp", n.addr); err != nil {
			c.t.Fatal(err)
		}
	}
	n.srv = &httptest.Server{Listener: n.ln, Config: &http.Server{Handler: n.leader}}
	n.srv.Start()
	n.ln = nil
}

// kill stops a leader the hard way: its server and store go, nothing is said.
func (c *cluster) kill(n *node) {
	n.stop()
	n.srv.CloseClientConnections()
	n.srv.Close()
	n.st.Close()
	n.st, n.leader = nil, nil
}

func put(st *store.Store, name string) error {
	return st.Write(false, func(tx *store.Tx) error {
		_, err := tx.Create(store.Put{Key: store.Key{Resource: "pods", Namespace: "default", Name: name}, UID: "uid-" + name,
			Encode: func(rev int64) ([]byte, error) { return []byte(fmt.Sprintf(`{"name":%q,"rv":%d}`, name, rev)), nil }})
		return err
	})
}

func waitFor(t *testing.T, what string, d time.Duration, ok func() bool) {
	t.Helper()
	end := time.Now().Add(d)
	for !ok() {
		if time.Now().After(end) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestFailoverKeepsConfirmedWrites(t *testing.T) {
	fastTimers(t)
	c := newCluster(t)
	a, b := c.nodes["a"], c.nodes["b"]
	stepped := make(chan string, 4)
	c.lead(a, 1, stepped)
	for i := 0; i < 3; i++ { // written while a is alone
		if err := put(a.st, fmt.Sprintf("solo-%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	promoted := make(chan int64, 1)
	sb := &Standby{Config: c.config("b", RoleStandby, 1), ConfigDir: b.dir, DataDir: b.dir, Client: http.DefaultClient,
		Witness: c.witnessClient(), Logf: c.logf, Promote: func(e int64) error { promoted <- e; return errors.New("promoted") }}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- sb.Run(ctx) }()

	// The standby catches up; the next write brings it back in sync and
	// from then on every confirmed write is on b before a commits it.
	waitFor(t, "the standby to catch up", 5*time.Second, func() bool { return sb.Status().Seq == a.leader.Status().Seq && sb.Status().Seq > 0 })
	var confirmed []string
	for i := 0; i < 20; i++ {
		name := fmt.Sprintf("sync-%d", i)
		if err := put(a.st, name); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		confirmed = append(confirmed, name)
	}
	if st := a.leader.Status(); !st.InSync || st.Confirmed != st.Seq {
		t.Fatalf("leader status: %+v\nlog:\n%s", st, strings.Join(c.logs, "\n"))
	}
	// What a serves is already on b.
	if bs := sb.Status(); bs.Seq != a.leader.Status().Seq {
		t.Fatalf("standby at %d, leader at %d", bs.Seq, a.leader.Status().Seq)
	}

	start := time.Now()
	c.kill(a)
	var epoch int64
	select {
	case epoch = <-promoted:
	case <-time.After(5 * time.Second):
		t.Fatalf("no takeover; log:\n%s", strings.Join(c.logs, "\n"))
	}
	took := time.Since(start)
	if epoch != 2 || took < FailAfter {
		t.Fatalf("epoch %d after %s", epoch, took)
	}
	if err := <-runDone; err == nil || !strings.Contains(err.Error(), "promoted") {
		t.Fatalf("Run returned %v", err)
	}
	// b becomes the leader on its replica: every confirmed write is there.
	c.lead(b, epoch, stepped)
	for _, name := range append([]string{"solo-0", "solo-1", "solo-2"}, confirmed...) {
		if _, err := b.st.Get(store.Key{Resource: "pods", Namespace: "default", Name: name}); err != nil {
			t.Fatalf("%s lost after failover: %v", name, err)
		}
	}
	if err := put(b.st, "after-failover"); err != nil {
		t.Fatalf("the new leader cannot write: %v", err)
	}

	// The old leader comes back as a standby and follows b from a snapshot.
	sa := &Standby{Config: c.config("a", RoleStandby, 1), ConfigDir: a.dir, DataDir: a.dir, Client: http.DefaultClient,
		Witness: c.witnessClient(), Logf: c.logf, Promote: func(int64) error { return errors.New("a must not take over") }}
	go sa.Run(ctx)
	waitFor(t, "the old leader to follow", 5*time.Second, func() bool { return sa.Status().Seq == b.leader.Status().Seq })
	if err := put(b.st, "back-in-sync"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "sync with the old leader", 5*time.Second, func() bool { st := b.leader.Status(); return st.InSync && st.Confirmed == st.Seq })
	entries, err := os.ReadDir(a.dir)
	if err != nil {
		t.Fatal(err)
	}
	kept := false
	for _, e := range entries {
		kept = kept || strings.HasPrefix(e.Name(), "state.db.replaced-")
	}
	if !kept {
		t.Fatal("the old leader's file was not kept aside")
	}
	select {
	case s := <-stepped:
		t.Fatalf("unexpected step-down: %s", s)
	default:
	}
}

func TestLeaderStepsDownWhenAloneInSync(t *testing.T) {
	fastTimers(t)
	c := newCluster(t)
	a, b := c.nodes["a"], c.nodes["b"]
	stepped := make(chan string, 4)
	c.lead(a, 1, stepped)
	sb := &Standby{Config: c.config("b", RoleStandby, 1), ConfigDir: b.dir, DataDir: b.dir, Client: http.DefaultClient,
		Witness: c.witnessClient(), Logf: c.logf, Promote: func(int64) error { return errors.New("stop") }}
	ctx, cancel := context.WithCancel(context.Background())
	go sb.Run(ctx)
	if err := put(a.st, "x"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "sync", 5*time.Second, func() bool {
		put(a.st, fmt.Sprintf("y-%d", time.Now().UnixNano()))
		return a.leader.Status().InSync
	})
	// The standby stops and the witness goes away: a write cannot be
	// confirmed by anyone, so it fails and a stops leading.
	cancel()
	c.witness.CloseClientConnections()
	c.witness.Close()
	err := put(a.st, "unconfirmed")
	if !errors.Is(err, store.ErrUnavailable) {
		t.Fatalf("write without standby and witness: %v", err)
	}
	select {
	case <-stepped:
	case <-time.After(5 * time.Second):
		t.Fatal("no step-down")
	}
	if _, err := a.st.Get(store.Key{Resource: "pods", Namespace: "default", Name: "unconfirmed"}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the unconfirmed write is on the leader: %v", err)
	}
	if err := put(a.st, "after"); !errors.Is(err, store.ErrUnavailable) {
		t.Fatalf("a fenced leader wrote: %v", err)
	}
}

func TestStandbyGoesSoloWithTheWitness(t *testing.T) {
	fastTimers(t)
	c := newCluster(t)
	a, b := c.nodes["a"], c.nodes["b"]
	stepped := make(chan string, 4)
	c.lead(a, 1, stepped)
	sb := &Standby{Config: c.config("b", RoleStandby, 1), ConfigDir: b.dir, DataDir: b.dir, Client: http.DefaultClient,
		Witness: c.witnessClient(), Logf: c.logf, Promote: func(int64) error { return errors.New("stop") }}
	ctx, cancel := context.WithCancel(context.Background())
	go sb.Run(ctx)
	waitFor(t, "sync", 5*time.Second, func() bool {
		put(a.st, fmt.Sprintf("y-%d", time.Now().UnixNano()))
		return a.leader.Status().InSync
	})
	cancel() // the standby dies; the witness is still there
	if err := put(a.st, "alone"); err != nil {
		t.Fatalf("write after the standby died: %v", err)
	}
	if a.leader.Status().InSync {
		t.Fatal("still waiting for a dead standby")
	}
	st, err := c.witnessClient().Call(context.Background(), "state", WitnessRequest{})
	if err != nil || len(st.State.InSync) != 1 || st.State.InSync[0] != "a" {
		t.Fatalf("witness: %+v %v", st, err)
	}
	// Alone and out of sync, the leader keeps writing with no lease to lose.
	time.Sleep(2 * LeaseFor)
	if err := put(a.st, "still-alone"); err != nil {
		t.Fatal(err)
	}
	select {
	case s := <-stepped:
		t.Fatalf("stepped down: %s", s)
	default:
	}
}

func TestForward(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "leader") }))
	defer up.Close()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	target := strings.TrimPrefix(up.URL, "http://")
	go Forward(ctx, ln, func() string { return target }, nil)
	res, err := http.Get("http://" + ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 6)
	res.Body.Read(b)
	res.Body.Close()
	if string(b) != "leader" {
		t.Fatalf("%q", b)
	}
}

func TestConfigSaveLoad(t *testing.T) {
	dir := t.TempDir()
	c := Config{Self: "a", Role: RoleLeader, Epoch: 3, Token: "x", Members: []Member{{"a", "https://10.0.0.1:6443"}, {"b", "https://10.0.0.2:6443"}}}
	if err := c.Save(dir); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir)
	if err != nil || got.Epoch != 3 || got.Members[1].Name != "b" {
		t.Fatalf("%+v %v", got, err)
	}
	if p, ok := got.Peer(); !ok || p.Name != "b" {
		t.Fatal(p)
	}
	info, err := os.Stat(filepath.Join(dir, ConfigFile))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v %v", info.Mode(), err)
	}
	if err := (Config{Self: "z", Role: RoleLeader, Token: "x", Members: c.Members}).Save(dir); err == nil {
		t.Fatal("saved a config whose self is not a member")
	}
}
