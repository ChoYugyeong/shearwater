package edge

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"shearwater/internal/apis"
	"shearwater/internal/objects"
	"shearwater/internal/store"
)

// Server is the public entry. HTTP is required. With HTTPS and Certs set,
// HTTP only answers ACME challenges and redirects to HTTPS; it never carries
// app traffic in plain text for a host that is meant to be encrypted.
type Server struct {
	Store *store.Store
	HTTP  net.Listener
	HTTPS net.Listener // nil: plain HTTP only
	Certs *Certs       // required with HTTPS
	// Dial reaches a backend; nil dials ClusterIPs only.
	Dial func(ctx context.Context, network, address string) (net.Conn, error)
	// OnStatus receives the routing and certificate state after each change.
	OnStatus func(Status)

	table atomic.Pointer[Table]
	proxy *httputil.ReverseProxy
	last  string
}

// Status is the state `sw edge status` prints.
type Status struct {
	HTTP     string               `json:"http"`
	HTTPS    string               `json:"https,omitempty"`
	Routes   []Route              `json:"routes"`
	Problems []string             `json:"problems,omitempty"`
	Certs    map[string]CertState `json:"certificates,omitempty"`
}

func dialClusterIP(ctx context.Context, network, address string) (net.Conn, error) {
	ap, err := netip.ParseAddrPort(address)
	if err != nil || !apis.ServiceCIDR.Contains(ap.Addr()) {
		return nil, fmt.Errorf("edge: %s is not a Service address", address)
	}
	return (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, network, address)
}

type routeKey struct{}

func (s *Server) init() {
	dial := s.Dial
	if dial == nil {
		dial = dialClusterIP
	}
	s.proxy = &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			route := r.In.Context().Value(routeKey{}).(Route)
			r.SetURL(&url.URL{Scheme: "http", Host: route.Backend})
			r.Out.Host = r.In.Host
			r.SetXForwarded()
		},
		Transport: &http.Transport{
			DialContext:           dial,
			MaxIdleConnsPerHost:   16,
			IdleConnTimeout:       60 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if errors.Is(err, context.Canceled) {
				return
			}
			http.Error(w, "the app did not answer", http.StatusBadGateway)
		},
		ErrorLog: log.New(discard{}, "", 0),
	}
	s.table.Store(emptyTable())
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

func (s *Server) forward(w http.ResponseWriter, r *http.Request, route Route) {
	s.proxy.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), routeKey{}, route)))
}

func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if s.Certs != nil && strings.HasPrefix(r.URL.Path, "/.well-known/acme-challenge/") {
		if body, ok := s.Certs.Challenge(r.URL.Path); ok {
			w.Header().Set("Content-Type", "text/plain")
			w.Write([]byte(body))
			return
		}
	}
	tbl := s.table.Load()
	reqHost := requestHost(r.Host)
	route, ok := tbl.Match(r.Host, r.URL.Path)
	namedHost := route.Host
	if namedHost == "" && tbl.HasHost(reqHost) {
		namedHost = reqHost
	}
	switch {
	case !ok:
		http.Error(w, "no app is published at this host name", http.StatusNotFound)
	case s.HTTPS == nil || namedHost == "":
		s.forward(w, r, route)
	case !s.Certs.Ready(namedHost):
		w.Header().Set("Retry-After", "30")
		http.Error(w, "the certificate for this host is not ready yet", http.StatusServiceUnavailable)
	default:
		target := "https://" + namedHost
		if _, port, err := net.SplitHostPort(s.HTTPS.Addr().String()); err == nil && port != "443" {
			target += ":" + port
		}
		http.Redirect(w, r, target+r.URL.RequestURI(), http.StatusPermanentRedirect)
	}
}

func (s *Server) serveHTTPS(w http.ResponseWriter, r *http.Request) {
	tbl := s.table.Load()
	reqHost := requestHost(r.Host)
	// The certificate was chosen by TLS server name; refuse a different Host
	// header so one published app cannot be reached under another's name.
	if r.TLS == nil || reqHost == "" || requestHost(r.TLS.ServerName) != reqHost || !tbl.HasHost(reqHost) {
		http.Error(w, "no app is published at this host name", http.StatusMisdirectedRequest)
		return
	}
	route, ok := tbl.Match(r.Host, r.URL.Path)
	if !ok {
		http.Error(w, "no app is published at this path", http.StatusNotFound)
		return
	}
	s.forward(w, r, route)
}

func (s *Server) refresh() ([]string, []string, error) {
	svcRecs, _, err := s.Store.List("", "services", "")
	if err != nil {
		return nil, nil, err
	}
	ingRecs, _, err := s.Store.List("networking.k8s.io", "ingresses", "")
	if err != nil {
		return nil, nil, err
	}
	secRecs, _, err := s.Store.List("", "secrets", "")
	if err != nil {
		return nil, nil, err
	}
	services := make([]apis.Service, 0, len(svcRecs))
	for _, rec := range svcRecs {
		var svc apis.Service
		if json.Unmarshal(rec.Data, &svc) == nil {
			services = append(services, svc)
		}
	}
	ingresses := make([]apis.Ingress, 0, len(ingRecs))
	for _, rec := range ingRecs {
		var ing apis.Ingress
		if json.Unmarshal(rec.Data, &ing) == nil {
			ingresses = append(ingresses, ing)
		}
	}
	secrets := make([]apis.Secret, 0, len(secRecs))
	for _, rec := range secRecs {
		var sec apis.Secret
		if json.Unmarshal(rec.Data, &sec) == nil {
			secrets = append(secrets, sec)
		}
	}
	tbl, problems := BuildTable(services, ingresses)
	if s.Certs != nil {
		secretCerts, certProblems := LoadSecretCerts(tbl.SecretTLS, secrets)
		problems = append(problems, certProblems...)
		s.Certs.SetSecretCerts(secretCerts)
	}
	s.table.Store(tbl)
	s.syncIngressStatus(ingRecs)
	return tbl.ACMEHosts, problems, nil
}

func (s *Server) listenIP() string {
	if s.HTTP == nil {
		return ""
	}
	if ta, ok := s.HTTP.Addr().(*net.TCPAddr); ok && ta.IP != nil && !ta.IP.IsUnspecified() {
		return ta.IP.String()
	}
	c, err := net.Dial("udp4", "192.0.2.1:9")
	if err != nil {
		return ""
	}
	defer c.Close()
	if ua, ok := c.LocalAddr().(*net.UDPAddr); ok && ua.IP != nil {
		return ua.IP.String()
	}
	return ""
}

func (s *Server) syncIngressStatus(ingRecs []store.Record) {
	ip := s.listenIP()
	if ip == "" || len(ingRecs) == 0 {
		return
	}
	res := apis.Lookup("networking.k8s.io", "v1", "ingresses")
	if res == nil {
		return
	}
	for _, rec := range ingRecs {
		if rec.Deleting {
			continue
		}
		var ing apis.Ingress
		if json.Unmarshal(rec.Data, &ing) != nil || !HandlesIngress(&ing) {
			continue
		}
		lb := ing.Status.LoadBalancer.Ingress
		if len(lb) == 1 && lb[0].IP == ip && lb[0].Hostname == "" && len(lb[0].Ports) == 0 {
			continue
		}
		_ = s.Store.Write(false, func(tx *store.Tx) error {
			cur, err := tx.Get(rec.Key)
			if err != nil || cur.Deleting || cur.UID != rec.UID {
				return nil
			}
			var latest apis.Ingress
			if json.Unmarshal(cur.Data, &latest) != nil || !HandlesIngress(&latest) {
				return nil
			}
			latest.Status.LoadBalancer.Ingress = []apis.IngressLoadBalancerIngress{{IP: ip}}
			_, _, err = tx.Update(objects.Put(res, &latest), cur.Rev)
			return err
		})
	}
}

func (s *Server) report(problems []string) {
	if s.OnStatus == nil {
		return
	}
	tbl := s.table.Load()
	routes := slices.Clone(tbl.AllRoutes)
	if routes == nil {
		routes = []Route{}
	}
	st := Status{HTTP: s.HTTP.Addr().String(), Problems: problems, Routes: routes}
	if s.HTTPS != nil {
		st.HTTPS, st.Certs = s.HTTPS.Addr().String(), s.Certs.States()
	}
	b, _ := json.Marshal(st)
	if string(b) != s.last {
		s.last = string(b)
		s.OnStatus(st)
	}
}

// Run serves until ctx ends. Routing follows Service changes within a moment;
// certificates are obtained in the background and never block routing.
func (s *Server) Run(ctx context.Context) error {
	if s.HTTPS != nil && s.Certs == nil {
		return errors.New("edge: HTTPS needs a certificate source")
	}
	s.init()
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	servers := []*http.Server{{Handler: http.HandlerFunc(s.serveHTTP)}}
	if s.HTTPS != nil {
		servers = append(servers, &http.Server{Handler: http.HandlerFunc(s.serveHTTPS),
			TLSConfig: &tls.Config{GetCertificate: s.Certs.GetCertificate, MinVersion: tls.VersionTLS12}})
	}
	var wg sync.WaitGroup
	failed := make(chan error, len(servers)+1)
	for i, hs := range servers {
		hs.ReadHeaderTimeout, hs.IdleTimeout, hs.MaxHeaderBytes = 10*time.Second, 2*time.Minute, 64<<10
		hs.ErrorLog = log.New(discard{}, "", 0) // handshakes from scanners are not operator news
		wg.Add(1)
		go func() {
			defer wg.Done()
			var err error
			if i == 0 {
				err = hs.Serve(s.HTTP)
			} else {
				err = hs.ServeTLS(s.HTTPS, "", "")
			}
			if !errors.Is(err, http.ErrServerClosed) {
				failed <- err
			}
		}()
	}
	var mu sync.Mutex
	var hosts, problems []string
	issue := make(chan struct{}, 1)
	if s.HTTPS != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				mu.Lock()
				current := slices.Clone(hosts)
				mu.Unlock()
				wait := s.Certs.Ensure(ctx, current)
				mu.Lock()
				s.report(problems)
				mu.Unlock()
				t := time.NewTimer(wait)
				select {
				case <-ctx.Done():
				case <-issue:
				case <-t.C:
				}
				t.Stop()
			}
		}()
	}
	var runErr error
	for ctx.Err() == nil && runErr == nil {
		changed := s.Store.Changed()
		h, p, err := s.refresh()
		wait := 30 * time.Second
		if err != nil {
			log.Printf("public entry: %v", err)
			wait = time.Second
		} else {
			mu.Lock()
			grew := !slices.Equal(h, hosts)
			hosts, problems = h, p
			s.report(problems)
			mu.Unlock()
			if grew {
				select {
				case issue <- struct{}{}:
				default:
				}
			}
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
		case runErr = <-failed:
		case <-changed:
		case <-t.C:
		}
		t.Stop()
	}
	stop()
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, hs := range servers {
		hs.Shutdown(shutdown)
	}
	wg.Wait()
	return runErr
}
