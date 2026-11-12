// Package pki creates and loads what the API server needs to speak TLS and to
// know its admin: a self-signed CA, a server certificate, and a bearer token.
package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Material struct {
	CACert []byte // PEM
	Server tls.Certificate
	Token  string
}

const validFor = 10 * 365 * 24 * time.Hour

func exists(path string) bool {
	_, err := os.Stat(path)
	return !errors.Is(err, fs.ErrNotExist)
}

func newCert(tmpl, parent *x509.Certificate, parentKey *ecdsa.PrivateKey) (certPEM, keyPEM []byte, key *ecdsa.PrivateKey, err error) {
	key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, nil, err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	tmpl.SerialNumber = serial
	tmpl.NotBefore = time.Now().Add(-time.Hour)
	tmpl.NotAfter = time.Now().Add(validFor)
	if parent == nil {
		parent, parentKey = tmpl, key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &key.PublicKey, parentKey)
	if err != nil {
		return nil, nil, nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), key, nil
}

// Ensure loads the material in dir, creating whatever is missing. Private
// files are readable by the owner only.
func Ensure(dir string) (*Material, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	caCrt, caKey := filepath.Join(dir, "ca.crt"), filepath.Join(dir, "ca.key")
	srvCrt, srvKey := filepath.Join(dir, "server.crt"), filepath.Join(dir, "server.key")
	tokenFile := filepath.Join(dir, "admin.token")

	newCA := !exists(caCrt) || !exists(caKey)
	if newCA {
		certPEM, keyPEM, _, err := newCert(&x509.Certificate{
			Subject:               pkix.Name{CommonName: "shearwater-ca"},
			IsCA:                  true,
			BasicConstraintsValid: true,
			KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		}, nil, nil)
		if err != nil {
			return nil, err
		}
		if err := writeFiles(map[string][]byte{caCrt: certPEM, caKey: keyPEM}); err != nil {
			return nil, err
		}
	}
	// A new CA cannot vouch for a server certificate signed by the old one, so that is reissued too.
	if newCA || !exists(srvCrt) || !exists(srvKey) {
		ca, err := tls.LoadX509KeyPair(caCrt, caKey)
		if err != nil {
			return nil, fmt.Errorf("load CA from %s: %w", dir, err)
		}
		caCert, err := x509.ParseCertificate(ca.Certificate[0])
		if err != nil {
			return nil, err
		}
		certPEM, keyPEM, _, err := newCert(&x509.Certificate{
			Subject:     pkix.Name{CommonName: "shearwater-server"},
			KeyUsage:    x509.KeyUsageDigitalSignature,
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			DNSNames:    []string{"localhost"},
			IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
		}, caCert, ca.PrivateKey.(*ecdsa.PrivateKey))
		if err != nil {
			return nil, err
		}
		if err := writeFiles(map[string][]byte{srvCrt: certPEM, srvKey: keyPEM}); err != nil {
			return nil, err
		}
	}
	if !exists(tokenFile) {
		b := make([]byte, 32)
		rand.Read(b)
		if err := writeFiles(map[string][]byte{tokenFile: []byte(hex.EncodeToString(b) + "\n")}); err != nil {
			return nil, err
		}
	}
	return Load(dir)
}

// Load reads existing material. It fails with a message that says what to do
// when the directory is unreadable or incomplete.
func Load(dir string) (*Material, error) {
	m := &Material{}
	var err error
	if m.CACert, err = os.ReadFile(filepath.Join(dir, "ca.crt")); err != nil {
		return nil, fmt.Errorf("%w (start the server once so it creates %s, and run this as the same user)", err, dir)
	}
	if m.Server, err = tls.LoadX509KeyPair(filepath.Join(dir, "server.crt"), filepath.Join(dir, "server.key")); err != nil {
		return nil, err
	}
	token, err := os.ReadFile(filepath.Join(dir, "admin.token"))
	if err != nil {
		return nil, err
	}
	m.Token = strings.TrimSpace(string(token))
	return m, nil
}

func writeFiles(files map[string][]byte) error {
	for path, data := range files {
		mode := fs.FileMode(0o600)
		if strings.HasSuffix(path, ".crt") {
			mode = 0o644
		}
		if err := os.WriteFile(path, data, mode); err != nil {
			return err
		}
	}
	return nil
}

// Kubeconfig renders a kubeconfig for the admin user.
func Kubeconfig(m *Material, server string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: shearwater
  cluster:
    server: %s
    certificate-authority-data: %s
users:
- name: shearwater-admin
  user:
    token: %s
contexts:
- name: shearwater
  context:
    cluster: shearwater
    user: shearwater-admin
current-context: shearwater
`, server, base64.StdEncoding.EncodeToString(m.CACert), m.Token)
}
