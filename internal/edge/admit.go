package edge

import (
	"encoding/json"
	"slices"
	"strings"

	"shearwater/internal/admission"
	"shearwater/internal/apis"
)

// HostOwnershipAdmission refuses creating or updating an Ingress or a Service
// with shearwater.io/public-host when another namespace already owns one of
// its host names (via an Ingress or a Service annotation). Multiple objects in
// the same namespace may share a host (for example to split paths).
func HostOwnershipAdmission() admission.Func {
	return func(req admission.Request) error {
		if req.Resource == nil || req.Tx == nil {
			return nil
		}
		var claimed []string
		switch {
		case req.Resource.Group == "networking.k8s.io" && req.Resource.Plural == "ingresses":
			ing, ok := req.Object.(*apis.Ingress)
			if !ok || !HandlesIngress(ing) {
				return nil
			}
			claimed = ingressHosts(ing)
		case req.Resource.Group == "" && req.Resource.Plural == "services":
			svc, ok := req.Object.(*apis.Service)
			if !ok {
				return nil
			}
			claimed = serviceHosts(svc)
		default:
			return nil
		}
		if len(claimed) == 0 {
			return nil
		}

		records, err := req.Tx.Objects()
		if err != nil {
			return err
		}
		for _, rec := range records {
			if rec.Deleting || rec.Key.Namespace == req.Namespace {
				continue
			}
			switch {
			case rec.Key.Group == "networking.k8s.io" && rec.Key.Resource == "ingresses":
				var other apis.Ingress
				if json.Unmarshal(rec.Data, &other) != nil || !HandlesIngress(&other) {
					continue
				}
				for _, h := range ingressHosts(&other) {
					if slices.Contains(claimed, h) {
						return admission.Conflict(
							"HOST_CONFLICT",
							"choose a different host name, or place the resource in namespace "+rec.Key.Namespace,
							"host %q is already owned by Ingress %s/%s", h, rec.Key.Namespace, rec.Key.Name,
						)
					}
				}
			case rec.Key.Group == "" && rec.Key.Resource == "services":
				var other apis.Service
				if json.Unmarshal(rec.Data, &other) != nil {
					continue
				}
				for _, h := range serviceHosts(&other) {
					if slices.Contains(claimed, h) {
						return admission.Conflict(
							"HOST_CONFLICT",
							"choose a different host name, or place the resource in namespace "+rec.Key.Namespace,
							"host %q is already owned by Service %s/%s", h, rec.Key.Namespace, rec.Key.Name,
						)
					}
				}
			}
		}
		return nil
	}
}

func ingressHosts(ing *apis.Ingress) []string {
	var hosts []string
	add := func(raw string) {
		if h, ok := NormalizeHost(raw); ok && !slices.Contains(hosts, h) {
			hosts = append(hosts, h)
		}
	}
	for _, r := range ing.Spec.Rules {
		if r.Host != "" {
			add(r.Host)
		}
	}
	for _, t := range ing.Spec.TLS {
		for _, h := range t.Hosts {
			add(h)
		}
	}
	return hosts
}

func serviceHosts(svc *apis.Service) []string {
	raw := svc.Metadata.Annotations[HostAnnotation]
	if raw == "" {
		return nil
	}
	var hosts []string
	for part := range strings.SplitSeq(raw, ",") {
		if h, ok := NormalizeHost(part); ok && !slices.Contains(hosts, h) {
			hosts = append(hosts, h)
		}
	}
	return hosts
}
