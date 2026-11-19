package edge

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"shearwater/internal/acme"
	"shearwater/internal/acme/acmetest"
	"shearwater/internal/apis"
	"shearwater/internal/objects"
	"shearwater/internal/store"
)

func svc(ns, name, ip string, annotations map[string]string, ports ...apis.ServicePort) apis.Service {
	if ports == nil {
		ports = []apis.ServicePort{{Port: 80, Protocol: "TCP"}}
	}
	return apis.Service{Metadata: apis.ObjectMeta{Namespace: ns, Name: name, Annotations: annotations},
		Spec: apis.ServiceSpec{Type: "ClusterIP", ClusterIP: ip, Ports: ports}}
}

func TestRoutesPublishOnlyValidUnambiguousServices(t *testing.T) {
	older := &apis.Time{Time: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}
	newer := &apis.Time{Time: older.Add(time.Hour)}
	first := svc("default", "web", "10.96.0.10", map[string]string{HostAnnotation: "App.Example.com, www.example.com"})
	first.Metadata.CreationTimestamp = older
	thief := svc("other", "thief", "10.96.0.11", map[string]string{HostAnnotation: "app.example.com,own.example.com"})
	thief.Metadata.CreationTimestamp = newer
	two := []apis.ServicePort{{Name: "http", Port: 8080, Protocol: "TCP"}, {Name: "admin", Port: 9090, Protocol: "TCP"}}
	routes, problems := Routes([]apis.Service{
		thief, first,
		svc("default", "plain", "10.96.0.12", nil),
		svc("default", "ambiguous", "10.96.0.13", map[string]string{HostAnnotation: "a.example.com"}, two...),
		svc("default", "chosen", "10.96.0.14", map[string]string{HostAnnotation: "b.example.com", PortAnnotation: "admin"}, two...),
		svc("default", "bad", "10.96.0.15", map[string]string{HostAnnotation: "10.0.0.1,*.example.com,ok.example.com:443"}),
		svc("default", "outside", "192.168.0.5", map[string]string{HostAnnotation: "c.example.com"}),
	})
	want := map[string]string{
		"app.example.com": "10.96.0.10:80", "www.example.com": "10.96.0.10:80",
		"own.example.com": "10.96.0.11:80", "b.example.com": "10.96.0.14:9090",
	}
	if len(routes) != len(want) {
		t.Fatal("unexpected routes", routes)
	}
	for host, backend := range want {
		if routes[host].Backend != backend {
			t.Errorf("%s -> %q, want %q", host, routes[host].Backend, backend)
		}
	}
	all := strings.Join(problems, "\n")
	for _, part := range []string{"already belongs to Service default/web", "2 TCP ports", `"10.0.0.1" is not a host name`, `"*.example.com"`, `"ok.example.com:443"`, "default/outside is not published"} {
		if !strings.Contains(all, part) {
			t.Errorf("problems do not mention %q:\n%s", part, all)
		}
	}
}

type world struct {
	t          *testing.T
	st         *store.Store
	srv        *Server
	backend    *httptest.Server
	authority  *acmetest.Authority
	httpAddr   string
	httpsAddr  string
	statuses   atomic.Pointer[Status]
	backendHit atomic.Int32
}

func (w *world) publish(name, ip, hosts string) {
	w.t.Helper()
	s := svc("default", name, ip, map[string]string{HostAnnotation: hosts})
	res := apis.Lookup("", "v1", "services")
	objects.Stamp(res, &s)
	if err := w.st.Write(false, func(tx *store.Tx) error { _, e := tx.Create(objects.Put(res, &s)); return e }); err != nil {
		w.t.Fatal(err)
	}
}

func newWorld(t *testing.T, withTLS bool, issuer Issuer) *world {
	t.Helper()
	w := &world{t: t}
	var err error
	if w.st, err = store.Open(filepath.Join(t.TempDir(), "state.db")); err != nil {
		t.Fatal(err)
	}
	w.backend = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.backendHit.Add(1)
		fmt.Fprintf(rw, "app saw host=%s proto=%s for=%t path=%s", r.Host, r.Header.Get("X-Forwarded-Proto"), r.Header.Get("X-Forwarded-For") != "", r.URL.RequestURI())
	}))
	httpLn, _ := net.Listen("tcp", "127.0.0.1:0")
	w.httpAddr = httpLn.Addr().String()
	w.srv = &Server{Store: w.st, HTTP: httpLn, OnStatus: func(s Status) { w.statuses.Store(&s) }}
	// Every ClusterIP leads to the one test backend.
	w.srv.Dial = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, w.backend.Listener.Addr().String())
	}
	if withTLS {
		httpsLn, _ := net.Listen("tcp", "127.0.0.1:0")
		w.httpsAddr = httpsLn.Addr().String()
		w.srv.HTTPS = httpsLn
		if issuer == nil {
			w.authority = acmetest.New()
			w.authority.Fetch = func(domain, path string) (string, error) { return w.get("http", domain, path, nil) }
			key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			issuer = &acme.Client{Directory: w.authority.Directory(), Key: key, PollEvery: time.Millisecond}
		}
		w.srv.Certs = &Certs{Dir: filepath.Join(t.TempDir(), "certs"), Issuer: issuer}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.srv.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error("Run:", err)
		}
		w.backend.Close()
		if w.authority != nil {
			w.authority.Close()
		}
		w.st.Close()
	})
	return w
}

// get requests path from the edge as a client that resolved host to it.
func (w *world) get(scheme, host, path string, status *int) (string, error) {
	addr := w.httpAddr
	tr := &http.Transport{DisableKeepAlives: true}
	if scheme == "https" {
		addr = w.httpsAddr
		tr.TLSClientConfig = &tls.Config{ServerName: host}
		if w.authority != nil {
			tr.TLSClientConfig.RootCAs = w.authority.Roots
		}
	}
	tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	client := &http.Client{Transport: tr, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(scheme + "://" + host + path)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if status != nil {
		*status = resp.StatusCode
	} else if resp.StatusCode != 200 {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "" {
		return loc, nil
	}
	return string(body), nil
}

func (w *world) eventually(what string, ok func() bool) {
	w.t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if ok() {
			return
		}
	}
	w.t.Fatal("timed out waiting for " + what)
}

func TestPlainHTTPRoutesByHostAndFollowsServiceChanges(t *testing.T) {
	w := newWorld(t, false, nil)
	var status int
	if _, err := w.get("http", "app.example.com", "/", &status); err != nil || status != 404 {
		t.Fatal("an unpublished host must be 404", status, err)
	}
	w.publish("web", "10.96.0.10", "app.example.com")
	w.eventually("the new route", func() bool { _, err := w.get("http", "app.example.com", "/", nil); return err == nil })
	body, _ := w.get("http", "APP.example.com:80", "/a?b=1", nil)
	if body != "app saw host=APP.example.com:80 proto=http for=true path=/a?b=1" {
		t.Fatal("request was not forwarded intact:", body)
	}
	if st := w.statuses.Load(); st == nil || len(st.Routes) != 1 || st.Routes[0].Service != "web" || st.HTTPS != "" {
		t.Fatal("status does not describe the route", st)
	}
}

func TestHTTPSObtainsCertificateRedirectsAndRefusesOtherHosts(t *testing.T) {
	w := newWorld(t, true, nil)
	w.publish("web", "10.96.0.10", "app.example.com")
	w.publish("api", "10.96.0.11", "api.example.com")
	w.eventually("both certificates", func() bool {
		return w.srv.Certs.Ready("app.example.com") && w.srv.Certs.Ready("api.example.com")
	})
	body, err := w.get("https", "app.example.com", "/login", nil)
	if err != nil || body != "app saw host=app.example.com proto=https for=true path=/login" {
		t.Fatal("HTTPS with the issued certificate failed:", body, err)
	}
	var status int
	loc, _ := w.get("http", "app.example.com", "/login?x=1", &status)
	_, port, _ := net.SplitHostPort(w.httpsAddr)
	if status != 308 || loc != "https://app.example.com:"+port+"/login?x=1" {
		t.Fatal("plain HTTP must redirect to HTTPS", status, loc)
	}
	// A certificate for one app must not open another app or an unknown host.
	hits := w.backendHit.Load()
	if _, err = w.get("https", "nobody.example.com", "/", nil); err == nil {
		t.Fatal("handshake for an unpublished host must fail")
	}
	req, _ := http.NewRequest("GET", "https://app.example.com/", nil)
	req.Host = "api.example.com"
	tr := &http.Transport{TLSClientConfig: &tls.Config{ServerName: "app.example.com", RootCAs: w.authority.Roots},
		DialContext: func(ctx context.Context, n, _ string) (net.Conn, error) { return net.Dial(n, w.httpsAddr) }}
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil || resp.StatusCode != http.StatusMisdirectedRequest || w.backendHit.Load() != hits {
		t.Fatal("a Host header different from the TLS name must be refused", resp, err)
	}
	resp.Body.Close()
	st := w.statuses.Load()
	if st == nil || !st.Certs["app.example.com"].Ready || len(w.authority.Issued) != 2 || len(w.authority.Rejected) != 0 {
		t.Fatal("status or authority record is wrong", st, w.authority.Issued, w.authority.Rejected)
	}
}

type failing struct{ calls atomic.Int32 }

func (f *failing) Obtain(context.Context, string, func(string, string) func()) ([]byte, []byte, error) {
	f.calls.Add(1)
	return nil, nil, errors.New("DNS problem: NXDOMAIN looking up A for app.example.com")
}

func TestWithoutCertificateNoPlainTextAndOrdersBackOff(t *testing.T) {
	issuer := &failing{}
	w := newWorld(t, true, issuer)
	w.publish("web", "10.96.0.10", "app.example.com")
	w.eventually("the failed order", func() bool { return issuer.calls.Load() == 1 })
	var status int
	if _, err := w.get("http", "app.example.com", "/", &status); err != nil || status != 503 || w.backendHit.Load() != 0 {
		t.Fatal("an HTTPS host must not be served in plain text while its certificate is pending", status, err)
	}
	// More Service changes must not cause another order inside the backoff.
	w.publish("other", "10.96.0.12", "other.example.com")
	w.eventually("the second host's order", func() bool { return issuer.calls.Load() == 2 })
	time.Sleep(50 * time.Millisecond)
	if n := issuer.calls.Load(); n != 2 {
		t.Fatal("a failing host was ordered again without waiting:", n)
	}
	w.eventually("the error in status", func() bool {
		st := w.statuses.Load()
		return st != nil && strings.Contains(st.Certs["app.example.com"].LastError, "NXDOMAIN") && !st.Certs["app.example.com"].RetryAt.IsZero()
	})
}

func TestCertificatesReloadFromDiskAndRenewInTheLastThird(t *testing.T) {
	a := acmetest.New()
	defer a.Close()
	var c *Certs
	a.Fetch = func(_, path string) (string, error) {
		body, ok := c.Challenge(path)
		if !ok {
			return "", errors.New("404")
		}
		return body, nil
	}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	client := &acme.Client{Directory: a.Directory(), Key: key, PollEvery: time.Millisecond}
	dir := t.TempDir()
	now := time.Now()
	c = &Certs{Dir: dir, Issuer: client, Now: func() time.Time { return now }}
	c.Ensure(context.Background(), []string{"app.example.com"})
	if !c.Ready("app.example.com") || len(a.Issued) != 1 {
		t.Fatal("first certificate was not issued", c.States(), a.Rejected)
	}
	// A restarted server uses the saved certificate without a new order.
	c = &Certs{Dir: dir, Issuer: &failing{}, Now: func() time.Time { return now }}
	c.Ensure(context.Background(), []string{"app.example.com"})
	if !c.Ready("app.example.com") || c.States()["app.example.com"].LastError != "" {
		t.Fatal("saved certificate was not reused", c.States())
	}
	// 59 days into a 90-day certificate: keep. 61 days: renew.
	c = &Certs{Dir: dir, Issuer: client, Now: func() time.Time { return now }}
	now = now.Add(59 * 24 * time.Hour)
	c.Ensure(context.Background(), []string{"app.example.com"})
	if len(a.Issued) != 1 {
		t.Fatal("renewed too early")
	}
	now = now.Add(2 * 24 * time.Hour)
	c.Ensure(context.Background(), []string{"app.example.com"})
	if len(a.Issued) != 2 || !c.Ready("app.example.com") {
		t.Fatal("certificate was not renewed in its last third", a.Issued, c.States())
	}
	// An unpublished host loses its certificate in memory.
	c.Ensure(context.Background(), nil)
	if c.Ready("app.example.com") {
		t.Fatal("a host that is no longer published still answers TLS")
	}
}

func TestAccountKeyIsCreatedOnceAndPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "edge", "account.key")
	first, err := LoadAccountKey(path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadAccountKey(path)
	if err != nil || !first.Equal(second) {
		t.Fatal("the account key changed between starts", err)
	}
}

func TestEnsureComesBackWhenACertificateEntersItsRenewalWindow(t *testing.T) {
	a := acmetest.New()
	defer a.Close()
	a.Lifetime = 90 * time.Second
	var c *Certs
	a.Fetch = func(_, path string) (string, error) {
		body, ok := c.Challenge(path)
		if !ok {
			return "", errors.New("404")
		}
		return body, nil
	}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	now := time.Now()
	c = &Certs{Dir: t.TempDir(), Issuer: &acme.Client{Directory: a.Directory(), Key: key, PollEvery: time.Millisecond}, Now: func() time.Time { return now }}
	// The first pass issues; it must ask to be called again when the last
	// third of the short lifetime begins, not in twelve hours.
	near := func(got, want time.Duration) bool { return got > want-time.Second && got < want+time.Second }
	wait := c.Ensure(context.Background(), []string{"app.example.com"})
	st := c.States()["app.example.com"]
	if st.RenewAt.IsZero() || !st.RenewAt.Before(st.NotAfter) || !st.RenewAt.After(now) {
		t.Fatal("status does not say when renewal starts", st)
	}
	if !near(wait, st.RenewAt.Sub(now)) || wait > 2*time.Minute {
		t.Fatalf("after issuing, next check in %v, want %v", wait, st.RenewAt.Sub(now))
	}
	now = now.Add(10 * time.Second)
	if wait = c.Ensure(context.Background(), []string{"app.example.com"}); !near(wait, st.RenewAt.Sub(now)) || len(a.Issued) != 1 {
		t.Fatalf("with a valid certificate, next check in %v and %d orders", wait, len(a.Issued))
	}
	now = now.Add(wait)
	c.Ensure(context.Background(), []string{"app.example.com"})
	if len(a.Issued) != 2 {
		t.Fatal("the certificate was not renewed at the time Ensure asked for", a.Issued)
	}
	// A certificate that is already due when issued must not cause a busy loop.
	a.Lifetime = time.Second
	now = now.Add(time.Hour)
	if wait = c.Ensure(context.Background(), []string{"app.example.com"}); wait < 5*time.Second {
		t.Fatalf("next check in %v would spin", wait)
	}
}
