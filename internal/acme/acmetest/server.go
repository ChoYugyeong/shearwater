// Package acmetest is a small in-process ACME authority for tests. It checks
// what a real authority checks about a client: every request is a correctly
// signed ES256 JWS for exactly that URL with an unused nonce, the HTTP-01
// response matches the account key, and the CSR is for the ordered name.
package acmetest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"
)

// Authority is a fake certificate authority. Fetch is called to read the
// HTTP-01 response for a domain, the way a real authority connects to port 80.
type Authority struct {
	Server *httptest.Server
	Roots  *x509.CertPool
	Fetch  func(domain, path string) (string, error)
	// BadNonceOnce makes the next signed request fail with badNonce.
	BadNonceOnce bool
	// Lifetime of issued certificates; default 90 days.
	Lifetime time.Duration

	mu       sync.Mutex
	caKey    *ecdsa.PrivateKey
	caCert   *x509.Certificate
	nonces   map[string]bool
	accounts map[string]*ecdsa.PublicKey // account URL -> key
	orders   []*order
	Issued   []string // domains, in issue order
	Rejected []string // protocol violations seen
}

type order struct {
	domain, token   string
	account         string
	authz, finalize string // status: pending/valid/invalid; finalize: ""/processing/valid
	cert            []byte
	reason          string
}

func New() *Authority {
	a := &Authority{nonces: map[string]bool{}, accounts: map[string]*ecdsa.PublicKey{}}
	a.caKey, _ = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "acmetest root"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &a.caKey.PublicKey, a.caKey)
	a.caCert, _ = x509.ParseCertificate(der)
	a.Roots = x509.NewCertPool()
	a.Roots.AddCert(a.caCert)
	a.Server = httptest.NewServer(http.HandlerFunc(a.serve))
	return a
}

func (a *Authority) Close()            { a.Server.Close() }
func (a *Authority) Directory() string { return a.Server.URL + "/dir" }

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func (a *Authority) nonce() string {
	b := make([]byte, 12)
	rand.Read(b)
	n := b64(b)
	a.nonces[n] = true
	return n
}

func (a *Authority) problem(w http.ResponseWriter, status int, kind, detail string) {
	if kind != "badNonce" {
		a.Rejected = append(a.Rejected, kind+": "+detail)
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"type": "urn:ietf:params:acme:error:" + kind, "detail": detail, "status": status})
}

func thumbprint(k *ecdsa.PublicKey) string {
	b, _ := json.Marshal(map[string]string{"crv": "P-256", "kty": "EC", "x": b64(k.X.FillBytes(make([]byte, 32))), "y": b64(k.Y.FillBytes(make([]byte, 32)))})
	h := sha256.Sum256(b)
	return b64(h[:])
}

// verify authenticates one JWS and returns its payload and account URL.
func (a *Authority) verify(w http.ResponseWriter, r *http.Request) (payload []byte, account string, ok bool) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var jws struct{ Protected, Payload, Signature string }
	if r.Header.Get("Content-Type") != "application/jose+json" || json.Unmarshal(body, &jws) != nil {
		a.problem(w, 400, "malformed", "not a flattened JWS with application/jose+json")
		return
	}
	hb, _ := base64.RawURLEncoding.DecodeString(jws.Protected)
	var h struct {
		Alg, Nonce, URL, Kid string
		JWK                  *struct{ Crv, Kty, X, Y string }
	}
	if json.Unmarshal(hb, &h) != nil || h.Alg != "ES256" {
		a.problem(w, 400, "badSignatureAlgorithm", "ES256 required")
		return
	}
	if a.BadNonceOnce || !a.nonces[h.Nonce] {
		a.BadNonceOnce = false
		a.problem(w, 400, "badNonce", "nonce is unknown or already used")
		return
	}
	delete(a.nonces, h.Nonce)
	if h.URL != a.Server.URL+r.URL.Path {
		a.problem(w, 400, "malformed", "JWS url "+h.URL+" does not match the request")
		return
	}
	var key *ecdsa.PublicKey
	switch {
	case h.JWK != nil && h.Kid == "" && r.URL.Path == "/acct":
		x, _ := base64.RawURLEncoding.DecodeString(h.JWK.X)
		y, _ := base64.RawURLEncoding.DecodeString(h.JWK.Y)
		key = &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
		account = a.Server.URL + "/acct/" + thumbprint(key)
	case h.Kid != "" && h.JWK == nil && a.accounts[h.Kid] != nil:
		key, account = a.accounts[h.Kid], h.Kid
	default:
		a.problem(w, 400, "accountDoesNotExist", "use jwk only for newAccount and a registered kid elsewhere")
		return
	}
	sig, _ := base64.RawURLEncoding.DecodeString(jws.Signature)
	digest := sha256.Sum256([]byte(jws.Protected + "." + jws.Payload))
	if len(sig) != 64 || !ecdsa.Verify(key, digest[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		a.problem(w, 400, "malformed", "JWS signature does not verify")
		return
	}
	if r.URL.Path == "/acct" {
		a.accounts[account] = key
	}
	payload, _ = base64.RawURLEncoding.DecodeString(jws.Payload)
	return payload, account, true
}

func (a *Authority) serve(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	base := a.Server.URL
	w.Header().Set("Replay-Nonce", a.nonce())
	switch {
	case r.URL.Path == "/dir":
		json.NewEncoder(w).Encode(map[string]string{"newNonce": base + "/nonce", "newAccount": base + "/acct", "newOrder": base + "/order"})
		return
	case r.URL.Path == "/nonce":
		return
	}
	if r.Method != http.MethodPost {
		a.problem(w, 405, "malformed", "POST required")
		return
	}
	payload, account, ok := a.verify(w, r)
	if !ok {
		return
	}
	var id int
	var o *order
	for _, prefix := range []string{"/authz/", "/chall/", "/finalize/", "/orders/", "/cert/"} {
		if rest, found := strings.CutPrefix(r.URL.Path, prefix); found {
			fmt.Sscanf(rest, "%d", &id)
			if id < 1 || id > len(a.orders) || a.orders[id-1].account != account {
				a.problem(w, 403, "unauthorized", "order belongs to another account")
				return
			}
			o = a.orders[id-1]
		}
	}
	writeOrder := func(status int) {
		st := "pending"
		switch {
		case o.authz == "invalid":
			st = "invalid"
		case o.finalize == "valid":
			st = "valid"
		case o.finalize == "processing":
			st, o.finalize = "processing", "valid" // valid on the next poll
		case o.authz == "valid":
			st = "ready"
		}
		doc := map[string]any{"status": st, "authorizations": []string{fmt.Sprintf("%s/authz/%d", base, id)}, "finalize": fmt.Sprintf("%s/finalize/%d", base, id)}
		if st == "valid" {
			doc["certificate"] = fmt.Sprintf("%s/cert/%d", base, id)
		}
		w.Header().Set("Location", fmt.Sprintf("%s/orders/%d", base, id))
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(doc)
	}
	switch {
	case r.URL.Path == "/acct":
		var q struct {
			TermsOfServiceAgreed bool `json:"termsOfServiceAgreed"`
		}
		if json.Unmarshal(payload, &q) != nil || !q.TermsOfServiceAgreed {
			a.problem(w, 400, "userActionRequired", "terms of service were not agreed")
			return
		}
		w.Header().Set("Location", account)
		w.WriteHeader(201)
		w.Write([]byte(`{"status":"valid"}`))
	case r.URL.Path == "/order":
		var q struct {
			Identifiers []struct{ Type, Value string } `json:"identifiers"`
		}
		if json.Unmarshal(payload, &q) != nil || len(q.Identifiers) != 1 || q.Identifiers[0].Type != "dns" {
			a.problem(w, 400, "malformed", "one dns identifier required")
			return
		}
		tb := make([]byte, 16)
		rand.Read(tb)
		o = &order{domain: q.Identifiers[0].Value, token: b64(tb), account: account, authz: "pending"}
		a.orders = append(a.orders, o)
		id = len(a.orders)
		writeOrder(201)
	case strings.HasPrefix(r.URL.Path, "/authz/"):
		ch := map[string]any{"type": "http-01", "url": fmt.Sprintf("%s/chall/%d", base, id), "token": o.token, "status": o.authz}
		if o.authz == "invalid" {
			ch["error"] = map[string]any{"type": "urn:ietf:params:acme:error:unauthorized", "detail": o.reason, "status": 403}
		}
		json.NewEncoder(w).Encode(map[string]any{"status": o.authz, "challenges": []any{
			map[string]any{"type": "dns-01", "url": base + "/unsupported", "token": "x", "status": "pending"}, ch}})
	case strings.HasPrefix(r.URL.Path, "/chall/"):
		if string(payload) != "{}" {
			a.problem(w, 400, "malformed", "challenge response must be {}")
			return
		}
		want := o.token + "." + thumbprint(a.accounts[account])
		a.mu.Unlock() // the client serves the challenge while we fetch it
		got, err := a.Fetch(o.domain, "/.well-known/acme-challenge/"+o.token)
		a.mu.Lock()
		switch {
		case err != nil:
			o.authz, o.reason = "invalid", "could not fetch the challenge: "+err.Error()
		case strings.TrimSpace(got) != want:
			o.authz, o.reason = "invalid", "challenge response does not match the account key"
		default:
			o.authz = "valid"
		}
		w.Write([]byte(`{"status":"processing"}`))
	case strings.HasPrefix(r.URL.Path, "/finalize/"):
		var q struct {
			CSR string `json:"csr"`
		}
		json.Unmarshal(payload, &q)
		der, _ := base64.RawURLEncoding.DecodeString(q.CSR)
		csr, err := x509.ParseCertificateRequest(der)
		if o.authz != "valid" || err != nil || csr.CheckSignature() != nil || len(csr.DNSNames) != 1 || csr.DNSNames[0] != o.domain {
			a.problem(w, 403, "badCSR", "order is not ready or the CSR is not for exactly the ordered name")
			return
		}
		life := a.Lifetime
		if life == 0 {
			life = 90 * 24 * time.Hour
		}
		tmpl := &x509.Certificate{SerialNumber: big.NewInt(int64(100 + id)), Subject: pkix.Name{CommonName: o.domain}, DNSNames: []string{o.domain},
			NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(life),
			KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
		leaf, _ := x509.CreateCertificate(rand.Reader, tmpl, a.caCert, csr.PublicKey, a.caKey)
		o.cert = append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leaf}), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: a.caCert.Raw})...)
		o.finalize = "processing"
		a.Issued = append(a.Issued, o.domain)
		writeOrder(200)
	case strings.HasPrefix(r.URL.Path, "/orders/"):
		writeOrder(200)
	case strings.HasPrefix(r.URL.Path, "/cert/"):
		if len(payload) != 0 || o.cert == nil {
			a.problem(w, 400, "malformed", "certificate download is POST-as-GET on a valid order")
			return
		}
		w.Header().Set("Content-Type", "application/pem-certificate-chain")
		w.Write(o.cert)
	default:
		a.problem(w, 404, "malformed", "unknown endpoint")
	}
}

// RootPEM is the authority's root certificate, for clients that verify the
// certificates it issued.
func (a *Authority) RootPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: a.caCert.Raw})
}

// Log returns the domains issued and the protocol violations seen so far.
func (a *Authority) Log() (issued, rejected []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.Issued...), append([]string(nil), a.Rejected...)
}
