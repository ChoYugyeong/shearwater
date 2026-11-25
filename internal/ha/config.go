// Package ha keeps a second control plane in step with the leader and moves
// the leader role between the two (docs/design-agent-first-ha.md 3).
//
// Two control planes hold the data: the leader serves the API and hands every
// write transaction to its standby before committing it (store.Replicator);
// the standby applies the same batches, runs its own Pods as a worker and
// forwards API connections to the leader. A witness holds no data but gives
// the third vote: it records the leader's term (epoch), which control planes
// are in sync, and grants a standby the next epoch only when the leader has
// stopped renewing its lease. Without a witness nothing takes over on its own;
// `sw ha promote --force` lets an operator decide.
//
// Times are measured on each process's monotonic clock, so the wall-clock
// jumps of a WSL distribution do not shorten a lease.
package ha

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const (
	// Prefix is served by the leader's API server for its standby.
	Prefix = "/shearwater/ha/v1/"
	// WitnessPrefix is served by the witness.
	WitnessPrefix = "/witness/v1/"

	ConfigFile  = "ha.json"
	WitnessFile = "witness.json"

	RoleLeader  = "leader"
	RoleStandby = "standby"
)

// Timing (design 3.5). The leader's lease is shorter than the time after which
// a standby asks for votes, which is shorter than the time the witness waits
// before giving one, so a leader always stops writing before another starts.
var (
	BeatEvery   = time.Second
	LeaseFor    = 5 * time.Second
	FailAfter   = 6 * time.Second
	VoteAfter   = 7 * time.Second
	SyncTimeout = 2 * time.Second
	// CallTimeout bounds one call to the witness.
	CallTimeout = time.Second
)

// Member is one control plane.
type Member struct {
	Name string `json:"name"`
	URL  string `json:"url"` // https://IP:PORT of its API
}

// Config is a control plane's ha.json.
type Config struct {
	Self    string   `json:"self"`
	Members []Member `json:"members"`
	Witness string   `json:"witness,omitempty"` // https://IP:PORT of the witness
	Token   string   `json:"token"`             // shared by the control planes and the witness
	Role    string   `json:"role"`              // leader or standby
	Epoch   int64    `json:"epoch"`
	// Nonce is written when this control plane wins an election and handed
	// to the process it starts next; only that process starts as the leader.
	// Any other start (a crash, a reboot, an upgrade) starts as a standby.
	Nonce string `json:"nonce,omitempty"`
}

// Load reads dir/ha.json. os.ErrNotExist means this control plane is alone.
func Load(dir string) (Config, error) {
	var c Config
	b, err := os.ReadFile(filepath.Join(dir, ConfigFile))
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("%s: %w", ConfigFile, err)
	}
	return c, c.validate()
}

func (c Config) validate() error {
	switch {
	case c.Self == "":
		return errors.New("ha.json: self is empty")
	case c.Token == "":
		return errors.New("ha.json: token is empty")
	case c.Role != RoleLeader && c.Role != RoleStandby:
		return fmt.Errorf("ha.json: role must be leader or standby, got %q", c.Role)
	}
	if _, ok := c.member(c.Self); !ok {
		return fmt.Errorf("ha.json: self %q is not a member", c.Self)
	}
	return nil
}

func (c Config) member(name string) (Member, bool) {
	for _, m := range c.Members {
		if m.Name == name {
			return m, true
		}
	}
	return Member{}, false
}

// Peer is the other control plane, if there is one.
func (c Config) Peer() (Member, bool) {
	for _, m := range c.Members {
		if m.Name != c.Self {
			return m, true
		}
	}
	return Member{}, false
}

// Save writes dir/ha.json atomically, readable only by its owner.
func (c Config) Save(dir string) error {
	if err := c.validate(); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return writeFileSync(filepath.Join(dir, ConfigFile), append(b, '\n'))
}

// NewToken returns a random shared secret.
func NewToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// writeFileSync replaces path with data and makes the replacement durable
// before returning: a vote or an epoch must not be forgotten after a crash.
func writeFileSync(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // left only when the rename failed
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	if d, err := os.Open(filepath.Dir(path)); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}
