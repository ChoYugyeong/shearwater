package apis

import (
	"fmt"
	"net/netip"
	"slices"
)

// Limits from Kubernetes' pod validation (validateDNSPolicy, validatePodDNSConfig).
const (
	maxDNSNameservers   = 3
	maxDNSSearchDomains = 32
	maxDNSSearchChars   = 2048
)

var dnsPolicies = []string{"ClusterFirst", "ClusterFirstWithHostNet", "Default", "None"}

func validateDNS(s *PodSpec, path string, errs *ErrorList) {
	if s.DNSPolicy != "" && !slices.Contains(dnsPolicies, s.DNSPolicy) {
		errs.oneOf(path+".dnsPolicy", s.DNSPolicy, dnsPolicies...)
	}
	c := s.DNSConfig
	if s.DNSPolicy == "None" && (c == nil || len(c.Nameservers) == 0) {
		errs.required(path + ".dnsConfig.nameservers")
	}
	if c == nil {
		return
	}
	if len(c.Nameservers) > maxDNSNameservers {
		errs.invalid(path+".dnsConfig.nameservers", len(c.Nameservers), fmt.Sprintf("must not have more than %d nameservers", maxDNSNameservers))
	}
	for i, ns := range c.Nameservers {
		if _, err := netip.ParseAddr(ns); err != nil {
			errs.invalid(fmt.Sprintf("%s.dnsConfig.nameservers[%d]", path, i), ns, "must be a valid IP address")
		}
	}
	if len(c.Searches) > maxDNSSearchDomains {
		errs.invalid(path+".dnsConfig.searches", len(c.Searches), fmt.Sprintf("must not have more than %d search paths", maxDNSSearchDomains))
	}
	chars := 0
	for i, d := range c.Searches {
		chars += len(d)
		if d != "." && !IsDNS1123Subdomain(d) {
			errs.invalid(fmt.Sprintf("%s.dnsConfig.searches[%d]", path, i), d, "must be a lowercase RFC 1123 subdomain")
		}
	}
	if chars > maxDNSSearchChars {
		errs.invalid(path+".dnsConfig.searches", chars, fmt.Sprintf("must not have more than %d characters (including spaces) in the search list", maxDNSSearchChars))
	}
	for i, o := range c.Options {
		if o.Name == "" {
			errs.required(fmt.Sprintf("%s.dnsConfig.options[%d].name", path, i))
		}
	}
}
