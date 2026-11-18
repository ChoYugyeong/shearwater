// Package acme obtains TLS certificates with the ACME protocol (RFC 8555)
// using the HTTP-01 challenge. It implements only what a single-node server
// needs: one account key, one DNS name per certificate, no external account
// binding and no revocation. Only the Go standard library is used.
package acme

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// LetsEncrypt is the production directory. Staging has much higher rate
// limits and issues certificates browsers do not trust.
const (
	LetsEncrypt        = "https://acme-v02.api.letsencrypt.org/directory"
	LetsEncryptStaging = "https://acme-staging-v02.api.letsencrypt.org/directory"
)

// Client is one ACME account at one certificate authority.
type Client struct {
	Directory string
	Key       *ecdsa.PrivateKey // P-256 account key
	Contact   string            // optional email for expiry notices
	HTTP      *http.Client
	// PollEvery overrides the wait between status polls; tests shorten it.
	PollEvery time.Duration

	mu    sync.Mutex
	dir   *directory
	kid   string
	nonce string
}

type directory struct {
	NewNonce   string `json:"newNonce"`
	NewAccount string `json:"newAccount"`
	NewOrder   string `json:"newOrder"`
}

// Problem is an error document returned by the authority (RFC 7807).
type Problem struct {
	Type   string `json:"type"`
	Detail string `json:"detail"`
	Status int    `json:"status"`
}

func (p *Problem) Error() string {
	return fmt.Sprintf("acme: %s (%s, HTTP %d)", p.Detail, strings.TrimPrefix(p.Type, "urn:ietf:params:acme:error:"), p.Status)
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (c *Client) jwk() map[string]string {
	size := (c.Key.Curve.Params().BitSize + 7) / 8
	return map[string]string{
		"crv": "P-256", "kty": "EC",
		"x": b64(c.Key.X.FillBytes(make([]byte, size))),
		"y": b64(c.Key.Y.FillBytes(make([]byte, size))),
	}
}

// Thumbprint is the RFC 7638 digest of the account key. encoding/json writes
// map keys sorted, which is the canonical member order the RFC requires.
func (c *Client) Thumbprint() string {
	b, _ := json.Marshal(c.jwk())
	h := sha256.Sum256(b)
	return b64(h[:])
}

// KeyAuthorization is the body an HTTP-01 challenge response must serve.
func (c *Client) KeyAuthorization(token string) string { return token + "." + c.Thumbprint() }

func (c *Client) discover(ctx context.Context) (*directory, error) {
	if c.dir != nil {
		return c.dir, nil
	}
	if c.Key == nil || c.Key.Curve != elliptic.P256() {
		return nil, errors.New("acme: account key must be ECDSA P-256")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Directory, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var d directory
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("acme: directory returned HTTP %d", resp.StatusCode)
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&d); err != nil {
		return nil, err
	}
	if d.NewNonce == "" || d.NewAccount == "" || d.NewOrder == "" {
		return nil, errors.New("acme: directory is missing required endpoints")
	}
	c.dir = &d
	return c.dir, nil
}

func (c *Client) freshNonce(ctx context.Context) (string, error) {
	if n := c.nonce; n != "" {
		c.nonce = ""
		return n, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, c.dir.NewNonce, nil)
	if err != nil {
		return "", err
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	n := resp.Header.Get("Replay-Nonce")
	if n == "" {
		return "", errors.New("acme: authority returned no nonce")
	}
	return n, nil
}

// sign builds a flattened JWS with ES256. payload nil is POST-as-GET.
func (c *Client) sign(url, nonce string, payload any) ([]byte, error) {
	header := map[string]any{"alg": "ES256", "nonce": nonce, "url": url}
	if c.kid != "" {
		header["kid"] = c.kid
	} else {
		header["jwk"] = c.jwk()
	}
	hb, _ := json.Marshal(header)
	body := ""
	if payload != nil {
		pb, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = b64(pb)
	}
	protected := b64(hb)
	digest := sha256.Sum256([]byte(protected + "." + body))
	r, s, err := ecdsa.Sign(rand.Reader, c.Key, digest[:])
	if err != nil {
		return nil, err
	}
	sig := append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...)
	return json.Marshal(map[string]string{"protected": protected, "payload": body, "signature": b64(sig)})
}

// post sends one signed request. A badNonce answer is retried once with the
// nonce that came with it, as RFC 8555 section 6.5 asks.
func (c *Client) post(ctx context.Context, url string, payload any, out any) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		nonce, err := c.freshNonce(ctx)
		if err != nil {
			return nil, err
		}
		body, err := c.sign(url, nonce, payload)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/jose+json")
		resp, err := c.http().Do(req)
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		c.nonce = resp.Header.Get("Replay-Nonce")
		if resp.StatusCode >= 400 {
			p := &Problem{Status: resp.StatusCode}
			json.Unmarshal(data, p)
			if p.Detail == "" {
				p.Detail = "request rejected"
			}
			if attempt == 0 && strings.HasSuffix(p.Type, ":badNonce") {
				continue
			}
			return nil, p
		}
		if raw, ok := out.(*[]byte); ok {
			*raw = data
		} else if out != nil {
			if err = json.Unmarshal(data, out); err != nil {
				return nil, fmt.Errorf("acme: unreadable response from %s: %w", url, err)
			}
		}
		return resp, nil
	}
}

// register creates the account or finds the existing one for this key. The
// caller has confirmed that the operator agreed to the terms of service.
func (c *Client) register(ctx context.Context) error {
	if c.kid != "" {
		return nil
	}
	payload := map[string]any{"termsOfServiceAgreed": true}
	if c.Contact != "" {
		payload["contact"] = []string{"mailto:" + c.Contact}
	}
	resp, err := c.post(ctx, c.dir.NewAccount, payload, nil)
	if err != nil {
		return err
	}
	if c.kid = resp.Header.Get("Location"); c.kid == "" {
		return errors.New("acme: authority returned no account URL")
	}
	return nil
}

type order struct {
	Status         string   `json:"status"`
	Authorizations []string `json:"authorizations"`
	Finalize       string   `json:"finalize"`
	Certificate    string   `json:"certificate"`
	Error          *Problem `json:"error"`
}

type authorization struct {
	Status     string `json:"status"`
	Challenges []struct {
		Type   string   `json:"type"`
		URL    string   `json:"url"`
		Token  string   `json:"token"`
		Status string   `json:"status"`
		Error  *Problem `json:"error"`
	} `json:"challenges"`
}

func (c *Client) wait(ctx context.Context, resp *http.Response) error {
	d := c.PollEvery
	if d == 0 {
		d = 2 * time.Second
		if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 && s <= 30 {
			d = time.Duration(s) * time.Second
		}
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Obtain orders a certificate for one DNS name. present must make
// GET http://<domain>/.well-known/acme-challenge/<token> answer with
// keyAuthorization before it returns; the returned function removes it again.
// The certificate chain and a new P-256 private key are returned as PEM.
func (c *Client) Obtain(ctx context.Context, domain string, present func(token, keyAuthorization string) (remove func())) (certPEM, keyPEM []byte, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err = c.discover(ctx); err != nil {
		return nil, nil, err
	}
	if err = c.register(ctx); err != nil {
		return nil, nil, err
	}
	var o order
	resp, err := c.post(ctx, c.dir.NewOrder, map[string]any{"identifiers": []map[string]string{{"type": "dns", "value": domain}}}, &o)
	if err != nil {
		return nil, nil, err
	}
	orderURL := resp.Header.Get("Location")
	if orderURL == "" || o.Finalize == "" {
		return nil, nil, errors.New("acme: incomplete order")
	}
	for _, authzURL := range o.Authorizations {
		if err = c.authorize(ctx, authzURL, present); err != nil {
			return nil, nil, err
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: domain}, DNSNames: []string{domain}}, key)
	if err != nil {
		return nil, nil, err
	}
	if resp, err = c.post(ctx, o.Finalize, map[string]string{"csr": b64(csr)}, &o); err != nil {
		return nil, nil, err
	}
	for tries := 0; o.Status != "valid"; tries++ {
		if o.Status == "invalid" || tries > 60 {
			if o.Error != nil {
				return nil, nil, o.Error
			}
			return nil, nil, fmt.Errorf("acme: order ended as %q", o.Status)
		}
		if err = c.wait(ctx, resp); err != nil {
			return nil, nil, err
		}
		if resp, err = c.post(ctx, orderURL, nil, &o); err != nil {
			return nil, nil, err
		}
	}
	if o.Certificate == "" {
		return nil, nil, errors.New("acme: valid order without certificate URL")
	}
	if _, err = c.post(ctx, o.Certificate, nil, &certPEM); err != nil {
		return nil, nil, err
	}
	if err = matches(certPEM, domain, key.Public()); err != nil {
		return nil, nil, err
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return certPEM, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), nil
}

func (c *Client) authorize(ctx context.Context, url string, present func(string, string) func()) error {
	var a authorization
	resp, err := c.post(ctx, url, nil, &a)
	if err != nil {
		return err
	}
	if a.Status == "valid" {
		return nil
	}
	if a.Status != "pending" {
		return fmt.Errorf("acme: authorization is %q", a.Status)
	}
	for _, ch := range a.Challenges {
		if ch.Type != "http-01" {
			continue
		}
		if ch.Token == "" || strings.ContainsAny(ch.Token, "/.\\") {
			return errors.New("acme: authority sent an unusable challenge token")
		}
		remove := present(ch.Token, c.KeyAuthorization(ch.Token))
		defer remove()
		if resp, err = c.post(ctx, ch.URL, struct{}{}, nil); err != nil {
			return err
		}
		for tries := 0; ; tries++ {
			if err = c.wait(ctx, resp); err != nil {
				return err
			}
			if resp, err = c.post(ctx, url, nil, &a); err != nil {
				return err
			}
			switch {
			case a.Status == "valid":
				return nil
			case a.Status != "pending" && a.Status != "processing" || tries > 60:
				for _, failed := range a.Challenges {
					if failed.Error != nil {
						return failed.Error
					}
				}
				return fmt.Errorf("acme: authorization ended as %q", a.Status)
			}
		}
	}
	return errors.New("acme: authority offered no http-01 challenge")
}

// matches rejects a chain whose first certificate is not for this name and
// key, so a confused or hostile authority response is never installed.
func matches(chain []byte, domain string, pub crypto.PublicKey) error {
	block, _ := pem.Decode(chain)
	if block == nil || block.Type != "CERTIFICATE" {
		return errors.New("acme: authority returned no PEM certificate")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return err
	}
	if err = leaf.VerifyHostname(domain); err != nil {
		return fmt.Errorf("acme: issued certificate is not for %s: %w", domain, err)
	}
	if k, ok := leaf.PublicKey.(*ecdsa.PublicKey); !ok || !k.Equal(pub) {
		return errors.New("acme: issued certificate does not match the requested key")
	}
	return nil
}
