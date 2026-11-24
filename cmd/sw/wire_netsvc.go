package main

import (
	"context"
	"net/netip"

	"shearwater/internal/admission"
	"shearwater/internal/store"
)

// wireNetworkServices attaches the control-plane side of DNS and Ingress:
// the admission check that keeps a host name with the namespace that claimed
// it first (docs/design-multinode-tenancy.md 4). Owned by the
// network-services work.
func wireNetworkServices(w *wiring) []admission.Func {
	return nil
}

// runNodeNetworkServices runs this node's network services (the cluster DNS
// server) until ctx ends. It runs on every node that has a node agent: the
// control plane's own node and each worker. r reads the cluster state (the
// store on the control plane, the node API on a worker); gateway is this node's
// pod gateway address, the first address of its pod CIDR, where pods find DNS.
func runNodeNetworkServices(ctx context.Context, r store.Reader, gateway netip.Addr, logf func(string, ...any)) {
	<-ctx.Done()
}
