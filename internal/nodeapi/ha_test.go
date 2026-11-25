package nodeapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"shearwater/internal/auth"
	"shearwater/internal/ha"
	"shearwater/internal/pki"
	"shearwater/internal/store"
)

func roleGrant(t *testing.T, c *cluster, role string) string {
	t.Helper()
	tok, id, hash, err := auth.NewToken(auth.KindJoin)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(map[string]string{"role": role})
	if err := c.st.PutCredential(store.Credential{ID: id, Kind: auth.KindJoin, Hash: hash, Data: data}); err != nil {
		t.Fatal(err)
	}
	return tok
}

func requireJoinStatus(t *testing.T, err error, want int) {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) || e.Status != want {
		t.Fatalf("expected HTTP %d, got %v", want, err)
	}
}

func TestHAJoinGrants(t *testing.T) {
	for _, role := range []string{ha.RoleControlPlane, ha.RoleWitness} {
		t.Run(role, func(t *testing.T) {
			c := newCluster(t)
			ctx := context.Background()
			req := JoinRequest{Role: role, NodeName: "second", InternalIP: "192.0.2.12"}
			if role == ha.RoleControlPlane {
				_, csr, err := pki.NewNodeKey(req.NodeName)
				if err != nil {
					t.Fatal(err)
				}
				req.CSR = string(csr)
			}
			join := func(tok string, req JoinRequest) (JoinResponse, error) {
				return Join(ctx, c.srv.URL, tok, c.ca, req)
			}
			_, err := join(c.joinTk, req)
			requireJoinStatus(t, err, http.StatusForbidden)
			tok := roleGrant(t, c, role)
			_, err = join(tok, JoinRequest{NodeName: req.NodeName, InternalIP: req.InternalIP})
			requireJoinStatus(t, err, http.StatusForbidden)
			_, err = join(tok, req)
			requireJoinStatus(t, err, http.StatusConflict)

			urls := []string{c.srv.URL}
			want := &ha.Bundle{Config: ha.Config{Self: req.NodeName, Token: "shared"}, Listen: "192.0.2.12:6445"}
			c.api.ControlPlanes = func() []string { return urls }
			c.api.JoinHA = func(gotRole, name, ip string) (*ha.Bundle, error) {
				if gotRole != role || name != req.NodeName || ip != req.InternalIP {
					t.Errorf("wrong HA join arguments: %q %q %q", gotRole, name, ip)
				}
				if role == ha.RoleControlPlane {
					urls = append(urls, "https://192.0.2.12:6443")
				}
				return want, nil
			}
			got, err := join(tok, req)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got.ControlPlanes, urls) || !reflect.DeepEqual(got.HA, want) {
				t.Fatal("join omitted the new membership or changed the bundle")
			}
			creds, err := c.st.Credentials(auth.KindNode)
			if err != nil {
				t.Fatal(err)
			}
			_, err = c.st.Get(store.Key{Resource: "nodes", Name: req.NodeName})
			if role == ha.RoleWitness {
				if !errors.Is(err, store.ErrNotFound) || len(creds) != 0 || got.NodeToken != "" || got.PodCIDR != "" || got.ServingCert != "" {
					t.Fatal("a witness received a node registration or node credentials")
				}
			} else if err != nil || len(creds) != 1 || got.NodeToken == "" || got.ServingCert == "" || got.PodCIDR == "" {
				t.Fatal("standby is missing its worker registration")
			}
			_, err = join(tok, req)
			requireJoinStatus(t, err, http.StatusUnauthorized)
		})
	}
}

func TestNodeAPIAdvertisesMembersOnErrors(t *testing.T) {
	c := newCluster(t)
	c.api.ControlPlanes = func() []string { return []string{"https://one", "https://two"} }
	for _, path := range []string{"ca", "objects", "unknown"} {
		r, err := c.srv.Client().Get(c.srv.URL + Prefix + path)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if got := r.Header.Get(controlPlanesHeader); got != "https://one,https://two" {
			t.Fatalf("%s: member header = %q", path, got)
		}
	}
	w := httptest.NewRecorder()
	failErr(w, store.ErrUnavailable)
	var msg errorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &msg); err != nil || w.Code != 503 || msg.Code != "NOT_LEADER" || msg.Retry != "safe" {
		t.Fatalf("fenced write: HTTP %d %s", w.Code, w.Body)
	}
}

func TestClientLearnsLeaderAndReplaysRequest(t *testing.T) {
	var goodCalls, fencedCalls atomic.Int32
	const payload = `{"uid":"p1"}`
	good := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		goodCalls.Add(1)
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path != Prefix+"ips/release" || r.Header.Get("Authorization") != "Bearer node" || string(body) != payload {
			t.Error("the next leader did not receive the original authenticated POST")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer good.Close()
	fenced := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fencedCalls.Add(1)
		w.Header().Set(controlPlanesHeader, "https://"+r.Host+","+good.URL)
		failErr(w, store.ErrUnavailable)
	}))
	defer fenced.Close()
	dead := httptest.NewTLSServer(http.NotFoundHandler())
	dead.Close()
	var learned [][]string
	c := &Client{URL: dead.URL, URLs: []string{dead.URL, fenced.URL}, HTTP: good.Client(), Token: "node",
		OnControlPlanes: func(urls []string) { learned = append(learned, slices.Clone(urls)) }}
	for range 2 {
		if err := c.ReleaseIP("p1"); err != nil {
			t.Fatal(err)
		}
	}
	if len(learned) != 1 || !slices.Equal(learned[0], []string{fenced.URL, good.URL}) || goodCalls.Load() != 2 || fencedCalls.Load() != 1 {
		t.Fatalf("learned %v, good calls %d, fenced calls %d", learned, goodCalls.Load(), fencedCalls.Load())
	}
}

func TestClientMovesOnAfterCanceledLongPoll(t *testing.T) {
	started := make(chan struct{})
	stalled := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer stalled.Close()
	good := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, waitResponse{Rev: 22})
	}))
	defer good.Close()
	c := &Client{URL: stalled.URL, URLs: []string{stalled.URL, good.URL}, HTTP: good.Client()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.do(ctx, http.MethodGet, "wait", nil, nil, nil) }()
	select {
	case <-started:
		cancel()
	case err := <-done:
		t.Fatalf("wait never reached the first address: %v", err)
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled wait returned %v", err)
	}
	var resp waitResponse
	if err := c.do(context.Background(), http.MethodGet, "wait", nil, nil, &resp); err != nil || resp.Rev != 22 {
		t.Fatalf("next poll failed to move to the responding address: %v", err)
	}
}

func TestClientStopsAfterOnePassOrPermanentError(t *testing.T) {
	for _, code := range []int{http.StatusForbidden, http.StatusServiceUnavailable} {
		var calls atomic.Int32
		s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.Header().Set(controlPlanesHeader, "http://insecure.example")
			http.Error(w, "no", code)
		}))
		c := &Client{URL: s.URL, URLs: []string{s.URL, s.URL}, HTTP: s.Client()}
		_, _, err := c.List("", "nodes", "")
		s.Close()
		if err == nil || calls.Load() != 1 || strings.HasPrefix(c.URLs[0], "http://") {
			t.Fatalf("HTTP %d: error %v, calls %d, list %v", code, err, calls.Load(), c.URLs)
		}
	}
}

func TestLegacyClientLearnsOnce(t *testing.T) {
	c := newCluster(t)
	cl, _ := c.join(t, "old", "192.0.2.19")
	c.api.ControlPlanes = func() []string { return []string{c.srv.URL} }
	learned := 0
	cl.OnControlPlanes = func([]string) { learned++ }
	for range 2 {
		if _, _, err := cl.List("", "nodes", ""); err != nil {
			t.Fatal(err)
		}
	}
	if learned != 1 || len(cl.URLs) != 1 || cl.URLs[0] != cl.URL {
		t.Fatal("legacy client failed to adopt the advertised control plane")
	}
}
