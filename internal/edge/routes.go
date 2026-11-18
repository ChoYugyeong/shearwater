// Package edge is Shearwater's public entry: it accepts HTTP and HTTPS on the
// host and forwards each request to the ClusterIP Service that published the
// requested host name. Certificates come from an ACME authority.
package edge

import (
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"shearwater/internal/apis"
)

const (
	// HostAnnotation on a Service publishes it under one or more host names,
	// separated by commas: shearwater.io/public-host: app.example.com
	HostAnnotation = "shearwater.io/public-host"
	// PortAnnotation selects the Service port by number or name when the
	// Service has more than one TCP port.
	PortAnnotation = "shearwater.io/public-port"
	maxHosts       = 8
)

// Route sends one public host name to one Service port.
type Route struct {
	Host      string `json:"host"`
	Namespace string `json:"namespace"`
	Service   string `json:"service"`
	Backend   string `json:"backend"` // ClusterIP:port
}

// Routes builds the routing table from Services. A Service that cannot be
// published is skipped with a problem line; it never breaks other routes.
// When two Services claim one host the older Service keeps it.
func Routes(services []apis.Service) (map[string]Route, []string) {
	slices.SortStableFunc(services, func(a, b apis.Service) int {
		at, bt := a.Metadata.CreationTimestamp, b.Metadata.CreationTimestamp
		if at != nil && bt != nil && !at.Time.Equal(bt.Time) {
			return at.Time.Compare(bt.Time)
		}
		return strings.Compare(a.Metadata.Namespace+"/"+a.Metadata.Name, b.Metadata.Namespace+"/"+b.Metadata.Name)
	})
	routes := map[string]Route{}
	var problems []string
	for _, s := range services {
		value := s.Metadata.Annotations[HostAnnotation]
		if value == "" || s.Metadata.DeletionTimestamp != nil {
			continue
		}
		name := s.Metadata.Namespace + "/" + s.Metadata.Name
		backend, err := backendOf(s)
		if err != nil {
			problems = append(problems, fmt.Sprintf("Service %s is not published: %v", name, err))
			continue
		}
		hosts := strings.Split(value, ",")
		if len(hosts) > maxHosts {
			problems = append(problems, fmt.Sprintf("Service %s is not published: more than %d host names", name, maxHosts))
			continue
		}
		for _, h := range hosts {
			host, ok := NormalizeHost(h)
			if !ok {
				problems = append(problems, fmt.Sprintf("Service %s: %q is not a host name", name, strings.TrimSpace(h)))
				continue
			}
			if taken, dup := routes[host]; dup {
				if taken.Namespace != s.Metadata.Namespace || taken.Service != s.Metadata.Name {
					problems = append(problems, fmt.Sprintf("Service %s: host %s already belongs to Service %s/%s", name, host, taken.Namespace, taken.Service))
				}
				continue
			}
			routes[host] = Route{Host: host, Namespace: s.Metadata.Namespace, Service: s.Metadata.Name, Backend: backend}
		}
	}
	return routes, problems
}

// NormalizeHost lowercases a host name and rejects anything that is not a
// plain DNS name: IP literals, ports, wildcards and trailing dots.
func NormalizeHost(h string) (string, bool) {
	h = strings.ToLower(strings.TrimSpace(h))
	if _, err := netip.ParseAddr(h); err == nil || !apis.IsDNS1123Subdomain(h) {
		return "", false
	}
	return h, true
}

// requestHost extracts the routing key from a Host header or TLS server name.
func requestHost(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		hostport = h
	}
	h, _ := NormalizeHost(strings.TrimSuffix(hostport, "."))
	return h
}

func backendOf(s apis.Service) (string, error) {
	ip, err := netip.ParseAddr(s.Spec.ClusterIP)
	if err != nil || !ip.Is4() || !apis.ServiceCIDR.Contains(ip) {
		return "", fmt.Errorf("it has no ClusterIP")
	}
	var tcp []apis.ServicePort
	for _, p := range s.Spec.Ports {
		if p.Protocol == "" || p.Protocol == "TCP" {
			tcp = append(tcp, p)
		}
	}
	want := strings.TrimSpace(s.Metadata.Annotations[PortAnnotation])
	for _, p := range tcp {
		if want == "" && len(tcp) == 1 || want != "" && (want == p.Name || want == strconv.Itoa(int(p.Port))) {
			return netip.AddrPortFrom(ip, uint16(p.Port)).String(), nil
		}
	}
	if want != "" {
		return "", fmt.Errorf("%s=%q matches no TCP port", PortAnnotation, want)
	}
	return "", fmt.Errorf("it has %d TCP ports; choose one with %s", len(tcp), PortAnnotation)
}
