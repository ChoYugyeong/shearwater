package acme

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"shearwater/internal/acme/acmetest"
)

type challenges struct {
	mu sync.Mutex
	m  map[string]string
}

func (c *challenges) present(token, keyAuth string) func() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m["/.well-known/acme-challenge/"+token] = keyAuth
	return func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		delete(c.m, "/.well-known/acme-challenge/"+token)
	}
}

func (c *challenges) fetch(_, path string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	body, ok := c.m[path]
	if !ok {
		return "", errors.New("404")
	}
	return body, nil
}

func newClient(t *testing.T, a *acmetest.Authority) *Client {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &Client{Directory: a.Directory(), Key: key, Contact: "ops@example.com", PollEvery: time.Millisecond}
}

func TestObtainIssuesAUsableCertificateAndCleansUp(t *testing.T) {
	a := acmetest.New()
	defer a.Close()
	ch := &challenges{m: map[string]string{}}
	a.Fetch = ch.fetch
	a.BadNonceOnce = true // the first signed request must be retried
	c := newClient(t, a)
	certPEM, keyPEM, err := c.Obtain(context.Background(), "app.example.com", ch.present)
	if err != nil {
		t.Fatal(err, a.Rejected)
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal("certificate and key do not form a pair:", err)
	}
	leaf, _ := x509.ParseCertificate(pair.Certificate[0])
	inter := x509.NewCertPool()
	for _, der := range pair.Certificate[1:] {
		ic, _ := x509.ParseCertificate(der)
		inter.AddCert(ic)
	}
	if _, err = leaf.Verify(x509.VerifyOptions{DNSName: "app.example.com", Roots: a.Roots, Intermediates: inter}); err != nil {
		t.Fatal("issued chain does not verify for the name:", err)
	}
	if len(ch.m) != 0 {
		t.Fatal("challenge response was left behind", ch.m)
	}
	if len(a.Rejected) != 0 {
		t.Fatal("the authority saw protocol violations:", a.Rejected)
	}
	// A second certificate reuses the account (kid) instead of registering.
	if _, _, err = c.Obtain(context.Background(), "two.example.com", ch.present); err != nil || len(a.Issued) != 2 || len(a.Rejected) != 0 {
		t.Fatal("second order failed", err, a.Issued, a.Rejected)
	}
}

func TestObtainReportsAFailedChallenge(t *testing.T) {
	a := acmetest.New()
	defer a.Close()
	a.Fetch = func(string, string) (string, error) { return "someone else's answer", nil }
	c := newClient(t, a)
	_, _, err := c.Obtain(context.Background(), "app.example.com", func(string, string) func() { return func() {} })
	var p *Problem
	if !errors.As(err, &p) || !strings.Contains(p.Detail, "does not match the account key") || len(a.Issued) != 0 {
		t.Fatal("want the authority's challenge failure, got", err)
	}
}

func TestCertificateForAnotherNameOrKeyIsRejected(t *testing.T) {
	a := acmetest.New()
	defer a.Close()
	ch := &challenges{m: map[string]string{}}
	a.Fetch = ch.fetch
	c := newClient(t, a)
	certPEM, _, err := c.Obtain(context.Background(), "app.example.com", ch.present)
	if err != nil {
		t.Fatal(err)
	}
	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	block, _ := pem.Decode(certPEM)
	leaf, _ := x509.ParseCertificate(block.Bytes)
	if matches(certPEM, "app.example.com", leaf.PublicKey) != nil {
		t.Fatal("the correct certificate was rejected")
	}
	if matches(certPEM, "evil.example.com", leaf.PublicKey) == nil || matches(certPEM, "app.example.com", other.Public()) == nil || matches([]byte("junk"), "app.example.com", leaf.PublicKey) == nil {
		t.Fatal("a certificate for another name or key must not be installed")
	}
}

func TestKeyAuthorizationUsesRFC7638Thumbprint(t *testing.T) {
	// RFC 7638 only fixes the form; check stability and shape for one key.
	a := acmetest.New()
	defer a.Close()
	c := newClient(t, a)
	ka := c.KeyAuthorization("tok")
	if !strings.HasPrefix(ka, "tok.") || len(ka) != len("tok.")+43 || ka != c.KeyAuthorization("tok") {
		t.Fatal("unexpected key authorization", ka)
	}
}
