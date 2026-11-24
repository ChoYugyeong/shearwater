package main

import (
	"context"
	"errors"
	"log"
	"net/netip"

	"shearwater/internal/admission"
	"shearwater/internal/apis"
	"shearwater/internal/dns"
	"shearwater/internal/edge"
	"shearwater/internal/netpol"
	"shearwater/internal/objects"
	"shearwater/internal/runtime"
	"shearwater/internal/store"
)

// wireNetworkServices attaches the control-plane side of DNS and Ingress:
// bootstrapping the default "shearwater" IngressClass and registering the
// host-ownership admission check that keeps a host name with the namespace
// that claimed it first (docs/design-multinode-tenancy.md 4).
func wireNetworkServices(w *wiring) []admission.Func {
	if w != nil && w.Store != nil {
		if err := ensureDefaultIngressClass(w.Store); err != nil {
			log.Printf("network services: ensure default IngressClass: %v", err)
		}
	}
	return []admission.Func{edge.HostOwnershipAdmission()}
}

func ensureDefaultIngressClass(st *store.Store) error {
	res := apis.Lookup("networking.k8s.io", "v1", "ingressclasses")
	if res == nil {
		return nil
	}
	return st.Write(false, func(tx *store.Tx) error {
		key := objects.Key(res, "", apis.DefaultIngressClassName)
		if _, err := tx.Get(key); err == nil {
			return nil
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		ic := &apis.IngressClass{
			Metadata: apis.ObjectMeta{
				Name: apis.DefaultIngressClassName,
				Annotations: map[string]string{
					apis.DefaultIngressClassAnnotation: "true",
				},
			},
			Spec: apis.IngressClassSpec{
				Controller: apis.IngressClassController,
			},
		}
		objects.Stamp(res, ic)
		_, err := tx.Create(objects.Put(res, ic))
		return err
	})
}

// runNodeNetworkServices runs this node's network services (the cluster DNS
// server) until ctx ends. It runs on every node that has a node agent: the
// control plane's own node and each worker. r reads the cluster state (the
// store on the control plane, the node API on a worker); gateway is this node's
// pod gateway address, the first address of its pod CIDR, where pods find DNS.
func runNodeNetworkServices(ctx context.Context, r store.Reader, gateway netip.Addr, logf func(string, ...any)) {
	if !gateway.IsValid() {
		if logf != nil {
			logf("cluster DNS not started: invalid gateway address")
		}
		<-ctx.Done()
		return
	}
	srv := &dns.Server{
		Store: r,
		Addr:  netip.AddrPortFrom(gateway, 53),
		Logf:  logf,
	}
	if err := srv.Run(ctx); err != nil && ctx.Err() == nil && logf != nil {
		logf("cluster DNS stopped: %v", err)
	}
}

// runNodeNetworkPolicy reconciles NetworkPolicy rules for this node's local
// Pods until ctx ends. It runs on every node that has a node agent (control
// plane and workers) next to runNodeNetworkServices.
func runNodeNetworkPolicy(ctx context.Context, r store.Reader, podCIDR netip.Prefix, logf func(string, ...any)) {
	if !podCIDR.IsValid() || !podCIDR.Addr().Is4() {
		if logf != nil {
			logf("network policy not started: invalid pod CIDR %s", podCIDR)
		}
		<-ctx.Done()
		return
	}
	cidr := podCIDR.Masked()
	m := &netpol.Manager{
		Store:    r,
		PodCIDR:  cidr,
		DataPath: &runtime.KernelNetpol{PodCIDR: cidr},
		Logf:     logf,
	}
	m.Run(ctx)
}
