package api

import (
	"strings"
	"testing"
)

func TestSecretCreateMergesStringDataAndTablesHideValues(t *testing.T) {
	c := newClient(t)
	r := c.want(c.do("POST", "/api/v1/namespaces/default/secrets",
		`{"apiVersion":"v1","kind":"Secret","metadata":{"name":"db"},"data":{"user":"YWRtaW4="},"stringData":{"password":"s3cret","user":"app"}}`), 201)
	if r.at("type") != "Opaque" || r.at("stringData") != nil {
		t.Fatalf("type must default and stringData must not be stored: %s", r.raw)
	}
	data, _ := r.at("data").(map[string]any)
	if data["password"] != "czNjcmV0" || data["user"] != "YXBw" {
		t.Fatalf("stringData must be merged into data and win over it: %v", data)
	}
	table := c.want(c.do("GET", "/api/v1/namespaces/default/secrets", "", "Accept", "application/json;as=Table;v=v1;g=meta.k8s.io"), 200)
	if strings.Contains(table.raw, "czNjcmV0") || strings.Contains(table.raw, "s3cret") || !strings.Contains(table.raw, `"cells":["db","Opaque",2,`) {
		t.Fatalf("table must show name, type and key count only: %s", table.raw)
	}
}

func TestSecretValidationAndImmutability(t *testing.T) {
	c := newClient(t)
	for _, tc := range []struct{ body, want string }{
		{`{"metadata":{"name":"a"},"stringData":{"bad key":"x"}}`, "data"},
		{`{"metadata":{"name":"b"},"type":"kubernetes.io/tls","stringData":{"tls.crt":"x"}}`, "tls.key"},
		{`{"metadata":{"name":"c"},"stringData":{"..":"x"}}`, "data"},
		{`{"metadata":{"name":"d"},"stringData":{"k":"` + strings.Repeat("x", 1<<20+1) + `"}}`, "total size"},
	} {
		r := c.do("POST", "/api/v1/namespaces/default/secrets", tc.body)
		if r.code != 422 || !strings.Contains(r.raw, tc.want) {
			t.Errorf("%.60s: got %d %s", tc.body, r.code, r.raw)
		}
	}
	c.want(c.do("POST", "/api/v1/namespaces/default/secrets", `{"metadata":{"name":"frozen"},"immutable":true,"stringData":{"k":"v"}}`), 201)
	r := c.do("PATCH", "/api/v1/namespaces/default/secrets/frozen", `{"stringData":{"k":"changed"}}`, "Content-Type", "application/merge-patch+json")
	if r.code != 422 || !strings.Contains(r.raw, "immutable") {
		t.Fatalf("an immutable secret must reject data changes: %d %s", r.code, r.raw)
	}
	c.want(c.do("POST", "/api/v1/namespaces/default/secrets", `{"metadata":{"name":"typed"},"stringData":{"k":"v"}}`), 201)
	r = c.do("PATCH", "/api/v1/namespaces/default/secrets/typed", `{"type":"kubernetes.io/basic-auth"}`, "Content-Type", "application/merge-patch+json")
	if r.code != 422 || !strings.Contains(r.raw, "type") {
		t.Fatalf("type must be immutable: %d %s", r.code, r.raw)
	}
}

func TestPodEnvAcceptsOnlySecretSources(t *testing.T) {
	c := newClient(t)
	pod := func(container string) string {
		return `{"metadata":{"name":"p"},"spec":{"containers":[{"name":"c","image":"nginx",` + container + `}]}}`
	}
	ok := pod(`"envFrom":[{"prefix":"DB_","secretRef":{"name":"db","optional":true}}],"env":[{"name":"P","valueFrom":{"secretKeyRef":{"name":"db","key":"password"}}}]`)
	c.want(c.do("POST", "/api/v1/namespaces/default/pods", ok), 201)
	for _, tc := range []struct{ container, want string }{
		{`"env":[{"name":"A","valueFrom":{"configMapKeyRef":{"name":"x","key":"k"}}}]`, "configMapKeyRef"},
		{`"env":[{"name":"A","valueFrom":{"fieldRef":{"fieldPath":"metadata.name"}}}]`, "fieldRef"},
		{`"env":[{"name":"A","value":"v","valueFrom":{"secretKeyRef":{"name":"db","key":"k"}}}]`, "may not be specified"},
		{`"env":[{"name":"A","valueFrom":{"secretKeyRef":{"name":"db","key":"bad key"}}}]`, "secretKeyRef.key"},
		{`"envFrom":[{"configMapRef":{"name":"x"}}]`, "configMapRef"},
	} {
		r := c.do("POST", "/api/v1/namespaces/default/pods", strings.Replace(pod(tc.container), `"name":"p"`, `"name":"q"`, 1))
		if r.code != 422 || !strings.Contains(r.raw, tc.want) {
			t.Errorf("%s: got %d %s", tc.container, r.code, r.raw)
		}
	}
}

func TestTraceNeverRecordsSecretBodies(t *testing.T) {
	c := newClient(t)
	var trace strings.Builder
	c.srv.Trace = &trace
	c.want(c.do("POST", "/api/v1/namespaces/default/secrets", `{"metadata":{"name":"db"},"stringData":{"password":"trace-me-not"}}`), 201)
	c.want(c.do("POST", "/api/v1/namespaces/default/pods", `{"metadata":{"name":"p"},"spec":{"containers":[{"name":"c","image":"nginx"}]}}`), 201)
	if strings.Contains(trace.String(), "trace-me-not") || !strings.Contains(trace.String(), `"bodyRedacted"`) || !strings.Contains(trace.String(), `"image\":\"nginx\"`) {
		t.Fatalf("secret body must be redacted while other bodies stay traced:\n%s", trace.String())
	}
}
