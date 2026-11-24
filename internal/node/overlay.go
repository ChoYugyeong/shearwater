package node

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"os"
	"slices"
	"time"

	"shearwater/internal/apis"
	"shearwater/internal/objects"
	"shearwater/internal/runtime"
	"shearwater/internal/store"
)

// Annotations other nodes read from this node's Node object.
const (
	InternalIPAnnotation = "shearwater.io/internal-ip"
	VTEPAnnotation       = "shearwater.io/vtep-mac"
)

// Overlay keeps this node's VXLAN device and its peers in line with the Node
// objects (docs/design-multinode-tenancy.md 3.3). It runs on every node of a
// cluster with more than one server.
type Overlay struct {
	Store    store.Backend
	NodeName string
	NodeIP   netip.Addr
	PodCIDR  netip.Prefix
	Logf     func(format string, args ...any)

	device   runtime.Overlay
	mac      net.HardwareAddr
	reported int // peers last logged
}

const overlayRecheck = 30 * time.Second

func (o *Overlay) logf(format string, args ...any) {
	if o.Logf != nil {
		o.Logf(format, args...)
		return
	}
	fmt.Fprintf(os.Stderr, "overlay: "+format+"\n", args...)
}

// Run returns when ctx ends.
func (o *Overlay) Run(ctx context.Context) {
	o.device = runtime.Overlay{LocalIP: o.NodeIP, PodCIDR: o.PodCIDR}
	delay := time.Second
	var last []runtime.Peer
	published := false
	for ctx.Err() == nil {
		changed := o.Store.Changed()
		err := o.pass(&last, &published)
		wait := overlayRecheck
		if err != nil {
			o.logf("%v (again in %s)", err, delay)
			wait, delay = delay, min(2*delay, 30*time.Second)
			changed = nil
		} else {
			delay = time.Second
		}
		select {
		case <-ctx.Done():
		case <-changed:
		case <-time.After(wait):
			last = nil // re-apply: someone may have removed a route
		}
	}
}

func (o *Overlay) pass(last *[]runtime.Peer, published *bool) error {
	mac, err := o.device.Ensure()
	if err != nil {
		return err
	}
	if !*published || mac.String() != o.mac.String() {
		if err := o.publish(mac); err != nil {
			return err
		}
		o.mac, *published = mac, true
	}
	recs, _, err := o.Store.List("", "nodes", "")
	if err != nil {
		return err
	}
	var peers []runtime.Peer
	for _, rec := range recs {
		var n apis.Node
		if json.Unmarshal(rec.Data, &n) != nil || n.Metadata.Name == o.NodeName || n.Metadata.DeletionTimestamp != nil {
			continue
		}
		cidr, err1 := netip.ParsePrefix(n.Spec.PodCIDR)
		ip, err2 := netip.ParseAddr(n.Metadata.Annotations[InternalIPAnnotation])
		mac, err3 := net.ParseMAC(n.Metadata.Annotations[VTEPAnnotation])
		if err1 != nil || err2 != nil || err3 != nil {
			continue // not on the overlay yet
		}
		peers = append(peers, runtime.Peer{PodCIDR: cidr.Masked(), NodeIP: ip, MAC: mac})
	}
	slices.SortFunc(peers, func(a, b runtime.Peer) int { return a.PodCIDR.Addr().Compare(b.PodCIDR.Addr()) })
	if slices.EqualFunc(peers, *last, func(a, b runtime.Peer) bool {
		return a.PodCIDR == b.PodCIDR && a.NodeIP == b.NodeIP && a.MAC.String() == b.MAC.String()
	}) && *last != nil {
		return nil
	}
	if err := o.device.SetPeers(peers); err != nil {
		return err
	}
	if len(peers) != o.reported {
		o.logf("reaching %d other node(s) through %s", len(peers), runtime.OverlayName)
		o.reported = len(peers)
	}
	*last = peers
	return nil
}

// publish writes this node's underlay address and device MAC on its Node.
func (o *Overlay) publish(mac net.HardwareAddr) error {
	return o.Store.Transact(func(tx store.Txn) error {
		rec, err := tx.Get(store.Key{Resource: "nodes", Name: o.NodeName})
		if err != nil {
			return fmt.Errorf("node %s is not registered yet: %w", o.NodeName, err)
		}
		var n apis.Node
		if err := json.Unmarshal(rec.Data, &n); err != nil {
			return err
		}
		if n.Metadata.Annotations[InternalIPAnnotation] == o.NodeIP.String() && n.Metadata.Annotations[VTEPAnnotation] == mac.String() {
			return nil
		}
		if n.Metadata.Annotations == nil {
			n.Metadata.Annotations = map[string]string{}
		}
		n.Metadata.Annotations[InternalIPAnnotation] = o.NodeIP.String()
		n.Metadata.Annotations[VTEPAnnotation] = mac.String()
		_, _, err = tx.Update(objects.Put(nodesRes, &n), rec.Rev)
		return err
	})
}
