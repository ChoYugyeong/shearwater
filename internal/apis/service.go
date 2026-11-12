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

// UsablePodIP excludes the network address, bridge gateway and broadcast
// address. The node allocates the IPv4 addresses between those reservations.
func UsablePodIP(ip netip.Addr) bool {
	return ip.Is4() && PodCIDR.Contains(ip) &&
		ip.Compare(PodCIDR.Masked().Addr().Next()) > 0 && PodCIDR.Contains(ip.Next())
}
