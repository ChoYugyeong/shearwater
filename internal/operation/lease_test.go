package operation

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"shearwater/internal/apis"
	"shearwater/internal/objects"
	"shearwater/internal/store"
)

func TestLeaseAndPlanRollBackTogether(t *testing.T) {
	f := newFixture(t)
	// Leave enough room for the old ledger row, but not the new plan.
	f.write(func(tx *store.Tx) error {
		r, err := load(tx, f.id)
		if err != nil {
			return err
		}
		r.Plans = []Plan{{Discarded: true}}
		b, err := json.Marshal(r)
		if err != nil {
			return err
		}
		r.Plans[0].Failure = strings.Repeat("x", (256<<10)-len(b)-100)
		return save(tx, r, false)
	})
	f.reject(f.token, Request{Action: "deploy"}, "LIMIT")
	k := objects.Key(apis.LeaseResource, "default", LeasePrefix+"web")
	if _, err := f.m.Store.Get(k); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("lease survived failed ledger write", err)
	}
	if r := f.stored(); r.Pending != "" || len(r.Plans) != 1 {
		t.Fatal("failed plan was saved")
	}
}

func TestLeaseContentionAndExpiredTakeover(t *testing.T) {
	f := newFixture(t)
	spec := f.stored().Spec
	other, token, err := f.m.Create(spec)
	if err != nil {
		t.Fatal(err)
	}
	p := f.call(false, Request{Action: "deploy"})
	if p.Lease == nil || p.Lease.Holder != "task:"+f.id || p.Lease.Intent != "deploy web to "+imageOne || !p.Lease.Expires.Equal(p.Review.Expires) {
		t.Fatal("wrong holder, intent or deadline", p)
	}
	otherAct := func(action string) (Result, error) {
		return f.m.Act(context.Background(), other.Operation, token, false, Request{Action: action})
	}
	s, err := otherAct("status")
	if err != nil || s.Lease == nil || *s.Lease != *p.Lease {
		t.Fatal("status did not show the other holder", s, err)
	}
	_, err = otherAct("deploy")
	var conflict *Error
	if !errors.As(err, &conflict) || conflict.Code != "APP_LEASED" || conflict.Retry() != "safe" {
		t.Fatal(err)
	}
	for _, want := range []string{p.Lease.Holder, p.Lease.Intent, p.Lease.Expires.Format(time.RFC3339)} {
		if !strings.Contains(conflict.Message, want) {
			t.Fatal("missing conflict hint", want, conflict)
		}
	}
	f.now = f.now.Add(time.Minute)
	s = f.call(true, Request{Action: "review"})
	if s.Lease == nil || !s.Lease.Expires.Equal(p.Lease.Expires) {
		t.Fatal("renewal moved the plan deadline", s)
	}
	f.write(func(tx *store.Tx) error {
		l, err := appLease(tx, Record{Spec: spec})
		if err != nil {
			return err
		}
		if l.Spec.RenewTime != leaseStamp(f.now) || *l.Spec.LeaseDurationSeconds != 14*60 || l.Metadata.Labels[AppLabel] != "web" {
			t.Fatal("lease was not renewed on review", l)
		}
		return nil
	})
	f.now = p.Lease.Expires
	taken, err := otherAct("deploy")
	if err != nil || taken.Lease == nil || taken.Lease.Holder != "task:"+other.Operation {
		t.Fatal("expired holder blocked takeover", taken, err)
	}
	f.write(func(tx *store.Tx) error {
		l, err := appLease(tx, Record{Spec: spec})
		if err != nil {
			return err
		}
		if l.Spec.LeaseTransitions == nil || *l.Spec.LeaseTransitions != 1 {
			t.Fatal("missing transition", l)
		}
		return nil
	})
	f.call(true, Request{Action: "discard", Plan: p.Plan})
	s, err = otherAct("status")
	if err != nil || s.Lease == nil || s.Lease.Holder != taken.Lease.Holder {
		t.Fatal("old owner released its successor", s, err)
	}
}

func TestLeaseReleaseBoundaries(t *testing.T) {
	for _, action := range []string{"verify", "discard", "revoke", "expiry", "force", "failed"} {
		t.Run(action, func(t *testing.T) {
			f := newFixture(t)
			spec := f.stored().Spec
			p := f.call(false, Request{Action: "deploy"})
			switch action {
			case "verify":
				f.call(true, Request{Action: "approve", Plan: p.Plan})
				r := f.call(false, Request{Action: "continue", Plan: p.Plan})
				if r.State != "not_met" || r.Lease == nil {
					t.Fatal("lease lost during rollout", r)
				}
				f.ready("one")
				if r = f.call(false, Request{Action: "verify"}); r.State != "healthy" || r.Lease != nil {
					t.Fatal(r)
				}
			case "discard":
				f.call(true, Request{Action: "discard", Plan: p.Plan})
			case "revoke":
				f.call(true, Request{Action: "revoke"})
			case "expiry":
				f.now = p.Review.Expires
				f.call(true, Request{Action: "status"})
			case "force":
				f.call(true, Request{Action: "approve", Plan: p.Plan})
				f.write(func(tx *store.Tx) error {
					r, err := load(tx, f.id)
					if err != nil {
						return err
					}
					l, err := appLease(tx, r)
					if err != nil {
						return err
					}
					return releaseLease(tx, r, l)
				})
				f.reject(f.token, Request{Action: "continue", Plan: p.Plan}, "LEASE_LOST")
			case "failed":
				f.call(true, Request{Action: "approve", Plan: p.Plan})
				f.m.Backend = rejectedEngine{}
				f.reject(f.token, Request{Action: "continue", Plan: p.Plan}, "STALE")
			}
			f.write(func(tx *store.Tx) error {
				l, err := appLease(tx, Record{Spec: spec})
				if err == nil && l.Spec.HolderIdentity != "" {
					t.Fatal("lease still held", l.Spec)
				}
				return err
			})
		})
	}
}

type rejectedEngine struct{ Engine }

func (rejectedEngine) Apply(*store.Tx, *Record, Plan) error {
	return fail("STALE", "target changed")
}
