package ha

// The leader's side (design 3.4, 3.5): it is the store's Replicator, serves
// the batch stream and snapshots to its standby, renews its lease through the
// standby's polls and the witness's heartbeat answers, and steps down when
// the lease runs out or the witness says its epoch is over.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"shearwater/internal/store"
)

// StreamReply is one answer of the batch stream.
type StreamReply struct {
	Epoch   int64         `json:"epoch"`
	Leader  string        `json:"leader"`
	Seq     int64         `json:"seq"` // the leader's last committed batch
	Members []Member      `json:"members"`
	Witness string        `json:"witness,omitempty"`
	Batches []store.Batch `json:"batches"`
}

// Leader serves Prefix on the leader's API server and replicates its store.
type Leader struct {
	Store   *store.Store
	Config  Config
	DataDir string         // where snapshots are written before they are sent
	Witness *WitnessClient // nil: no automatic takeover, so no lease either
	// StepDown is called once when this process must stop being the leader;
	// the store is already fenced. It should start the process again as a
	// standby.
	StepDown func(reason string)
	Logf     func(format string, a ...any)

	mu          sync.Mutex
	acked       int64         // highest seq the standby confirmed
	ackCh       chan struct{} // closed and replaced when acked moves
	batchCh     chan struct{} // closed and replaced when a batch is offered
	pending     *store.Batch  // the batch waiting for confirmation, not yet committed
	sync        bool          // writes wait for the standby
	standbySeen time.Time     // when the standby last polled in this epoch
	leaseUntil  time.Time
	stepped     bool
}

func (l *Leader) logf(format string, a ...any) {
	if l.Logf != nil {
		l.Logf(format, a...)
	}
}

// Start begins replicating: from now on every write of the store waits for
// the standby (when it is in sync). Run the returned loops until ctx ends.
func (l *Leader) Start(ctx context.Context) error {
	l.ackCh, l.batchCh = make(chan struct{}), make(chan struct{})
	l.leaseUntil = time.Now().Add(LeaseFor)
	if err := l.Store.Replicate(l.Config.Epoch, l); err != nil {
		return err
	}
	if l.Witness != nil {
		go l.heartbeats(ctx)
		go l.watchLease(ctx)
	}
	return nil
}

func (l *Leader) peer() string {
	p, _ := l.Config.Peer()
	return p.Name
}

// Replicate is called by the store with its write lock held, before the
// local commit.
func (l *Leader) Replicate(b store.Batch) error {
	l.mu.Lock()
	if l.stepped {
		l.mu.Unlock()
		return errors.New("this control plane stepped down")
	}
	l.pending = &b
	close(l.batchCh)
	l.batchCh = make(chan struct{})
	insync := l.sync
	active := !l.standbySeen.IsZero() && time.Since(l.standbySeen) < FailAfter
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		l.pending = nil
		l.mu.Unlock()
	}()

	if !insync {
		if !active || l.peer() == "" {
			return nil // alone: the batch waits in the log
		}
		// The standby is polling. No other batch can arrive while this one
		// waits, so give it a moment to confirm every committed batch, then
		// ask the witness to count it in again and wait for it like any
		// synchronous write. A standby still behind stays out for now.
		if !l.waitAck(b.Seq-1, SyncTimeout) {
			return nil
		}
		if l.Witness != nil {
			if err := l.witnessCall("resync"); err != nil {
				l.logf("standby %s caught up but the witness did not take it back in sync: %v", l.peer(), err)
				return nil
			}
		}
		l.mu.Lock()
		l.sync = true
		l.leaseUntil = time.Now().Add(LeaseFor)
		l.mu.Unlock()
		l.logf("standby %s is in sync again from batch %d", l.peer(), b.Seq)
	}

	if l.waitAck(b.Seq, SyncTimeout) {
		return nil
	}
	return l.goSolo(b)
}

// waitAck waits up to d for the standby to confirm seq.
func (l *Leader) waitAck(seq int64, d time.Duration) bool {
	deadline := time.NewTimer(d)
	defer deadline.Stop()
	for {
		l.mu.Lock()
		if l.acked >= seq {
			l.mu.Unlock()
			return true
		}
		ch := l.ackCh
		l.mu.Unlock()
		select {
		case <-ch:
		case <-deadline.C:
			return false
		}
	}
}

// goSolo commits without the standby, which must first be taken out of the
// sync set by the witness: then it can never be elected with a stale copy.
func (l *Leader) goSolo(b store.Batch) error {
	if l.Witness != nil {
		if err := l.witnessCall("desync"); err != nil {
			// Neither the standby nor the witness answers: the standby may
			// hold this batch, so this file must not move on alone.
			l.stepDown(fmt.Sprintf("batch %d was not confirmed by the standby and the witness did not answer (%v)", b.Seq, err))
			return fmt.Errorf("neither the standby nor the witness confirmed batch %d", b.Seq)
		}
	}
	l.mu.Lock()
	l.sync = false
	l.mu.Unlock()
	l.logf("standby %s did not confirm batch %d within %s; continuing alone", l.peer(), b.Seq, SyncTimeout)
	return nil
}

func (l *Leader) witnessCall(op string) error {
	ctx, cancel := context.WithTimeout(context.Background(), CallTimeout)
	defer cancel()
	reply, err := l.Witness.Call(ctx, op, WitnessRequest{Epoch: l.Config.Epoch, Leader: l.Config.Self, Member: l.peer()})
	if err != nil {
		return err
	}
	if !reply.OK {
		if reply.State.Epoch > l.Config.Epoch {
			l.stepDown(fmt.Sprintf("the witness says epoch %d belongs to %s", reply.State.Epoch, reply.State.Leader))
		}
		return errors.New(reply.Reason)
	}
	return nil
}

func (l *Leader) heartbeats(ctx context.Context) {
	for ctx.Err() == nil && !l.Stepped() {
		sent := time.Now()
		ctx2, cancel := context.WithTimeout(ctx, CallTimeout)
		reply, err := l.Witness.Call(ctx2, "heartbeat", WitnessRequest{Epoch: l.Config.Epoch, Leader: l.Config.Self})
		cancel()
		switch {
		case err != nil:
		case !reply.OK:
			l.stepDown("the witness refused the heartbeat: " + reply.Reason)
			return
		default:
			l.extend(sent)
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Until(sent.Add(BeatEvery))):
		}
	}
}

// watchLease steps down when the lease ran out. The lease matters only while
// the standby is in sync: otherwise the witness will not elect it, and this
// leader may keep writing alone.
func (l *Leader) watchLease(ctx context.Context) {
	t := time.NewTicker(BeatEvery / 5)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		l.mu.Lock()
		expired := l.sync && time.Now().After(l.leaseUntil)
		stepped := l.stepped
		l.mu.Unlock()
		if stepped {
			return
		}
		if expired {
			l.stepDown("neither the standby nor the witness confirmed this leader for " + LeaseFor.String())
			return
		}
	}
}

// extend renews the lease from a confirmation of something sent at sent.
func (l *Leader) extend(sent time.Time) {
	l.mu.Lock()
	if until := sent.Add(LeaseFor); until.After(l.leaseUntil) {
		l.leaseUntil = until
	}
	l.mu.Unlock()
}

func (l *Leader) stepDown(reason string) {
	l.mu.Lock()
	if l.stepped {
		l.mu.Unlock()
		return
	}
	l.stepped = true
	l.mu.Unlock()
	l.Store.Fence("this control plane is no longer the leader: " + reason)
	l.logf("stepping down: %s", reason)
	if l.StepDown != nil {
		go l.StepDown(reason)
	}
}

// Stepped reports whether the leader stepped down.
func (l *Leader) Stepped() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.stepped
}

// ServeHTTP serves the standby: GET stream, GET snapshot, GET status.
func (l *Leader) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !bearer(r, l.Config.Token) {
		http.Error(w, "UNAUTHORIZED retry=never: wrong or missing cluster HA token", http.StatusUnauthorized)
		return
	}
	switch strings.TrimPrefix(r.URL.Path, Prefix) {
	case "stream":
		l.serveStream(w, r)
	case "snapshot":
		l.serveSnapshot(w, r)
	case "status":
		writeJSON(w, http.StatusOK, l.Status())
	default:
		http.NotFound(w, r)
	}
}

// Status is the leader's view for sw ha status.
type Status struct {
	Self      string `json:"self"`
	Role      string `json:"role"`
	Epoch     int64  `json:"epoch"`
	Seq       int64  `json:"seq"`
	Standby   string `json:"standby,omitempty"`
	InSync    bool   `json:"inSync"`
	Confirmed int64  `json:"confirmed"`         // the standby's last confirmed batch
	SeenAgo   string `json:"seenAgo,omitempty"` // since the standby last polled
	Witness   string `json:"witness,omitempty"`
}

func (l *Leader) Status() Status {
	seq, _ := l.Store.ReplicationState()
	l.mu.Lock()
	defer l.mu.Unlock()
	st := Status{Self: l.Config.Self, Role: RoleLeader, Epoch: l.Config.Epoch, Seq: seq, Standby: l.peer(),
		InSync: l.sync, Confirmed: l.acked, Witness: l.Config.Witness}
	if !l.standbySeen.IsZero() {
		st.SeenAgo = time.Since(l.standbySeen).Round(100 * time.Millisecond).String()
	}
	return st
}

func (l *Leader) serveStream(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	after, err1 := strconv.ParseInt(q.Get("after"), 10, 64)
	epoch, err2 := strconv.ParseInt(q.Get("epoch"), 10, 64)
	if err1 != nil || err2 != nil || q.Get("member") != l.peer() {
		http.Error(w, "INVALID retry=never: stream needs member, after and epoch", http.StatusBadRequest)
		return
	}
	if epoch != l.Config.Epoch {
		// A standby from another epoch starts again from a snapshot: it
		// may hold a batch this leader never committed.
		http.Error(w, fmt.Sprintf("NEW_EPOCH retry=safe: the leader is at epoch %d; take a snapshot", l.Config.Epoch), http.StatusConflict)
		return
	}
	received := time.Now()
	l.mu.Lock()
	if l.stepped {
		l.mu.Unlock()
		http.Error(w, "NOT_LEADER retry=safe: this control plane stepped down", http.StatusServiceUnavailable)
		return
	}
	if after > l.acked {
		l.acked = after
		close(l.ackCh)
		l.ackCh = make(chan struct{})
	}
	l.standbySeen = received
	l.mu.Unlock()
	if l.Witness != nil {
		l.extend(received)
	}

	wait := time.NewTimer(BeatEvery)
	defer wait.Stop()
	for {
		l.mu.Lock()
		ch := l.batchCh
		pending := l.pending
		l.mu.Unlock()
		batches, err := l.Store.Batches(after, 100)
		if errors.Is(err, store.ErrCompacted) {
			http.Error(w, "COMPACTED retry=safe: take a snapshot", http.StatusGone)
			return
		}
		if err != nil {
			http.Error(w, "INTERNAL retry=safe: "+err.Error(), http.StatusInternalServerError)
			return
		}
		next := after
		if len(batches) > 0 {
			next = batches[len(batches)-1].Seq
		}
		if pending != nil && pending.Seq == next+1 && len(batches) < 100 {
			batches = append(batches, *pending)
		}
		if len(batches) > 0 {
			l.reply(w, batches)
			return
		}
		select {
		case <-ch:
		case <-wait.C:
			l.reply(w, nil) // a heartbeat
			return
		case <-r.Context().Done():
			return
		}
	}
}

func (l *Leader) reply(w http.ResponseWriter, batches []store.Batch) {
	seq, _ := l.Store.ReplicationState()
	if batches == nil {
		batches = []store.Batch{}
	}
	writeJSON(w, http.StatusOK, StreamReply{Epoch: l.Config.Epoch, Leader: l.Config.Self, Seq: seq,
		Members: l.Config.Members, Witness: l.Config.Witness, Batches: batches})
}

func (l *Leader) serveSnapshot(w http.ResponseWriter, r *http.Request) {
	dir, err := os.MkdirTemp(l.DataDir, "snapshot-")
	if err != nil {
		http.Error(w, "INTERNAL retry=safe: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "state.db")
	seq, epoch, err := l.Store.Snapshot(r.Context(), path)
	if err != nil {
		http.Error(w, "INTERNAL retry=safe: "+err.Error(), http.StatusInternalServerError)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		http.Error(w, "INTERNAL retry=safe: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		http.Error(w, "INTERNAL retry=safe: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/vnd.sqlite3")
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	w.Header().Set("X-Shearwater-Seq", strconv.FormatInt(seq, 10))
	w.Header().Set("X-Shearwater-Epoch", strconv.FormatInt(epoch, 10))
	w.Header().Set("X-Shearwater-Leader-Epoch", strconv.FormatInt(l.Config.Epoch, 10))
	io.Copy(w, f)
}

// MarshalStatus is a helper for CLIs.
func MarshalStatus(v any) string {
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b)
}
