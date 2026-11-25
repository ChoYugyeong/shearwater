package store

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func putPod(t *testing.T, s *Store, a Actor, ns, name string) Record {
	t.Helper()
	var rec Record
	err := s.WriteAs(a, false, func(tx *Tx) (err error) {
		rec, err = tx.Create(Put{Key: Key{Resource: "pods", Namespace: ns, Name: name}, UID: ns + "-" + name,
			Encode: func(rev int64) ([]byte, error) { return []byte(`{"metadata":{"name":"` + name + `"}}`), nil }})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestWriteAsRecordsTheActorWithEachChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// Before InitActors (an older database) nothing is attributed and
	// nothing fails.
	before := putPod(t, s, Actor{Name: "alice", Via: "kubectl"}, "a", "early")
	if err := s.InitActors(); err != nil {
		t.Fatal(err)
	}
	alice := putPod(t, s, Actor{Name: "alice", Via: "kubectl"}, "a", "p1")
	system := putPod(t, s, Actor{}, "a", "p2")
	var two []Record
	if err := s.WriteAs(Actor{Name: "task:op1", Via: "task"}, false, func(tx *Tx) error {
		for _, n := range []string{"p3", "p4"} {
			rec, err := tx.Create(Put{Key: Key{Resource: "pods", Namespace: "b", Name: n}, UID: "b-" + n,
				Encode: func(rev int64) ([]byte, error) { return []byte(`{}`), nil }})
			if err != nil {
				return err
			}
			two = append(two, rec)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		rev  int64
		want Actor
	}{{alice.Rev, Actor{"alice", "kubectl"}}, {two[0].Rev, Actor{"task:op1", "task"}}, {two[1].Rev, Actor{"task:op1", "task"}}} {
		got, err := s.actorOf(c.rev)
		if err != nil || got != c.want {
			t.Fatalf("rev %d: %+v %v, want %+v", c.rev, got, err, c.want)
		}
	}
	for _, rev := range []int64{before.Rev, system.Rev} {
		if _, err := s.actorOf(rev); !errors.Is(err, ErrNotFound) {
			t.Fatalf("rev %d is attributed: %v", rev, err)
		}
	}
	// A dry run and a failed write leave no attribution behind.
	if err := s.WriteAs(Actor{Name: "bob"}, true, func(tx *Tx) error {
		_, err := tx.Create(Put{Key: Key{Resource: "pods", Namespace: "a", Name: "dry"}, UID: "dry",
			Encode: func(rev int64) ([]byte, error) { return []byte(`{}`), nil }})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.actorOf(two[1].Rev + 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("dry run attributed: %v", err)
	}

	// A reopened store keeps attributing (the table is found at Open).
	s.Close()
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	later := putPod(t, s2, Actor{Name: "carol", Via: "api"}, "a", "p5")
	if got, err := s2.actorOf(later.Rev); err != nil || got.Name != "carol" {
		t.Fatalf("after reopen: %+v %v", got, err)
	}
}

func TestHistoryAcrossResources(t *testing.T) {
	s := open(t)
	if err := s.InitActors(); err != nil {
		t.Fatal(err)
	}
	putPod(t, s, Actor{Name: "alice", Via: "kubectl"}, "a", "p1")
	if err := s.WriteAs(Actor{Name: "bob", Via: "api"}, false, func(tx *Tx) error {
		_, err := tx.Create(Put{Key: Key{Group: "apps", Resource: "deployments", Namespace: "b", Name: "web"}, UID: "d1",
			Encode: func(rev int64) ([]byte, error) { return []byte(`{}`), nil }})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Write(false, func(tx *Tx) error {
		_, err := tx.Create(Put{Key: Key{Resource: "namespaces", Name: "c"}, UID: "ns-c",
			Encode: func(rev int64) ([]byte, error) { return []byte(`{}`), nil }})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	all, next, err := s.History(0, "", 10)
	if err != nil || len(all) != 3 || next != 3 {
		t.Fatalf("%v next=%d err=%v", all, next, err)
	}
	if all[0].Actor.Name != "alice" || all[1].Key.Group != "apps" || all[1].Actor.Via != "api" || all[2].Actor.Name != "" || all[0].At == "" {
		t.Fatalf("%+v", all)
	}
	page, next, err := s.History(0, "", 2)
	if err != nil || len(page) != 2 || next != 2 {
		t.Fatalf("full page: %d next=%d %v", len(page), next, err)
	}
	ns, _, err := s.History(0, "b", 10)
	if err != nil || len(ns) != 1 || ns[0].Key.Name != "web" {
		t.Fatalf("namespace b: %+v %v", ns, err)
	}
	tail, next, err := s.History(-1, "", 2)
	if err != nil || len(tail) != 2 || tail[0].Rev != 2 || tail[1].Rev != 3 || next != 3 {
		t.Fatalf("tail: %+v next=%d %v", tail, next, err)
	}
}

func TestHistoryWithoutActorsTableAndCompaction(t *testing.T) {
	s := open(t)
	putPod(t, s, Actor{Name: "alice"}, "a", "p1")
	got, _, err := s.History(0, "", 10)
	if err != nil || len(got) != 1 || got[0].Actor.Name != "" {
		t.Fatalf("no table: %+v %v", got, err)
	}
	if err := s.InitActors(); err != nil {
		t.Fatal(err)
	}
	s.KeepChanges, s.KeepFor, s.CompactEvery = 1, -time.Hour, 0
	putPod(t, s, Actor{Name: "alice"}, "a", "p2")
	putPod(t, s, Actor{Name: "alice"}, "a", "p3")
	if _, _, err := s.History(0, "", 10); !errors.Is(err, ErrCompacted) {
		t.Fatalf("want ErrCompacted, got %v", err)
	}
	var left int
	if err := s.r.QueryRow(`SELECT count(*) FROM change_actors`).Scan(&left); err != nil || left > 1 {
		t.Fatalf("compaction left %d attribution rows (%v)", left, err)
	}
}
