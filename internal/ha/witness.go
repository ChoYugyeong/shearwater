package ha

// The witness (design 3.5): no data, one vote. It remembers on disk the
// current epoch, its leader and which control planes are in sync, and
// in memory when it last heard the leader. Every answer that changes the
// state is written to disk before it is sent.

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
)

// WitnessState is what the witness keeps on disk.
type WitnessState struct {
	Epoch  int64    `json:"epoch"`
	Leader string   `json:"leader"`
	InSync []string `json:"inSync"`
}

// WitnessRequest is the body of every witness call.
type WitnessRequest struct {
	Epoch     int64  `json:"epoch"`
	Leader    string `json:"leader,omitempty"`    // heartbeat, desync, resync
	Member    string `json:"member,omitempty"`    // desync, resync
	Candidate string `json:"candidate,omitempty"` // vote
}

// WitnessReply answers every witness call.
type WitnessReply struct {
	OK     bool         `json:"ok"`
	Reason string       `json:"reason,omitempty"`
	State  WitnessState `json:"state"`
	// SinceLeader is how long ago the witness last heard the leader.
	SinceLeader string `json:"sinceLeader,omitempty"`
}

// Witness serves WitnessPrefix.
type Witness struct {
	Path  string // the state file (witness.json is the join configuration; this is witness-state.json)
	Token string
	Now   func() time.Time // tests

	mu       sync.Mutex
	st       WitnessState
	loaded   bool
	lastBeat time.Time
}

func (w *Witness) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w *Witness) load() error {
	if w.loaded {
		return nil
	}
	b, err := os.ReadFile(w.Path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return err
	default:
		if err := json.Unmarshal(b, &w.st); err != nil {
			return fmt.Errorf("%s: %w", w.Path, err)
		}
	}
	// Until the leader is heard, act as if it just spoke: a witness that
	// restarts must not hand out a vote while the leader may still be alive.
	w.lastBeat, w.loaded = w.now(), true
	return nil
}

func (w *Witness) save() error {
	b, err := json.MarshalIndent(w.st, "", "  ")
	if err != nil {
		return err
	}
	return writeFileSync(w.Path, append(b, '\n'))
}

// State returns the current state (sw ha status).
func (w *Witness) State() (WitnessState, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.load(); err != nil {
		return WitnessState{}, err
	}
	return w.st, nil
}

func (w *Witness) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	if !bearer(r, w.Token) {
		http.Error(rw, "UNAUTHORIZED retry=never: wrong or missing cluster HA token", http.StatusUnauthorized)
		return
	}
	op, ok := strings.CutPrefix(r.URL.Path, WitnessPrefix)
	if !ok {
		http.NotFound(rw, r)
		return
	}
	var req WitnessRequest
	if r.Method == http.MethodPost {
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
			http.Error(rw, "INVALID retry=never: "+err.Error(), http.StatusBadRequest)
			return
		}
	} else if op != "state" {
		http.Error(rw, "INVALID retry=never: use POST", http.StatusMethodNotAllowed)
		return
	}
	reply, err := w.handle(op, req)
	if err != nil {
		http.Error(rw, "INTERNAL retry=safe: "+err.Error(), http.StatusInternalServerError)
		return
	}
	code := http.StatusOK
	if !reply.OK {
		code = http.StatusConflict
	}
	writeJSON(rw, code, reply)
}

func (w *Witness) handle(op string, req WitnessRequest) (WitnessReply, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.load(); err != nil {
		return WitnessReply{}, err
	}
	refuse := func(format string, a ...any) (WitnessReply, error) {
		return WitnessReply{Reason: fmt.Sprintf(format, a...), State: w.st, SinceLeader: w.since()}, nil
	}
	accept := func(changed bool) (WitnessReply, error) {
		if changed {
			if err := w.save(); err != nil {
				return WitnessReply{}, err
			}
		}
		return WitnessReply{OK: true, State: w.st, SinceLeader: w.since()}, nil
	}
	switch op {
	case "state":
		return accept(false)
	case "heartbeat":
		switch {
		case req.Leader == "" || req.Epoch < 1:
			return refuse("a heartbeat names the leader and its epoch")
		case req.Epoch < w.st.Epoch:
			return refuse("epoch %d is over: epoch %d belongs to %s", req.Epoch, w.st.Epoch, w.st.Leader)
		case req.Epoch == w.st.Epoch && req.Leader != w.st.Leader:
			return refuse("epoch %d belongs to %s", w.st.Epoch, w.st.Leader)
		case req.Epoch > w.st.Epoch:
			// A new cluster's first leader, or a witness that lost its file.
			w.st = WitnessState{Epoch: req.Epoch, Leader: req.Leader, InSync: []string{req.Leader}}
			w.lastBeat = w.now()
			return accept(true)
		}
		w.lastBeat = w.now()
		return accept(false)
	case "desync", "resync":
		if req.Epoch != w.st.Epoch || req.Leader != w.st.Leader {
			return refuse("only the leader of epoch %d (%s) may change who is in sync", w.st.Epoch, w.st.Leader)
		}
		if req.Member == "" || req.Member == req.Leader {
			return refuse("name the standby")
		}
		in := slices.Contains(w.st.InSync, req.Member)
		if op == "desync" && in {
			w.st.InSync = slices.DeleteFunc(slices.Clone(w.st.InSync), func(m string) bool { return m == req.Member })
			return accept(true)
		}
		if op == "resync" && !in {
			w.st.InSync = append(slices.Clone(w.st.InSync), req.Member)
			slices.Sort(w.st.InSync)
			return accept(true)
		}
		return accept(false)
	case "vote":
		switch {
		case req.Candidate == "":
			return refuse("name the candidate")
		case req.Epoch <= w.st.Epoch:
			return refuse("epoch %d is already used (current %d, leader %s)", req.Epoch, w.st.Epoch, w.st.Leader)
		case !slices.Contains(w.st.InSync, req.Candidate):
			return refuse("%s is not in sync with the last leader; it must catch up from the leader first", req.Candidate)
		case req.Candidate != w.st.Leader && w.now().Sub(w.lastBeat) < VoteAfter:
			// The leader of the current epoch restarting may run again at
			// once; anyone else waits until that leader's lease has surely ended.
			return refuse("the leader %s was heard %s ago", w.st.Leader, w.since())
		}
		w.st = WitnessState{Epoch: req.Epoch, Leader: req.Candidate, InSync: []string{req.Candidate}}
		w.lastBeat = w.now()
		return accept(true)
	}
	return refuse("unknown call %q", op)
}

func (w *Witness) since() string {
	return w.now().Sub(w.lastBeat).Round(100 * time.Millisecond).String()
}

func bearer(r *http.Request, token string) bool {
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && token != "" && subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	b, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	w.Write(b)
}

// WitnessClient calls a witness.
type WitnessClient struct {
	URL    string // https://IP:PORT
	Token  string
	Client *http.Client
}

// Call sends one request. A refusal is a WitnessReply with OK false and a nil
// error; err means the witness did not answer.
func (c *WitnessClient) Call(ctx context.Context, op string, req WitnessRequest) (WitnessReply, error) {
	var reply WitnessReply
	body, _ := json.Marshal(req)
	method := http.MethodPost
	if op == "state" {
		method, body = http.MethodGet, nil
	}
	hr, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(c.URL, "/")+WitnessPrefix+op, bytes.NewReader(body))
	if err != nil {
		return reply, err
	}
	hr.Header.Set("Authorization", "Bearer "+c.Token)
	hr.Header.Set("Content-Type", "application/json")
	res, err := c.Client.Do(hr)
	if err != nil {
		return reply, err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, 1<<16))
	if err != nil {
		return reply, err
	}
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusConflict {
		return reply, fmt.Errorf("witness %s replied HTTP %d: %s", c.URL, res.StatusCode, strings.TrimSpace(string(b)))
	}
	if err := json.Unmarshal(b, &reply); err != nil {
		return reply, err
	}
	return reply, nil
}
