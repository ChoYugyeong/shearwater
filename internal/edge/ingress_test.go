package edge

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"shearwater/internal/admission"
	"shearwater/internal/apis"
	"shearwater/internal/objects"
	"shearwater/internal/store"
)

func strPtr(s string) *string { return &s }

func mkIngress(ns, name string, class *string, created time.Time, rules []apis.IngressRule, def *apis.IngressBackend, tlsCfg []apis.IngressTLS) apis.Ingress {
	ing := apis.Ingress{
		Metadata: apis.ObjectMeta{Namespace: ns, Name: name},
		Spec:     apis.IngressSpec{IngressClassName: class, Rules: rules, DefaultBackend: def, TLS: tlsCfg},
	}
	if !created.IsZero() {
		ing.Metadata.CreationTimestamp = &apis.Time{Time: created}
	}
	return ing
}

// TestIngressPathMatchingExamples verifies every case from the Kubernetes
// Ingress documentation path-matching table plus longest-match, Exact-over-Prefix,
// hostless rules, defaultBackend, named/numeric ports and class filtering.
func TestIngressPathMatchingExamples(t *testing.T) {
	services := []apis.Service{
		svc("default", "root", "10.96.0.10", nil, apis.ServicePort{Name: "http", Port: 80, Protocol: "TCP"}),
		svc("default", "bb", "10.96.0.11", nil, apis.ServicePort{Name: "http", Port: 80, Protocol: "TCP"}),
		svc("default", "bbb", "10.96.0.12", nil, apis.ServicePort{Name: "http", Port: 80, Protocol: "TCP"}),
		svc("default", "bbb-slash", "10.96.0.13", nil, apis.ServicePort{Name: "http", Port: 80, Protocol: "TCP"}),
		svc("default", "exact-foo", "10.96.0.14", nil, apis.ServicePort{Name: "web", Port: 8080, Protocol: "TCP"}),
		svc("default", "prefix-foo", "10.96.0.15", nil, apis.ServicePort{Name: "http", Port: 80, Protocol: "TCP"}),
		svc("default", "exact-slash", "10.96.0.16", nil, apis.ServicePort{Name: "http", Port: 80, Protocol: "TCP"}),
		svc("default", "impl", "10.96.0.17", nil, apis.ServicePort{Name: "http", Port: 80, Protocol: "TCP"}),
		svc("default", "hostless", "10.96.0.18", nil, apis.ServicePort{Name: "http", Port: 80, Protocol: "TCP"}),
		svc("default", "fallback", "10.96.0.19", nil, apis.ServicePort{Name: "http", Port: 9000, Protocol: "TCP"}),
		svc("default", "other-class", "10.96.0.20", nil, apis.ServicePort{Name: "http", Port: 80, Protocol: "TCP"}),
	}
	be := func(name string, num int32, portName string) apis.IngressBackend {
		return apis.IngressBackend{Service: &apis.IngressServiceBackend{Name: name, Port: apis.ServiceBackendPort{Number: num, Name: portName}}}
	}
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	ingresses := []apis.Ingress{
		mkIngress("default", "main", strPtr("shearwater"), t0, []apis.IngressRule{
			{
				Host: "k8s.example.com",
				HTTP: &apis.HTTPIngressRuleValue{Paths: []apis.HTTPIngressPath{
					{Path: "/", PathType: strPtr(apis.PathTypePrefix), Backend: be("root", 80, "")},
					{Path: "/aaa/bb", PathType: strPtr(apis.PathTypePrefix), Backend: be("bb", 80, "")},
					{Path: "/aaa/bbb", PathType: strPtr(apis.PathTypePrefix), Backend: be("bbb", 80, "")},
					{Path: "/aaa/bbb/", PathType: strPtr(apis.PathTypePrefix), Backend: be("bbb-slash", 80, "")},
					{Path: "/foo", PathType: strPtr(apis.PathTypePrefix), Backend: be("prefix-foo", 80, "")},
					{Path: "/foo", PathType: strPtr(apis.PathTypeExact), Backend: be("exact-foo", 0, "web")},
					{Path: "/bar/", PathType: strPtr(apis.PathTypeExact), Backend: be("exact-slash", 80, "")},
					{Path: "/impl", PathType: strPtr(apis.PathTypeImplementationSpecific), Backend: be("impl", 80, "")},
				}},
			},
			{
				Host: "", // hostless catch-all
				HTTP: &apis.HTTPIngressRuleValue{Paths: []apis.HTTPIngressPath{
					{Path: "/healthz", PathType: strPtr(apis.PathTypeExact), Backend: be("hostless", 80, "")},
				}},
			},
		}, &apis.IngressBackend{Service: &apis.IngressServiceBackend{Name: "fallback", Port: apis.ServiceBackendPort{Number: 9000}}}, nil),
		// Ingress with another class must be ignored completely.
		mkIngress("default", "ignored", strPtr("nginx"), t0.Add(time.Minute), []apis.IngressRule{
			{Host: "other.example.com", HTTP: &apis.HTTPIngressRuleValue{Paths: []apis.HTTPIngressPath{
				{Path: "/", PathType: strPtr(apis.PathTypePrefix), Backend: be("other-class", 80, "")},
			}}},
		}, nil, nil),
	}

	tbl, problems := BuildTable(services, ingresses)
	if len(problems) > 0 {
		t.Fatalf("unexpected problems: %v", problems)
	}
	if tbl.HasHost("other.example.com") {
		t.Fatal("Ingress with ingressClassName=nginx must be ignored")
	}

	cases := []struct {
		host, path, wantSvc, wantBackend string
	}{
		// Prefix "/"
		{"k8s.example.com", "/", "root", "10.96.0.10:80"},
		{"k8s.example.com", "/zzz", "root", "10.96.0.10:80"},
		// Prefix "/aaa/bb" vs "/aaa/bbb" vs "/aaa/bbb/"
		{"k8s.example.com", "/aaa/bb", "bb", "10.96.0.11:80"},
		{"k8s.example.com", "/aaa/bb/", "bb", "10.96.0.11:80"},
		{"k8s.example.com", "/aaa/bb/ccc", "bb", "10.96.0.11:80"},
		{"k8s.example.com", "/aaa/b", "root", "10.96.0.10:80"},
		{"k8s.example.com", "/aaa/bbb", "bbb", "10.96.0.12:80"},
		{"k8s.example.com", "/aaa/bbb/", "bbb-slash", "10.96.0.13:80"},
		{"k8s.example.com", "/aaa/bbb/ccc", "bbb-slash", "10.96.0.13:80"},
		{"k8s.example.com", "/aaa/bbbb", "root", "10.96.0.10:80"},
		// Exact vs Prefix of equal length (/foo) and named port resolution
		{"k8s.example.com", "/foo", "exact-foo", "10.96.0.14:8080"},
		{"k8s.example.com", "/foo/", "prefix-foo", "10.96.0.15:80"},
		{"k8s.example.com", "/foo/sub", "prefix-foo", "10.96.0.15:80"},
		{"k8s.example.com", "/foobar", "root", "10.96.0.10:80"},
		{"k8s.example.com", "/FOO", "root", "10.96.0.10:80"},
		// Exact with trailing slash
		{"k8s.example.com", "/bar/", "exact-slash", "10.96.0.16:80"},
		{"k8s.example.com", "/bar", "root", "10.96.0.10:80"},
		// ImplementationSpecific treated as Prefix
		{"k8s.example.com", "/impl", "impl", "10.96.0.17:80"},
		{"k8s.example.com", "/impl/x", "impl", "10.96.0.17:80"},
		{"k8s.example.com", "/implx", "root", "10.96.0.10:80"},
		// Hostless rule and defaultBackend for another host
		{"any.example.com", "/healthz", "hostless", "10.96.0.18:80"},
		{"any.example.com", "/unmatched", "fallback", "10.96.0.19:9000"},
	}
	for _, tc := range cases {
		got, ok := tbl.Match(tc.host, tc.path)
		if !ok || got.Service != tc.wantSvc || got.Backend != tc.wantBackend {
			t.Errorf("Match(%q, %q) = (%+v, %v), want %s (%s)", tc.host, tc.path, got, ok, tc.wantSvc, tc.wantBackend)
		}
	}
}

func makeSelfSignedCert(t *testing.T, dnsName string, serial int64) (certPEM, keyPEM []byte, leaf *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(serial),
		Subject:               pkix.Name{CommonName: dnsName},
		DNSNames:              []string{dnsName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ = x509.ParseCertificate(der)
	keyDER, _ := x509.MarshalECPrivateKey(key)
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, leaf
}

func TestIngressTLSSecretSelectionAndRotationAndStatus(t *testing.T) {
	w := newWorld(t, true, nil)

	// Create Service, TLS Secret, and Ingress in default namespace.
	svcRes := apis.Lookup("", "v1", "services")
	secRes := apis.Lookup("", "v1", "secrets")
	ingRes := apis.Lookup("networking.k8s.io", "v1", "ingresses")

	s := svc("default", "web", "10.96.0.30", nil)
	objects.Stamp(svcRes, &s)

	cert1, key1, leaf1 := makeSelfSignedCert(t, "secret.example.com", 101)
	sec := apis.Secret{
		Metadata:   apis.ObjectMeta{Namespace: "default", Name: "web-tls"},
		SecretType: apis.SecretTypeTLS,
		Data:       map[string][]byte{"tls.crt": cert1, "tls.key": key1},
	}
	objects.Stamp(secRes, &sec)

	ing := mkIngress("default", "web-ing", nil, time.Now(), []apis.IngressRule{
		{Host: "secret.example.com", HTTP: &apis.HTTPIngressRuleValue{Paths: []apis.HTTPIngressPath{
			{Path: "/v1", PathType: strPtr(apis.PathTypePrefix), Backend: apis.IngressBackend{Service: &apis.IngressServiceBackend{Name: "web", Port: apis.ServiceBackendPort{Number: 80}}}},
		}}},
		{Host: "acme.example.com", HTTP: &apis.HTTPIngressRuleValue{Paths: []apis.HTTPIngressPath{
			{Path: "/", PathType: strPtr(apis.PathTypePrefix), Backend: apis.IngressBackend{Service: &apis.IngressServiceBackend{Name: "web", Port: apis.ServiceBackendPort{Number: 80}}}},
		}}},
	}, nil, []apis.IngressTLS{
		{Hosts: []string{"secret.example.com"}, SecretName: "web-tls"},
	})
	objects.Stamp(ingRes, &ing)

	if err := w.st.Write(false, func(tx *store.Tx) error {
		if _, err := tx.Create(objects.Put(svcRes, &s)); err != nil {
			return err
		}
		if _, err := tx.Create(objects.Put(secRes, &sec)); err != nil {
			return err
		}
		_, err := tx.Create(objects.Put(ingRes, &ing))
		return err
	}); err != nil {
		t.Fatal(err)
	}

	w.eventually("secret and ACME certificates ready", func() bool {
		return w.srv.Certs.Ready("secret.example.com") && w.srv.Certs.Ready("acme.example.com")
	})

	// ACME must ONLY have issued for acme.example.com, NOT secret.example.com.
	for _, issued := range w.authority.Issued {
		if issued == "secret.example.com" {
			t.Fatalf("ACME must not order a certificate for a host with spec.tls[].secretName: %v", w.authority.Issued)
		}
	}

	// Verify served TLS cert serial for secret.example.com matches leaf1.
	servedSerial := func(host string) *big.Int {
		t.Helper()
		conn, err := tls.Dial("tcp", w.httpsAddr, &tls.Config{ServerName: host, InsecureSkipVerify: true})
		if err != nil {
			t.Fatalf("TLS dial %s: %v", host, err)
		}
		defer conn.Close()
		return conn.ConnectionState().PeerCertificates[0].SerialNumber
	}
	if got := servedSerial("secret.example.com"); got.Cmp(leaf1.SerialNumber) != 0 {
		t.Fatalf("served serial %v, want %v", got, leaf1.SerialNumber)
	}

	// Rotate the Secret and verify the edge reflects the new certificate automatically.
	cert2, key2, leaf2 := makeSelfSignedCert(t, "secret.example.com", 202)
	if err := w.st.Write(false, func(tx *store.Tx) error {
		rec, err := tx.Get(objects.Key(secRes, "default", "web-tls"))
		if err != nil {
			return err
		}
		var cur apis.Secret
		_ = json.Unmarshal(rec.Data, &cur)
		cur.Data["tls.crt"], cur.Data["tls.key"] = cert2, key2
		_, _, err = tx.Update(objects.Put(secRes, &cur), rec.Rev)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	w.eventually("rotated secret cert", func() bool {
		return servedSerial("secret.example.com").Cmp(leaf2.SerialNumber) == 0
	})

	// Verify Ingress status.loadBalancer.ingress[].ip was populated with 127.0.0.1.
	w.eventually("Ingress loadBalancer IP", func() bool {
		rec, err := w.st.Get(objects.Key(ingRes, "default", "web-ing"))
		if err != nil {
			return false
		}
		var cur apis.Ingress
		_ = json.Unmarshal(rec.Data, &cur)
		return len(cur.Status.LoadBalancer.Ingress) == 1 && cur.Status.LoadBalancer.Ingress[0].IP == "127.0.0.1"
	})

	// Verify HTTPS request routing by path: /v1 succeeds, /other returns 404.
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(cert2)
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{ServerName: "secret.example.com", RootCAs: pool},
		DialContext:     func(ctx context.Context, n, _ string) (net.Conn, error) { return net.Dial(n, w.httpsAddr) },
	}
	client := &http.Client{Transport: tr}
	resp, err := client.Get("https://secret.example.com/v1/items")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("HTTPS /v1/items failed: resp=%v err=%v", resp, err)
	}
	resp.Body.Close()

	resp, err = client.Get("https://secret.example.com/unmatched")
	if err != nil || resp.StatusCode != 404 {
		t.Fatalf("HTTPS /unmatched should be 404, got resp=%v err=%v", resp, err)
	}
	resp.Body.Close()
}

func TestHostOwnershipAdmission(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	admit := HostOwnershipAdmission()
	ingRes := apis.Lookup("networking.k8s.io", "v1", "ingresses")
	svcRes := apis.Lookup("", "v1", "services")

	createObj := func(res *apis.Resource, obj apis.Object) error {
		objects.Stamp(res, obj)
		return st.Write(false, func(tx *store.Tx) error {
			req := admission.Request{
				Operation: admission.Create,
				Resource:  res,
				Namespace: obj.Meta().Namespace,
				Name:      obj.Meta().Name,
				Object:    obj,
				Tx:        tx,
			}
			if err := admit(req); err != nil {
				return err
			}
			_, err := tx.Create(objects.Put(res, obj))
			return err
		})
	}

	assertConflict := func(t *testing.T, err error, wantSub string) {
		t.Helper()
		var d *admission.Denied
		if !errors.As(err, &d) || d.Code != http.StatusConflict || !strings.Contains(d.Message, wantSub) {
			t.Fatalf("got err=%v, want 409 Conflict containing %q", err, wantSub)
		}
	}

	// 1. ns-a creates Ingress claiming app.example.com -> OK.
	ingA := mkIngress("ns-a", "ing-a", nil, time.Now(), []apis.IngressRule{
		{Host: "app.example.com", HTTP: &apis.HTTPIngressRuleValue{Paths: []apis.HTTPIngressPath{
			{Path: "/web", PathType: strPtr(apis.PathTypePrefix), Backend: apis.IngressBackend{Service: &apis.IngressServiceBackend{Name: "web", Port: apis.ServiceBackendPort{Number: 80}}}},
		}}},
	}, nil, nil)
	if err := createObj(ingRes, &ingA); err != nil {
		t.Fatalf("ns-a Ingress should succeed: %v", err)
	}

	// 2. Same namespace (ns-a) creates a second Ingress on app.example.com (path /api) -> ALLOWED.
	ingA2 := mkIngress("ns-a", "ing-a2", nil, time.Now(), []apis.IngressRule{
		{Host: "app.example.com", HTTP: &apis.HTTPIngressRuleValue{Paths: []apis.HTTPIngressPath{
			{Path: "/api", PathType: strPtr(apis.PathTypePrefix), Backend: apis.IngressBackend{Service: &apis.IngressServiceBackend{Name: "api", Port: apis.ServiceBackendPort{Number: 80}}}},
		}}},
	}, nil, nil)
	if err := createObj(ingRes, &ingA2); err != nil {
		t.Fatalf("second Ingress in same namespace must be allowed: %v", err)
	}

	// 3. ns-b creates Ingress claiming app.example.com -> 409 Conflict.
	ingB := mkIngress("ns-b", "ing-b", nil, time.Now(), []apis.IngressRule{
		{Host: "APP.EXAMPLE.COM", HTTP: &apis.HTTPIngressRuleValue{Paths: []apis.HTTPIngressPath{
			{Path: "/", PathType: strPtr(apis.PathTypePrefix), Backend: apis.IngressBackend{Service: &apis.IngressServiceBackend{Name: "web", Port: apis.ServiceBackendPort{Number: 80}}}},
		}}},
	}, nil, nil)
	assertConflict(t, createObj(ingRes, &ingB), "Ingress ns-a/ing-a")

	// 4. ns-b creates Service with shearwater.io/public-host=app.example.com -> 409 Conflict.
	svcB := svc("ns-b", "steal", "10.96.0.40", map[string]string{HostAnnotation: "app.example.com"})
	assertConflict(t, createObj(svcRes, &svcB), "Ingress ns-a/ing-a")

	// 5. ns-b creates Service claiming legacy.example.com -> OK.
	svcLeg := svc("ns-b", "legacy", "10.96.0.41", map[string]string{HostAnnotation: "legacy.example.com"})
	if err := createObj(svcRes, &svcLeg); err != nil {
		t.Fatalf("ns-b Service claiming free host should succeed: %v", err)
	}

	// 6. ns-a creates Ingress claiming legacy.example.com -> 409 Conflict against Service ns-b/legacy.
	ingConflictSvc := mkIngress("ns-a", "steal-svc", nil, time.Now(), []apis.IngressRule{
		{Host: "legacy.example.com", HTTP: &apis.HTTPIngressRuleValue{Paths: []apis.HTTPIngressPath{
			{Path: "/", PathType: strPtr(apis.PathTypePrefix), Backend: apis.IngressBackend{Service: &apis.IngressServiceBackend{Name: "web", Port: apis.ServiceBackendPort{Number: 80}}}},
		}}},
	}, nil, nil)
	assertConflict(t, createObj(ingRes, &ingConflictSvc), "Service ns-b/legacy")

	// 7. ns-b Ingress with a different ingressClassName (nginx) is not blocked.
	ingOtherClass := mkIngress("ns-b", "nginx-ing", strPtr("nginx"), time.Now(), []apis.IngressRule{
		{Host: "app.example.com", HTTP: &apis.HTTPIngressRuleValue{Paths: []apis.HTTPIngressPath{
			{Path: "/", PathType: strPtr(apis.PathTypePrefix), Backend: apis.IngressBackend{Service: &apis.IngressServiceBackend{Name: "web", Port: apis.ServiceBackendPort{Number: 80}}}},
		}}},
	}, nil, nil)
	if err := createObj(ingRes, &ingOtherClass); err != nil {
		t.Fatalf("Ingress with non-shearwater class should be ignored by admission: %v", err)
	}

	// 8. Updating an Ingress in ns-a to add legacy.example.com (owned by Service ns-b/legacy) -> 409 Conflict.
	ingAUpdated := ingA
	ingAUpdated.Spec.Rules = append(ingAUpdated.Spec.Rules, apis.IngressRule{
		Host: "legacy.example.com",
		HTTP: &apis.HTTPIngressRuleValue{Paths: []apis.HTTPIngressPath{
			{Path: "/", PathType: strPtr(apis.PathTypePrefix), Backend: apis.IngressBackend{Service: &apis.IngressServiceBackend{Name: "web", Port: apis.ServiceBackendPort{Number: 80}}}},
		}},
	})
	err = st.Write(false, func(tx *store.Tx) error {
		return admit(admission.Request{
			Operation: admission.Update,
			Resource:  ingRes,
			Namespace: "ns-a",
			Name:      "ing-a",
			Object:    &ingAUpdated,
			Old:       &ingA,
			Tx:        tx,
		})
	})
	assertConflict(t, err, "Service ns-b/legacy")
}

func TestIngressRoutesMultipleBackendsEndToEnd(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	webBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "web:%s", r.URL.Path)
	}))
	defer webBackend.Close()
	apiBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "api:%s", r.URL.Path)
	}))
	defer apiBackend.Close()

	httpLn, _ := net.Listen("tcp", "127.0.0.1:0")
	srv := &Server{
		Store: st,
		HTTP:  httpLn,
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			target := webBackend.Listener.Addr().String()
			if addr == "10.96.0.51:8080" {
				target = apiBackend.Listener.Addr().String()
			}
			return (&net.Dialer{}).DialContext(ctx, network, target)
		},
	}

	svcRes := apis.Lookup("", "v1", "services")
	ingRes := apis.Lookup("networking.k8s.io", "v1", "ingresses")
	sWeb := svc("default", "web", "10.96.0.50", nil, apis.ServicePort{Name: "http", Port: 80, Protocol: "TCP"})
	sAPI := svc("default", "api", "10.96.0.51", nil, apis.ServicePort{Name: "http", Port: 8080, Protocol: "TCP"})
	ing := mkIngress("default", "app", nil, time.Now(), []apis.IngressRule{
		{Host: "site.example.com", HTTP: &apis.HTTPIngressRuleValue{Paths: []apis.HTTPIngressPath{
			{Path: "/", PathType: strPtr(apis.PathTypePrefix), Backend: apis.IngressBackend{Service: &apis.IngressServiceBackend{Name: "web", Port: apis.ServiceBackendPort{Number: 80}}}},
			{Path: "/api", PathType: strPtr(apis.PathTypePrefix), Backend: apis.IngressBackend{Service: &apis.IngressServiceBackend{Name: "api", Port: apis.ServiceBackendPort{Name: "http"}}}},
		}}},
	}, nil, nil)
	objects.Stamp(svcRes, &sWeb)
	objects.Stamp(svcRes, &sAPI)
	objects.Stamp(ingRes, &ing)
	if err := st.Write(false, func(tx *store.Tx) error {
		for _, p := range []store.Put{objects.Put(svcRes, &sWeb), objects.Put(svcRes, &sAPI), objects.Put(ingRes, &ing)} {
			if _, err := tx.Create(p); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	defer func() { cancel(); <-done }()

	get := func(path string) string {
		t.Helper()
		for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(15 * time.Millisecond) {
			req, _ := http.NewRequest("GET", "http://"+httpLn.Addr().String()+path, nil)
			req.Host = "site.example.com"
			resp, err := http.DefaultClient.Do(req)
			if err == nil && resp.StatusCode == 200 {
				var buf [64]byte
				n, _ := resp.Body.Read(buf[:])
				resp.Body.Close()
				return string(buf[:n])
			}
			if resp != nil {
				resp.Body.Close()
			}
		}
		t.Fatalf("timed out fetching %s", path)
		return ""
	}
	if got := get("/"); got != "web:/" {
		t.Fatalf("GET / = %q, want web:/", got)
	}
	if got := get("/api/users"); got != "api:/api/users" {
		t.Fatalf("GET /api/users = %q, want api:/api/users", got)
	}
	if got := get("/apiv2"); got != "web:/apiv2" {
		t.Fatalf("GET /apiv2 = %q, want web:/apiv2 (segment prefix must not match /apiv2)", got)
	}
}
