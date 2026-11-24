package apis

import (
	"strings"
	"testing"
)

func TestIngressDefaultsAndValidation(t *testing.T) {
	if Lookup("networking.k8s.io", "v1", "ingresses") == nil {
		t.Fatal("Ingress resource is not registered")
	}
	if Lookup("networking.k8s.io", "v1", "ingressclasses") == nil {
		t.Fatal("IngressClass resource is not registered")
	}

	valid := decodeInto[Ingress](t, `{
		"apiVersion": "networking.k8s.io/v1",
		"kind": "Ingress",
		"metadata": {"name": "web", "namespace": "default"},
		"spec": {
			"ingressClassName": "shearwater",
			"defaultBackend": {"service": {"name": "fallback", "port": {"number": 8080}}},
			"tls": [{"hosts": ["app.example.com"], "secretName": "app-tls"}],
			"rules": [{
				"host": "app.example.com",
				"http": {"paths": [
					{"pathType": "Prefix", "backend": {"service": {"name": "web", "port": {"name": "http"}}}},
					{"path": "/api", "pathType": "Exact", "backend": {"service": {"name": "api", "port": {"number": 80}}}}
				]}
			}]
		}
	}`)
	DefaultIngress(valid)
	if errs := ValidateIngress(valid); len(errs) > 0 {
		t.Fatalf("valid Ingress failed validation:\n%s", reasons(errs))
	}
	if valid.Spec.Rules[0].HTTP.Paths[0].Path != "/" {
		t.Errorf("empty Prefix path should default to '/', got %q", valid.Spec.Rules[0].HTTP.Paths[0].Path)
	}

	bad := []struct{ name, body, want string }{
		{"neither rules nor defaultBackend", `{"metadata":{"name":"x"},"spec":{}}`, "either `defaultBackend` or `rules`"},
		{"IP host", `{"metadata":{"name":"x"},"spec":{"rules":[{"host":"10.0.0.1","http":{"paths":[{"path":"/","pathType":"Prefix","backend":{"service":{"name":"web","port":{"number":80}}}}]}}]}}`, "not an IP address"},
		{"host with port", `{"metadata":{"name":"x"},"spec":{"rules":[{"host":"a.example.com:443","http":{"paths":[{"path":"/","pathType":"Prefix","backend":{"service":{"name":"web","port":{"number":80}}}}]}}]}}`, "must not include a port"},
		{"wildcard host unsupported", `{"metadata":{"name":"x"},"spec":{"rules":[{"host":"*.example.com","http":{"paths":[{"path":"/","pathType":"Prefix","backend":{"service":{"name":"web","port":{"number":80}}}}]}}]}}`, "wildcard hosts are not supported"},
		{"missing pathType", `{"metadata":{"name":"x"},"spec":{"rules":[{"host":"a.example.com","http":{"paths":[{"path":"/","backend":{"service":{"name":"web","port":{"number":80}}}}]}}]}}`, "pathType: Required"},
		{"bad pathType", `{"metadata":{"name":"x"},"spec":{"rules":[{"host":"a.example.com","http":{"paths":[{"path":"/","pathType":"Regex","backend":{"service":{"name":"web","port":{"number":80}}}}]}}]}}`, "Unsupported value"},
		{"relative path", `{"metadata":{"name":"x"},"spec":{"rules":[{"host":"a.example.com","http":{"paths":[{"path":"api","pathType":"Prefix","backend":{"service":{"name":"web","port":{"number":80}}}}]}}]}}`, "starting with '/'"},
		{"dotdot path", `{"metadata":{"name":"x"},"spec":{"rules":[{"host":"a.example.com","http":{"paths":[{"path":"/a/../b","pathType":"Prefix","backend":{"service":{"name":"web","port":{"number":80}}}}]}}]}}`, "'//'"},
		{"resource backend unsupported", `{"metadata":{"name":"x"},"spec":{"defaultBackend":{"resource":{"kind":"StorageBucket","name":"b"}}}}`, "only service backends are supported"},
		{"both port name and number", `{"metadata":{"name":"x"},"spec":{"defaultBackend":{"service":{"name":"web","port":{"name":"http","number":80}}}}}`, "cannot set both `name` and `number`"},
		{"missing port", `{"metadata":{"name":"x"},"spec":{"defaultBackend":{"service":{"name":"web"}}}}`, "service.port: Required"},
		{"port out of range", `{"metadata":{"name":"x"},"spec":{"defaultBackend":{"service":{"name":"web","port":{"number":70000}}}}}`, "between 1 and 65535"},
		{"bad secretName", `{"metadata":{"name":"x"},"spec":{"tls":[{"hosts":["a.example.com"],"secretName":"BAD_NAME"}],"defaultBackend":{"service":{"name":"web","port":{"number":80}}}}}`, "tls[0].secretName"},
	}
	for _, tc := range bad {
		ing := decodeInto[Ingress](t, tc.body)
		DefaultIngress(ing)
		got := reasons(ValidateIngress(ing))
		if !strings.Contains(got, tc.want) {
			t.Errorf("%s: want error containing %q, got:\n%s", tc.name, tc.want, got)
		}
	}
}

func TestIngressClassValidation(t *testing.T) {
	ic := decodeInto[IngressClass](t, `{"metadata":{"name":"shearwater"},"spec":{"controller":"shearwater.io/ingress"}}`)
	if errs := ValidateIngressClass(ic); len(errs) > 0 {
		t.Fatalf("valid IngressClass failed:\n%s", reasons(errs))
	}
	bad := decodeInto[IngressClass](t, `{"metadata":{"name":"bad"},"spec":{"controller":"no-slash"}}`)
	if got := reasons(ValidateIngressClass(bad)); !strings.Contains(got, "domain-prefixed path") {
		t.Fatalf("expected controller error, got:\n%s", got)
	}
	changed := decodeInto[IngressClass](t, `{"metadata":{"name":"shearwater"},"spec":{"controller":"other.io/ingress"}}`)
	if got := reasons(ValidateIngressClassUpdate(changed, ic)); !strings.Contains(got, "immutable") {
		t.Fatalf("changing spec.controller must be forbidden, got:\n%s", got)
	}
}

func TestPodDNSConfigDecode(t *testing.T) {
	p := decodeInto[Pod](t, `{
		"metadata": {"name": "p"},
		"spec": {
			"dnsPolicy": "None",
			"dnsConfig": {
				"nameservers": ["1.1.1.1"],
				"searches": ["example.local"],
				"options": [{"name": "ndots", "value": "2"}, {"name": "edns0"}]
			},
			"containers": [{"name": "c", "image": "nginx:1.27"}]
		}
	}`)
	if p.Spec.DNSConfig == nil || len(p.Spec.DNSConfig.Nameservers) != 1 || p.Spec.DNSConfig.Nameservers[0] != "1.1.1.1" {
		t.Fatalf("DNSConfig lost during decode: %+v", p.Spec.DNSConfig)
	}
	if len(p.Spec.DNSConfig.Options) != 2 || p.Spec.DNSConfig.Options[0].Value == nil || *p.Spec.DNSConfig.Options[0].Value != "2" || p.Spec.DNSConfig.Options[1].Value != nil {
		t.Fatalf("DNSConfig options decoded wrongly: %+v", p.Spec.DNSConfig.Options)
	}
}
