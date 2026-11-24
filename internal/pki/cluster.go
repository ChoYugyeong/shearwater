package pki

// Certificates for a cluster of several servers (docs/design-multinode-tenancy.md 3.2).

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// ServerName is the common name of the API server certificate. A worker
// checks it, besides the cluster CA, because the CA also signs node serving
// certificates that must not be able to stand in for the control plane.
const ServerName = "shearwater-server"

// NodeName is the common name prefix of a node's serving certificate.
const NodeName = "system:node:"

const nodeCertFor = 365 * 24 * time.Hour

// CAHash returns the hash of a CA certificate's public key in the form
// kubeadm uses for --discovery-token-ca-cert-hash: "sha256:<hex>".
func CAHash(caPEM []byte) (string, error) {
	block, _ := pem.Decode(caPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return "", errors.New("no PEM certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func loadCA(dir string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	ca, err := tls.LoadX509KeyPair(filepath.Join(dir, "ca.crt"), filepath.Join(dir, "ca.key"))
	if err != nil {
		return nil, nil, fmt.Errorf("load CA from %s: %w", dir, err)
	}
	cert, err := x509.ParseCertificate(ca.Certificate[0])
	if err != nil {
		return nil, nil, err
	}
	key, ok := ca.PrivateKey.(*ecdsa.PrivateKey)
	if !ok {
		return nil, nil, errors.New("the CA key is not an ECDSA key")
	}
	return cert, key, nil
}

// EnsureServerAddresses reissues the API server certificate when it does not
// cover one of ips, so kubectl on another machine can reach the server by that
// address. The CA and the token stay as they are.
func EnsureServerAddresses(dir string, ips []net.IP) error {
	cur, err := tls.LoadX509KeyPair(filepath.Join(dir, "server.crt"), filepath.Join(dir, "server.key"))
	if err != nil {
		return err
	}
	leaf, err := x509.ParseCertificate(cur.Certificate[0])
	if err != nil {
		return err
	}
	want := []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback}
	missing := false
	for _, ip := range ips {
		if !slices.ContainsFunc(want, ip.Equal) {
			want = append(want, ip)
		}
		if !slices.ContainsFunc(leaf.IPAddresses, ip.Equal) {
			missing = true
		}
	}
	if !missing {
		return nil
	}
	caCert, caKey, err := loadCA(dir)
	if err != nil {
		return err
	}
	certPEM, keyPEM, _, err := newCert(&x509.Certificate{
		Subject:     pkix.Name{CommonName: ServerName},
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:    []string{"localhost"},
		IPAddresses: want,
	}, caCert, caKey)
	if err != nil {
		return err
	}
	return writeFiles(map[string][]byte{filepath.Join(dir, "server.crt"): certPEM, filepath.Join(dir, "server.key"): keyPEM})
}

// SignNodeServing signs a node's certificate request for its serving
// certificate (the log endpoint the control plane calls). The name and the
// addresses come from the control plane, not from the request.
func SignNodeServing(dir string, csrPEM []byte, node string, ips []net.IP) ([]byte, error) {
	block, _ := pem.Decode(csrPEM)
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return nil, errors.New("no PEM certificate request")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, err
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("certificate request signature: %w", err)
	}
	caCert, caKey, err := loadCA(dir)
	if err != nil {
		return nil, err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: NodeName + node},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(nodeCertFor),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  ips,
		DNSNames:     []string{node},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, csr.PublicKey, caKey)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}

// NewNodeKey makes a node's private key and its certificate request.
func NewNodeKey(node string) (keyPEM, csrPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: NodeName + node}}, key)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}), nil
}

// PinnedTLS returns a client TLS configuration that trusts only certificates
// signed by caPEM whose common name is want (ServerName, or NodeName+node).
// Host names are not checked: servers are reached by address, and the pinned
// private CA together with the expected name is what identifies them.
func PinnedTLS(caPEM []byte, want string) (*tls.Config, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("no CA certificate")
	}
	return &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true, // replaced by VerifyPeerCertificate below
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 {
				return errors.New("the server sent no certificate")
			}
			leaf, err := x509.ParseCertificate(raw[0])
			if err != nil {
				return err
			}
			inter := x509.NewCertPool()
			for _, der := range raw[1:] {
				if c, err := x509.ParseCertificate(der); err == nil {
					inter.AddCert(c)
				}
			}
			if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, Intermediates: inter,
				KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
				return err
			}
			if leaf.Subject.CommonName != want {
				return fmt.Errorf("the server certificate is for %q, not %q", leaf.Subject.CommonName, want)
			}
			return nil
		},
	}, nil
}

// IsNodeCert reports whether a verified certificate names a node.
func IsNodeCert(cn string) (string, bool) { return strings.CutPrefix(cn, NodeName) }
