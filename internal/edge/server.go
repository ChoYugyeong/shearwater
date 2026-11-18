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

	routes atomic.Pointer[map[string]Route]
	proxy  *httputil.ReverseProxy
	last   string
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
	empty := map[string]Route{}
	s.routes.Store(&empty)
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

func (s *Server) lookup(hostport string) (Route, bool) {
	route, ok := (*s.routes.Load())[requestHost(hostport)]
	return route, ok
}

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
	route, ok := s.lookup(r.Host)
	switch {
	case !ok:
		http.Error(w, "no app is published at this host name", http.StatusNotFound)
	case s.HTTPS == nil:
		s.forward(w, r, route)
	case !s.Certs.Ready(route.Host):
		w.Header().Set("Retry-After", "30")
		http.Error(w, "the certificate for this host is not ready yet", http.StatusServiceUnavailable)
	default:
		target := "https://" + route.Host
		if _, port, err := net.SplitHostPort(s.HTTPS.Addr().String()); err == nil && port != "443" {
			target += ":" + port
		}
		http.Redirect(w, r, target+r.URL.RequestURI(), http.StatusPermanentRedirect)
	}
}

func (s *Server) serveHTTPS(w http.ResponseWriter, r *http.Request) {
	route, ok := s.lookup(r.Host)
	// The certificate was chosen by TLS server name; refuse a different Host
	// header so one published app cannot be reached under another's name.
	if !ok || r.TLS == nil || requestHost(r.TLS.ServerName) != route.Host {
		http.Error(w, "no app is published at this host name", http.StatusMisdirectedRequest)
		return
	}
	s.forward(w, r, route)
}

func (s *Server) refresh() ([]string, []string, error) {
	records, _, err := s.Store.List("", "services", "")
	if err != nil {
		return nil, nil, err
	}
	services := make([]apis.Service, 0, len(records))
	for _, rec := range records {
		var svc apis.Service
		if json.Unmarshal(rec.Data, &svc) == nil {
			services = append(services, svc)
		}
	}
	routes, problems := Routes(services)
	s.routes.Store(&routes)
	hosts := make([]string, 0, len(routes))
	for h := range routes {
		hosts = append(hosts, h)
	}
	slices.Sort(hosts)
	return hosts, problems, nil
}

func (s *Server) report(hosts, problems []string) {
	if s.OnStatus == nil {
		return
	}
	st := Status{HTTP: s.HTTP.Addr().String(), Problems: problems, Routes: []Route{}}
	routes := *s.routes.Load()
	for _, h := range hosts {
		st.Routes = append(st.Routes, routes[h])
	}
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
				s.report(hosts, problems)
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
			s.report(hosts, problems)
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
