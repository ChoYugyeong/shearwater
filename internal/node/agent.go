// Package node is the node agent: the kubelet's part (design 6 and 7). It
// reads the pods bound to this node from the store and what containerd and the
// kernel actually have, and moves the second towards the first.
package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"sync"
	"time"

	"shearwater/internal/apis"
	"shearwater/internal/runtime"
	"shearwater/internal/store"
)

const (
	resyncEvery    = 30 * time.Second // re-read everything; catches what an event missed
	nodeCheckEvery = 10 * time.Second // is the runtime still there?
	hostCheckEvery = 5 * time.Minute  // bridge and rules (Docker restarts drop rules)
	failureRetry   = 10 * time.Second // after an error nobody planned for
)

var podsRes = apis.Lookup("", "v1", "pods")

type Agent struct {
	Store    *store.Store
	Runtime  runtime.Runtime
	Net      runtime.Network
	NodeName string
	DataDir  string       // /var/lib/shearwater
	LogDir   string       // /var/log/shearwater
	PodCIDR  netip.Prefix // 10.244.0.0/24
	Version  string       // reported as the kubelet version
	Now      func() time.Time
	Logf     func(format string, args ...any)
	ProbeRun func(context.Context, *apis.Probe, *apis.Container, string, string) error // injectable action, not the scheduling policy

	ctx context.Context

	wg       sync.WaitGroup // looks at pods that are under way
	mu       sync.Mutex
	keys     map[string]store.Key // pod uid -> key, pods bound to this node
	work     map[string]*work
	fails    map[string]int         // "uid/container" -> pulls or starts that failed in a row
	holds    map[string]hold        // "uid/container" -> no new start before this time
	resets   map[string]int         // "uid/container" -> the last attempt that ran long enough to clear the delay
	termSent map[string]bool        // container IDs that got SIGTERM for a delete
	probes   map[string]*probeState // uid/container; one Pod worker owns each value

	hostOK    bool
	hostAt    time.Time
	nodeReady bool
}

// hold is the delay after a failed start, with the state shown meanwhile.
type hold struct {
	until time.Time
	state apis.ContainerStateWaiting
}

type work struct {
	running     bool
	dirty       bool
	timer       *time.Timer
	cancelStart context.CancelFunc
}

func (a *Agent) init() {
	if a.Now == nil {
		a.Now = time.Now
	}
	if a.Logf == nil {
		a.Logf = func(format string, args ...any) { fmt.Fprintf(os.Stderr, "node: "+format+"\n", args...) }
	}
	if a.keys == nil {
		a.keys, a.work = map[string]store.Key{}, map[string]*work{}
		a.fails, a.resets, a.termSent = map[string]int{}, map[string]int{}, map[string]bool{}
		a.holds = map[string]hold{}
		a.probes = map[string]*probeState{}
	}
	if a.ctx == nil {
		a.ctx = context.Background()
	}
}

// Run drives the node until ctx ends. Containers are never stopped because the
// agent stops: the shims keep them, and the next start picks them up.
func (a *Agent) Run(ctx context.Context) {
	a.init()
	a.ctx = ctx
	a.syncNode(ctx)
	cursor := a.resync(ctx)
	nodeTick := time.NewTicker(nodeCheckEvery)
	defer nodeTick.Stop()
	resyncTick := time.NewTicker(resyncEvery)
	defer resyncTick.Stop()
	for {
		// Take the channel first, then read: a commit in between still wakes us.
		changed := a.Store.Changed()
		for ctx.Err() == nil {
			changes, next, err := a.Store.Changes("", podsRes.Plural, "", cursor, 500)
			if errors.Is(err, store.ErrCompacted) {
				cursor = a.resync(ctx)
				continue
			}
			if err != nil {
				a.Logf("reading pod changes: %v", err)
				break
			}
			for _, c := range changes {
				a.observe(c)
			}
			cursor = next
			if len(changes) < 500 {
				break
			}
		}
		select {
		case <-ctx.Done():
			a.stopTimers()
			// Let looks that are under way notice and return, so the store is
			// not closed under them. They only stop early; nothing is undone.
			done := make(chan struct{})
			go func() { a.wg.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
			}
			return
		case <-changed:
		case uid := <-a.Runtime.Exits():
			a.enqueue(uid)
		case <-nodeTick.C:
			a.syncNode(ctx)
		case <-resyncTick.C:
			a.resync(ctx)
		}
	}
}

func (a *Agent) stopTimers() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, w := range a.work {
		if w.timer != nil {
			w.timer.Stop()
		}
	}
}

// observe takes one pod change: remember where the pod lives and look at it.
func (a *Agent) observe(c store.Change) {
	var pod apis.Pod
	if err := json.Unmarshal(c.Data, &pod); err != nil {
		a.Logf("pod %s/%s is not readable: %v", c.Key.Namespace, c.Key.Name, err)
		return
	}
	if pod.Spec.NodeName != a.NodeName {
		return
	}
	uid := pod.Metadata.UID
	a.mu.Lock()
	if c.Type == "DELETED" || pod.Metadata.DeletionTimestamp != nil {
		if w := a.work[uid]; w != nil && w.cancelStart != nil {
			w.cancelStart()
		}
	}
	if c.Type == "DELETED" {
		delete(a.keys, uid)
	} else {
		a.keys[uid] = c.Key
	}
	a.mu.Unlock()
	a.enqueue(uid)
}

// resync looks at everything that could need work: every pod bound here, and
// every uid that still has a container, a namespace or an address. It returns
// the revision the pod list was read at.
func (a *Agent) resync(ctx context.Context) int64 {
	if err := a.Runtime.SweepSnapshots(ctx); err != nil {
		a.Logf("checking for orphan snapshots: %v", err)
	}
	recs, rev, err := a.Store.List("", podsRes.Plural, "")
	if err != nil {
		a.Logf("listing pods: %v", err)
		return rev
	}
	uids := map[string]bool{}
	keys := map[string]store.Key{}
	for _, rec := range recs {
		var pod apis.Pod
		if json.Unmarshal(rec.Data, &pod) == nil && pod.Spec.NodeName == a.NodeName {
			keys[rec.UID], uids[rec.UID] = rec.Key, true
		}
	}
	a.mu.Lock()
	a.keys = keys
	a.mu.Unlock()
	if cs, err := a.Runtime.Containers(ctx, ""); err == nil {
		for _, c := range cs {
			uids[c.PodUID] = true
		}
	}
	if nets, err := a.Net.Pods(); err == nil {
		for _, uid := range nets {
			uids[uid] = true
		}
	}
	if ips, err := a.Store.IPs("pod"); err == nil {
		for uid := range ips {
			uids[uid] = true
		}
	}
	for uid := range uids {
		a.enqueue(uid)
	}
	return rev
}

// enqueue asks for one pod to be looked at. At most one look per pod runs at
// a time; a request during a look makes it run once more.
func (a *Agent) enqueue(uid string) {
	if a.ctx.Err() != nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	w := a.work[uid]
	if w == nil {
		w = &work{}
		a.work[uid] = w
	}
	if w.timer != nil {
		w.timer.Stop()
		w.timer = nil
	}
	if w.running {
		w.dirty = true
		return
	}
	w.running = true
	a.wg.Add(1)
	go a.runPod(uid, w)
}

func (a *Agent) runPod(uid string, w *work) {
	defer a.wg.Done()
	for {
		retry, err := a.SyncPod(a.ctx, uid)
		if err != nil && a.ctx.Err() == nil {
			a.Logf("pod %s: %v", uid, err)
			if retry == 0 {
				retry = failureRetry
			}
		}
		a.mu.Lock()
		if w.dirty && a.ctx.Err() == nil {
			w.dirty = false
			a.mu.Unlock()
			continue
		}
		w.running = false
		switch {
		case a.ctx.Err() != nil:
		case retry > 0:
			w.timer = time.AfterFunc(retry, func() { a.enqueue(uid) })
		default:
			if _, known := a.keys[uid]; !known {
				delete(a.work, uid)
			}
		}
		a.mu.Unlock()
		return
	}
}

// ensureHost prepares the shared network once, and again every few minutes.
func (a *Agent) ensureHost() error {
	a.mu.Lock()
	fresh := a.hostOK && a.Now().Sub(a.hostAt) < hostCheckEvery
	a.mu.Unlock()
	if fresh {
		return nil
	}
	err := a.Net.EnsureHost()
	a.mu.Lock()
	a.hostOK, a.hostAt = err == nil, a.Now()
	a.mu.Unlock()
	if err != nil {
		return fmt.Errorf("prepare the pod network: %w", err)
	}
	return nil
}

func (a *Agent) forgetPod(uid string, containers []apis.Container) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, c := range containers {
		delete(a.fails, uid+"/"+c.Name)
		delete(a.holds, uid+"/"+c.Name)
		delete(a.resets, uid+"/"+c.Name)
		delete(a.probes, uid+"/"+c.Name)
	}
}
