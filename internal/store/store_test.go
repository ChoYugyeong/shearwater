package store

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func key(name string) Key { return Key{Resource: "pods", Namespace: "default", Name: name} }

// obj encodes a tiny object whose content is body.
func obj(name, body string, owners ...string) Put {
	return Put{Key: key(name), UID: "uid-" + name, Generation: 1, Owners: owners,
		Encode: func(rev int64) ([]byte, error) {
			return []byte(fmt.Sprintf(`{"metadata":{"name":%q,"resourceVersion":"%d"},"body":%q}`, name, rev, body)), nil
		}}
}

func count(t *testing.T, s *Store, table string) (n int) {
	t.Helper()
	if err := s.r.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func rev(t *testing.T, s *Store) int64 {
	t.Helper()
	r, err := s.Rev()
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRevisionsGrowOnePerObject(t *testing.T) {
	s := open(t)
	var a, b Record
	err := s.Write(false, func(tx *Tx) (err error) {
		if a, err = tx.Create(obj("a", "1")); err != nil {
			return err
		}
		b, err = tx.Create(obj("b", "1"))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if a.Rev != 1 || b.Rev != 2 || rev(t, s) != 2 {
		t.Fatalf("two objects in one transaction must get revisions 1 and 2; got %d, %d, store %d", a.Rev, b.Rev, rev(t, s))
	}
	got, err := s.Get(key("b"))
	if err != nil || string(got.Data) != `{"metadata":{"name":"b","resourceVersion":"2"},"body":"1"}` {
		t.Fatalf("stored bytes must carry the revision: %s %v", got.Data, err)
	}
	if err := s.Write(false, func(tx *Tx) error { _, err := tx.Create(obj("a", "again")); return err }); !errors.Is(err, ErrExists) {
		t.Fatalf("creating an existing key: got %v, want ErrExists", err)
	}
	if rev(t, s) != 2 || count(t, s, "changes") != 2 {
		t.Fatal("a failed create must leave no trace")
	}
}

func TestSameContentWritesNothing(t *testing.T) {
	s := open(t)
	s.Write(false, func(tx *Tx) error { _, err := tx.Create(obj("a", "1")); return err })
	var changed bool
	var rec Record
	err := s.Write(false, func(tx *Tx) (err error) {
		rec, changed, err = tx.Update(obj("a", "1"), 1)
		return err
	})
	if err != nil || changed || rec.Rev != 1 || rev(t, s) != 1 || count(t, s, "changes") != 1 {
		t.Fatalf("an update with identical content must not advance anything: changed=%v rev=%d store=%d changes=%d err=%v",
			changed, rec.Rev, rev(t, s), count(t, s, "changes"), err)
	}
	err = s.Write(false, func(tx *Tx) (err error) {
		rec, changed, err = tx.Update(obj("a", "2"), 1)
		return err
	})
	if err != nil || !changed || rec.Rev != 2 {
		t.Fatalf("a real update must advance the revision: changed=%v rev=%d err=%v", changed, rec.Rev, err)
	}
	var typ string
	var prev []byte
	s.r.QueryRow(`SELECT type, prev_data FROM changes WHERE rev = 2`).Scan(&typ, &prev)
	if typ != "MODIFIED" || string(prev) != `{"metadata":{"name":"a","resourceVersion":"1"},"body":"1"}` {
		t.Fatalf("the change log must keep the object as it was before: %s %s", typ, prev)
	}
}

func TestStaleUpdateRollsBackEverything(t *testing.T) {
	s := open(t)
	s.Write(false, func(tx *Tx) error { _, err := tx.Create(obj("a", "1")); return err })
	s.Write(false, func(tx *Tx) error { _, _, err := tx.Update(obj("a", "2"), 1); return err })

	err := s.Write(false, func(tx *Tx) error {
		if _, err := tx.Create(obj("b", "1")); err != nil { // would be revision 3
			return err
		}
		_, _, err := tx.Update(obj("a", "3"), 1) // stale: a is at 2
		return err
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("got %v, want ErrConflict", err)
	}
	if _, err := s.Get(key("b")); !errors.Is(err, ErrNotFound) {
		t.Fatal("the create in the failed transaction must be rolled back")
	}
	if rev(t, s) != 2 || count(t, s, "changes") != 2 {
		t.Fatalf("revision and change log must be untouched: rev=%d changes=%d", rev(t, s), count(t, s, "changes"))
	}
	if err := s.Write(false, func(tx *Tx) error { _, _, err := tx.Update(obj("gone", "1"), 0); return err }); !errors.Is(err, ErrNotFound) {
		t.Fatalf("updating a missing object: got %v, want ErrNotFound", err)
	}
}

func TestDryRunLeavesNoTrace(t *testing.T) {
	s := open(t)
	s.Write(false, func(tx *Tx) error { _, err := tx.Create(obj("a", "1", "owner-1")); return err })
	before := []int{count(t, s, "objects"), count(t, s, "changes"), count(t, s, "owners")}

	var created Record
	err := s.Write(true, func(tx *Tx) (err error) {
		if created, err = tx.Create(obj("b", "1", "owner-2")); err != nil {
			return err
		}
		if _, _, err = tx.Update(obj("a", "2"), 1); err != nil {
			return err
		}
		_, err = tx.Delete(key("a"), 0, obj("a", "2").Encode)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.Rev != 2 {
		t.Fatalf("a dry run still reports the object it would have stored, got rev %d", created.Rev)
	}
	after := []int{count(t, s, "objects"), count(t, s, "changes"), count(t, s, "owners")}
	if fmt.Sprint(before) != fmt.Sprint(after) || rev(t, s) != 1 {
		t.Fatalf("a dry run must change nothing: before %v after %v rev %d", before, after, rev(t, s))
	}
}

func TestListSnapshotRevision(t *testing.T) {
	s := open(t)
	recs, r, err := s.List("", "pods", "default")
	if err != nil || len(recs) != 0 || r != 0 {
		t.Fatalf("empty list: %v %d %v", recs, r, err)
	}
	s.Write(false, func(tx *Tx) error { _, err := tx.Create(obj("b", "1")); return err })
	s.Write(false, func(tx *Tx) error { _, err := tx.Create(obj("a", "1")); return err })
	s.Write(false, func(tx *Tx) error {
		p := obj("x", "1")
		p.Key.Resource = "services"
		_, err := tx.Create(p)
		return err
	})
	recs, r, _ = s.List("", "pods", "default")
	if len(recs) != 2 || recs[0].Key.Name != "a" || r != 3 {
		t.Fatalf("list must be ordered by name and carry the store revision (3), got %d items, first %q, rev %d", len(recs), recs[0].Key.Name, r)
	}
	if recs, r, _ = s.List("", "pods", "elsewhere"); len(recs) != 0 || r != 3 {
		t.Fatalf("an empty list still has the store revision, got %d", r)
	}
	if recs, _, _ = s.List("", "pods", ""); len(recs) != 2 {
		t.Fatalf("an empty namespace lists all namespaces, got %d", len(recs))
	}
}

func TestDeleteAndOwners(t *testing.T) {
	s := open(t)
	s.Write(false, func(tx *Tx) error { _, err := tx.Create(obj("a", "1", "o1", "o2")); return err })
	if count(t, s, "owners") != 2 {
		t.Fatal("owner rows must be written with the object")
	}
	s.Write(false, func(tx *Tx) error { _, _, err := tx.Update(obj("a", "2", "o2"), 0); return err })
	if count(t, s, "owners") != 1 {
		t.Fatal("owner rows must follow an update")
	}
	if err := s.Write(false, func(tx *Tx) error { _, err := tx.Delete(key("a"), 1, obj("a", "2").Encode); return err }); !errors.Is(err, ErrConflict) {
		t.Fatalf("deleting at a stale revision: got %v, want ErrConflict", err)
	}
	var rec Record
	if err := s.Write(false, func(tx *Tx) (err error) { rec, err = tx.Delete(key("a"), 2, obj("a", "2").Encode); return err }); err != nil {
		t.Fatal(err)
	}
	var typ string
	s.r.QueryRow(`SELECT type FROM changes WHERE rev = 3`).Scan(&typ)
	if rec.Rev != 3 || typ != "DELETED" || count(t, s, "owners") != 0 || count(t, s, "objects") != 0 {
		t.Fatalf("delete must take a revision, log DELETED and drop owner rows: rev=%d type=%s", rec.Rev, typ)
	}
	if err := s.Write(false, func(tx *Tx) error {
		empty, err := tx.NamespaceEmpty("default")
		if err == nil && !empty {
			err = errors.New("namespace should be empty")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestReopenKeepsState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Write(false, func(tx *Tx) error { _, err := tx.Create(obj("a", "1")); return err })
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if rec, err := s.Get(key("a")); err != nil || rec.Rev != 1 || rev(t, s) != 1 {
		t.Fatalf("state must survive a reopen: %v %v", rec, err)
	}
}
