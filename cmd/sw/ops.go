package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"shearwater/internal/apis"
)

type operationError struct {
	code, retry, detail, hint string
}

func (e *operationError) Error() string {
	return fmt.Sprintf("%s retry=%s: %s\nhint: %s", e.code, e.retry, e.detail, e.hint)
}

func opsError(code, retry, detail, hint string) error {
	return &operationError{code, retry, detail, hint}
}

type opsOptions struct {
	Listen, DataDir, ConfigDir, RunDir, LogDir, Socket, NodeName, Runtime string
}

func (o *opsOptions) flags(fs *flag.FlagSet) {
	fs.StringVar(&o.Listen, "listen", defaultListen, "local HTTPS host:port (127.0.0.1, localhost, or ::1)")
	fs.StringVar(&o.DataDir, "data-dir", defaultDataDir, "persistent SQLite and Pod data directory (Linux filesystem)")
	fs.StringVar(&o.ConfigDir, "config-dir", defaultConfigDir, "TLS material and kubeconfig directory")
	fs.StringVar(&o.RunDir, "run-dir", defaultRunDir, "temporary server state and Pod network namespace directory")
	fs.StringVar(&o.LogDir, "log-dir", defaultLogPath(defaultLogDir, defaultDataDir), "server and container logs directory")
	fs.StringVar(&o.Socket, "containerd-socket", defaultSocket, "existing containerd socket")
	fs.StringVar(&o.NodeName, "node-name", "", "node name (default: hostname)")
	fs.StringVar(&o.Runtime, "runtime", "containerd", "containerd or none (API only)")
}

func parseOps(fs *flag.FlagSet, args []string, o *opsOptions) error {
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return opsError("INVALID", "never", "unexpected positional arguments", "run sw "+fs.Name()+" -h")
	}
	return o.normalize()
}

func (o *opsOptions) normalize() error {
	if err := localListen(o.Listen); err != nil {
		return err
	}
	if _, port, _ := net.SplitHostPort(o.Listen); strings.HasPrefix(o.Listen, "localhost:") {
		o.Listen = net.JoinHostPort("127.0.0.1", port)
	}
	paths := []*string{&o.DataDir, &o.ConfigDir, &o.RunDir, &o.LogDir, &o.Socket}
	for _, path := range paths {
		// Require explicit paths so the same flags still identify the same
		// server when an operator runs down from a different directory.
		if !filepath.IsAbs(*path) || filepath.Clean(*path) == "/" {
			return opsError("INVALID", "never", fmt.Sprintf("%q is not an absolute instance path", *path), "use absolute Linux paths such as /var/lib/shearwater")
		}
		*path = filepath.Clean(*path)
	}
	if o.Runtime != "none" && o.Runtime != "containerd" {
		return opsError("INVALID", "never", "runtime must be containerd or none", "use --runtime=none for an API-only development server")
	}
	if o.NodeName == "" {
		name, err := os.Hostname()
		if err != nil {
			return err
		}
		o.NodeName = strings.ToLower(name)
	}
	if o.Runtime == "containerd" && !apis.IsDNS1123Subdomain(o.NodeName) {
		return opsError("INVALID", "never", "invalid node name "+o.NodeName, "pass --node-name with lowercase letters, digits, dots or hyphens")
	}
	return nil
}

// Only names and addresses on the server certificate are allowed here. In
// particular, an operator typo must never send the admin token over the LAN.
func localListen(listen string) error {
	host, port, err := net.SplitHostPort(listen)
	n, numErr := strconv.Atoi(port)
	if err != nil || numErr != nil || n < 1 || n > 65535 || (host != "127.0.0.1" && host != "localhost" && host != "::1") {
		return opsError("INVALID", "never", fmt.Sprintf("not a supported local HTTPS address: %q", listen), "use --listen=127.0.0.1:6443 or another local port")
	}
	return nil
}

type agentClient struct {
	client *http.Client
	url    string
	token  string
}

func localAPI(configDir, listen string) (*agentClient, error) {
	if err := localListen(listen); err != nil {
		return nil, err
	}
	ca, err := os.ReadFile(filepath.Join(configDir, "ca.crt"))
	if err != nil {
		return nil, localConfigError(configDir, err)
	}
	token, err := os.ReadFile(filepath.Join(configDir, "admin.token"))
	if err != nil {
		return nil, localConfigError(configDir, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) || strings.TrimSpace(string(token)) == "" {
		return nil, opsError("INVALID", "never", "invalid CA or empty admin token in "+configDir, "check the instance's certificate and token files")
	}
	transport := &http.Transport{
		// The API is local. Ignore environment proxies and connect to an
		// actual loopback address even if /etc/hosts changes localhost.
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			d := net.Dialer{Timeout: time.Second}
			if host == "localhost" {
				conn, err := d.DialContext(ctx, network, net.JoinHostPort("127.0.0.1", port))
				if err == nil {
					return conn, nil
				}
				address = net.JoinHostPort("::1", port)
			}
			return d.DialContext(ctx, network, address)
		},
		TLSClientConfig:   &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		DisableKeepAlives: true,
	}
	return &agentClient{
		client: &http.Client{Transport: transport, Timeout: 3 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		url: "https://" + listen, token: strings.TrimSpace(string(token)),
	}, nil
}

func localConfigError(dir string, err error) error {
	code, hint := "INVALID", "check --config-dir and run as the same user as sw up"
	if errors.Is(err, os.ErrNotExist) {
		code, hint = "NOT_RUNNING", "run sw up or pass the active instance's --config-dir"
	}
	return opsError(code, "never", fmt.Sprintf("cannot read client credentials in %s: %v", dir, err), hint)
}

func (c *agentClient) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, "GET", c.url+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	res, err := c.client.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return opsError("TIMEOUT", "safe", "no reply from "+c.url, "check sw doctor and the server log")
		}
		var verifyErr *tls.CertificateVerificationError
		if errors.As(err, &verifyErr) {
			return opsError("UNAUTHORIZED", "never", "server TLS identity does not match "+c.url, "check --listen and --config-dir")
		}
		return opsError("NOT_RUNNING", "never", fmt.Sprintf("cannot reach %s: %v", c.url, err), "run sw up or check --listen and sw doctor")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		code, retry := "RUNTIME_ERROR", "safe"
		if res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden {
			code, retry = "UNAUTHORIZED", "never"
		}
		return opsError(code, retry, fmt.Sprintf("%s replied HTTP %d to %s", c.url, res.StatusCode, path), "check sw doctor and the instance credentials")
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(res.Body, 32<<20)).Decode(out)
}
