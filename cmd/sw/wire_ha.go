package main

// Control plane high availability wiring (docs/design-agent-first-ha.md 3).
// haWiring belongs to a control plane that advertises an address. It makes
// the control plane HA-ready when the first control-plane or witness join
// token is issued, answers joins with their bundle, and tells nodes where
// every control plane is.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	"shearwater/internal/ha"
	"shearwater/internal/pki"
)

type haWiring struct {
	w         *wiring
	self      string     // this control plane's member name (its node name)
	advertise netip.Addr // the address other servers reach
	port      string     // the API port
	logf      func(format string, a ...any)
	// reloadTLS makes the API server present the current server.crt.
	reloadTLS func() error
	// restart starts this process again (as a standby after stepping down,
	// as the leader after winning an election).
	restart func(reason string)

	mu     sync.Mutex
	ctx    context.Context // the server's lifetime; set by start
	cfg    *ha.Config      // nil until HA is enabled
	leader *ha.Leader
}

func (h *haWiring) url(ip string) string {
	return "https://" + net.JoinHostPort(ip, h.port)
}

// ServeHTTP serves ha.Prefix while this process leads.
func (h *haWiring) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	l := h.leader
	h.mu.Unlock()
	if l == nil {
		http.Error(w, "NOT_LEADER retry=safe: this control plane does not replicate", http.StatusServiceUnavailable)
		return
	}
	l.ServeHTTP(w, r)
}

// start remembers the server's lifetime and, when ha.json makes this
// process the leader, starts replicating.
func (h *haWiring) start(ctx context.Context, cfg *ha.Config) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ctx = ctx
	if cfg == nil {
		return nil
	}
	if err := h.startLocked(*cfg); err != nil {
		return err
	}
	if err := h.prepareSelfWorker(); err != nil {
		h.logf("cannot prepare this node's worker credentials for a later step-down: %v", err)
	}
	h.logf("leading epoch %d with %d control plane(s)", cfg.Epoch, len(cfg.Members))
	return nil
}

func (h *haWiring) startLocked(cfg ha.Config) error {
	c, err := h.client()
	if err != nil {
		return err
	}
	l := &ha.Leader{Store: h.w.Store, Config: cfg, DataDir: h.w.DataDir, Logf: h.logf, HTTP: c,
		StepDown: func(reason string) { h.stepDown(reason) }}
	if cfg.Witness != "" {
		l.Witness = &ha.WitnessClient{URL: cfg.Witness, Token: cfg.Token, Client: c}
	}
	if err := l.Start(h.ctx); err != nil {
		return err
	}
	h.cfg, h.leader = &cfg, l
	return nil
}

// client is an HTTPS client that trusts only the cluster CA.
func (h *haWiring) client() (*http.Client, error) {
	return haHTTPClient(h.w.ConfigDir)
}

func haHTTPClient(configDir string) (*http.Client, error) {
	ca, err := os.ReadFile(filepath.Join(configDir, "ca.crt"))
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, errors.New("ca.crt holds no certificate")
	}
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil
	t.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	return &http.Client{Transport: t}, nil
}

// Enable makes this control plane HA-ready: it writes ha.json with itself as
// the only member and the leader of epoch 1, and starts replicating.
func (h *haWiring) Enable() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cfg != nil {
		return nil
	}
	if h.ctx == nil {
		return errors.New("the server is still starting")
	}
	cfg := ha.Config{Self: h.self, Members: []ha.Member{{Name: h.self, URL: h.url(h.advertise.String())}},
		Token: ha.NewToken(), Role: ha.RoleLeader, Epoch: 1}
	if err := cfg.Save(h.w.ConfigDir); err != nil {
		return err
	}
	if err := h.startLocked(cfg); err != nil {
		return err
	}
	if err := h.prepareSelfWorker(); err != nil {
		return err
	}
	h.logf("high availability enabled: this control plane leads epoch 1")
	return nil
}

// ControlPlanes lists every control plane's API, this (the leader) first.
func (h *haWiring) ControlPlanes() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cfg == nil {
		return []string{h.url(h.advertise.String())}
	}
	out := []string{}
	for _, m := range h.cfg.Members {
		if m.Name == h.self {
			out = append([]string{m.URL}, out...)
		} else {
			out = append(out, m.URL)
		}
	}
	return out
}

// Join answers a joining control plane or witness with its bundle.
func (h *haWiring) Join(role, name, internalIP string) (*ha.Bundle, error) {
	ip, err := netip.ParseAddr(internalIP)
	if err != nil || !ip.Is4() {
		return nil, fmt.Errorf("%q is not an IPv4 address", internalIP)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cfg == nil || h.leader == nil {
		return nil, errors.New("high availability is not enabled on this control plane (sw node token --control-plane enables it)")
	}
	ca, err := os.ReadFile(filepath.Join(h.w.ConfigDir, "ca.crt"))
	if err != nil {
		return nil, err
	}
	cfg := *h.cfg
	switch role {
	case ha.RoleControlPlane:
		if name == h.self {
			return nil, fmt.Errorf("%s is this control plane's own name", name)
		}
		if p, ok := cfg.Peer(); ok && p.Name != name {
			return nil, fmt.Errorf("this cluster already has a second control plane, %s (two at most)", p.Name)
		}
		key, err := os.ReadFile(filepath.Join(h.w.ConfigDir, "ca.key"))
		if err != nil {
			return nil, err
		}
		admin, err := os.ReadFile(filepath.Join(h.w.ConfigDir, "admin.token"))
		if err != nil {
			return nil, err
		}
		// Clients that name the standby reach this leader through it, so
		// this certificate must carry the standby's address too. The
		// certificate is reissued for exactly these addresses (and loopback).
		if err := pki.EnsureServerAddresses(h.w.ConfigDir, []net.IP{net.IP(h.advertise.AsSlice()), net.IP(ip.AsSlice())}); err != nil {
			return nil, err
		}
		if h.reloadTLS != nil {
			if err := h.reloadTLS(); err != nil {
				return nil, err
			}
		}
		cfg.Members = []ha.Member{{Name: h.self, URL: h.url(h.advertise.String())}, {Name: name, URL: h.url(ip.String())}}
		if err := h.update(cfg); err != nil {
			return nil, err
		}
		joiner := cfg
		joiner.Self, joiner.Role, joiner.Nonce = name, ha.RoleStandby, ""
		return &ha.Bundle{Config: joiner, CACert: string(ca), CAKey: string(key), AdminToken: strings.TrimSpace(string(admin))}, nil
	case ha.RoleWitness:
		listen := net.JoinHostPort(ip.String(), strconv.Itoa(ha.WitnessPort))
		certPEM, keyPEM, err := signServing(h.w.ConfigDir, name, ip)
		if err != nil {
			return nil, err
		}
		cfg.Witness = "https://" + listen
		if err := h.update(cfg); err != nil {
			return nil, err
		}
		witness := cfg
		witness.Self, witness.Role, witness.Nonce = name, ha.RoleStandby, ""
		witness.Members = append(slices.Clone(witness.Members), ha.Member{Name: name, URL: cfg.Witness})
		return &ha.Bundle{Config: witness, CACert: string(ca), ServingCert: string(certPEM), ServingKey: string(keyPEM), Listen: listen}, nil
	}
	return nil, fmt.Errorf("unknown role %q", role)
}

// update saves a changed configuration and hands it to the running leader.
func (h *haWiring) update(cfg ha.Config) error {
	if err := cfg.Save(h.w.ConfigDir); err != nil {
		return err
	}
	h.cfg = &cfg
	return h.leader.SetConfig(cfg, h.ctx)
}

// stepDown records that this control plane is a standby and starts again.
func (h *haWiring) stepDown(reason string) {
	h.mu.Lock()
	cfg := h.cfg
	h.mu.Unlock()
	if cfg != nil {
		c := *cfg
		c.Role, c.Nonce = ha.RoleStandby, ""
		if err := c.Save(h.w.ConfigDir); err != nil {
			h.logf("cannot save %s: %v", ha.ConfigFile, err)
		}
	}
	if h.restart != nil {
		h.restart("stepped down: " + reason)
	}
}

// signServing issues a server certificate for ip signed by the cluster CA.
func signServing(configDir, name string, ip netip.Addr) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: name}}, key)
	if err != nil {
		return nil, nil, err
	}
	csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csr})
	certPEM, err = pki.SignNodeServing(configDir, csrPEM, name, []net.IP{net.IP(ip.AsSlice())})
	if err != nil {
		return nil, nil, err
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return certPEM, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), nil
}
