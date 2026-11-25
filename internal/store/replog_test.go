package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// collect is a Replicator that keeps the batches (and can refuse them).
type collect struct {
	batches []Batch
	refuse  error
}

func (c *collect) Replicate(b Batch) error {
	if c.refuse != nil {
		return c.refuse
	}
	c.batches = append(c.batches, b)
	return nil
}

// dump renders every replicated table so two files can be compared.
func dump(t *testing.T, s *Store) string {
	t.Helper()
	var out []string
	for _, q := range []string{
		`SELECT key, value FROM meta WHERE key NOT IN ('cluster_uid') ORDER BY key`,
		`SELECT grp, resource, namespace, name, uid, rev, generation, deleting, hex(data) FROM objects ORDER BY 1, 2, 3, 4`,
		`SELECT owner_uid, child_uid FROM owners ORDER BY 1, 2`,
		`SELECT rev, grp, resource, namespace, name, type, hex(data), quote(prev_data), at FROM changes ORDER BY rev`,
		`SELECT ip, kind, owner_uid FROM ip_alloc ORDER BY ip`,
		`SELECT rev, actor, via FROM change_actors ORDER BY rev`,
		`SELECT id, kind, name, groups, hash, created, expires, quote(data) FROM credentials ORDER BY id`,
		`SELECT id, hex(data) FROM agent_operations ORDER BY id`,
		`SELECT seq, epoch, hex(data), at FROM replog ORDER BY seq`,
	} {
		table := strings.Fields(q[strings.Index(q, " FROM ")+6:])[0]
		if ok, err := hasTable(s.r, table); err != nil || !ok {
			out = append(out, "no table "+table)
			continue
		}
		rows, err := s.r.Query(q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			out = append(out, fmt.Sprint(vals...))
		}
		rows.Close()
	}
	return strings.Join(out, "\n")
}

func put(name, body string) Put {
	return Put{Key: Key{Resource: "pods", Namespace: "default", Name: name}, UID: "uid-" + name,
		Encode: func(rev int64) ([]byte, error) { return []byte(fmt.Sprintf(`{"rv":%d,%s}`, rev, body)), nil }}
}

// workload runs every kind of write the store has.
func workload(t *testing.T, s *Store) {
	t.Helper()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.InitCredentials())
	must(s.InitOperations())
	must(s.InitActors())
	must(s.WriteAs(Actor{Name: "alice", Via: "kubectl"}, false, func(tx *Tx) error {
		if _, err := tx.Create(put("a", `"x":1`)); err != nil {
			return err
		}
		p := put("b", `"x":2`)
		p.Owners = []string{"uid-a"}
		_, err := tx.Create(p)
		return err
	}))
	must(s.Write(false, func(tx *Tx) error {
		_, _, err := tx.Update(put("a", `"x":3`), 0)
		return err
	}))
	ip, err := s.AllocIP("pod", "uid-a", func(yield func(string) bool) { yield("10.244.0.5") })
	must(err)
	if ip != "10.244.0.5" {
		t.Fatal(ip)
	}
	must(s.PutCredential(Credential{ID: "c1", Kind: "user", Name: "alice", Hash: "h", Created: time.Unix(1700000000, 0)}))
	must(s.Write(false, func(tx *Tx) error {
		if err := tx.PutOperation("op1", []byte(`{"state":"planned"}`), true); err != nil {
			return err
		}
		return tx.SetDeploymentRevision("dep-1", 4)
	}))
	must(s.Write(false, func(tx *Tx) error {
		_, err := tx.Delete(Key{Resource: "pods", Namespace: "default", Name: "b"}, 0,
			func(rev int64) ([]byte, error) { return []byte(fmt.Sprintf(`{"rv":%d}`, rev)), nil })
		return err
	}))
	must(s.ReleaseIP("uid-a"))
	if _, err := s.DeleteCredentials("user", "alice"); err != nil {
		t.Fatal(err)
	}
	// A write that changes nothing makes no batch.
	must(s.Write(false, func(tx *Tx) error {
		_, _, err := tx.Update(put("a", `"x":3`), 0)
		return err
	}))
}

func TestStandbyAppliesTheSameState(t *testing.T) {
	leader, standby := open(t), open(t)
	c := &collect{}
	if err := leader.Replicate(1, c); err != nil {
		t.Fatal(err)
	}
	if err := standby.Follow(); err != nil {
		t.Fatal(err)
	}
	leader.KeepChanges, leader.KeepFor, leader.CompactEvery = 3, -time.Hour, 0 // compaction is replicated too
	workload(t, leader)
	if len(c.batches) == 0 {
		t.Fatal("no batches")
	}
	for i, b := range c.batches {
		if b.Seq != int64(i+1) || b.Epoch != 1 {
			t.Fatalf("batch %d: seq %d epoch %d", i, b.Seq, b.Epoch)
		}
		if err := standby.Apply(b); err != nil {
			t.Fatalf("apply %d: %v", b.Seq, err)
		}
	}
	if got, want := dump(t, standby), dump(t, leader); got != want {
		t.Fatalf("standby differs from leader\n--- leader\n%s\n--- standby\n%s", want, got)
	}
	if seq, epoch := standby.ReplicationState(); seq != int64(len(c.batches)) || epoch != 1 {
		t.Fatalf("standby at %d/%d", seq, epoch)
	}
	// Out of order, repeated and older-epoch batches are refused.
	last := c.batches[len(c.batches)-1]
	if err := standby.Apply(last); !errors.Is(err, ErrGap) {
		t.Fatalf("repeat: %v", err)
	}
	if err := standby.Apply(Batch{Seq: last.Seq + 1, Epoch: 0}); err == nil {
		t.Fatal("older epoch accepted")
	}
	// A standby takes no other writes.
	if err := standby.Write(false, func(tx *Tx) error { _, err := tx.Create(put("z", `"x":0`)); return err }); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("standby write: %v", err)
	}
}

func TestRefusedBatchRollsBack(t *testing.T) {
	s := open(t)
	c := &collect{}
	if err := s.Replicate(2, c); err != nil {
		t.Fatal(err)
	}
	if err := s.Write(false, func(tx *Tx) error { _, err := tx.Create(put("a", `"x":1`)); return err }); err != nil {
		t.Fatal(err)
	}
	before := dump(t, s)
	c.refuse = errors.New("standby did not confirm")
	err := s.Write(false, func(tx *Tx) error { _, err := tx.Create(put("b", `"x":2`)); return err })
	if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "standby did not confirm") {
		t.Fatalf("got %v", err)
	}
	if dump(t, s) != before {
		t.Fatal("a refused batch changed the file")
	}
	if seq, _ := s.ReplicationState(); seq != 1 {
		t.Fatalf("seq %d", seq)
	}
	c.refuse = nil
	if err := s.Write(false, func(tx *Tx) error { _, err := tx.Create(put("b", `"x":2`)); return err }); err != nil {
		t.Fatal(err)
	}
	if got := c.batches[len(c.batches)-1].Seq; got != 2 {
		t.Fatalf("next seq %d, want 2 (no hole after a refusal)", got)
	}
	s.Fence("lease lost")
	if err := s.Write(false, func(tx *Tx) error { _, err := tx.Create(put("c", `"x":3`)); return err }); !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "lease lost") {
		t.Fatalf("fenced: %v", err)
	}
}

func TestSnapshotThenFollow(t *testing.T) {
	leader := open(t)
	c := &collect{}
	if err := leader.Replicate(3, c); err != nil {
		t.Fatal(err)
	}
	workload(t, leader)
	path := filepath.Join(t.TempDir(), "snap.db")
	seq, epoch, err := leader.Snapshot(context.Background(), path)
	if err != nil || seq != int64(len(c.batches)) || epoch != 3 {
		t.Fatalf("snapshot %d/%d %v", seq, epoch, err)
	}
	if _, _, err := leader.Snapshot(context.Background(), path); err == nil {
		t.Fatal("snapshot over an existing file")
	}
	standby, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer standby.Close()
	if err := standby.Follow(); err != nil {
		t.Fatal(err)
	}
	if got, _ := standby.ReplicationState(); got != seq {
		t.Fatalf("standby starts at %d, want %d", got, seq)
	}
	n := len(c.batches)
	if err := leader.WriteAs(Actor{Name: "bob", Via: "api"}, false, func(tx *Tx) error { _, err := tx.Create(put("later", `"x":9`)); return err }); err != nil {
		t.Fatal(err)
	}
	for _, b := range c.batches[n:] {
		if err := standby.Apply(b); err != nil {
			t.Fatal(err)
		}
	}
	if dump(t, standby) != dump(t, leader) {
		t.Fatal("standby differs after snapshot and batches")
	}
	// The kept log serves batches; older ones are reported as compacted.
	got, err := leader.Batches(seq, 10)
	if err != nil || len(got) != 1 || got[0].Seq != seq+1 {
		t.Fatalf("batches after %d: %+v %v", seq, got, err)
	}
	if _, err := leader.w.Exec(`DELETE FROM replog WHERE seq <= ?`, seq); err != nil {
		t.Fatal(err)
	}
	if _, err := leader.Batches(seq-1, 10); !errors.Is(err, ErrCompacted) {
		t.Fatalf("want ErrCompacted, got %v", err)
	}
	if got, err := leader.Batches(seq+1, 10); err != nil || len(got) != 0 {
		t.Fatalf("caught up: %+v %v", got, err)
	}
}

func TestArgsKeepTheirTypes(t *testing.T) {
	for _, v := range []any{nil, "s", []byte(nil), []byte{}, []byte("x"), true, false, int64(-3), 7, int32(2), 1.5} {
		a, err := toArg(v)
		if err != nil {
			t.Fatal(err)
		}
		got, err := a.value()
		if err != nil {
			t.Fatal(err)
		}
		if b, ok := v.([]byte); ok {
			g := got.([]byte)
			if (b == nil) != (g == nil) || !slices.Equal(b, g) {
				t.Fatalf("%#v became %#v", v, got)
			}
			continue
		}
		if i, ok := v.(int); ok {
			v = int64(i)
		}
		if i, ok := v.(int32); ok {
			v = int64(i)
		}
		if got != v {
			t.Fatalf("%#v became %#v", v, got)
		}
	}
	if _, err := toArg(time.Now()); err == nil {
		t.Fatal("a time.Time argument would replicate in a driver-specific form")
	}
}

// Replicated statements must not compute values inside SQLite.
func TestStoreSQLIsDeterministic(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		src := strings.ToLower(string(b))
		for _, bad := range []string{"current_timestamp", "current_date", "current_time", "random(", "randomblob(", "datetime('now", "julianday('now", "strftime(", "changes()", "last_insert_rowid()"} {
			if strings.Contains(src, bad) {
				t.Errorf("%s uses %s, which a standby would compute differently", f, bad)
			}
		}
	}
}
