package nodeapi

import (
	"sync"
	"time"
)

// Node liveness timing (Kubernetes defaults).
const (
	HeartbeatEvery = 10 * time.Second
	// UnreachableAfter without hearing from a node marks it NotReady
	// (Ready=Unknown) and taints it node.kubernetes.io/unreachable.
	UnreachableAfter = 40 * time.Second
	// EvictAfter on an unreachable node, Pods that do not tolerate the
	// taint for longer are deleted so their controllers replace them elsewhere.
	EvictAfter = 5 * time.Minute
)

// Liveness remembers when each worker node was last heard from. It lives in
// memory only: heartbeats cost no disk writes, so an idle cluster still
// writes nothing. After a control plane restart every node starts as just
// seen, which gives workers UnreachableAfter to reconnect.
type Liveness struct {
	mu      sync.Mutex
	seen    map[string]time.Time
	started time.Time
	Now     func() time.Time
}

func NewLiveness() *Liveness { return &Liveness{seen: map[string]time.Time{}, started: time.Now()} }

func (l *Liveness) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

// Seen records that node made an authenticated request.
func (l *Liveness) Seen(node string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seen[node] = l.now()
}

// LastSeen returns when node was last heard from in this process.
func (l *Liveness) LastSeen(node string) (time.Time, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	t, ok := l.seen[node]
	return t, ok
}

// Silence is how long node has been silent. A node not heard from since the
// control plane started counts from the start.
func (l *Liveness) Silence(node string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	t, ok := l.seen[node]
	if !ok {
		t = l.started
	}
	return l.now().Sub(t)
}

// Forget drops a removed node.
func (l *Liveness) Forget(node string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.seen, node)
}
