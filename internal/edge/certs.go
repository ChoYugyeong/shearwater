package edge

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"shearwater/internal/acme"
)

// Issuer obtains a certificate for one host name. *acme.Client implements it.
type Issuer interface {
	Obtain(ctx context.Context, domain string, present func(token, keyAuthorization string) (remove func())) (certPEM, keyPEM []byte, err error)
}

// Certs keeps one certificate per published host: it loads saved ones,
// obtains missing ones, renews them in the last third of their lifetime and
// answers TLS handshakes. Only hosts in the routing table get a certificate,
// so a stranger's TLS server name can never trigger an order.
type Certs struct {
	Dir    string // private directory: <host>.crt and <host>.key
	Issuer Issuer
	Now    func() time.Time

	mu     sync.Mutex
	certs  map[string]*tls.Certificate
	tokens map[string]string // challenge path -> key authorization
	state  map[string]*CertState
}

// CertState is what `sw edge status` shows for one host.
type CertState struct {
	Ready     bool      `json:"ready"`
	NotAfter  time.Time `json:"notAfter,omitzero"`
	RenewAt   time.Time `json:"renewAt,omitzero"`
	LastError string    `json:"lastError,omitempty"`
	RetryAt   time.Time `json:"retryAt,omitzero"`
	failures  int
}

func (c *Certs) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Certs) init() {
	if c.certs == nil {
		c.certs, c.tokens, c.state = map[string]*tls.Certificate{}, map[string]string{}, map[string]*CertState{}
	}
}

// LoadAccountKey reads the ACME account key, creating it on first use.
func LoadAccountKey(path string) (*ecdsa.PrivateKey, error) {
	if data, err := os.ReadFile(path); err == nil {
		block, _ := pem.Decode(data)
		if block == nil {
			return nil, fmt.Errorf("%s is not a PEM key", path)
		}
		return x509.ParseECPrivateKey(block.Bytes)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	return key, writePrivate(path, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}))
}

func writePrivate(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// GetCertificate serves TLS handshakes. An unknown or not yet certified host
// fails the handshake instead of receiving another host's certificate.
func (c *Certs) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cert := c.certs[requestHost(hello.ServerName)]; cert != nil {
		return cert, nil
	}
	return nil, fmt.Errorf("no certificate for %q", hello.ServerName)
}

// Ready reports whether HTTPS can be served for the host now.
func (c *Certs) Ready(host string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.certs[host] != nil
}

// Challenge returns the HTTP-01 response for a request path, if one is active.
func (c *Certs) Challenge(path string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	body, ok := c.tokens[path]
	return body, ok
}

func (c *Certs) present(token, keyAuthorization string) func() {
	path := "/.well-known/acme-challenge/" + token
	c.mu.Lock()
	c.tokens[path] = keyAuthorization
	c.mu.Unlock()
	return func() {
		c.mu.Lock()
		delete(c.tokens, path)
		c.mu.Unlock()
	}
}

// States returns a copy of the per-host certificate state.
func (c *Certs) States() map[string]CertState {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]CertState, len(c.state))
	for h, s := range c.state {
		out[h] = *s
	}
	return out
}

func leafOf(cert *tls.Certificate) *x509.Certificate {
	if cert.Leaf == nil {
		cert.Leaf, _ = x509.ParseCertificate(cert.Certificate[0])
	}
	return cert.Leaf
}

// renewAt is when the last third of a certificate's lifetime begins.
func renewAt(leaf *x509.Certificate) time.Time {
	return leaf.NotAfter.Add(-leaf.NotAfter.Sub(leaf.NotBefore) / 3)
}

// due reports whether the certificate is missing, unusable for the host, or
// in the last third of its lifetime.
func (c *Certs) due(host string, cert *tls.Certificate) bool {
	if cert == nil {
		return true
	}
	leaf := leafOf(cert)
	if leaf == nil || leaf.VerifyHostname(host) != nil {
		return true
	}
	return !c.now().Before(renewAt(leaf))
}

// Ensure brings certificates in line with the published hosts. It returns
// how long to wait before calling it again. Orders run one at a time; a
// failure backs off per host so a wrong DNS record cannot exhaust the
// authority's rate limits.
func (c *Certs) Ensure(ctx context.Context, hosts []string) time.Duration {
	c.mu.Lock()
	c.init()
	wanted := map[string]bool{}
	for _, h := range hosts {
		wanted[h] = true
	}
	for h := range c.certs {
		if !wanted[h] {
			delete(c.certs, h)
			delete(c.state, h)
		}
	}
	c.mu.Unlock()
	next := 12 * time.Hour
	for _, host := range hosts {
		if ctx.Err() != nil {
			break
		}
		c.mu.Lock()
		st := c.state[host]
		if st == nil {
			st = &CertState{}
			c.state[host] = st
		}
		cert := c.certs[host]
		c.mu.Unlock()
		if cert == nil {
			if loaded, err := tls.LoadX509KeyPair(filepath.Join(c.Dir, host+".crt"), filepath.Join(c.Dir, host+".key")); err == nil {
				if leaf := leafOf(&loaded); leaf != nil && leaf.VerifyHostname(host) == nil && c.now().Before(leaf.NotAfter) {
					cert = &loaded
					c.install(host, cert, st)
				}
			}
		}
		if !c.due(host, cert) {
			// Come back when this certificate enters its renewal window; a
			// short-lived certificate must not wait for the 12-hour check.
			next = min(next, renewAt(leafOf(cert)).Sub(c.now()))
			continue
		}
		if wait := st.RetryAt.Sub(c.now()); wait > 0 {
			next = min(next, wait)
			continue
		}
		certPEM, keyPEM, err := c.Issuer.Obtain(ctx, host, c.present)
		if err == nil {
			var pair tls.Certificate
			if pair, err = tls.X509KeyPair(certPEM, keyPEM); err == nil {
				if err = os.MkdirAll(c.Dir, 0o700); err == nil {
					if err = writePrivate(filepath.Join(c.Dir, host+".key"), keyPEM); err == nil {
						err = writePrivate(filepath.Join(c.Dir, host+".crt"), certPEM)
					}
				}
				if err == nil {
					c.install(host, &pair, st)
					next = min(next, renewAt(leafOf(&pair)).Sub(c.now()))
					continue
				}
			}
		}
		c.mu.Lock()
		st.failures++
		backoff := min(10*time.Minute<<min(st.failures-1, 7), 12*time.Hour)
		st.LastError, st.RetryAt = err.Error(), c.now().Add(backoff)
		c.mu.Unlock()
		next = min(next, backoff)
	}
	// An authority that issues certificates already inside their renewal
	// window must not turn this into a busy loop.
	return max(next, 5*time.Second)
}

func (c *Certs) install(host string, cert *tls.Certificate, st *CertState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.certs[host] = cert
	st.Ready, st.NotAfter, st.RenewAt = true, leafOf(cert).NotAfter, renewAt(leafOf(cert))
	st.LastError, st.RetryAt, st.failures = "", time.Time{}, 0
}

var _ Issuer = (*acme.Client)(nil)
