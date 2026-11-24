package apis

// PodDNSConfig defines the DNS parameters of a pod in addition to those
// generated from dnsPolicy (docs/design-multinode-tenancy.md 4.2).
type PodDNSConfig struct {
	Nameservers []string             `json:"nameservers,omitempty"`
	Searches    []string             `json:"searches,omitempty"`
	Options     []PodDNSConfigOption `json:"options,omitempty"`
}

// PodDNSConfigOption defines one entry in resolv.conf's options line.
type PodDNSConfigOption struct {
	Name  string  `json:"name,omitempty"`
	Value *string `json:"value,omitempty"`
}
