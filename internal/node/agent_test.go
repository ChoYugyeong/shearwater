package node

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"shearwater/internal/apis"
	"shearwater/internal/objects"
	"shearwater/internal/runtime"
	"shearwater/internal/store"
)

// The tests drive SyncPod by hand with a fake runtime and a clock they move,
// so nothing here needs root, containerd or real time (design 7).

type clock struct{ t time.Time }

func (c *clock) Now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

type env struct {
	t     *testing.T
	st    *store.Store
	fake  *runtime.Fake
	clk   *clock
	agent *Agent
}

func newEnv(t *testing.T) *env {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &env{t: t, st: st, fake: runtime.NewFake(), clk: &clock{t: time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)}}
	e.fake.Now = e.clk.Now
	e.agent = e.newAgent()
	return e
}

// newAgent is a fresh agent on the same store and runtime: sw server restarted.
func (e *env) newAgent() *Agent {
	dir := e.t.TempDir()
	return &Agent{
		Store: e.st, Runtime: e.fake, Net: e.fake, NodeName: "n1", DataDir: filepath.Join(dir, "lib"), LogDir: filepath.Join(dir, "log"),
		PodCIDR: netip.MustParsePrefix("10.244.0.0/24"), Version: "test", Now: e.clk.Now, Logf: e.t.Logf,
	}
}

func (e *env) createPod(name string, mutate func(*apis.Pod)) *apis.Pod {
	e.t.Helper()
	pod := &apis.Pod{}
	pod.Metadata.Name, pod.Metadata.Namespace = name, "default"
	pod.Spec.NodeName = "n1"
	pod.Spec.Containers = []apis.Container{{Name: "main", Image: "busybox:1.36"}}
	if mutate != nil {
		mutate(pod)
	}
	objects.Stamp(podsRes, pod)
	if errs := apis.ValidatePod(pod); len(errs) > 0 {
		e.t.Fatalf("test pod is invalid: %v", errs)
	}
	if err := e.st.Write(false, func(tx *store.Tx) error {
		_, err := tx.Create(objects.Put(podsRes, pod))
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	return pod
}

func (e *env) get(pod *apis.Pod) *apis.Pod {
	e.t.Helper()
	rec, err := e.st.Get(objects.Key(podsRes, pod.Metadata.Namespace, pod.Metadata.Name))
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		e.t.Fatal(err)
	}
	out := &apis.Pod{}
	if err := json.Unmarshal(rec.Data, out); err != nil {
		e.t.Fatal(err)
	}
	return out
}

func (e *env) sync(a *Agent, pod *apis.Pod) time.Duration {
	e.t.Helper()
	retry, err := a.SyncPod(context.Background(), pod.Metadata.UID)
	if err != nil {
		e.t.Fatalf("SyncPod: %v", err)
	}
	return retry
}

// markDeleted does what the API server does for a graceful delete.
func (e *env) markDeleted(pod *apis.Pod, grace int64) {
	e.t.Helper()
	err := e.st.Write(false, func(tx *store.Tx) error {
		key := objects.Key(podsRes, pod.Metadata.Namespace, pod.Metadata.Name)
		rec, err := tx.Get(key)
		if err != nil {
			return err
		}
		cur := &apis.Pod{}
		json.Unmarshal(rec.Data, cur)
		cur.Metadata.DeletionTimestamp = &apis.Time{Time: e.clk.Now().Add(time.Duration(grace) * time.Second)}
		cur.Metadata.DeletionGracePeriodSeconds = &grace
		_, _, err = tx.Update(objects.Put(podsRes, cur), rec.Rev)
		return err
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

func id(pod *apis.Pod, attempt int) string {
	return runtime.ContainerID(pod.Metadata.UID, "main", attempt)
}

func status(t *testing.T, pod *apis.Pod) *apis.ContainerStatus {
	t.Helper()
	if pod == nil || len(pod.Status.ContainerStatuses) != 1 {
		t.Fatalf("want one container status, got %+v", pod)
	}
	return &pod.Status.ContainerStatuses[0]
}

func TestPodStarts(t *testing.T) {
	e := newEnv(t)
	pod := e.createPod("web", func(p *apis.Pod) {
		p.Spec.Volumes = []apis.Volume{{Name: "scratch", EmptyDir: &apis.EmptyDirVolumeSource{}}, {Name: "host", HostPath: &apis.HostPathVolumeSource{Path: "/srv/data"}}}
		c := &p.Spec.Containers[0]
		c.Env = []apis.EnvVar{{Name: "A", Value: "1"}}
		c.VolumeMounts = []apis.VolumeMount{{Name: "scratch", MountPath: "/scratch"}, {Name: "host", MountPath: "/data", ReadOnly: true}}
		c.Resources.Limits = map[string]apis.Quantity{"cpu": "250m", "memory": "64Mi"}
	})
	if retry := e.sync(e.agent, pod); retry != 0 {
		t.Errorf("a running pod needs no retry, got %v", retry)
	}
	got := e.get(pod)
	if got.Status.Phase != "Running" || got.Status.PodIP != "10.244.0.2" || len(got.Status.PodIPs) != 1 {
		t.Fatalf("status: phase %q ip %q", got.Status.Phase, got.Status.PodIP)
	}
	cs := status(t, got)
	if cs.State.Running == nil || !cs.Ready || cs.RestartCount != 0 || cs.ContainerID != "containerd://"+id(pod, 0) ||
		cs.ImageID != "docker.io/library/busybox@sha256:fake" {
		t.Errorf("container status: %+v", cs)
	}
	for _, c := range got.Status.Conditions {
		if c.Status != "True" {
			t.Errorf("condition %s is %s", c.Type, c.Status)
		}
	}
	if ip, ok := e.fake.NetIP(pod.Metadata.UID); !ok || ip.String() != "10.244.0.2" {
		t.Errorf("network: %v %v", ip, ok)
	}
	cfg, ok := e.fake.Config(id(pod, 0))
	if !ok {
		t.Fatal("container was not started")
	}
	if cfg.Image != "docker.io/library/busybox:1.36" || cfg.NetNS != e.fake.Path(pod.Metadata.UID) || cfg.Hostname != "web" ||
		cfg.CPUQuota != 25000 || cfg.MemoryLimit != 64<<20 || len(cfg.Env) != 1 || cfg.Env[0] != "A=1" {
		t.Errorf("config: %+v", cfg)
	}
	if !strings.HasSuffix(cfg.LogPath, "/pods/default_web_"+pod.Metadata.UID+"/main/0.log") {
		t.Errorf("log path: %s", cfg.LogPath)
	}
	var dests []string
	for _, m := range cfg.Mounts {
		dests = append(dests, m.Destination)
		if m.Destination == "/data" && (m.Source != "/srv/data" || !m.ReadOnly) {
			t.Errorf("hostPath mount: %+v", m)
		}
		if m.Destination == "/scratch" && !strings.HasSuffix(m.Source, "/pods/"+pod.Metadata.UID+"/volumes/scratch") {
			t.Errorf("emptyDir mount: %+v", m)
		}
	}
	if strings.Join(dests, " ") != "/etc/hosts /etc/hostname /etc/resolv.conf /scratch /data" {
		t.Errorf("mounts: %v", dests)
	}

	// Looking again changes nothing: no write, no revision.
	before, _ := e.st.Rev()
	e.sync(e.agent, pod)
	if after, _ := e.st.Rev(); after != before {
		t.Errorf("an unchanged pod was written again: revision %d -> %d", before, after)
	}
}

func TestOtherNodesPodsAreIgnored(t *testing.T) {
	e := newEnv(t)
	pod := e.createPod("elsewhere", func(p *apis.Pod) { p.Spec.NodeName = "n2" })
	unbound := e.createPod("unbound", func(p *apis.Pod) { p.Spec.NodeName = "" })
	e.sync(e.agent, pod)
	e.sync(e.agent, unbound)
	if cs, _ := e.fake.Containers(context.Background(), ""); len(cs) != 0 {
		t.Errorf("started containers for pods that are not ours: %v", cs)
	}
	if ips, _ := e.st.IPs("pod"); len(ips) != 0 {
		t.Errorf("allocated addresses: %v", ips)
	}
}

func TestRestartDelay(t *testing.T) {
	e := newEnv(t)
	pod := e.createPod("crash", nil)
	e.sync(e.agent, pod)

	// First failure: restarted at once.
	e.clk.advance(2 * time.Second)
	ended := e.clk.Now()
	e.fake.Exit(id(pod, 0), 1)
	if retry := e.sync(e.agent, pod); retry != 0 {
		t.Fatalf("the first restart is immediate, got a wait of %v", retry)
	}
	cs := status(t, e.get(pod))
	if cs.State.Running == nil || cs.RestartCount != 1 || cs.ContainerID != "containerd://"+id(pod, 1) {
		t.Fatalf("after the first restart: %+v", cs)
	}
	if lt := cs.LastState.Terminated; lt == nil || lt.ExitCode != 1 || lt.Reason != "Error" || lt.FinishedAt == nil || !lt.FinishedAt.Equal(ended) {
		t.Fatalf("lastState: %+v", cs.LastState.Terminated)
	}

	// Second failure: 10 s, shown as CrashLoopBackOff.
	e.clk.advance(time.Second)
	e.fake.Exit(id(pod, 1), 1)
	if retry := e.sync(e.agent, pod); retry != 10*time.Second {
		t.Fatalf("want a 10s wait, got %v", retry)
	}
	got := e.get(pod)
	cs = status(t, got)
	if cs.State.Waiting == nil || cs.State.Waiting.Reason != "CrashLoopBackOff" || cs.RestartCount != 1 || cs.Ready {
		t.Fatalf("while waiting: %+v", cs)
	}
	if got.Status.Phase != "Running" {
		t.Errorf("phase while backing off: %s", got.Status.Phase)
	}
	e.clk.advance(4 * time.Second)
	if retry := e.sync(e.agent, pod); retry != 6*time.Second {
		t.Fatalf("want the rest of the wait (6s), got %v", retry)
	}
	e.clk.advance(6 * time.Second)
	e.sync(e.agent, pod)
	if cs = status(t, e.get(pod)); cs.State.Running == nil || cs.RestartCount != 2 {
		t.Fatalf("after the wait: %+v", cs)
	}

	// Third failure: 20 s.
	e.fake.Exit(id(pod, 2), 1)
	if retry := e.sync(e.agent, pod); retry != 20*time.Second {
		t.Fatalf("want a 20s wait, got %v", retry)
	}
	// Only the newest attempt is kept in the runtime.
	if cs, _ := e.fake.Containers(context.Background(), pod.Metadata.UID); len(cs) != 1 || cs[0].Attempt != 2 {
		t.Errorf("containers left: %+v", cs)
	}
}

func TestRestartKeepsCgroupsSeparate(t *testing.T) {
	e := newEnv(t)
	pod := e.createPod("restart-cgroup", nil)
	e.sync(e.agent, pod)
	first, ok := e.fake.Config(id(pod, 0))
	if !ok {
		t.Fatal("initial container missing")
	}
	e.fake.Exit(id(pod, 0), 137)
	e.sync(e.agent, pod)
	next, ok := e.fake.Config(id(pod, 1))
	if !ok || first.CgroupPath == next.CgroupPath {
		t.Fatalf("replacement must have its own cgroup: %q -> %q", first.CgroupPath, next.CgroupPath)
	}
	if filepath.Dir(first.CgroupPath) != filepath.Dir(next.CgroupPath) {
		t.Fatal("attempts must remain sibling leaves in the same Pod cgroup")
	}
	// Fake.Remove models the real busy-group error. Both tasks remaining
	// would mean cleanup was silently ignored after starting the replacement.
	left, err := e.fake.Containers(context.Background(), pod.Metadata.UID)
	if err != nil || len(left) != 1 || left[0].ID != id(pod, 1) || left[0].State != runtime.StateRunning {
		t.Fatalf("old attempt was not cleaned without harming the replacement: %+v, %v", left, err)
	}
}

func TestRestartDoesNotJoinLegacySiblingCgroup(t *testing.T) {
	e := newEnv(t)
	pod := e.createPod("legacy-sibling", func(p *apis.Pod) {
		p.Spec.Containers = append(p.Spec.Containers, apis.Container{Name: "main-1", Image: "busybox:1.36"})
	})
	e.sync(e.agent, pod)
	siblingID := runtime.ContainerID(pod.Metadata.UID, "main-1", 0)
	sibling, ok := e.fake.Config(siblingID)
	if !ok {
		t.Fatal("sibling was not started")
	}
	if err := e.fake.Remove(context.Background(), siblingID); err != nil {
		t.Fatal(err)
	}
	// Recreate that sibling in the path a pre-upgrade sw would have used.
	sibling.CgroupPath = filepath.Join(filepath.Dir(sibling.CgroupPath), sibling.Name)
	if err := e.fake.Start(context.Background(), sibling); err != nil {
		t.Fatal(err)
	}
	e.fake.Exit(id(pod, 0), 137)
	e.sync(e.agent, pod)
	next, ok := e.fake.Config(id(pod, 1))
	if !ok || next.CgroupPath == sibling.CgroupPath {
		t.Fatalf("a restarted container joined its legacy sibling's cgroup: %q", next.CgroupPath)
	}
	left, _ := e.fake.Containers(context.Background(), pod.Metadata.UID)
	if len(left) != 2 || left[0].State != runtime.StateRunning || left[1].State != runtime.StateRunning {
		t.Fatalf("both containers must remain alive in separate groups: %+v", left)
	}
}

func TestCleanupHandlesLegacySharedCgroup(t *testing.T) {
	e := newEnv(t)
	pod := e.createPod("legacy-cgroup", nil)
	e.sync(e.agent, pod)
	first, _ := e.fake.Config(id(pod, 0))
	e.fake.Exit(id(pod, 0), 137)
	legacy := first
	legacy.ID, legacy.Attempt = id(pod, 1), 1
	// Deliberately keep the old cgroup, as a pre-upgrade runtime can do.
	if err := e.fake.Start(context.Background(), legacy); err != nil {
		t.Fatal(err)
	}
	if err := e.fake.Remove(context.Background(), first.ID); err == nil {
		t.Fatal("fixture must reproduce a stopped task sharing a live cgroup")
	}
	actual, _ := e.fake.Containers(context.Background(), pod.Metadata.UID)
	if _, err := e.agent.cleanup(context.Background(), pod.Metadata.UID, actual, true); err != nil {
		t.Fatalf("cleanup should remove the live owner before the stopped task: %v", err)
	}
	left, _ := e.fake.Containers(context.Background(), pod.Metadata.UID)
	if len(left) != 0 {
		t.Fatalf("legacy attempts remain: %+v", left)
	}
}

func TestBackoffLimits(t *testing.T) {
	for n, want := range map[int]time.Duration{0: 0, 1: 10 * time.Second, 2: 20 * time.Second, 5: 160 * time.Second, 6: 300 * time.Second, 40: 300 * time.Second} {
		if got := backoff(n); got != want {
			t.Errorf("backoff(%d) = %v, want %v", n, got, want)
		}
	}
}

// The delay is not kept in memory only: a restarted sw server must not turn a
// crash loop into restarts without pause (design 6).
func TestRestartDelaySurvivesServerRestart(t *testing.T) {
	e := newEnv(t)
	pod := e.createPod("crash", nil)
	e.sync(e.agent, pod)
	e.fake.Exit(id(pod, 0), 1)
	e.sync(e.agent, pod) // attempt 1 runs
	e.fake.Exit(id(pod, 1), 1)
	e.sync(e.agent, pod) // waits 10s
	e.sync(e.agent, pod)

	fresh := e.newAgent()
	e.clk.advance(3 * time.Second)
	if retry := e.sync(fresh, pod); retry != 7*time.Second {
		t.Fatalf("a new agent must keep waiting (7s left), got %v", retry)
	}
	if cs := status(t, e.get(pod)); cs.State.Waiting == nil || cs.RestartCount != 1 {
		t.Fatalf("status under the new agent: %+v", cs)
	}
}

func TestLongRunClearsTheDelay(t *testing.T) {
	e := newEnv(t)
	pod := e.createPod("mostly-fine", nil)
	e.sync(e.agent, pod)
	for i := 0; i < 3; i++ { // fail quickly three times
		e.fake.Exit(id(pod, i), 1)
		e.clk.advance(e.sync(e.agent, pod))
		e.sync(e.agent, pod)
	}
	if cs := status(t, e.get(pod)); cs.State.Running == nil || cs.RestartCount != 3 {
		t.Fatalf("setup: %+v", cs)
	}
	e.clk.advance(11 * time.Minute) // attempt 3 runs fine for a while
	e.fake.Exit(id(pod, 3), 1)
	if retry := e.sync(e.agent, pod); retry != 0 {
		t.Fatalf("after a long run the restart is immediate, got %v", retry)
	}
	e.clk.advance(time.Second)
	e.fake.Exit(id(pod, 4), 1)
	if retry := e.sync(e.agent, pod); retry != 10*time.Second {
		t.Fatalf("the delay starts over at 10s, got %v", retry)
	}
}

// A container that died while sw server was down: counted, with the real end time.
func TestCatchUpAfterDowntime(t *testing.T) {
	e := newEnv(t)
	pod := e.createPod("web", nil)
	e.sync(e.agent, pod)
	e.clk.advance(30 * time.Second)
	died := e.clk.Now()
	e.fake.Exit(id(pod, 0), 137)
	e.clk.advance(5 * time.Minute) // nobody is watching

	fresh := e.newAgent()
	e.sync(fresh, pod)
	cs := status(t, e.get(pod))
	if cs.State.Running == nil || cs.RestartCount != 1 {
		t.Fatalf("not restarted: %+v", cs)
	}
	if lt := cs.LastState.Terminated; lt == nil || lt.ExitCode != 137 || !lt.FinishedAt.Equal(died) {
		t.Fatalf("lastState must carry the real end: %+v", cs.LastState.Terminated)
	}
}

// After a reboot the namespaces and tasks are gone, the pod and its address are not.
func TestRebootKeepsTheAddress(t *testing.T) {
	e := newEnv(t)
	other := e.createPod("other", nil)
	pod := e.createPod("web", nil)
	e.sync(e.agent, other)
	e.sync(e.agent, pod)
	if ip := e.get(pod).Status.PodIP; ip != "10.244.0.3" {
		t.Fatalf("setup: ip %s", ip)
	}
	e.clk.advance(time.Hour)
	e.fake.Lose(id(pod, 0))
	e.fake.Lose(id(other, 0))
	e.fake.DropNets()

	fresh := e.newAgent()
	e.sync(fresh, pod)
	got := e.get(pod)
	if ip, ok := e.fake.NetIP(pod.Metadata.UID); !ok || ip.String() != "10.244.0.3" || got.Status.PodIP != "10.244.0.3" {
		t.Fatalf("address after the reboot: runtime %v, status %s", ip, got.Status.PodIP)
	}
	cs := status(t, got)
	if cs.State.Running == nil || cs.RestartCount != 1 {
		t.Fatalf("not restarted: %+v", cs)
	}
	lt := cs.LastState.Terminated
	if lt == nil || lt.Reason != "Unknown" || lt.FinishedAt == nil || lt.ContainerID != "containerd://"+id(pod, 0) {
		t.Fatalf("lastState after a lost task: %+v", lt)
	}
}

type wrongNamespace struct{ *runtime.Fake }

func (wrongNamespace) ContainsTask(string, uint32) (bool, error) { return false, nil }

func TestStopsTaskInOldNamespace(t *testing.T) {
	e := newEnv(t)
	pod := e.createPod("web", nil)
	e.sync(e.agent, pod)
	ip := e.get(pod).Status.PodIP
	e.agent.Net = wrongNamespace{e.fake}
	e.sync(e.agent, pod)
	got := e.get(pod)
	cs := status(t, got)
	if cs.State.Running == nil || cs.RestartCount != 1 || got.Status.PodIP != ip {
		t.Fatalf("expected a replacement on the same IP, got %+v, IP %s", cs, got.Status.PodIP)
	}
}

type stalledPull struct {
	*runtime.Fake
	started chan struct{}
}

func (r stalledPull) EnsureImage(ctx context.Context, _, _ string) (string, error) {
	close(r.started)
	<-ctx.Done()
	return "", ctx.Err()
}

func TestWithdrawsReadyBeforeRestartImagePull(t *testing.T) {
	e := newEnv(t)
	pod := e.createPod("web", func(p *apis.Pod) {
		p.Spec.Containers = append(p.Spec.Containers, apis.Container{Name: "sidecar", Image: "busybox:1.36"})
	})
	a := e.agent
	e.sync(a, pod)
	e.fake.Exit(runtime.ContainerID(pod.Metadata.UID, "sidecar", 0), 1)
	r := stalledPull{Fake: e.fake, started: make(chan struct{})}
	a.Runtime = r
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.SyncPod(ctx, pod.Metadata.UID)
	}()
	defer func() { cancel(); <-done }()
	select {
	case <-r.started:
	case <-done:
		t.Fatal("sync returned before starting the replacement pull")
	case <-ctx.Done():
		t.Fatal("replacement pull did not start")
	}
	got := e.get(pod)
	n := 0
	for _, c := range got.Status.Conditions {
		if (c.Type == "Ready" || c.Type == "ContainersReady") && c.Status == "False" {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("expected Ready and ContainersReady to be false during the pull: %+v", got.Status.Conditions)
	}
}

func TestDeleteCancelsPull(t *testing.T) {
	e := newEnv(t)
	pod := e.createPod("web", nil)
	a := e.agent
	a.init()
	a.work[pod.Metadata.UID] = &work{running: true}
	r := stalledPull{Fake: e.fake, started: make(chan struct{})}
	a.Runtime = r
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := a.SyncPod(ctx, pod.Metadata.UID)
		result <- err
	}()
	select {
	case <-r.started:
	case err := <-result:
		t.Fatalf("sync returned before pull: %v", err)
	case <-ctx.Done():
		t.Fatal("pull did not start")
	}
	e.markDeleted(pod, 30)
	key := objects.Key(podsRes, pod.Metadata.Namespace, pod.Metadata.Name)
	rec, err := e.st.Get(key)
	if err != nil {
		t.Fatal(err)
	}
	a.observe(store.Change{Key: key, Type: "MODIFIED", Data: rec.Data})
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("expected delete to cancel the pull, got %v", err)
	}
	e.sync(a, pod)
	if e.get(pod) != nil {
		t.Fatal("pod with no running task was not deleted")
	}
}

// A lost task whose restart has to wait: the invented end time must not move,
// or the wait would never end.
func TestUnknownEndTimeIsStable(t *testing.T) {
	e := newEnv(t)
	pod := e.createPod("web", nil)
	e.sync(e.agent, pod)
	e.fake.Exit(id(pod, 0), 1)
	e.sync(e.agent, pod) // attempt 1
	e.fake.Lose(id(pod, 1))
	if retry := e.sync(e.agent, pod); retry != 10*time.Second {
		t.Fatalf("want a 10s wait, got %v", retry)
	}
	e.clk.advance(4 * time.Second)
	if retry := e.sync(e.agent, pod); retry != 6*time.Second {
		t.Fatalf("the wait must shrink (6s), got %v", retry)
	}
	e.clk.advance(6 * time.Second)
	e.sync(e.agent, pod)
	if cs := status(t, e.get(pod)); cs.State.Running == nil || cs.RestartCount != 2 {
		t.Fatalf("after the wait: %+v", cs)
	}
}

func TestRestartPolicies(t *testing.T) {
	cases := []struct {
		policy  string
		code    int32
		phase   string
		restart bool
	}{
		{"Never", 0, "Succeeded", false},
		{"Never", 3, "Failed", false},
		{"OnFailure", 0, "Succeeded", false},
		{"OnFailure", 3, "Running", true},
		{"Always", 0, "Running", true},
	}
	for _, c := range cases {
		e := newEnv(t)
		pod := e.createPod("job", func(p *apis.Pod) { p.Spec.RestartPolicy = c.policy })
		e.sync(e.agent, pod)
		e.fake.Exit(id(pod, 0), c.code)
		e.sync(e.agent, pod)
		e.sync(e.agent, pod)
		got := e.get(pod)
		cs := status(t, got)
		if got.Status.Phase != c.phase || (cs.RestartCount == 1) != c.restart {
			t.Errorf("%s, exit %d: phase %s, restartCount %d; want %s, restart=%v", c.policy, c.code, got.Status.Phase, cs.RestartCount, c.phase, c.restart)
		}
		if !c.restart {
			if tm := cs.State.Terminated; tm == nil || tm.ExitCode != c.code || tm.FinishedAt == nil {
				t.Errorf("%s, exit %d: state %+v", c.policy, c.code, cs.State)
			}
			if c.code == 0 {
				for _, cond := range got.Status.Conditions {
					if cond.Type == "Ready" && (cond.Status != "False" || cond.Reason != "PodCompleted") {
						t.Errorf("Ready condition of a completed pod: %+v", cond)
					}
				}
			}
		}
	}
}

func TestImagePullFailure(t *testing.T) {
	e := newEnv(t)
	e.fake.PullErr = errors.New("registry says no")
	pod := e.createPod("web", nil)
	if retry := e.sync(e.agent, pod); retry != 10*time.Second {
		t.Fatalf("want a 10s wait after a failed pull, got %v", retry)
	}
	got := e.get(pod)
	cs := status(t, got)
	if got.Status.Phase != "Pending" || cs.State.Waiting == nil || cs.State.Waiting.Reason != "ErrImagePull" || !strings.Contains(cs.State.Waiting.Message, "registry says no") {
		t.Fatalf("after a failed pull: phase %s, %+v", got.Status.Phase, cs.State.Waiting)
	}
	// Our own status write wakes us at once; that must not be another pull.
	e.fake.PullErr = nil
	if retry := e.sync(e.agent, pod); retry != 10*time.Second {
		t.Fatalf("the wait must hold, got %v", retry)
	}
	if cs = status(t, e.get(pod)); cs.State.Waiting == nil || cs.State.Waiting.Reason != "ImagePullBackOff" {
		t.Fatalf("while waiting: %+v", cs.State)
	}
	if n := e.fake.Pulls["docker.io/library/busybox:1.36"]; n != 0 {
		t.Fatalf("pulled during the wait (%d times)", n)
	}
	e.clk.advance(10 * time.Second)
	e.sync(e.agent, pod)
	if got = e.get(pod); got.Status.Phase != "Running" {
		t.Fatalf("after the wait: %s", got.Status.Phase)
	}
}

func TestStartFailure(t *testing.T) {
	e := newEnv(t)
	e.fake.StartErr = errors.New("exec: no such file")
	pod := e.createPod("web", nil)
	e.sync(e.agent, pod)
	if cs := status(t, e.get(pod)); cs.State.Waiting == nil || cs.State.Waiting.Reason != "RunContainerError" {
		t.Fatalf("after a failed start: %+v", cs.State)
	}
}

func TestGracefulDelete(t *testing.T) {
	e := newEnv(t)
	pod := e.createPod("web", nil)
	e.sync(e.agent, pod)
	uid := pod.Metadata.UID
	e.fake.IgnoreTERM = true
	e.markDeleted(pod, 30)

	if retry := e.sync(e.agent, pod); retry <= 0 || retry > 5*time.Second {
		t.Fatalf("while the grace period runs we look again soon, got %v", retry)
	}
	e.sync(e.agent, pod)
	if got := strings.Join(e.fake.Signals, " "); got != id(pod, 0)+":15" {
		t.Fatalf("TERM is sent once, got signals %q", got)
	}
	if e.get(pod) == nil {
		t.Fatal("the object went away while its container runs")
	}
	// A restarted agent sends TERM again; that is harmless and the deadline stays.
	e.clk.advance(31 * time.Second)
	if retry := e.sync(e.agent, pod); retry != time.Second {
		t.Fatalf("after the deadline: KILL and look again in 1s, got %v", retry)
	}
	if !strings.HasSuffix(strings.Join(e.fake.Signals, " "), ":"+"9") {
		t.Fatalf("no KILL after the grace period: %v", e.fake.Signals)
	}
	e.sync(e.agent, pod)
	if e.get(pod) != nil {
		t.Fatal("the object is still there after its containers are gone")
	}
	e.sync(e.agent, pod) // the DELETED event leads here: nothing left, address returned
	if cs, _ := e.fake.Containers(context.Background(), uid); len(cs) != 0 {
		t.Errorf("containers left: %v", cs)
	}
	if _, ok := e.fake.NetIP(uid); ok {
		t.Error("the network namespace is left")
	}
	if ips, _ := e.st.IPs("pod"); len(ips) != 0 {
		t.Errorf("the address was not returned: %v", ips)
	}
	if len(e.fake.GonePods) == 0 {
		t.Error("PodGone was not called")
	}
}

func TestDeleteWithObedientContainer(t *testing.T) {
	e := newEnv(t)
	pod := e.createPod("web", nil)
	e.sync(e.agent, pod)
	e.markDeleted(pod, 30)
	e.sync(e.agent, pod) // TERM, and the fake container obeys
	e.sync(e.agent, pod)
	if e.get(pod) != nil {
		t.Fatal("the object must go as soon as the container has ended")
	}
	if got := strings.Join(e.fake.Signals, " "); got != id(pod, 0)+":"+"15" {
		t.Errorf("signals: %q", got)
	}
	_ = syscall.SIGTERM
}

func TestDeleteWaitsForFinalizers(t *testing.T) {
	e := newEnv(t)
	pod := e.createPod("held", func(p *apis.Pod) { p.Metadata.Finalizers = []string{"example.com/hold"} })
	e.sync(e.agent, pod)
	e.markDeleted(pod, 30)
	e.sync(e.agent, pod)
	e.sync(e.agent, pod)
	got := e.get(pod)
	if got == nil {
		t.Fatal("a pod with a finalizer was removed")
	}
	if g := got.Metadata.DeletionGracePeriodSeconds; g == nil || *g != 0 {
		t.Errorf("grace must become 0 when the node is done, got %v", g)
	}
	if cs := status(t, got); cs.State.Terminated == nil {
		t.Errorf("container state: %+v", cs.State)
	}
	if ips, _ := e.st.IPs("pod"); len(ips) != 1 {
		t.Errorf("the address stays while the object does: %v", ips)
	}
	if cs, _ := e.fake.Containers(context.Background(), pod.Metadata.UID); len(cs) != 0 {
		t.Errorf("containers left: %v", cs)
	}
}

func TestOrphanCleanup(t *testing.T) {
	e := newEnv(t)
	keep := e.createPod("keep", nil)
	e.sync(e.agent, keep)

	gone := objects.NewUID()
	halfMade := objects.NewUID()
	ip, err := e.st.AllocIP("pod", gone, e.agent.podAddrs)
	if err != nil {
		t.Fatal(err)
	}
	e.fake.Setup(gone, netip.MustParseAddr(ip))
	e.fake.Add(runtime.Container{ID: runtime.ContainerID(gone, "main", 4), PodUID: gone, Name: "main", Attempt: 4, State: runtime.StateRunning})
	// Creation was cut short before the labels: found by its name alone.
	e.fake.Add(runtime.Container{ID: runtime.ContainerID(halfMade, "main", 0), PodUID: halfMade, Name: "main", State: runtime.StateUnknown})

	e.agent.resync(context.Background())
	e.agent.wg.Wait()

	cs, _ := e.fake.Containers(context.Background(), "")
	if len(cs) != 1 || cs[0].PodUID != keep.Metadata.UID {
		t.Fatalf("containers after the sweep: %+v", cs)
	}
	if nets, _ := e.fake.Pods(); len(nets) != 1 || nets[0] != keep.Metadata.UID {
		t.Errorf("namespaces after the sweep: %v", nets)
	}
	if ips, _ := e.st.IPs("pod"); len(ips) != 1 || ips[keep.Metadata.UID] == "" {
		t.Errorf("addresses after the sweep: %v", ips)
	}
	if got := e.get(keep); got.Status.Phase != "Running" {
		t.Errorf("the live pod was disturbed: %s", got.Status.Phase)
	}
}

func TestNodeObject(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	read := func() (*apis.Node, int64) {
		rec, err := e.st.Get(objects.Key(nodesRes, "", "n1"))
		if err != nil {
			t.Fatal(err)
		}
		n := &apis.Node{}
		json.Unmarshal(rec.Data, n)
		return n, rec.Rev
	}
	ready := func(n *apis.Node) string {
		for _, c := range n.Status.Conditions {
			if c.Type == "Ready" {
				return c.Status
			}
		}
		return ""
	}
	e.agent.syncNode(ctx)
	n, rev := read()
	if ready(n) != "True" || n.Spec.PodCIDR != "10.244.0.0/24" || n.Status.NodeInfo.ContainerRuntimeVersion != "fake://1.0" ||
		n.Metadata.Labels["kubernetes.io/hostname"] != "n1" || n.Metadata.UID == "" {
		t.Fatalf("node: %+v", n)
	}
	if e.fake.HostCalls != 1 {
		t.Errorf("EnsureHost calls: %d", e.fake.HostCalls)
	}
	// No heartbeat: an unchanged node is not written.
	e.clk.advance(time.Minute)
	e.agent.syncNode(ctx)
	if _, again := read(); again != rev {
		t.Errorf("the node was written without a change: revision %d -> %d", rev, again)
	}
	e.fake.SetDown(true)
	e.agent.syncNode(ctx)
	if n, _ = read(); ready(n) != "False" || n.Status.NodeInfo.ContainerRuntimeVersion != "fake://1.0" {
		t.Fatalf("with the runtime down: ready=%s, %+v", ready(n), n.Status.NodeInfo)
	}
	e.fake.SetDown(false)
	e.agent.syncNode(ctx)
	if n, _ = read(); ready(n) != "True" {
		t.Fatal("the node did not come back")
	}
	if e.fake.HostCalls != 2 {
		t.Errorf("the network is prepared again when the runtime returns, got %d calls", e.fake.HostCalls)
	}
}

// A runtime that cannot be reached must never look like "no containers".
func TestRuntimeDownChangesNothing(t *testing.T) {
	e := newEnv(t)
	pod := e.createPod("web", nil)
	e.sync(e.agent, pod)
	e.fake.SetDown(true)
	if _, err := e.agent.SyncPod(context.Background(), pod.Metadata.UID); err == nil {
		t.Fatal("want an error while the runtime is down")
	}
	e.fake.SetDown(false)
	if cs := status(t, e.get(pod)); cs.State.Running == nil || cs.RestartCount != 0 {
		t.Fatalf("status changed while the runtime was down: %+v", cs)
	}
}

func TestPodAddrs(t *testing.T) {
	a := &Agent{PodCIDR: netip.MustParsePrefix("10.244.0.0/29")}
	var got []string
	a.podAddrs(func(s string) bool { got = append(got, s); return true })
	// .0 network, .1 gateway, .7 broadcast are never given out.
	if strings.Join(got, " ") != "10.244.0.2 10.244.0.3 10.244.0.4 10.244.0.5 10.244.0.6" {
		t.Errorf("addresses: %v", got)
	}
}
