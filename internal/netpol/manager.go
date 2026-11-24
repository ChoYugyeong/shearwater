package netpol

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"time"

	"shearwater/internal/runtime"
	"shearwater/internal/store"
)

const (
	refreshEvery = 30 * time.Second
	minBackoff   = time.Second
	maxBackoff   = 30 * time.Second
)

// DataPath abstracts privileged Linux NetworkPolicy enforcement so the manager
// can be tested without root or iptables.
type DataPath interface {
	Reconcile(context.Context, runtime.NetpolPlan) error
}

// Manager watches the cluster store (via store.Reader on both the control plane
// and workers), compiles node-local NetworkPolicy plans, and applies them via
// DataPath with exponential backoff on errors.
type Manager struct {
	Store    store.Reader
	PodCIDR  netip.Prefix
	DataPath DataPath
	Logf     func(format string, args ...any)

	// Optional overrides for tests; zero uses package defaults.
	RefreshEvery time.Duration
	MinBackoff   time.Duration
	MaxBackoff   time.Duration
}

func (m *Manager) logf(format string, args ...any) {
	if m.Logf != nil {
		m.Logf(format, args...)
		return
	}
	fmt.Fprintf(os.Stderr, "network policy: "+format+"\n", args...)
}

// Sync compiles the current NetworkPolicy plan from m.Store and applies it.
func (m *Manager) Sync(ctx context.Context) error {
	plan, err := CompileFromStore(m.Store, m.PodCIDR)
	if err != nil {
		return err
	}
	return m.DataPath.Reconcile(ctx, plan)
}

// Run reconciles immediately and on every store change until ctx ends.
// Like proxy.KernelManager, Run deliberately does not tear down rules on exit
// so ordinary sw restarts keep NetworkPolicy enforcement intact.
func (m *Manager) Run(ctx context.Context) {
	refresh := m.RefreshEvery
	if refresh <= 0 {
		refresh = refreshEvery
	}
	minB := m.MinBackoff
	if minB <= 0 {
		minB = minBackoff
	}
	maxB := m.MaxBackoff
	if maxB <= 0 {
		maxB = maxBackoff
	}

	backoff := minB
	for ctx.Err() == nil {
		changed := m.Store.Changed()
		wait := refresh
		if err := m.Sync(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			m.logf("sync failed: %v (retrying in %s)", err, backoff)
			wait, changed = backoff, nil
			backoff = min(backoff*2, maxB)
		} else {
			backoff = minB
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
