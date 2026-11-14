package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/netip"
	"sync"
	"time"

	"shearwater/internal/apis"
	"shearwater/internal/drain"
	"shearwater/internal/runtime"
	"shearwater/internal/store"
)

// DataPath separates policy (UID, selector, readiness) from privileged Linux
// rules. Tests inject it to exercise withdrawal and IP reuse without root.
type DataPath interface {
	Reconcile(context.Context, []runtime.ServiceRoute) error
	Counts(context.Context) (map[netip.Addr]int, error)
	ForgetPod(context.Context, netip.Addr) error
}

type KernelManager struct {
	Manager
	DataPath DataPath
	Drain    *drain.State
	mu       sync.Mutex // snapshot + rules + Pod IP retirement form one serialized operation
}

func (m *KernelManager) Run(ctx context.Context) {
	for ctx.Err() == nil {
		changed := m.Store.Changed()
		wait := refreshEvery
		if err := m.sync(ctx); err != nil {
			log.Printf("service forwarding: %v", err)
			wait = time.Second
		} else if m.Drain.Pending() {
			wait = 200 * time.Millisecond
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
		case <-changed:
		case <-timer.C:
		}
		timer.Stop()
	}
	// Deliberately no teardown here: ordinary server restarts keep both
	// established connections and new connections to the last healthy set.
}

func (m *KernelManager) sync(ctx context.Context) (err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	defer func() {
		if err != nil {
			m.Drain.Observe(time.Now(), nil)
		}
	}()
	wanted, view, err := m.plan(ctx)
	if err != nil {
		return err
	}
	if err := m.DataPath.Reconcile(ctx, wanted); err != nil {
		return err
	}
	observations := make(map[string]drain.Observation)
	ips := make(map[string]netip.Addr)
	for _, rec := range view.Pods {
		var p apis.Pod
		if err := json.Unmarshal(rec.Data, &p); err != nil {
			return err
		}
		if token := p.Metadata.Annotations[drain.Annotation]; token != "" && !rec.Deleting {
			observations[rec.UID] = drain.Observation{Token: token}
			if raw := view.PodIPs[rec.UID]; raw != "" {
				ip, err := netip.ParseAddr(raw)
				if err != nil {
					return err
				}
				ips[rec.UID] = ip
			}
		}
	}
	if len(observations) > 0 {
		counts, err := m.DataPath.Counts(ctx)
		if err != nil {
			return err
		}
		for uid, o := range observations {
			o.Active = counts[ips[uid]]
			observations[uid] = o
		}
	}
	m.Drain.Observe(time.Now(), observations)
	return nil
}

func (m *KernelManager) reconcileKernel(ctx context.Context) error {
	wanted, _, err := m.plan(ctx)
	if err != nil {
		return err
	}
	return m.DataPath.Reconcile(ctx, wanted)
}

func (m *KernelManager) plan(ctx context.Context) ([]runtime.ServiceRoute, store.NetworkView, error) {
	view, err := m.Store.ServiceNetwork(ctx)
	if err != nil {
		return nil, view, err
	}
	routes, err := buildRoutes(view.Services, view.EndpointSlices)
	if err != nil {
		return nil, view, err
	}
	pods := make(map[store.Key]store.Record, len(view.Pods))
	for _, p := range view.Pods {
		pods[p.Key] = p
	}
	wanted := make([]runtime.ServiceRoute, 0, len(routes))
	for address, r := range routes {
		out := runtime.ServiceRoute{UID: r.uid, Address: address}
		for _, b := range r.backends {
			if view.PodIPs[b.uid] == b.address.Addr().String() && validBackendPod(pods[b.pod], &r, b) {
				out.Backends = append(out.Backends, runtime.ServiceBackend{UID: b.uid, Address: b.address})
			}
		}
		wanted = append(wanted, out)
	}
	return wanted, view, nil
}

// RetirePod is the node's barrier BEFORE releasing a Pod IP. A delayed slice
// can never republish the old backend: reconciliation rechecks the actual Pod
// reservation, and shares this lock with retirement. Flush just that old Pod's
// Service tuples so they cannot send an old client's bytes to a new occupant.
func (m *KernelManager) RetirePod(ctx context.Context, uid string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	allocations, err := m.Store.IPs("pod")
	if err != nil {
		return err
	}
	raw := allocations[uid]
	if raw == "" {
		return nil
	}
	ip, err := netip.ParseAddr(raw)
	if err != nil {
		return err
	}
	rows, _, err := m.Store.List("", "pods", "")
	if err != nil {
		return err
	}
	for _, rec := range rows {
		if rec.UID == uid && !rec.Deleting {
			return fmt.Errorf("cannot release Service routes for live Pod %s", uid)
		}
	}
	if err := m.reconcileKernel(ctx); err != nil {
		return err
	}
	return m.DataPath.ForgetPod(ctx, ip)
}
