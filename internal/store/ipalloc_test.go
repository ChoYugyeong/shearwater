package store

import (
	"errors"
	"maps"
	"slices"
	"testing"
)

func TestIPAllocationSharesPoolAndKeepsUIDReservation(t *testing.T) {
	s := open(t)
	const first, second, third = "10.96.0.1", "10.96.0.2", "10.96.0.3"
	candidates := slices.Values([]string{first, second, third})
	wake := s.Changed()
	if ip, err := s.AllocIP("pod", "pod-uid", candidates); err != nil || ip != first {
		t.Fatalf("standalone Pod allocation = %q, %v", ip, err)
	}
	if err := s.Write(false, func(tx *Tx) error {
		if _, err := tx.AllocIP("service", "svc-uid", first, nil); !errors.Is(err, ErrIPAllocated) {
			t.Fatalf("explicit request for a Pod's IP = %v, want ErrIPAllocated", err)
		}
		if ip, err := tx.AllocIP("service", "svc-uid", second, nil); err != nil || ip != second {
			t.Fatalf("explicit reservation = %q, %v", ip, err)
		}
		if ip, err := tx.AllocIP("service", "svc-uid", second, nil); err != nil || ip != second {
			t.Fatalf("repeated explicit reservation = %q, %v", ip, err)
		}
		if _, err := tx.AllocIP("service", "svc-uid", third, nil); err == nil {
			t.Fatal("the same owner cannot move its explicit reservation")
		}
		if ip, err := tx.AllocIP("service", "auto-uid", "", candidates); err != nil || ip != third {
			t.Fatalf("automatic reservation = %q, %v, want %q", ip, err, third)
		}
		_, err := tx.AllocIP("service", "extra-uid", "", candidates)
		if !errors.Is(err, ErrIPExhausted) {
			t.Fatalf("reservation in a full pool = %v, want ErrIPExhausted", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"svc-uid": second, "auto-uid": third}
	if ips, err := s.IPs("service"); err != nil || !maps.Equal(ips, want) {
		t.Fatalf("committed Service IPs = %v, %v, want %v", ips, err, want)
	}
	if ip, err := s.AllocIP("pod", "pod-uid", slices.Values([]string{third})); err != nil || ip != first {
		t.Fatalf("existing Pod address = %q, %v, want %q", ip, err, first)
	}
	if rev(t, s) != 0 || count(t, s, "changes") != 0 || s.Changed() != wake {
		t.Fatal("allocation bookkeeping alone must not publish API changes")
	}
}

func TestServiceIPReservationRollsBackWithObject(t *testing.T) {
	s := open(t)
	p := obj("svc", "body")
	p.Key.Resource = "services"
	const ip = "10.96.0.1"
	abort := errors.New("encoding failed")
	wake := s.Changed()
	for _, dry := range []bool{true, false} {
		create := p
		want := error(nil)
		if !dry {
			create.Encode = func(int64) ([]byte, error) { return nil, abort }
			want = abort
		}
		err := s.Write(dry, func(tx *Tx) error {
			if _, err := tx.AllocIP("service", p.UID, ip, nil); err != nil {
				return err
			}
			_, err := tx.Create(create)
			return err
		})
		if !errors.Is(err, want) {
			t.Fatalf("create (dry=%v) returned %v, want %v", dry, err, want)
		}
		if ips, err := s.IPs("service"); err != nil || len(ips) != 0 {
			t.Fatalf("create (dry=%v) left a reservation: %v, %v", dry, ips, err)
		}
		if _, err := s.Get(p.Key); !errors.Is(err, ErrNotFound) {
			t.Fatalf("create (dry=%v) left an object: %v", dry, err)
		}
		if rev(t, s) != 0 || count(t, s, "changes") != 0 || s.Changed() != wake {
			t.Fatalf("create (dry=%v) left a revision or watch event", dry)
		}
	}
}

func TestServiceIPReleaseIsPartOfFinalDelete(t *testing.T) {
	s := open(t)
	p := obj("svc", "body")
	p.Key.Resource = "services"
	const ip = "10.96.0.1"
	if err := s.Write(false, func(tx *Tx) error {
		if _, err := tx.AllocIP("service", p.UID, ip, nil); err != nil {
			return err
		}
		_, err := tx.Create(p)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	checkOwner := func() {
		t.Helper()
		if ips, err := s.IPs("service"); err != nil || len(ips) != 1 || ips[p.UID] != ip {
			t.Fatalf("Service IPs = %v, %v, want %q owned by %q", ips, err, ip, p.UID)
		}
	}
	// A node cleanup call has authority only over Pod IPs.
	if err := s.ReleaseIP(p.UID); err != nil {
		t.Fatal(err)
	}
	checkOwner()

	p.Deleting = true
	if err := s.Write(false, func(tx *Tx) error {
		_, _, err := tx.Update(p, 1)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	checkOwner()
	if err := s.Write(false, func(tx *Tx) error {
		_, err := tx.Delete(p.Key, 1, p.Encode)
		return err
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale delete = %v, want ErrConflict", err)
	}
	checkOwner()

	abort := errors.New("later change failed")
	if err := s.Write(false, func(tx *Tx) error {
		if _, err := tx.Delete(p.Key, 2, p.Encode); err != nil {
			return err
		}
		// A later write in the same transaction sees the address as free.
		if _, err := tx.AllocIP("service", "replacement", ip, nil); err != nil {
			return err
		}
		return abort
	}); !errors.Is(err, abort) {
		t.Fatalf("failed delete transaction = %v, want %v", err, abort)
	}
	checkOwner()
	if rev(t, s) != 2 || count(t, s, "changes") != 2 {
		t.Fatal("the failed transaction advanced object history")
	}
	if err := s.Write(false, func(tx *Tx) error {
		_, err := tx.Delete(p.Key, 2, p.Encode)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if ips, err := s.IPs("service"); err != nil || len(ips) != 0 {
		t.Fatalf("final delete left the IP reserved: %v, %v", ips, err)
	}
}
