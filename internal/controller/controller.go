// Package controller reconciles the single-node scheduler, workload owners and EndpointSlices.
package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"shearwater/internal/apis"
	"shearwater/internal/objects"
	"shearwater/internal/store"
)

type Manager struct {
	Store    *store.Store
	NodeName string
}

// Run rechecks the current state when the store changes and periodically.
// A notification can be coalesced because a pass reads the current state.
func (m Manager) Run(ctx context.Context) {
	delay := time.Second
	for ctx.Err() == nil {
		changed := m.Store.Changed()
		wait := 5 * time.Minute
		if err := m.Sync(); err != nil {
			log.Printf("workload controllers: %v", err)
			wait = delay
			delay = min(2*delay, time.Minute)
			changed = nil
		} else {
			delay = time.Second
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
		case <-changed:
		case <-timer.C:
		}
		timer.Stop()
	}
}

type item struct {
	res *apis.Resource
	rec store.Record
	obj apis.Object
}

func (i *item) update(tx *store.Tx) error {
	rec, _, err := tx.Update(objects.Put(i.res, i.obj), i.rec.Rev)
	if err == nil {
		i.rec = rec
	}
	return err
}

type view struct {
	all, pods, sets, deployments []*item
	services, endpointSlices     []*item
	byUID                        map[string]*item
	rolloutSlots                 map[string]*rolloutSlots // ReplicaSet UID -> shared Deployment capacity
	node                         *apis.Node
}

func load(tx *store.Tx, nodeName string) (view, error) {
	var v view
	v.byUID = make(map[string]*item)
	v.rolloutSlots = make(map[string]*rolloutSlots)
	records, err := tx.Objects()
	if err != nil {
		return v, err
	}
	for _, rec := range records {
		res := apis.Lookup(rec.Key.Group, "v1", rec.Key.Resource)
		if res == nil {
			continue
		}
		obj := res.New()
		if err := json.Unmarshal(rec.Data, obj); err != nil {
			return v, fmt.Errorf("decode %s: %w", rec.Key, err)
		}
		i := &item{res: res, rec: rec, obj: obj}
		v.all = append(v.all, i)
		v.byUID[rec.UID] = i
		switch o := obj.(type) {
		case *apis.Pod:
			v.pods = append(v.pods, i)
		case *apis.ReplicaSet:
			v.sets = append(v.sets, i)
		case *apis.Deployment:
			v.deployments = append(v.deployments, i)
		case *apis.Service:
			v.services = append(v.services, i)
		case *apis.EndpointSlice:
			v.endpointSlices = append(v.endpointSlices, i)
		case *apis.Node:
			if o.Metadata.Name == nodeName {
				v.node = o
			}
		}
	}
	return v, nil
}

// Sync makes a level-based pass in a write transaction. Adoption, scheduling,
// endpoint selection and the ownership checks for GC share a consistent view.
func (m Manager) Sync() error {
	return m.Store.Write(false, func(tx *store.Tx) error {
		v, err := load(tx, m.NodeName)
		if err != nil {
			return err
		}
		now := time.Now().UTC().Truncate(time.Second)
		changed, err := m.collect(tx, v, now)
		if err != nil {
			return err
		}
		if changed {
			if len(v.services) == 0 && len(v.endpointSlices) == 0 {
				return nil // refresh the workload view on the next notification
			}
			// GC can start a Pod deletion. Reload so that its endpoint can
			// be withdrawn in the transaction that marks the Pod terminating.
			v, err = load(tx, m.NodeName)
			if err != nil {
				return err
			}
			return reconcileEndpointSlices(tx, v)
		}
		for _, i := range v.deployments {
			if !i.rec.Deleting {
				if err := m.deployment(tx, i, v, now); err != nil {
					return err
				}
			}
		}
		budget := 64 // cap Pod creates per transaction
		for _, i := range v.sets {
			if !i.rec.Deleting {
				if err := m.replicaSet(tx, i, v, &budget, now); err != nil {
					return err
				}
			}
		}
		if err := m.schedule(tx, v, now); err != nil {
			return err
		}
		// ReplicaSet scale-down has marked its Pods terminating in this view.
		// Publish their removal from the slices in the same transaction.
		return reconcileEndpointSlices(tx, v)
	})
}

func controllerRef(m *apis.ObjectMeta) (apis.OwnerReference, bool) {
	for _, r := range m.OwnerReferences {
		if r.Controller != nil && *r.Controller {
			return r, true
		}
	}
	return apis.OwnerReference{}, false
}

func controlledBy(child, owner *item) bool {
	ref, ok := controllerRef(child.obj.Meta())
	return ok && references(child, ref, owner)
}

func references(child *item, ref apis.OwnerReference, owner *item) bool {
	return owner != nil && ref.UID == owner.rec.UID && ref.Name == owner.rec.Key.Name &&
		ref.Kind == owner.res.Kind && ref.APIVersion == owner.res.APIVersion() &&
		(!owner.res.Namespaced || child.rec.Key.Namespace == owner.rec.Key.Namespace)
}

func ownerRef(i *item) apis.OwnerReference {
	yes := true
	return apis.OwnerReference{APIVersion: i.res.APIVersion(), Kind: i.res.Kind, Name: i.rec.Key.Name,
		UID: i.rec.UID, Controller: &yes, BlockOwnerDeletion: &yes}
}

func podReady(p *apis.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == "Ready" {
			return c.Status == "True"
		}
	}
	return false
}
