package ha

// The standby's side (design 3.3, 3.5, 3.6): start from a snapshot of the
// leader, apply its batch stream, and ask the witness for the next epoch when
// the leader has been silent for FailAfter. Promotion itself (writing the new
// role and starting the process again as the leader) is the caller's.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"shearwater/internal/store"
)

// PromoteFile in the config directory asks a standby to take over without a
// witness (sw ha promote --force).
const PromoteFile = "ha-promote-request"

// Standby follows the leader into DataDir/state.db.
type Standby struct {
	Config    Config
	ConfigDir string
	DataDir   string
	Client    *http.Client
	Witness   *WitnessClient // nil: never take over without an operator
	// Promote is called when this standby won epoch; it should persist the
	// role and start the process again as the leader. The replica is closed
	// before the call. An error ends Run.
	Promote func(epoch int64) error
	Logf    func(format string, a ...any)
	// CampaignNow asks for the next epoch at once instead of following: a
	// leader that restarted (its witness lets the current leader run again
	// without waiting) keeps its own file when it wins.
	CampaignNow bool

	mu      sync.Mutex
	st      *store.Store
	follow  int64 // the leader epoch the replica follows
	contact time.Time
	leader  string
	lastLog map[string]time.Time // logEvery, per message
}

func (s *Standby) logf(format string, a ...any) {
	if s.Logf != nil {
		s.Logf(format, a...)
	}
}

// StandbyStatus is the standby's view for sw ha status.
type StandbyStatus struct {
	Self       string `json:"self"`
	Role       string `json:"role"`
	Leader     string `json:"leader,omitempty"`
	Epoch      int64  `json:"epoch"`
	Seq        int64  `json:"seq"`
	ContactAgo string `json:"contactAgo,omitempty"`
}

func (s *Standby) Status() StandbyStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := StandbyStatus{Self: s.Config.Self, Role: RoleStandby, Leader: s.leader, Epoch: s.follow}
	if s.st != nil {
		st.Seq, _ = s.st.ReplicationState()
	}
	if !s.contact.IsZero() {
		st.ContactAgo = time.Since(s.contact).Round(100 * time.Millisecond).String()
	}
	return st
}

// LeaderAddr is host:port of the leader while this standby is following it
// and heard from it within FailAfter, else "": a standby that is not sure
// where the leader is refuses connections instead of passing them on (two
// standbys would otherwise pass a connection back and forth).
func (s *Standby) LeaderAddr() string {
	s.mu.Lock()
	following := s.st != nil && time.Since(s.contact) < FailAfter
	s.mu.Unlock()
	if !following {
		return ""
	}
	u, err := url.Parse(s.LeaderURL())
	if err != nil {
		return ""
	}
	return u.Host
}

// WriteStatus keeps StatusFile in the config directory current until ctx ends.
func (s *Standby) WriteStatus(ctx context.Context) {
	for {
		if b, err := json.MarshalIndent(s.Status(), "", "  "); err == nil {
			writeFileSync(filepath.Join(s.ConfigDir, StatusFile), append(b, '\n'))
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

// LeaderURL is the API address of the control plane this standby follows.
func (s *Standby) LeaderURL() string {
	p, ok := s.Config.Peer()
	if !ok {
		return ""
	}
	return p.URL
}

// Run follows the leader until ctx ends or this standby is promoted.
func (s *Standby) Run(ctx context.Context) error {
	defer s.closeStore()
	needSnapshot := true
	s.mu.Lock()
	s.contact = time.Now()
	s.mu.Unlock()
	if s.CampaignNow && s.Witness != nil {
		ctx2, cancel := context.WithTimeout(ctx, CallTimeout)
		reply, err := s.Witness.Call(ctx2, "vote", WitnessRequest{Epoch: s.Config.Epoch + 1, Candidate: s.Config.Self})
		cancel()
		if err == nil && reply.OK {
			s.logf("the witness let this control plane lead again at epoch %d", s.Config.Epoch+1)
			return s.Promote(s.Config.Epoch + 1)
		}
		s.logf("not leading again (%v %s); following the leader", err, reply.Reason)
	}
	for ctx.Err() == nil {
		if s.promoteRequested() {
			return s.takeOver(ctx, true)
		}
		if needSnapshot {
			if err := s.bootstrap(ctx); err != nil {
				s.logEvery("cannot take a snapshot from the leader: %v", err)
			} else {
				needSnapshot = false
				s.touch()
			}
		}
		if !needSnapshot {
			switch code, err := s.poll(ctx); {
			case err == nil:
				s.touch()
			case code == http.StatusConflict || code == http.StatusGone || errors.Is(err, store.ErrGap):
				// A new epoch, a log that no longer reaches back, or a
				// batch out of order: start again from a snapshot.
				s.logf("starting again from a snapshot: %v", err)
				needSnapshot = true
				s.touch()
			default:
				s.logEvery("leader unreachable: %v", err)
			}
		}
		if s.silentFor() > FailAfter {
			if err := s.takeOver(ctx, false); err != nil {
				return err
			}
		}
		if needSnapshot || s.silentFor() > BeatEvery {
			select { // do not spin while the leader is away
			case <-ctx.Done():
			case <-time.After(BeatEvery / 2):
			}
		}
	}
	return ctx.Err()
}

func (s *Standby) touch() {
	s.mu.Lock()
	s.contact = time.Now()
	s.mu.Unlock()
}

func (s *Standby) silentFor() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Since(s.contact)
}

// logEvery logs a recurring message at most every ten seconds.
func (s *Standby) logEvery(format string, a ...any) {
	s.mu.Lock()
	if s.lastLog == nil {
		s.lastLog = map[string]time.Time{}
	}
	quiet := time.Since(s.lastLog[format]) < 10*time.Second
	if !quiet {
		s.lastLog[format] = time.Now()
	}
	s.mu.Unlock()
	if !quiet {
		s.logf(format, a...)
	}
}

func (s *Standby) promoteRequested() bool {
	_, err := os.Stat(filepath.Join(s.ConfigDir, PromoteFile))
	return err == nil
}

// takeOver asks the witness for the next epoch (or, forced, takes it) and
// hands over to Promote. It returns nil when the vote was refused.
func (s *Standby) takeOver(ctx context.Context, forced bool) error {
	s.mu.Lock()
	epoch := max(s.follow, s.Config.Epoch) + 1
	s.mu.Unlock()
	if !forced {
		if s.Witness == nil {
			s.logEvery("the leader has been silent for %s; without a witness this standby does not take over (sw ha promote --force)", FailAfter)
			return nil
		}
		ctx2, cancel := context.WithTimeout(ctx, CallTimeout)
		reply, err := s.Witness.Call(ctx2, "vote", WitnessRequest{Epoch: epoch, Candidate: s.Config.Self})
		cancel()
		if err != nil {
			s.logEvery("the leader is silent and the witness does not answer: %v", err)
			return nil
		}
		if !reply.OK {
			s.logEvery("the witness refused epoch %d: %s", epoch, reply.Reason)
			return nil
		}
		s.logf("the witness granted epoch %d; taking over", epoch)
	} else {
		os.Remove(filepath.Join(s.ConfigDir, PromoteFile))
		s.logf("taking over at epoch %d on an operator's request, without the witness", epoch)
	}
	s.closeStore()
	return s.Promote(epoch)
}

func (s *Standby) closeStore() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st != nil {
		s.st.Close()
		s.st = nil
	}
}

func (s *Standby) get(ctx context.Context, path string, q url.Values) (*http.Response, error) {
	base := s.LeaderURL()
	if base == "" {
		return nil, errors.New("no other control plane is configured")
	}
	u := strings.TrimSuffix(base, "/") + Prefix + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+s.Config.Token)
	return s.Client.Do(req)
}

// bootstrap replaces the replica with a fresh snapshot of the leader. The
// file it replaces is kept beside it, never deleted.
func (s *Standby) bootstrap(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	res, err := s.get(ctx, "snapshot", nil)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return fmt.Errorf("HTTP %d: %s", res.StatusCode, strings.TrimSpace(string(b)))
	}
	follow, err := strconv.ParseInt(res.Header.Get("X-Shearwater-Leader-Epoch"), 10, 64)
	if err != nil {
		return fmt.Errorf("snapshot without the leader's epoch")
	}
	incoming := filepath.Join(s.DataDir, "state.db.incoming")
	os.Remove(incoming) // our own unfinished download
	f, err := os.OpenFile(incoming, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, res.Body)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && res.ContentLength >= 0 && n != res.ContentLength {
		err = fmt.Errorf("snapshot ended after %d of %d bytes", n, res.ContentLength)
	}
	if err != nil {
		os.Remove(incoming)
		return err
	}
	s.closeStore()
	if err := setAside(filepath.Join(s.DataDir, "state.db"), time.Now()); err != nil {
		return err
	}
	if err := os.Rename(incoming, filepath.Join(s.DataDir, "state.db")); err != nil {
		return err
	}
	st, err := store.Open(filepath.Join(s.DataDir, "state.db"))
	if err != nil {
		return err
	}
	if err := st.Follow(); err != nil {
		st.Close()
		return err
	}
	seq, _ := st.ReplicationState()
	s.mu.Lock()
	s.st, s.follow = st, follow
	s.mu.Unlock()
	s.logf("following the leader at epoch %d from batch %d (snapshot of %d bytes)", follow, seq, n)
	return nil
}

// setAside renames a database and its WAL files out of the way, keeping them.
func setAside(db string, now time.Time) error {
	if _, err := os.Stat(db); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	aside := fmt.Sprintf("%s.replaced-%s", db, now.UTC().Format("20060102T150405.000000000Z"))
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Rename(db+suffix, aside+suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// poll asks for the batches after the last applied one and applies them.
func (s *Standby) poll(ctx context.Context) (int, error) {
	s.mu.Lock()
	st, follow := s.st, s.follow
	s.mu.Unlock()
	seq, _ := st.ReplicationState()
	ctx, cancel := context.WithTimeout(ctx, 3*BeatEvery)
	defer cancel()
	res, err := s.get(ctx, "stream", url.Values{"member": {s.Config.Self}, "after": {strconv.FormatInt(seq, 10)},
		"epoch": {strconv.FormatInt(follow, 10)}})
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 256<<20))
	if err != nil {
		return 0, err
	}
	if res.StatusCode != http.StatusOK {
		return res.StatusCode, fmt.Errorf("HTTP %d: %s", res.StatusCode, strings.TrimSpace(string(body)))
	}
	var reply StreamReply
	if err := json.Unmarshal(body, &reply); err != nil {
		return 0, err
	}
	if reply.Epoch != follow {
		return http.StatusConflict, fmt.Errorf("the leader moved to epoch %d", reply.Epoch)
	}
	for _, b := range reply.Batches {
		if err := st.Apply(b); err != nil {
			return 0, err
		}
	}
	s.mu.Lock()
	s.leader = reply.Leader
	changed := reply.Witness != s.Config.Witness || !sameMembers(reply.Members, s.Config.Members)
	if changed {
		s.Config.Witness, s.Config.Members = reply.Witness, reply.Members
		if s.Witness == nil && reply.Witness != "" {
			s.Witness = &WitnessClient{URL: reply.Witness, Token: s.Config.Token, Client: s.Client}
		}
	}
	cfg := s.Config
	s.mu.Unlock()
	if changed && cfg.validate() == nil {
		// Learn a witness or member added on the leader after this standby joined.
		if err := cfg.Save(s.ConfigDir); err != nil {
			s.logf("cannot save %s: %v", ConfigFile, err)
		}
	}
	return http.StatusOK, nil
}

func sameMembers(a, b []Member) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
