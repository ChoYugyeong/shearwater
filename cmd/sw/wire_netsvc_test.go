package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"shearwater/internal/admission"
	"shearwater/internal/api"
	"shearwater/internal/apis"
	"shearwater/internal/objects"
	"shearwater/internal/store"
)

func TestWireNetworkServicesBootstrapsIngressClassAndHostAdmission(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	srv := &api.Server{Store: st, Token: "secret"}
	plugins := wireNetworkServices(&wiring{Server: srv, Store: st})
	srv.Admit = admission.Chain(plugins...)
	if err := srv.Bootstrap(); err != nil {
		t.Fatal(err)
	}

	// Default IngressClass "shearwater" exists and is marked default.
	icRes := apis.Lookup("networking.k8s.io", "v1", "ingressclasses")
	rec, err := st.Get(objects.Key(icRes, "", apis.DefaultIngressClassName))
	if err != nil {
		t.Fatalf("default IngressClass not created: %v", err)
	}
	var ic apis.IngressClass
	if err := json.Unmarshal(rec.Data, &ic); err != nil {
		t.Fatal(err)
	}
	if ic.Spec.Controller != apis.IngressClassController || ic.Metadata.Annotations[apis.DefaultIngressClassAnnotation] != "true" {
		t.Fatalf("unexpected default IngressClass: %+v", ic)
	}

	// Calling wireNetworkServices a second time is idempotent.
	if p2 := wireNetworkServices(&wiring{Server: srv, Store: st}); len(p2) != 1 {
		t.Fatalf("expected 1 admission plugin, got %d", len(p2))
	}

	do := func(method, path, body string, headers ...string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer secret")
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		for i := 0; i+1 < len(headers); i += 2 {
			req.Header.Set(headers[i], headers[i+1])
		}
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		return w
	}

	// Create team-b namespace.
	if w := do("POST", "/api/v1/namespaces", `{"metadata":{"name":"team-b"}}`); w.Code != http.StatusCreated {
		t.Fatalf("create namespace team-b: %d %s", w.Code, w.Body.String())
	}

	// Create Ingress in default namespace.
	ingBody := `{
		"apiVersion": "networking.k8s.io/v1",
		"kind": "Ingress",
		"metadata": {"name": "web"},
		"spec": {
			"ingressClassName": "shearwater",
			"tls": [{"hosts": ["shop.example.com"], "secretName": "shop-tls"}],
			"rules": [{"host": "shop.example.com", "http": {"paths": [{"path": "/", "pathType": "Prefix", "backend": {"service": {"name": "web", "port": {"number": 80}}}}]}}]
		}
	}`
	if w := do("POST", "/apis/networking.k8s.io/v1/namespaces/default/ingresses", ingBody); w.Code != http.StatusCreated {
		t.Fatalf("create Ingress in default: %d %s", w.Code, w.Body.String())
	}

	// Table output for Ingress must have NAME CLASS HOSTS ADDRESS PORTS AGE.
	w := do("GET", "/apis/networking.k8s.io/v1/namespaces/default/ingresses", "", "Accept", "application/json;as=Table;v=v1;g=meta.k8s.io")
	if w.Code != http.StatusOK {
		t.Fatalf("table get ingresses: %d %s", w.Code, w.Body.String())
	}
	var tb map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &tb)
	cols := tb["columnDefinitions"].([]any)
	var names []string
	for _, c := range cols {
		names = append(names, c.(map[string]any)["name"].(string))
	}
	if got := strings.Join(names, ","); got != "Name,Class,Hosts,Address,Ports,Age" {
		t.Fatalf("Ingress table columns = %q, want Name,Class,Hosts,Address,Ports,Age", got)
	}
	row := tb["rows"].([]any)[0].(map[string]any)["cells"].([]any)
	if row[0] != "web" || row[1] != "shearwater" || row[2] != "shop.example.com" || row[4] != "80, 443" {
		t.Fatalf("unexpected Ingress table row: %v", row)
	}

	// Creating an Ingress in team-b with the same host must be refused with 409 Conflict.
	if w := do("POST", "/apis/networking.k8s.io/v1/namespaces/team-b/ingresses", ingBody); w.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict for cross-namespace Ingress host claim, got %d %s", w.Code, w.Body.String())
	}

	// Creating a Service in team-b with shearwater.io/public-host=shop.example.com must also be refused with 409 Conflict.
	svcBody := `{"metadata":{"name":"steal","annotations":{"shearwater.io/public-host":"shop.example.com"}},"spec":{"ports":[{"port":80}]}}`
	if w := do("POST", "/api/v1/namespaces/team-b/services", svcBody); w.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict for cross-namespace Service host claim, got %d %s", w.Code, w.Body.String())
	}
}
