package protobuf_test

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestIngressAndIngressClassProtobuf(t *testing.T) {
	// networking.k8s.io/v1 Ingress matching what `kubectl create ingress` sends.
	svcPortNum := vfield(2, 80)
	svcPortName := sfield(1, "http")
	backend1 := bfield(4, cat(sfield(1, "web"), bfield(2, svcPortNum)))
	backend2 := bfield(4, cat(sfield(1, "api"), bfield(2, svcPortName)))
	defBackend := bfield(4, cat(sfield(1, "fallback"), bfield(2, vfield(2, 8080))))

	path1 := bfield(1, cat(sfield(1, "/"), bfield(2, backend1), sfield(3, "Prefix")))
	path2 := bfield(1, cat(sfield(1, "/api"), bfield(2, backend2), sfield(3, "Exact")))
	rule := bfield(3, cat(sfield(1, "app.example.com"), bfield(2, bfield(1, cat(path1, path2)))))
	tls := bfield(2, cat(sfield(1, "app.example.com"), sfield(2, "app-tls")))
	spec := cat(bfield(1, defBackend), tls, rule, sfield(4, "shearwater"))

	body := envelope("networking.k8s.io/v1", "Ingress", cat(
		bfield(1, cat(sfield(1, "web-ing"), sfield(3, "default"))),
		bfield(2, spec),
	))
	v := mustDecode(t, body)
	if at(v, "apiVersion") != "networking.k8s.io/v1" || at(v, "kind") != "Ingress" {
		t.Fatalf("envelope lost: %v", v)
	}
	if at(v, "spec", "ingressClassName") != "shearwater" ||
		at(v, "spec", "defaultBackend", "service", "name") != "fallback" ||
		at(v, "spec", "defaultBackend", "service", "port", "number") != float64(8080) ||
		at(v, "spec", "tls", 0, "secretName") != "app-tls" ||
		at(v, "spec", "rules", 0, "host") != "app.example.com" ||
		at(v, "spec", "rules", 0, "http", "paths", 1, "path") != "/api" ||
		at(v, "spec", "rules", 0, "http", "paths", 1, "pathType") != "Exact" ||
		at(v, "spec", "rules", 0, "http", "paths", 1, "backend", "service", "port", "name") != "http" {
		t.Fatalf("Ingress decoded wrongly: %v", v)
	}
	out, unknown := decodeLikeTheServer(t, "Ingress", must(json.Marshal(v)))
	if len(unknown) > 0 || !strings.Contains(out, `"ingressClassName":"shearwater"`) {
		t.Fatalf("server decode of Ingress failed: unknown=%v out=%s", unknown, out)
	}

	// IngressClass
	icBody := envelope("networking.k8s.io/v1", "IngressClass", cat(
		bfield(1, sfield(1, "shearwater")),
		bfield(2, sfield(1, "shearwater.io/ingress")),
	))
	ic := mustDecode(t, icBody)
	if at(ic, "spec", "controller") != "shearwater.io/ingress" {
		t.Fatalf("IngressClass decoded wrongly: %v", ic)
	}
	out, unknown = decodeLikeTheServer(t, "IngressClass", must(json.Marshal(ic)))
	if len(unknown) > 0 || !strings.Contains(out, `"controller":"shearwater.io/ingress"`) {
		t.Fatalf("server decode of IngressClass failed: unknown=%v out=%s", unknown, out)
	}

	// Real body captured from kubectl v1.36.6:
	// kubectl create ingress demo --class=shearwater \
	//   --rule="app.example.com/web=web:80,tls=web-tls" \
	//   --rule="app.example.com/api*=api:http" --default-backend=fallback:8080
	realB64 := "azhzAAofChRuZXR3b3JraW5nLms4cy5pby92MRIHSW5ncmVzcxKyAQoUCgRkZW1vEgAaACIAKgAyADgAQgASlQEKEyIRCghmYWxsYmFjaxIFCgAQkD8SGgoPYXBwLmV4YW1wbGUuY29tEgd3ZWItdGxzGlYKD2FwcC5leGFtcGxlLmNvbRJDCkEKHAoEL3dlYhINIgsKA3dlYhIECgAQUBoFRXhhY3QKIQoEL2FwaRIRIg8KA2FwaRIICgRodHRwEAAaBlByZWZpeCIKc2hlYXJ3YXRlchoCCgAaACIA"
	realBody := must(base64.StdEncoding.DecodeString(realB64))
	realV := mustDecode(t, realBody)
	if at(realV, "metadata", "name") != "demo" ||
		at(realV, "spec", "ingressClassName") != "shearwater" ||
		at(realV, "spec", "defaultBackend", "service", "name") != "fallback" ||
		at(realV, "spec", "defaultBackend", "service", "port", "number") != float64(8080) ||
		at(realV, "spec", "tls", 0, "secretName") != "web-tls" ||
		at(realV, "spec", "rules", 0, "http", "paths", 0, "pathType") != "Exact" ||
		at(realV, "spec", "rules", 0, "http", "paths", 1, "pathType") != "Prefix" ||
		at(realV, "spec", "rules", 0, "http", "paths", 1, "backend", "service", "port", "name") != "http" {
		t.Fatalf("real kubectl v1.36.6 create ingress decoded wrongly: %v", realV)
	}
	if _, unknown := decodeLikeTheServer(t, "Ingress", must(json.Marshal(realV))); len(unknown) > 0 {
		t.Fatalf("real kubectl create ingress produced unknown fields: %v", unknown)
	}
}
