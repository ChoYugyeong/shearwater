package store

// Replication log (design-agent-first-ha 3.3). A store that replicates keeps
// every write transaction as one batch of SQL statements with their bound
// values, numbered by seq. The control plane leader hands each batch to its
// Replicator before committing locally; a standby applies the same statements
// in the same order in one transaction (Apply), so both files hold the same
// state and the standby can continue numbering when it takes over.
//
// Statements must be deterministic given their arguments: times, UIDs and
// random values are made in Go and bound, never computed by SQLite (its
// current-time and random functions are not used; a test checks).
//
// Batches are stored in the replog table so a standby that briefly loses the
// stream can continue from where it stopped. A standby that falls behind the
// kept window, or follows a new leader, starts again from a snapshot.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"
)

var (
	// ErrUnavailable: the write was not committed because the control plane
	// could not confirm it with its standby or witness, or is not the leader.
	ErrUnavailable = errors.New("the control plane cannot accept writes right now")
	// ErrGap: a batch does not follow the last applied one.
	ErrGap = errors.New("replication batch out of order")
)

// Arg is one bound value of a statement with its SQL type kept, so the
// standby binds exactly what the leader bound (a nil BLOB stays nil).
type Arg struct {
	T string  `json:"t"` // i integer, f real, s text, b blob, t bool, n null
	I int64   `json:"i,omitempty"`
	F float64 `json:"f,omitempty"`
	S string  `json:"s,omitempty"`
	B []byte  `json:"b,omitempty"`
	Z bool    `json:"z,omitempty"` // T=b: a non-nil empty blob
}

// Stmt is one executed statement.
type Stmt struct {
	SQL  string `json:"sql"`
	Args []Arg  `json:"args,omitempty"`
}

// Batch is one committed write transaction.
type Batch struct {
	Seq   int64  `json:"seq"`
	Epoch int64  `json:"epoch"`
	At    string `json:"at"`
	Stmts []Stmt `json:"stmts"`
}

// Replicator receives the batch of every write transaction of a replicating
// store, while the writer lock is held and before the local commit. A nil
// return lets the transaction commit; an error rolls it back and is returned
// to the writer wrapped in ErrUnavailable.
type Replicator interface {
	Replicate(b Batch) error
}

func toArg(v any) (Arg, error) {
	switch x := v.(type) {
	case nil:
		return Arg{T: "n"}, nil
	case string:
		return Arg{T: "s", S: x}, nil
	case []byte:
		if x == nil {
			return Arg{T: "b"}, nil
		}
		return Arg{T: "b", B: x, Z: len(x) == 0}, nil
	case bool:
		return Arg{T: "t", Z: x}, nil
	case int64:
		return Arg{T: "i", I: x}, nil
	case int:
		return Arg{T: "i", I: int64(x)}, nil
	case int32:
		return Arg{T: "i", I: int64(x)}, nil
	case float64:
		return Arg{T: "f", F: x}, nil
	}
	return Arg{}, fmt.Errorf("store: cannot replicate a %T argument", v)
}

func (a Arg) value() (any, error) {
	switch a.T {
	case "n":
		return nil, nil
	case "s":
		return a.S, nil
	case "b":
		if a.B == nil && a.Z {
			return []byte{}, nil
		}
		if a.B == nil {
			return []byte(nil), nil
		}
		return a.B, nil
	case "t":
		return a.Z, nil
	case "i":
		return a.I, nil
	case "f":
		return a.F, nil
	}
	return nil, fmt.Errorf("store: unknown replicated argument type %q", a.T)
}

// exec runs a write statement of the transaction and records it for the
// replication log when the store replicates.
func (t *Tx) exec(query string, args ...any) (sql.Result, error) {
	if t.capture {
		st := Stmt{SQL: query}
		for _, a := range args {
			v, err := toArg(a)
			if err != nil {
				return nil, err
			}
			st.Args = append(st.Args, v)
		}
		t.stmts = append(t.stmts, st)
	}
	return t.tx.Exec(query, args...)
}

// execBookkeeping is exec for writes that are not API objects (credentials,
// tables, address releases): the transaction commits although no object
// revision moved.
func (t *Tx) execBookkeeping(query string, args ...any) (sql.Result, error) {
	res, err := t.exec(query, args...)
	if err == nil {
		t.bookkeepingChanged = true
	}
	return res, err
}

const replogSchema = `CREATE TABLE IF NOT EXISTS replog (
	seq INTEGER PRIMARY KEY, epoch INTEGER NOT NULL, data BLOB NOT NULL, at TEXT NOT NULL)`

// Replication state kept in meta: the last committed batch and the epoch of
// the leader that wrote it.
const (
	metaSeq   = "repl_seq"
	metaEpoch = "repl_epoch"
)

func readMetaIntOr(q querier, key string, def int64) (int64, error) {
	n, err := readMetaInt(q, key)
	if errors.Is(err, sql.ErrNoRows) {
		return def, nil
	}
	return n, err
}

// Replicate makes the store keep a replication log and hand every write to r
// before committing it (the leader). epoch is the leader's term; batches carry
// it. A nil r keeps the log without waiting for anyone (tests, a leader whose
// standby is out of sync with the witness's consent).
func (s *Store) Replicate(epoch int64, r Replicator) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.initReplog(); err != nil {
		return err
	}
	s.replicating, s.epoch, s.replicator = true, epoch, r
	return nil
}

// SetReplicator changes who confirms batches, keeping the log (the leader
// switching between its standby and solo mode).
func (s *Store) SetReplicator(r Replicator) {
	s.mu.Lock()
	s.replicator = r
	s.mu.Unlock()
}

func (s *Store) initReplog() error {
	if _, err := s.w.Exec(replogSchema); err != nil {
		return err
	}
	seq, err := readMetaIntOr(s.w, metaSeq, 0)
	if err != nil {
		return err
	}
	s.seq = seq
	return nil
}

// Follow prepares a standby: the store only changes through Apply.
func (s *Store) Follow() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.initReplog(); err != nil {
		return err
	}
	epoch, err := readMetaIntOr(s.w, metaEpoch, 0)
	if err != nil {
		return err
	}
	s.epoch, s.standby = epoch, true
	return nil
}

// Fence refuses every further write with ErrUnavailable (a leader that lost
// its lease, or could not tell whether its standby applied a batch).
func (s *Store) Fence(reason string) {
	s.mu.Lock()
	if s.fenced == "" {
		s.fenced = reason
	}
	s.mu.Unlock()
}

// ReplicationState is the last committed batch and its epoch.
func (s *Store) ReplicationState() (seq, epoch int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seq, s.epoch
}

// logBatch adds the batch of t to the replication log inside t and returns it
// (the leader's side of write).
func (s *Store) logBatch(t *Tx) (Batch, error) {
	b := Batch{Seq: s.seq + 1, Epoch: s.epoch, At: t.now, Stmts: t.stmts}
	data, err := json.Marshal(b.Stmts)
	if err != nil {
		return b, err
	}
	if err := recordBatch(t.tx, b.Seq, b.Epoch, data, b.At); err != nil {
		return b, err
	}
	return b, nil
}

func recordBatch(tx *sql.Tx, seq, epoch int64, data []byte, at string) error {
	if _, err := tx.Exec(`INSERT INTO replog (seq, epoch, data, at) VALUES (?, ?, ?, ?)`, seq, epoch, data, at); err != nil {
		return err
	}
	for k, v := range map[string]int64{metaSeq: seq, metaEpoch: epoch} {
		if _, err := tx.Exec(`INSERT INTO meta (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
			k, strconv.FormatInt(v, 10)); err != nil {
			return err
		}
	}
	// Keep the newest batches and the last hour; a standby further behind
	// starts again from a snapshot.
	if seq%100 == 0 {
		cutoff := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
		if _, err := tx.Exec(`DELETE FROM replog WHERE seq <= ? AND at < ?`, seq-10000, cutoff); err != nil {
			return err
		}
	}
	return nil
}

// Apply commits one batch from the leader (the standby's side). It must
// directly follow the last applied batch.
func (s *Store) Apply(b Batch) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.standby {
		return errors.New("store: Apply on a store that is not a standby")
	}
	if b.Seq != s.seq+1 {
		return fmt.Errorf("%w: have %d, got %d", ErrGap, s.seq, b.Seq)
	}
	if b.Epoch < s.epoch {
		return fmt.Errorf("store: batch %d from epoch %d, already following epoch %d", b.Seq, b.Epoch, s.epoch)
	}
	tx, err := s.w.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, st := range b.Stmts {
		args := make([]any, len(st.Args))
		for i, a := range st.Args {
			if args[i], err = a.value(); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(st.SQL, args...); err != nil {
			return fmt.Errorf("apply batch %d: %w", b.Seq, err)
		}
	}
	data, err := json.Marshal(b.Stmts)
	if err != nil {
		return err
	}
	if err := recordBatch(tx, b.Seq, b.Epoch, data, b.At); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.seq, s.epoch = b.Seq, b.Epoch
	s.hubMu.Lock()
	close(s.wake)
	s.wake = make(chan struct{})
	s.hubMu.Unlock()
	return nil
}

// Batches returns committed batches after seq after, oldest first, at most
// limit. ErrCompacted means the log no longer reaches back that far: the
// standby must start from a snapshot.
func (s *Store) Batches(after int64, limit int) ([]Batch, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("store: Batches needs a positive limit, got %d", limit)
	}
	tx, err := s.r.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var oldest sql.NullInt64
	if err := tx.QueryRow(`SELECT min(seq) FROM replog`).Scan(&oldest); err != nil {
		return nil, err
	}
	last, err := readMetaIntOr(tx, metaSeq, 0)
	if err != nil {
		return nil, err
	}
	if after < last && (!oldest.Valid || after+1 < oldest.Int64) {
		return nil, ErrCompacted
	}
	rows, err := tx.Query(`SELECT seq, epoch, data, at FROM replog WHERE seq > ? ORDER BY seq LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Batch
	for rows.Next() {
		var b Batch
		var data []byte
		if err := rows.Scan(&b.Seq, &b.Epoch, &data, &b.At); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, &b.Stmts); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// Snapshot writes a consistent copy of the database to path (which must not
// exist) and returns the batch it includes. Writes wait meanwhile.
func (s *Store) Snapshot(ctx context.Context, path string) (seq, epoch int64, err error) {
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return 0, 0, fmt.Errorf("store: snapshot target %s already exists or cannot be checked (%v)", path, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.w.ExecContext(ctx, `VACUUM INTO ?`, path); err != nil {
		os.Remove(path) // a partial copy of ours
		return 0, 0, err
	}
	return s.seq, s.epoch, nil
}
