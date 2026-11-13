package node

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"shearwater/internal/apis"
	"strconv"
	"testing"
	"time"
)

func TestHTTPProbeStatusHeadersTimeoutAndNoRedirect(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			if r.Host != "app.local" {
				w.WriteHeader(400)
			}
		case "/bad":
			w.WriteHeader(503)
		case "/redirect":
			w.Header().Set("Location", "http://192.0.2.99/")
			w.WriteHeader(302)
		case "/slow":
			select {
			case <-r.Context().Done():
			case <-time.After(time.Second):
			}
		}
	}))
	defer s.Close()
	host, port, _ := net.SplitHostPort(s.Listener.Addr().String())
	n, _ := strconv.Atoi(port)
	a := &Agent{PodCIDR: netip.MustParsePrefix("127.0.0.0/8")}
	c := &apis.Container{}
	for _, test := range []struct {
		path string
		ok   bool
	}{{"/ok", true}, {"/bad", false}, {"/redirect", true}, {"/slow", false}} {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		p := &apis.Probe{HTTPGet: &apis.HTTPProbe{Path: test.path, Port: apis.Int(int32(n)), Scheme: "HTTP", HTTPHeaders: []apis.HTTPHeader{{Name: "Host", Value: "app.local"}}}}
		err := a.runProbe(ctx, p, c, host, "")
		cancel()
		if (err == nil) != test.ok {
			t.Fatal(test.path, err)
		}
	}
	if err := a.runProbe(context.Background(), &apis.Probe{TCPSocket: &apis.TCPProbe{Port: apis.Int(80)}}, c, "192.0.2.1", ""); err == nil {
		t.Fatal("allowed target outside Pod range")
	}
}
