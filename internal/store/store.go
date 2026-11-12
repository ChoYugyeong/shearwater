// Package store keeps every API object in one SQLite file (design 5).
//
// A write is one transaction: it takes the next revision, writes the object with
// that revision as its resourceVersion, and appends the change to the log that
// watches read. Nothing is written when nothing changed.
package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

var (
	ErrNotFound = errors.New("not found")
	ErrExists   = errors.New("already exists")
	ErrConflict = errors.New("the object changed since it was read")
	// ErrCompacted: the change log no longer reaches back to the requested revision.
	ErrCompacted = errors.New("the requested revision is older than the change log")
	// ErrIPAllocated means a requested address belongs to another UID.
	ErrIPAllocated = errors.New("the IP address is already allocated")
	// ErrIPExhausted means every candidate address is already allocated.
	ErrIPExhausted = errors.New("no free IP address left")
)

const schemaVersion = "1"

const schema = `
CREATE TABLE IF NOT EXISTS meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS objects (
  grp        TEXT NOT NULL,
  resource   TEXT NOT NULL,
  namespace  TEXT NOT NULL,
  name       TEXT NOT NULL,
  uid        TEXT NOT NULL UNIQUE,
  rev        INTEGER NOT NULL,
  generation INTEGER NOT NULL,
  deleting   INTEGER NOT NULL DEFAULT 0,
  data       BLOB NOT NULL,
  PRIMARY KEY (grp, resource, namespace, name)
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS owners (
  owner_uid TEXT NOT NULL,
  child_uid TEXT NOT NULL,
  PRIMARY KEY (owner_uid, child_uid)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS owners_by_child ON owners (child_uid);

CREATE TABLE IF NOT EXISTS changes (
  rev       INTEGER PRIMARY KEY,
  grp       TEXT NOT NULL,
  resource  TEXT NOT NULL,
  namespace TEXT NOT NULL,
  name      TEXT NOT NULL,
  type      TEXT NOT NULL CHECK (type IN ('ADDED','MODIFIED','DELETED')),
  data      BLOB NOT NULL,
  prev_data BLOB,
  at        TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS changes_by_resource ON changes (grp, resource, namespace, rev);

CREATE TABLE IF NOT EXISTS ip_alloc (
  ip        TEXT PRIMARY KEY,
  kind      TEXT NOT NULL CHECK (kind IN ('service','pod')),
  owner_uid TEXT NOT NULL UNIQUE
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS audit (
  id        INTEGER PRIMARY KEY,
  at        TEXT NOT NULL,
  actor     TEXT NOT NULL,
  via       TEXT NOT NULL,
  verb      TEXT NOT NULL,
  target    TEXT NOT NULL,
  dry_run   INTEGER NOT NULL,
  confirmed INTEGER NOT NULL,
  result    TEXT NOT NULL
);
`

// Key names one object. Namespace is "" for cluster-scoped resources.
type Key struct{ Group, Resource, Namespace, Name string }

func (k Key) String() string {
	s := k.Resource
	if k.Group != "" {
		s += "." + k.Group
	}
	if k.Namespace != "" {
		return s + " " + k.Namespace + "/" + k.Name
	}
	return s + " " + k.Name
}

// Record is one stored object. Data is the full JSON, with Rev as its
// metadata.resourceVersion.
type Record struct {
	Key        Key
	UID        string
	Rev        int64
	Generation int64
	Deleting   bool
	Data       []byte
}

// Put is what a caller hands over to create or update an object. Encode must
// return the object's JSON with the given revision as its resourceVersion; the
// store calls it to learn whether anything changed and to produce the stored bytes.
type Put struct {
	Key        Key
	UID        string
	Generation int64
	Deleting   bool
	Owners     []string // uids from metadata.ownerReferences
	Encode     func(rev int64) ([]byte, error)
}

type Store struct {
	w, r *sql.DB
	mu   sync.Mutex // one writer at a time

	// Change-log retention (design 5): entries newer than KeepFor are kept, and
	// so are the newest KeepChanges; whatever is older than both goes. A negative
	// KeepFor leaves only the count rule (tests). Compaction runs inside a write
	// transaction, at most once per CompactEvery. Set these before the first write.
	KeepChanges  int64
	KeepFor      time.Duration
	CompactEvery time.Duration
	lastCompact  time.Time

	// The watch hub: wake is closed after every commit and replaced. It carries
	// no data; watchers read the change log themselves.
	hubMu sync.Mutex
	wake  chan struct{}
}

// Open opens (and if needed creates) the database at path. The file must be on
// a local filesystem; SQLite locking is unreliable on 9p mounts such as /mnt/c.
func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)"
	w, err := sql.Open("sqlite", dsn+"&_txlock=immediate")
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1)
	r, err := sql.Open("sqlite", dsn)
	if err != nil {
		w.Close()
		return nil, err
	}
	r.SetMaxOpenConns(4)
	s := &Store{w: w, r: r, KeepChanges: 10000, KeepFor: time.Hour, CompactEvery: time.Minute, wake: make(chan struct{})}
	if err := s.init(); err != nil {
		s.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return s, nil
}

func (s *Store) init() error {
	// A database that already has the current schema is opened without writing.
	var v string
	err := s.w.QueryRow(`SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&v)
	if err == nil {
		if v != schemaVersion {
			return fmt.Errorf("database has schema version %s, this binary needs %s", v, schemaVersion)
		}
		return nil
	}
	uid := make([]byte, 16)
	rand.Read(uid)
	tx, err := s.w.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(schema); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO meta (key, value) VALUES ('schema_version', ?), ('cluster_uid', ?), ('rev', '0'), ('compacted_rev', '0')`,
		schemaVersion, hex.EncodeToString(uid)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Close() error {
	return errors.Join(s.r.Close(), s.w.Close())
}

type querier interface {
	QueryRow(query string, args ...any) *sql.Row
}

func readMetaInt(q querier, key string) (int64, error) {
	var v string
	if err := q.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&v); err != nil {
		return 0, err
	}
	return strconv.ParseInt(v, 10, 64)
}

func readRev(q querier) (int64, error) { return readMetaInt(q, "rev") }

func getRecord(q querier, k Key) (Record, error) {
	rec := Record{Key: k}
	err := q.QueryRow(`SELECT uid, rev, generation, deleting, data FROM objects WHERE grp = ? AND resource = ? AND namespace = ? AND name = ?`,
		k.Group, k.Resource, k.Namespace, k.Name).Scan(&rec.UID, &rec.Rev, &rec.Generation, &rec.Deleting, &rec.Data)
	if errors.Is(err, sql.ErrNoRows) {
		return rec, ErrNotFound
	}
	return rec, err
}

// Rev returns the current revision.
func (s *Store) Rev() (int64, error) { return readRev(s.r) }

// Get returns one object.
func (s *Store) Get(k Key) (Record, error) { return getRecord(s.r, k) }

// PodAtIP reads a Pod only while its UID owns the node agent's reservation.
// The join checks the object and reservation in the same SQLite snapshot.
// Pod status alone is insufficient: it can be edited or lag behind cleanup.
func (s *Store) PodAtIP(ctx context.Context, namespace, name, ip string) (Record, error) {
	rec := Record{Key: Key{Resource: "pods", Namespace: namespace, Name: name}}
	err := s.r.QueryRowContext(ctx, `SELECT o.uid, o.rev, o.generation, o.deleting, o.data
		FROM objects o JOIN ip_alloc a ON a.owner_uid = o.uid
		WHERE o.grp = '' AND o.resource = 'pods' AND o.namespace = ? AND o.name = ? AND a.kind = 'pod' AND a.ip = ?`,
		namespace, name, ip).Scan(&rec.UID, &rec.Rev, &rec.Generation, &rec.Deleting, &rec.Data)
	if errors.Is(err, sql.ErrNoRows) {
		return rec, ErrNotFound
	}
	return rec, err
}

// List returns the objects of one resource, ordered by namespace and name, and
// the revision of the snapshot they were read from. An empty namespace lists all
// namespaces. The revision is the store's revision at that moment, not the
// highest among the items, so an empty list has one too.
func (s *Store) List(group, resource, namespace string) ([]Record, int64, error) {
	tx, err := s.r.Begin()
	if err != nil {
		return nil, 0, err
	}
	defer tx.Rollback()
	rev, err := readRev(tx)
	if err != nil {
		return nil, 0, err
	}
	q := `SELECT namespace, name, uid, rev, generation, deleting, data FROM objects WHERE grp = ? AND resource = ?`
	args := []any{group, resource}
	if namespace != "" {
		q += ` AND namespace = ?`
		args = append(args, namespace)
	}
	rows, err := tx.Query(q+` ORDER BY namespace, name`, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []Record
	for rows.Next() {
		rec := Record{Key: Key{Group: group, Resource: resource}}
		if err := rows.Scan(&rec.Key.Namespace, &rec.Key.Name, &rec.UID, &rec.Rev, &rec.Generation, &rec.Deleting, &rec.Data); err != nil {
			return nil, 0, err
		}
		out = append(out, rec)
	}
	return out, rev, rows.Err()
}

// Tx is a write transaction. Every object it changes gets its own revision.
type Tx struct {
	tx                 *sql.Tx
	rev                int64
	start              int64
	now                string
	bookkeepingChanged bool
}

// Write runs fn in one transaction. Errors, dry runs and no-ops roll back all
// changes, including address reservations. Bookkeeping can commit on its own
// without advancing the object revision or notifying watchers.
func (s *Store) Write(dryRun bool, fn func(tx *Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sqlTx, err := s.w.Begin()
	if err != nil {
		return err
	}
	defer sqlTx.Rollback()
	rev, err := readRev(sqlTx)
	if err != nil {
		return err
	}
	t := &Tx{tx: sqlTx, rev: rev, start: rev, now: time.Now().UTC().Format(time.RFC3339)}
	if err := fn(t); err != nil {
		return err
	}
	if dryRun {
		return nil
	}
	if t.rev == t.start {
		if t.bookkeepingChanged {
			return sqlTx.Commit() // Internal bookkeeping has no watch event.
		}
		return nil
	}
	if _, err := sqlTx.Exec(`UPDATE meta SET value = ? WHERE key = 'rev'`, strconv.FormatInt(t.rev, 10)); err != nil {
		return err
	}
	if now := time.Now(); now.Sub(s.lastCompact) >= s.CompactEvery {
		if err := t.compact(s.KeepChanges, now.Add(-s.KeepFor)); err != nil {
			return err
		}
		s.lastCompact = now
	}
	if err := sqlTx.Commit(); err != nil {
		return err
	}
	s.hubMu.Lock()
	close(s.wake)
	s.wake = make(chan struct{})
	s.hubMu.Unlock()
	return nil
}

// Changed returns a channel that is closed at the next commit. Take the
// channel first and read the change log after; a commit that lands in between
// then still wakes the reader.
func (s *Store) Changed() <-chan struct{} {
	s.hubMu.Lock()
	defer s.hubMu.Unlock()
	return s.wake
}

// compact drops change-log entries that are older than cutoff and not among
// the newest keep, and records how far the log now reaches back. The delete and
// the new compacted_rev are part of the surrounding write transaction.
func (t *Tx) compact(keep int64, cutoff time.Time) error {
	to := t.rev - keep
	var firstRecent int64
	err := t.tx.QueryRow(`SELECT rev FROM changes WHERE at >= ? ORDER BY rev LIMIT 1`, cutoff.UTC().Format(time.RFC3339)).Scan(&firstRecent)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return err
	case firstRecent-1 < to:
		to = firstRecent - 1
	}
	compacted, err := readMetaInt(t.tx, "compacted_rev")
	if err != nil || to <= compacted {
		return err
	}
	if _, err := t.tx.Exec(`DELETE FROM changes WHERE rev <= ?`, to); err != nil {
		return err
	}
	_, err = t.tx.Exec(`UPDATE meta SET value = ? WHERE key = 'compacted_rev'`, strconv.FormatInt(to, 10))
	return err
}

// Change is one entry of the change log.
type Change struct {
	Rev  int64
	Key  Key
	Type string // ADDED, MODIFIED or DELETED
	Data []byte // the object at Rev (for DELETED: as it was, carrying Rev)
	Prev []byte // the object before the change; nil for ADDED
}

// Compacted returns the revision up to which the change log has been dropped.
// Changes after it are all still there.
func (s *Store) Compacted() (int64, error) { return readMetaInt(s.r, "compacted_rev") }

// Changes returns the changes to one resource after revision after, oldest
// first, at most limit of them. An empty namespace means all namespaces. next is
// where to continue: the last returned revision when the batch is full,
// otherwise the revision of the snapshot, so a reader with nothing to read
// still moves forward. ErrCompacted means entries after `after` may be gone;
// the reader must list again.
func (s *Store) Changes(group, resource, namespace string, after int64, limit int) (changes []Change, next int64, err error) {
	if limit <= 0 {
		// Not "no limit": a batch must be bounded, so a caller that asks for
		// nothing has a bug. An error says so; SQL's LIMIT 0 would say "no changes".
		return nil, 0, fmt.Errorf("store: Changes needs a positive limit, got %d", limit)
	}
	tx, err := s.r.Begin()
	if err != nil {
		return nil, 0, err
	}
	defer tx.Rollback()
	compacted, err := readMetaInt(tx, "compacted_rev")
	if err != nil {
		return nil, 0, err
	}
	if after < compacted {
		return nil, 0, ErrCompacted
	}
	if next, err = readRev(tx); err != nil {
		return nil, 0, err
	}
	q := `SELECT rev, namespace, name, type, data, prev_data FROM changes WHERE grp = ? AND resource = ?`
	args := []any{group, resource}
	if namespace != "" {
		q += ` AND namespace = ?`
		args = append(args, namespace)
	}
	rows, err := tx.Query(q+` AND rev > ? ORDER BY rev LIMIT ?`, append(args, after, limit)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		c := Change{Key: Key{Group: group, Resource: resource}}
		if err := rows.Scan(&c.Rev, &c.Key.Namespace, &c.Key.Name, &c.Type, &c.Data, &c.Prev); err != nil {
			return nil, 0, err
		}
		changes = append(changes, c)
	}
	if len(changes) == limit {
		next = changes[len(changes)-1].Rev
	}
	return changes, next, rows.Err()
}

func (t *Tx) Get(k Key) (Record, error) { return getRecord(t.tx, k) }

// DeploymentRevision is the highest rollout number assigned to the given
// Deployment UID. It survives pruning or manual deletion of ReplicaSets.
func (t *Tx) DeploymentRevision(uid string) (int64, error) {
	n, err := readMetaInt(t.tx, "deployment_revision/"+uid)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return n, err
}

func (t *Tx) SetDeploymentRevision(uid string, revision int64) error {
	_, err := t.tx.Exec(`INSERT INTO meta (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		"deployment_revision/"+uid, strconv.FormatInt(revision, 10))
	t.bookkeepingChanged = t.bookkeepingChanged || err == nil
	return err
}

// Objects lists the objects visible to this write transaction.
func (t *Tx) Objects() ([]Record, error) {
	rows, err := t.tx.Query(`SELECT grp, resource, namespace, name, uid, rev, generation, deleting, data
		FROM objects ORDER BY grp, resource, namespace, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Record
	for rows.Next() {
		var r Record
		if err := rows.Scan(&r.Key.Group, &r.Key.Resource, &r.Key.Namespace, &r.Key.Name,
			&r.UID, &r.Rev, &r.Generation, &r.Deleting, &r.Data); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// NamespaceEmpty reports whether no namespaced object lives in ns.
func (t *Tx) NamespaceEmpty(ns string) (bool, error) {
	var n int
	err := t.tx.QueryRow(`SELECT count(*) FROM (SELECT 1 FROM objects WHERE namespace = ? LIMIT 1)`, ns).Scan(&n)
	return n == 0, err
}

func (t *Tx) logChange(k Key, rev int64, typ string, data, prev []byte) error {
	_, err := t.tx.Exec(`INSERT INTO changes (rev, grp, resource, namespace, name, type, data, prev_data, at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rev, k.Group, k.Resource, k.Namespace, k.Name, typ, data, prev, t.now)
	return err
}

func (t *Tx) setOwners(uid string, owners []string, isNew bool) error {
	if !isNew {
		if _, err := t.tx.Exec(`DELETE FROM owners WHERE child_uid = ?`, uid); err != nil {
			return err
		}
	}
	for _, o := range owners {
		if _, err := t.tx.Exec(`INSERT OR IGNORE INTO owners (owner_uid, child_uid) VALUES (?, ?)`, o, uid); err != nil {
			return err
		}
	}
	return nil
}

// Create stores a new object. ErrExists if the key is taken.
func (t *Tx) Create(p Put) (Record, error) {
	if _, err := t.Get(p.Key); err == nil {
		return Record{}, ErrExists
	} else if !errors.Is(err, ErrNotFound) {
		return Record{}, err
	}
	rev := t.rev + 1
	data, err := p.Encode(rev)
	if err != nil {
		return Record{}, err
	}
	k := p.Key
	if _, err := t.tx.Exec(`INSERT INTO objects (grp, resource, namespace, name, uid, rev, generation, deleting, data) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		k.Group, k.Resource, k.Namespace, k.Name, p.UID, rev, p.Generation, p.Deleting, data); err != nil {
		return Record{}, err
	}
	if err := t.logChange(k, rev, "ADDED", data, nil); err != nil {
		return Record{}, err
	}
	if err := t.setOwners(p.UID, p.Owners, true); err != nil {
		return Record{}, err
	}
	t.rev = rev
	return Record{Key: k, UID: p.UID, Rev: rev, Generation: p.Generation, Deleting: p.Deleting, Data: data}, nil
}

// Update replaces an object that is still at expectRev (0 skips the check).
// ErrNotFound if it is gone, ErrConflict if it moved on. When the new content
// equals the stored content the revision does not advance and changed is false.
func (t *Tx) Update(p Put, expectRev int64) (rec Record, changed bool, err error) {
	old, err := t.Get(p.Key)
	if err != nil {
		return Record{}, false, err
	}
	if expectRev != 0 && old.Rev != expectRev {
		return Record{}, false, ErrConflict
	}
	same, err := p.Encode(old.Rev)
	if err != nil {
		return Record{}, false, err
	}
	if bytes.Equal(same, old.Data) && p.Deleting == old.Deleting && p.Generation == old.Generation {
		return old, false, nil
	}
	rev := t.rev + 1
	data, err := p.Encode(rev)
	if err != nil {
		return Record{}, false, err
	}
	k := p.Key
	res, err := t.tx.Exec(`UPDATE objects SET rev = ?, generation = ?, deleting = ?, data = ? WHERE grp = ? AND resource = ? AND namespace = ? AND name = ? AND rev = ?`,
		rev, p.Generation, p.Deleting, data, k.Group, k.Resource, k.Namespace, k.Name, old.Rev)
	if err != nil {
		return Record{}, false, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return Record{}, false, ErrConflict
	}
	if err := t.logChange(k, rev, "MODIFIED", data, old.Data); err != nil {
		return Record{}, false, err
	}
	if err := t.setOwners(old.UID, p.Owners, false); err != nil {
		return Record{}, false, err
	}
	t.rev = rev
	return Record{Key: k, UID: old.UID, Rev: rev, Generation: p.Generation, Deleting: p.Deleting, Data: data}, true, nil
}

// Delete removes an object that is still at expectRev (0 skips the check).
// encode produces the object as it goes into the DELETED change, carrying the
// revision of the deletion. The returned record holds those bytes.
func (t *Tx) Delete(k Key, expectRev int64, encode func(rev int64) ([]byte, error)) (Record, error) {
	old, err := t.Get(k)
	if err != nil {
		return Record{}, err
	}
	if expectRev != 0 && old.Rev != expectRev {
		return Record{}, ErrConflict
	}
	rev := t.rev + 1
	data, err := encode(rev)
	if err != nil {
		return Record{}, err
	}
	if _, err := t.tx.Exec(`DELETE FROM objects WHERE grp = ? AND resource = ? AND namespace = ? AND name = ?`,
		k.Group, k.Resource, k.Namespace, k.Name); err != nil {
		return Record{}, err
	}
	if err := t.logChange(k, rev, "DELETED", data, old.Data); err != nil {
		return Record{}, err
	}
	if _, err := t.tx.Exec(`DELETE FROM owners WHERE child_uid = ?`, old.UID); err != nil {
		return Record{}, err
	}
	if k.Group == "" && k.Resource == "services" {
		// API DELETE, clearing the last finalizer and garbage collection all
		// converge here. A deletionTimestamp alone keeps the reservation.
		// Release by UID so that deleting an old object can never return
		// the IP of a different Service. The proxy checks the committed owner
		// to fence a listener that still has this IP bound after the commit.
		if _, err := t.tx.Exec(`DELETE FROM ip_alloc WHERE kind = 'service' AND owner_uid = ?`, old.UID); err != nil {
			return Record{}, err
		}
	}
	if k.Group == "apps" && k.Resource == "deployments" {
		if _, err := t.tx.Exec(`DELETE FROM meta WHERE key = ?`, "deployment_revision/"+old.UID); err != nil {
			return Record{}, err
		}
	}
	t.rev = rev
	old.Rev, old.Data = rev, data
	return old, nil
}

// ---- address allocation (design 7) ----
//
// Addresses are not API objects: taking or returning one moves no revision and
// wakes no watcher.

// AllocIP returns the address uid holds, or gives it the first free candidate.
// The node agent uses this to persist Pod IPs across process and host restarts.
// Use Tx.AllocIP to commit a Service IP together with its API object.
func (s *Store) AllocIP(kind, uid string, candidates func(yield func(string) bool)) (string, error) {
	var ip string
	err := s.Write(false, func(tx *Tx) (err error) {
		ip, err = tx.AllocIP(kind, uid, "", candidates)
		return err
	})
	if err != nil {
		return "", err
	}
	return ip, nil
}

// AllocIP reserves requested for uid, or picks the first free candidate when
// requested is empty. An existing reservation for the same UID and kind is
// idempotent. An explicit request must still match that reservation, so a
// retry cannot silently change the address. Callers validate address ranges
// and provide canonical address strings.
func (t *Tx) AllocIP(kind, uid, requested string, candidates func(yield func(string) bool)) (string, error) {
	if uid == "" {
		return "", errors.New("store: AllocIP needs an owner uid")
	}
	var ip, heldKind string
	err := t.tx.QueryRow(`SELECT ip, kind FROM ip_alloc WHERE owner_uid = ?`, uid).Scan(&ip, &heldKind)
	if err == nil {
		if heldKind != kind || (requested != "" && requested != ip) {
			return "", fmt.Errorf("store: UID %q already holds %s IP %s", uid, heldKind, ip)
		}
		return ip, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if requested != "" {
		candidates = slices.Values([]string{requested})
	}
	// The same primary key covers Pod and Service addresses. Target just the
	// IP constraint so a collision can be skipped or reported to the client
	// without swallowing unrelated SQLite errors. Write holds the SQLite
	// writer transaction through both this insert and the object change.
	for ip := range candidates {
		res, err := t.tx.Exec(`INSERT INTO ip_alloc (ip, kind, owner_uid) VALUES (?, ?, ?) ON CONFLICT(ip) DO NOTHING`,
			ip, kind, uid)
		if err != nil {
			return "", err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return "", err
		}
		if n == 1 {
			t.bookkeepingChanged = true
			return ip, nil
		}
	}
	if requested != "" {
		return "", fmt.Errorf("store: %w: %s", ErrIPAllocated, requested)
	}
	return "", fmt.Errorf("store: %w for %s", ErrIPExhausted, kind)
}

// ReleaseIP returns a Pod address after node cleanup. Holding none is fine.
// Service addresses are released by Tx.Delete with the object.
func (s *Store) ReleaseIP(uid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.w.Exec(`DELETE FROM ip_alloc WHERE kind = 'pod' AND owner_uid = ?`, uid)
	return err
}

// IPs returns the allocations of one kind: owner uid -> address.
func (s *Store) IPs(kind string) (map[string]string, error) {
	rows, err := s.r.Query(`SELECT owner_uid, ip FROM ip_alloc WHERE kind = ?`, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var uid, ip string
		if err := rows.Scan(&uid, &ip); err != nil {
			return nil, err
		}
		out[uid] = ip
	}
	return out, rows.Err()
}
