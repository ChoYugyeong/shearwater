package operation

import (
	"bytes"
	"errors"
	"fmt"
	"time"

	"shearwater/internal/apis"
	"shearwater/internal/objects"
	"shearwater/internal/store"
)

const LeasePrefix = "shearwater-app-"
const IntentAnnotation = "shearwater.io/intent"
const AppLabel = "shearwater.io/app"

type LeaseInfo struct {
	Holder  string    `json:"holder"`
	Intent  string    `json:"intent"`
	Expires time.Time `json:"expires"`
}

func appLease(tx *store.Tx, r Record) (*apis.Lease, error) {
	k := objects.Key(apis.LeaseResource, r.Spec.Namespace, LeasePrefix+r.Spec.Name)
	rec, err := tx.Get(k)
	if errors.Is(err, store.ErrNotFound) {
		return &apis.Lease{Metadata: apis.ObjectMeta{Namespace: k.Namespace, Name: k.Name}}, nil
	}
	if err != nil {
		return nil, err
	}
	var l apis.Lease
	return &l, decode(rec, &l)
}

func heldLease(l *apis.Lease, now time.Time) *LeaseInfo {
	if l.Spec.HolderIdentity == "" || !now.Before(l.Expiry()) {
		return nil
	}
	return &LeaseInfo{Holder: l.Spec.HolderIdentity, Intent: l.Metadata.Annotations[IntentAnnotation], Expires: l.Expiry()}
}

func checkAvailable(l *apis.Lease, r Record, now time.Time) error {
	if info := heldLease(l, now); info != nil && info.Holder != "task:"+r.ID {
		return fail("APP_LEASED", fmt.Sprintf("app lease is held\nhint: holder=%q expires=%s intent=%q", info.Holder, info.Expires.UTC().Format(time.RFC3339Nano), info.Intent))
	}
	if l.Metadata.DeletionTimestamp != nil {
		return fail("CONFLICT", "app lease is being deleted")
	}
	return nil
}

func leaseStamp(t time.Time) string { return t.UTC().Format(apis.MicroTimeLayout) }

// Both the engine and Compose keep leases in the objects table of the same
// SQLite database as the operation. Compose only serves the operation API.
func (m *Manager) holdLease(tx *store.Tx, r Record, p *Plan, l *apis.Lease, expires time.Time) error {
	now := m.now()
	if err := checkAvailable(l, r, now); err != nil {
		return err
	}
	holder := "task:" + r.ID
	if p.Lease != "" && (p.Lease != l.Metadata.UID || l.Spec.HolderIdentity != holder) {
		return fail("LEASE_LOST", "operator released or replaced the app lease; discard this plan and review a new one")
	}
	oldSpec := apis.SpecJSON(l)
	create := l.Metadata.UID == ""
	previous := l.Spec.HolderIdentity
	if create {
		objects.Stamp(apis.LeaseResource, l)
	}
	if previous != holder {
		transitions := int32(0)
		if !create {
			if l.Spec.LeaseTransitions != nil {
				transitions = *l.Spec.LeaseTransitions
			}
			if transitions == 1<<31-1 {
				return fail("LIMIT", "app lease transition counter is full")
			}
			transitions++
		}
		l.Spec.LeaseTransitions = &transitions
	}
	if !expires.After(now) {
		return fail("EXPIRED", "plan expired; prepare a new plan")
	}
	// Kubernetes expresses durations in whole seconds. Align the timestamp
	// to the exact deadline so rounding cannot outlive the plan.
	seconds := int32((expires.Sub(now) + time.Second - 1) / time.Second)
	l.Spec.LeaseDurationSeconds = &seconds
	l.Spec.RenewTime = leaseStamp(expires.Add(-time.Duration(seconds) * time.Second))
	if previous != holder {
		l.Spec.AcquireTime = l.Spec.RenewTime
	}
	l.Spec.HolderIdentity = holder
	if l.Metadata.Labels == nil {
		l.Metadata.Labels = map[string]string{}
	}
	if l.Metadata.Annotations == nil {
		l.Metadata.Annotations = map[string]string{}
	}
	l.Metadata.Labels[AppLabel] = r.Spec.Name
	l.Metadata.Annotations[IntentAnnotation] = bounded(fmt.Sprintf("%s %s to %s", p.Action, r.Spec.Name, p.Image))
	p.Lease = l.Metadata.UID
	if create {
		_, err := tx.Create(objects.Put(apis.LeaseResource, l))
		return err
	}
	if !bytes.Equal(oldSpec, apis.SpecJSON(l)) {
		l.Metadata.Generation++
	}
	_, _, err := tx.Update(objects.Put(apis.LeaseResource, l), 0)
	return err
}

func releaseLease(tx *store.Tx, r Record, l *apis.Lease) error {
	if l.Spec.HolderIdentity != "task:"+r.ID {
		return nil
	}
	l.Metadata.Generation++
	l.Spec.HolderIdentity = ""
	delete(l.Metadata.Annotations, IntentAnnotation)
	_, _, err := tx.Update(objects.Put(apis.LeaseResource, l), 0)
	return err
}

// Refresh only the caller's lease. Observing another task must not extend
// its lease, and a forced release must never be undone by a status request.
func (m *Manager) refreshLease(tx *store.Tx, r Record) (*LeaseInfo, error) {
	l, err := appLease(tx, r)
	if err != nil {
		return nil, err
	}
	now := m.now()
	if l.Spec.HolderIdentity != "task:"+r.ID {
		return heldLease(l, now), nil
	}
	if len(r.Plans) > 0 && !r.Revoked && now.Before(r.Spec.Expires) {
		p := r.Plans[len(r.Plans)-1]
		if p.Lease == l.Metadata.UID && (r.Pending == p.Hash || p.Applied && p.Verification == "") {
			end := p.Expires
			if r.Spec.Expires.Before(end) {
				end = r.Spec.Expires
			}
			if now.Before(end) {
				err = m.holdLease(tx, r, &p, l, end)
				return heldLease(l, now), err
			}
			// A started external execution can be reconciled after its
			// window closes, provided no one took or released its lease.
			// Expiry also frees an applied rollout for takeover. Keep its old
			// identity until the final observation so a still-authorized recovery
			// can distinguish natural expiry from an operator force release.
			if p.Applied || r.Pending == p.Hash && p.Started != nil {
				return heldLease(l, now), nil
			}
		}
	}
	return nil, releaseLease(tx, r, l)
}

func (m *Manager) executionLease(tx *store.Tx, r Record, p *Plan) error {
	l, err := appLease(tx, r)
	if err != nil {
		return err
	}
	end := p.Expires
	if p.Started != nil && !m.now().Before(end) {
		end = m.now().Add(15 * time.Minute)
	}
	if r.Spec.Expires.Before(end) {
		end = r.Spec.Expires
	}
	return m.holdLease(tx, r, p, l, end)
}
