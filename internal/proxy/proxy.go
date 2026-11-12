// Package proxy forwards TCP connections from local Service IPs to ready Pods.
package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"net/netip"
	"time"

	"shearwater/internal/apis"
	"shearwater/internal/store"
)

const refreshEvery = 30 * time.Second

type Manager struct {
	Store *store.Store
	// Addresses reconciles the kernel's local addresses and returns the IPs
	// attached successfully, even if one of the other addresses failed.
	Addresses func(context.Context, map[netip.Addr]bool) (map[netip.Addr]bool, error)
}

// Run keeps listeners in sync with SQLite. A store change is a wake-up signal,
// so if several updates arrive together we reconcile their latest state once.
func (m Manager) Run(ctx context.Context) {
	listeners := make(map[netip.AddrPort]*forwarder)
	defer func() {
		for _, f := range listeners {
			f.stop()
		}
	}()
	var attached map[netip.Addr]bool
	var lastSync time.Time
	backoff := time.Second
	for ctx.Err() == nil {
		changed := m.Store.Changed()
		wait := refreshEvery
		if err := m.reconcile(ctx, listeners, &attached, &lastSync); err != nil {
			log.Printf("service proxy: %v", err)
			wait, changed = backoff, nil
			backoff = min(2*backoff, 10*time.Second)
		} else {
			backoff = time.Second
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

func (m Manager) reconcile(ctx context.Context, listeners map[netip.AddrPort]*forwarder,
	attached *map[netip.Addr]bool, lastSync *time.Time) error {
	routes, err := snapshot(m.Store)
	if err != nil {
		return err
	}
	wanted := make(map[netip.Addr]bool)
	for address := range routes {
		wanted[address.Addr()] = true
	}
	// A newly created Service can reuse the old address and port. Tear
	// down the old listener (including its streams) before binding again.
	for address, f := range listeners {
		r, keep := routes[address]
		if !keep || r.uid != f.current.Load().uid {
			f.stop()
			delete(listeners, address)
		} else {
			f.current.Store(&r)
		}
	}
	var failures []error
	if !maps.Equal(wanted, *attached) || time.Since(*lastSync) >= refreshEvery {
		have, err := m.Addresses(ctx, wanted)
		*attached = have
		if err != nil {
			*lastSync = time.Time{} // a failed detach must also be retried
			failures = append(failures, err)
		} else {
			*lastSync = time.Now()
		}
		for address, f := range listeners {
			if !have[address.Addr()] {
				f.stop()
				delete(listeners, address)
			}
		}
	}
	for address, r := range routes {
		if ctx.Err() != nil {
			break
		}
		if listeners[address] != nil || !(*attached)[address.Addr()] {
			continue
		}
		f, err := listen(ctx, address, r, m.current, m.currentPod)
		if err != nil {
			failures = append(failures, fmt.Errorf("listen on %s for %s: %w", address, r.service, err))
			continue
		}
		listeners[address] = f
	}
	return errors.Join(failures...)
}

func (m Manager) currentPod(ctx context.Context, r *route, b backend) bool {
	// EndpointSlices are reconciled asynchronously. A terminating Pod can
	// still accept a TCP dial, and a released IP can already belong to a new
	// Pod. Read the actual reservation with the Pod, then check readiness.
	ip := b.address.Addr().String()
	rec, err := m.Store.PodAtIP(ctx, b.pod.Namespace, b.pod.Name, ip)
	if err != nil || rec.Deleting || rec.UID != b.uid {
		return false
	}
	var pod apis.Pod
	if json.Unmarshal(rec.Data, &pod) != nil || pod.Status.PodIP != ip ||
		pod.Status.Phase == "Succeeded" || pod.Status.Phase == "Failed" {
		return false
	}
	for key, value := range r.selector {
		if label, ok := pod.Metadata.Labels[key]; !ok || label != value {
			return false
		}
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == "Ready" {
			return c.Status == "True"
		}
	}
	return false
}

func (m Manager) current(r *route) bool {
	// Accept can race deletion/recreation before the watch wakes Run. Check
	// the UID and generation before using the listener's cached endpoints.
	rec, err := m.Store.Get(r.service)
	return err == nil && !rec.Deleting && rec.UID == r.uid && rec.Generation == r.generation
}
