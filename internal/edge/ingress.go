package edge

import (
	"cmp"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"slices"
	"strings"
	"time"

	"shearwater/internal/apis"
)

// HandlesIngress reports whether Shearwater's edge handles ing: the
// ingressClassName must be unset, empty, or "shearwater"
// (docs/design-multinode-tenancy.md 4.3).
func HandlesIngress(ing *apis.Ingress) bool {
	if ing.Spec.IngressClassName == nil {
		return true
	}
	c := *ing.Spec.IngressClassName
	return c == "" || c == apis.DefaultIngressClassName
}

// PathRule is one compiled host+path routing rule.
type PathRule struct {
	Host     string // normalized lowercase DNS name, or "" for hostless catch-all
	Path     string // e.g. "/", "/api", "/v2/"
	PathType string // Exact, Prefix or ImplementationSpecific
	Route    Route
}

// SecretCertSpec identifies the kubernetes.io/tls Secret that provides TLS
// for one host on an Ingress.
type SecretCertSpec struct {
	Namespace   string
	SecretName  string
	IngressName string
}

// Table is an immutable routing and TLS specification built from Services and
// Ingresses.
type Table struct {
	ByHost         map[string][]PathRule
	Hostless       []PathRule
	DefaultBackend *Route
	HostOwner      map[string]string // normalized host -> owning namespace
	AllRoutes      []Route
	SecretTLS      map[string]SecretCertSpec
	Hosts          []string // all named hosts in ByHost (sorted)
	ACMEHosts      []string // named hosts without a SecretTLS entry (sorted)
}

func emptyTable() *Table {
	return &Table{
		ByHost:    map[string][]PathRule{},
		HostOwner: map[string]string{},
		SecretTLS: map[string]SecretCertSpec{},
	}
}

// Match selects the best backend for (hostport, reqPath) using Kubernetes
// Ingress matching semantics:
//   - Host-specific rules take precedence over hostless rules.
//   - Within a host, Exact matches strictly; Prefix (and ImplementationSpecific)
//     matches by path segments.
//   - Longest matching path wins; on equal length Exact beats Prefix.
//   - If no path rule matches, DefaultBackend is used when present.
func (t *Table) Match(hostport, reqPath string) (Route, bool) {
	if t == nil {
		return Route{}, false
	}
	if reqPath == "" {
		reqPath = "/"
	}
	host := requestHost(hostport)
	if host != "" {
		if rules, ok := t.ByHost[host]; ok {
			if r, matched := matchRules(rules, reqPath); matched {
				return r, true
			}
		}
	}
	if len(t.Hostless) > 0 {
		if r, matched := matchRules(t.Hostless, reqPath); matched {
			return r, true
		}
	}
	if t.DefaultBackend != nil {
		return *t.DefaultBackend, true
	}
	return Route{}, false
}

// HasHost reports whether host (normalized) is a published named host.
func (t *Table) HasHost(host string) bool {
	if t == nil || host == "" {
		return false
	}
	_, ok := t.ByHost[host]
	return ok
}

func matchRules(rules []PathRule, reqPath string) (Route, bool) {
	bestIdx := -1
	bestLen := -1
	bestExact := false

	for i, rule := range rules {
		if !pathMatches(rule.PathType, rule.Path, reqPath) {
			continue
		}
		l := len(rule.Path)
		exact := rule.PathType == apis.PathTypeExact
		if bestIdx < 0 || l > bestLen || (l == bestLen && exact && !bestExact) {
			bestIdx = i
			bestLen = l
			bestExact = exact
		}
	}
	if bestIdx < 0 {
		return Route{}, false
	}
	return rules[bestIdx].Route, true
}

// pathMatches implements Kubernetes Ingress path matching:
//   - Exact: case-sensitive exact string match.
//   - Prefix / ImplementationSpecific: case-sensitive segment-prefix match.
//     "/" matches all paths starting with "/".
//     A path ending with "/" (e.g. "/foo/") matches "/foo/" and "/foo/bar", not "/foo".
//     A path without trailing "/" (e.g. "/foo") matches "/foo", "/foo/" and "/foo/bar", not "/foobar".
func pathMatches(pathType, rulePath, reqPath string) bool {
	if pathType == apis.PathTypeExact {
		return reqPath == rulePath
	}
	// Prefix and ImplementationSpecific.
	if rulePath == "" || rulePath == "/" {
		return strings.HasPrefix(reqPath, "/")
	}
	if strings.HasSuffix(rulePath, "/") {
		return strings.HasPrefix(reqPath, rulePath)
	}
	return reqPath == rulePath || strings.HasPrefix(reqPath, rulePath+"/")
}

type routeSource struct {
	isIngress bool
	created   time.Time
	hasTime   bool
	key       string
	svc       apis.Service
	ing       apis.Ingress
}

// BuildTable compiles the routing table and TLS Secret mappings from Services
// and Ingresses. Older objects keep host and exact path claims when newer
// objects conflict; problems are reported without breaking other routes.
func BuildTable(services []apis.Service, ingresses []apis.Ingress) (*Table, []string) {
	t := emptyTable()
	var problems []string

	// Index Services by namespace/name for Ingress backend lookup.
	svcByKey := make(map[string]apis.Service, len(services))
	var sources []routeSource
	for _, s := range services {
		if s.Metadata.DeletionTimestamp != nil {
			continue
		}
		key := s.Metadata.Namespace + "/" + s.Metadata.Name
		svcByKey[key] = s
		if s.Metadata.Annotations[HostAnnotation] != "" {
			var ts time.Time
			hasTime := s.Metadata.CreationTimestamp != nil
			if hasTime {
				ts = s.Metadata.CreationTimestamp.Time
			}
			sources = append(sources, routeSource{
				isIngress: false, created: ts, hasTime: hasTime, key: "Service/" + key, svc: s,
			})
		}
	}
	for _, ing := range ingresses {
		if ing.Metadata.DeletionTimestamp != nil || !HandlesIngress(&ing) {
			continue
		}
		key := ing.Metadata.Namespace + "/" + ing.Metadata.Name
		var ts time.Time
		hasTime := ing.Metadata.CreationTimestamp != nil
		if hasTime {
			ts = ing.Metadata.CreationTimestamp.Time
		}
		sources = append(sources, routeSource{
			isIngress: true, created: ts, hasTime: hasTime, key: "Ingress/" + key, ing: ing,
		})
	}

	slices.SortStableFunc(sources, func(a, b routeSource) int {
		if a.hasTime && b.hasTime && !a.created.Equal(b.created) {
			return a.created.Compare(b.created)
		}
		return cmp.Compare(a.key, b.key)
	})

	ownerLabel := map[string]string{} // normalized host -> "Service ns/name" or "Ingress ns/name"
	seenRules := map[struct {
		host  string
		exact bool
		path  string
	}]string{}

	claimHost := func(host, ns, who string) bool {
		if existingNS, taken := t.HostOwner[host]; taken && existingNS != ns {
			problems = append(problems, fmt.Sprintf("%s: host %s already belongs to %s", who, host, ownerLabel[host]))
			return false
		}
		if _, taken := t.HostOwner[host]; !taken {
			t.HostOwner[host] = ns
			ownerLabel[host] = who
		}
		return true
	}

	addRule := func(rule PathRule, who string) {
		k := struct {
			host  string
			exact bool
			path  string
		}{
			host:  rule.Host,
			exact: rule.PathType == apis.PathTypeExact,
			path:  rule.Path,
		}
		if prev, dup := seenRules[k]; dup {
			if prev != who {
				if rule.Host != "" && rule.Path == "/" && !k.exact && strings.HasPrefix(who, "Service ") && strings.HasPrefix(prev, "Service ") {
					problems = append(problems, fmt.Sprintf("%s: host %s already belongs to %s", who, rule.Host, prev))
				} else {
					problems = append(problems, fmt.Sprintf("%s: host %q path %q (%s) already belongs to %s", who, rule.Host, rule.Path, rule.PathType, prev))
				}
			}
			return
		}
		seenRules[k] = who
		if rule.Host == "" {
			t.Hostless = append(t.Hostless, rule)
		} else {
			t.ByHost[rule.Host] = append(t.ByHost[rule.Host], rule)
		}
	}

	resolveBackend := func(ns string, b *apis.IngressBackend) (svcName, addr string, err error) {
		if b == nil || b.Service == nil {
			return "", "", fmt.Errorf("backend has no service")
		}
		svc, ok := svcByKey[ns+"/"+b.Service.Name]
		if !ok {
			return "", "", fmt.Errorf("Service %s/%s not found", ns, b.Service.Name)
		}
		addr, err = backendOfPort(svc, b.Service.Port)
		if err != nil {
			return "", "", fmt.Errorf("Service %s/%s: %w", ns, b.Service.Name, err)
		}
		return b.Service.Name, addr, nil
	}

	for _, src := range sources {
		if !src.isIngress {
			s := src.svc
			ns := s.Metadata.Namespace
			name := ns + "/" + s.Metadata.Name
			who := "Service " + name
			backend, err := backendOf(s)
			if err != nil {
				problems = append(problems, fmt.Sprintf("Service %s is not published: %v", name, err))
				continue
			}
			hosts := strings.Split(s.Metadata.Annotations[HostAnnotation], ",")
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
				if !claimHost(host, ns, who) {
					continue
				}
				addRule(PathRule{
					Host:     host,
					Path:     "/",
					PathType: apis.PathTypePrefix,
					Route:    Route{Host: host, Namespace: ns, Service: s.Metadata.Name, Backend: backend},
				}, who)
			}
			continue
		}

		ing := src.ing
		ns := ing.Metadata.Namespace
		name := ns + "/" + ing.Metadata.Name
		who := "Ingress " + name

		if ing.Spec.DefaultBackend != nil {
			svcName, backend, err := resolveBackend(ns, ing.Spec.DefaultBackend)
			if err != nil {
				problems = append(problems, fmt.Sprintf("%s defaultBackend: %v", who, err))
			} else if t.DefaultBackend == nil {
				r := Route{Namespace: ns, Service: svcName, Backend: backend, Source: who}
				t.DefaultBackend = &r
			}
		}

		var ingHosts []string
		for _, rule := range ing.Spec.Rules {
			host := ""
			if rule.Host != "" {
				var ok bool
				host, ok = NormalizeHost(rule.Host)
				if !ok {
					problems = append(problems, fmt.Sprintf("%s: %q is not a host name", who, rule.Host))
					continue
				}
				if !claimHost(host, ns, who) {
					continue
				}
				if !slices.Contains(ingHosts, host) {
					ingHosts = append(ingHosts, host)
				}
			}
			if rule.HTTP == nil {
				continue
			}
			for _, p := range rule.HTTP.Paths {
				svcName, backend, err := resolveBackend(ns, &p.Backend)
				if err != nil {
					problems = append(problems, fmt.Sprintf("%s path %q: %v", who, p.Path, err))
					continue
				}
				pt := apis.PathTypePrefix
				if p.PathType != nil && *p.PathType != "" {
					pt = *p.PathType
				}
				path := p.Path
				if path == "" {
					path = "/"
				}
				addRule(PathRule{
					Host:     host,
					Path:     path,
					PathType: pt,
					Route: Route{
						Host:      host,
						Path:      path,
						PathType:  pt,
						Namespace: ns,
						Service:   svcName,
						Backend:   backend,
						Source:    who,
					},
				}, who)
			}
		}

		for _, tlsSpec := range ing.Spec.TLS {
			if tlsSpec.SecretName == "" {
				continue
			}
			targetHosts := tlsSpec.Hosts
			if len(targetHosts) == 0 {
				targetHosts = ingHosts
			}
			for _, rawHost := range targetHosts {
				host, ok := NormalizeHost(rawHost)
				if !ok {
					problems = append(problems, fmt.Sprintf("%s TLS: %q is not a host name", who, rawHost))
					continue
				}
				if owner, exists := t.HostOwner[host]; exists && owner != ns {
					problems = append(problems, fmt.Sprintf("%s TLS: host %s already belongs to %s", who, host, ownerLabel[host]))
					continue
				}
				if _, exists := t.SecretTLS[host]; !exists {
					t.SecretTLS[host] = SecretCertSpec{
						Namespace:   ns,
						SecretName:  tlsSpec.SecretName,
						IngressName: ing.Metadata.Name,
					}
				}
			}
		}
	}

	t.Hosts = make([]string, 0, len(t.ByHost))
	for h := range t.ByHost {
		t.Hosts = append(t.Hosts, h)
	}
	slices.Sort(t.Hosts)
	for _, h := range t.Hosts {
		if _, hasSecret := t.SecretTLS[h]; !hasSecret {
			t.ACMEHosts = append(t.ACMEHosts, h)
		}
		for _, rule := range t.ByHost[h] {
			t.AllRoutes = append(t.AllRoutes, rule.Route)
		}
	}
	for _, rule := range t.Hostless {
		t.AllRoutes = append(t.AllRoutes, rule.Route)
	}
	if t.DefaultBackend != nil {
		t.AllRoutes = append(t.AllRoutes, *t.DefaultBackend)
	}
	return t, problems
}

// LoadSecretCerts parses kubernetes.io/tls Secrets referenced by Ingress TLS
// rules into tls.Certificates keyed by normalized host name.
func LoadSecretCerts(specs map[string]SecretCertSpec, secrets []apis.Secret) (map[string]*tls.Certificate, []string) {
	if len(specs) == 0 {
		return nil, nil
	}
	byKey := make(map[string]apis.Secret, len(secrets))
	for _, s := range secrets {
		if s.Metadata.DeletionTimestamp != nil {
			continue
		}
		byKey[s.Metadata.Namespace+"/"+s.Metadata.Name] = s
	}

	hosts := make([]string, 0, len(specs))
	for h := range specs {
		hosts = append(hosts, h)
	}
	slices.Sort(hosts)

	certs := map[string]*tls.Certificate{}
	var problems []string
	for _, host := range hosts {
		spec := specs[host]
		key := spec.Namespace + "/" + spec.SecretName
		sec, ok := byKey[key]
		if !ok {
			problems = append(problems, fmt.Sprintf("Ingress %s/%s: TLS Secret %s not found for host %s", spec.Namespace, spec.IngressName, key, host))
			continue
		}
		if sec.SecretType != apis.SecretTypeTLS {
			problems = append(problems, fmt.Sprintf("Ingress %s/%s: TLS Secret %s has type %q, want %q", spec.Namespace, spec.IngressName, key, sec.SecretType, apis.SecretTypeTLS))
			continue
		}
		certPEM, keyPEM := sec.Data["tls.crt"], sec.Data["tls.key"]
		if len(certPEM) == 0 || len(keyPEM) == 0 {
			problems = append(problems, fmt.Sprintf("Ingress %s/%s: TLS Secret %s is missing tls.crt or tls.key", spec.Namespace, spec.IngressName, key))
			continue
		}
		pair, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			problems = append(problems, fmt.Sprintf("Ingress %s/%s: TLS Secret %s is not a valid key pair: %v", spec.Namespace, spec.IngressName, key, err))
			continue
		}
		if len(pair.Certificate) > 0 {
			pair.Leaf, _ = x509.ParseCertificate(pair.Certificate[0])
		}
		certs[host] = &pair
	}
	return certs, problems
}
