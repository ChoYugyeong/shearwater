package apis

import "net/netip"

// The EndpointSlice controller writes these markers and the TCP proxy reads
// them. The name finds the Service, its owner reference identifies the object
// across name reuse, and the generation ties the slice to one Service spec.
const (
	ServiceNameLabel            = "kubernetes.io/service-name"
	EndpointSliceManagedByLabel = "endpointslice.kubernetes.io/managed-by"
	EndpointSliceManager        = "shearwater"
	ServiceGenerationAnnotation = "shearwater.io/service-generation"
)

// UsablePodIP accepts an address a node agent can have given a Pod: inside
// the cluster range and, within its node's /24, not the network address, the
// bridge gateway or the broadcast address. With several nodes the Pods of a
// Service live in different /24s (docs/design-multinode-tenancy.md 3.3).
func UsablePodIP(ip netip.Addr) bool {
	if !ip.Is4() || !ClusterCIDR.Contains(ip) {
		return false
	}
	node := netip.PrefixFrom(ip, 24).Masked()
	return ip.Compare(node.Addr().Next()) > 0 && node.Contains(ip.Next())
}
