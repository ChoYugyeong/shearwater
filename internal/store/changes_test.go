package store

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func woke(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func create(t *testing.T, s *Store, names ...string) {
	t.Helper()
	for _, n := range names {
		if err := s.Write(false, func(tx *Tx) error { _, err := tx.Create(obj(n, "1")); return err }); err != nil {
			t.Fatal(err)
		}
	}
}

func TestHubWakesOnCommitOnly(t *testing.T) {
	s := open(t)
	ch := s.Changed()
	if woke(ch) {
		t.Fatal("woke before any write")
	}
	if err := s.Write(true, func(tx *Tx) error { _, err := tx.Create(obj("dry", "1")); return err }); err != nil {
		t.Fatal(err)
	}
	if woke(ch) {
		t.Fatal("a dry run woke the watchers")
	}
	s.Write(false, func(tx *Tx) error { tx.Create(obj("a", "1")); return errors.New("rolled back") })
	if woke(ch) {
		t.Fatal("a failed write woke the watchers")
	}
	create(t, s, "a")
	if !woke(ch) {
		t.Fatal("a commit did not wake the watchers")
	}
	ch = s.Changed()
	if err := s.Write(false, func(tx *Tx) error { _, _, err := tx.Update(obj("a", "1"), 0); return err }); err != nil {
		t.Fatal(err)
	}
	if woke(ch) {
		t.Fatal("a write that changed nothing woke the watchers")
	}
}

func TestChanges(t *testing.T) {
	s := open(t)
	create(t, s, "a", "b")
	s.Write(false, func(tx *Tx) error { _, _, err := tx.Update(obj("a", "2"), 0); return err })
	s.Write(false, func(tx *Tx) error {
		_, err := tx.Delete(key("b"), 0, func(rev int64) ([]byte, error) { return []byte(fmt.Sprintf(`{"gone":%d}`, rev)), nil })
		return err
	})
	other := Put{Key: Key{Resource: "pods", Namespace: "other", Name: "x"}, UID: "uid-x", Generation: 1,
		Encode: func(rev int64) ([]byte, error) { return []byte(`{"x":1}`), nil }}
	s.Write(false, func(tx *Tx) error { _, err := tx.Create(other); return err })
	svc := Put{Key: Key{Resource: "services", Namespace: "default", Name: "s"}, UID: "uid-s", Generation: 1,
		Encode: func(rev int64) ([]byte, error) { return []byte(`{"s":1}`), nil }}
	s.Write(false, func(tx *Tx) error { _, err := tx.Create(svc); return err })

	all, next, err := s.Changes("", "pods", "", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var got string
	for _, c := range all {
		got += fmt.Sprintf("%d:%s:%s/%s ", c.Rev, c.Type, c.Key.Namespace, c.Key.Name)
	}
	if want := "1:ADDED:default/a 2:ADDED:default/b 3:MODIFIED:default/a 4:DELETED:default/b 5:ADDED:other/x "; got != want {
		t.Fatalf("changes:\n got %s\nwant %s", got, want)
	}
	if next != 6 {
		t.Fatalf("a batch that is not full must continue from the snapshot revision 6, got %d", next)
	}
	if all[0].Prev != nil || string(all[2].Prev) != string(all[0].Data) || string(all[3].Prev) != string(all[1].Data) {
		t.Fatalf("prev must be the object before the change: %q %q", all[2].Prev, all[3].Prev)
	}
	if string(all[3].Data) != `{"gone":4}` {
		t.Fatalf("a DELETED change carries the object with the delete's revision: %s", all[3].Data)
	}

	two, next, _ := s.Changes("", "pods", "", 1, 2)
	if len(two) != 2 || two[0].Rev != 2 || next != 3 {
		t.Fatalf("a full batch continues from its last revision: %d entries, next %d", len(two), next)
	}
	ns, _, _ := s.Changes("", "pods", "other", 0, 100)
	if len(ns) != 1 || ns[0].Key.Name != "x" {
		t.Fatalf("namespace filter: %v", ns)
	}
	// A batch must be bounded: zero or less is refused, not read as "everything" or "nothing".
	for _, limit := range []int{0, -1} {
		if got, _, err := s.Changes("", "pods", "", 0, limit); err == nil || got != nil {
			t.Fatalf("limit %d: got %v, %v; want an error", limit, got, err)
		}
	}
	none, next, _ := s.Changes("", "pods", "", 6, 100)
	if len(none) != 0 || next != 6 {
		t.Fatalf("nothing after the current revision: %v next %d", none, next)
	}
}

func TestCompactionByCount(t *testing.T) {
	s := open(t)
	s.KeepChanges, s.KeepFor, s.CompactEvery = 3, -time.Hour, 0
	create(t, s, "a", "b", "c", "d", "e", "f", "g", "h", "i", "j")
	if n := count(t, s, "changes"); n != 3 {
		t.Fatalf("change log has %d entries, want the newest 3", n)
	}
	if c, _ := s.Compacted(); c != 7 {
		t.Fatalf("compacted revision %d, want 7", c)
	}
	if _, _, err := s.Changes("", "pods", "", 6, 100); !errors.Is(err, ErrCompacted) {
		t.Fatalf("reading from before the log: %v, want ErrCompacted", err)
	}
	got, _, err := s.Changes("", "pods", "", 7, 100)
	if err != nil || len(got) != 3 || got[0].Rev != 8 {
		t.Fatalf("reading from exactly the compacted revision must work: %v %v", got, err)
	}
	if n := count(t, s, "objects"); n != 10 {
		t.Fatalf("compaction touched the objects: %d left", n)
	}
}

func TestCompactionKeepsWhatIsRecent(t *testing.T) {
	s := open(t)
	s.KeepChanges, s.CompactEvery = 3, 0 // KeepFor stays one hour: everything here is recent
	create(t, s, "a", "b", "c", "d", "e", "f")
	if n := count(t, s, "changes"); n != 6 {
		t.Fatalf("change log has %d entries; entries newer than KeepFor must stay", n)
	}
	if c, _ := s.Compacted(); c != 0 {
		t.Fatalf("compacted revision %d, want 0", c)
	}
}

func TestCompactionIsThrottled(t *testing.T) {
	s := open(t)
	s.KeepChanges, s.KeepFor, s.CompactEvery = 1, -time.Hour, time.Hour
	create(t, s, "a", "b", "c", "d")
	// The first write compacts (nothing to drop yet); the next ones fall inside CompactEvery.
	if n := count(t, s, "changes"); n != 4 {
		t.Fatalf("change log has %d entries; compaction must not run on every write", n)
	}
}
