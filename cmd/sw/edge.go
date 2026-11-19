package main

// `sw edge` configures the public entry: the listener on the host's ports 80
// and 443 that forwards each host name to the Service that published it and
// obtains certificates from an ACME authority. The setting is a file in the
// config directory, so it applies however the server is started (sw up, the
// systemd service or the foreground server) and survives restarts.
import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"text/tabwriter"
	"time"

	"shearwater/internal/acme"
	"shearwater/internal/edge"
	"shearwater/internal/store"
)

type edgeConfig struct {
	Enabled bool   `json:"enabled"`
	HTTP    string `json:"http"`
	HTTPS   string `json:"https,omitempty"` // empty: plain HTTP only
	ACME    struct {
		Email     string `json:"email,omitempty"`
		AgreeTOS  bool   `json:"agreeTOS,omitempty"`
		Directory string `json:"directory,omitempty"`
		CA        string `json:"ca,omitempty"` // PEM roots for a private or test authority
	} `json:"acme"`
}

func edgeConfigPath(configDir string) string { return filepath.Join(configDir, "edge.json") }
func edgeStatusPath(dataDir string) string   { return filepath.Join(dataDir, "edge", "status.json") }

func readEdgeConfig(configDir string) (edgeConfig, error) {
	var c edgeConfig
	b, err := os.ReadFile(edgeConfigPath(configDir))
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err = json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("read %s: %w", edgeConfigPath(configDir), err)
	}
	return c, nil
}

func (c edgeConfig) validate() error {
	addrs := []string{c.HTTP}
	if c.HTTPS != "" {
		addrs = append(addrs, c.HTTPS)
	}
	for _, addr := range addrs {
		if _, _, err := net.SplitHostPort(addr); err != nil {
			return fmt.Errorf("public address %q must be [host]:port, such as :80", addr)
		}
	}
	if c.HTTP == c.HTTPS {
		return errors.New("public HTTP and HTTPS need different ports")
	}
	if c.HTTPS != "" && !c.ACME.AgreeTOS {
		return errors.New("HTTPS certificates come from an ACME authority; read its subscriber agreement and pass --agree-tos, or use --https=off")
	}
	return nil
}

func runEdge(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: sw edge enable|disable|status [flags]")
	}
	fs := flag.NewFlagSet("sw edge "+args[0], flag.ContinueOnError)
	configDir := fs.String("config-dir", defaultConfigDir, "directory with the server's configuration")
	dataDir := fs.String("data-dir", defaultDataDir, "the server's data directory")
	switch args[0] {
	case "enable":
		httpAddr := fs.String("http", ":80", "public HTTP address; ACME challenges arrive here")
		httpsAddr := fs.String("https", ":443", "public HTTPS address, or off for plain HTTP only")
		email := fs.String("email", "", "contact the certificate authority warns before expiry (optional)")
		agree := fs.Bool("agree-tos", false, "you agree to the certificate authority's subscriber agreement")
		staging := fs.Bool("staging", false, "use Let's Encrypt staging: untrusted certificates, generous rate limits")
		directory := fs.String("acme-directory", "", "another ACME directory URL")
		ca := fs.String("acme-ca", "", "PEM file of roots that sign the ACME directory's own HTTPS certificate")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		c := edgeConfig{Enabled: true, HTTP: *httpAddr, HTTPS: *httpsAddr}
		if *httpsAddr == "off" {
			c.HTTPS = ""
		} else {
			c.ACME.Email, c.ACME.AgreeTOS, c.ACME.Directory, c.ACME.CA = *email, *agree, *directory, *ca
			if *staging && c.ACME.Directory == "" {
				c.ACME.Directory = acme.LetsEncryptStaging
			}
		}
		if err := c.validate(); err != nil {
			return err
		}
		b, _ := json.MarshalIndent(c, "", "  ")
		if err := atomicPrivateFile(edgeConfigPath(*configDir), append(b, '\n')); err != nil {
			return err
		}
		fmt.Fprintf(out, "public entry saved: HTTP %s", c.HTTP)
		if c.HTTPS != "" {
			fmt.Fprintf(out, ", HTTPS %s", c.HTTPS)
		}
		fmt.Fprint(out, "\nrestart the server to apply it (sw down, then sw up; or restart the systemd service)\n"+
			"publish an app: kubectl annotate service NAME "+edge.HostAnnotation+"=app.example.com\n")
		if c.HTTPS != "" {
			fmt.Fprint(out, "the host name's DNS record must point at this server and port 80 must be reachable from the internet\n")
		}
		return nil
	case "disable":
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		c, err := readEdgeConfig(*configDir)
		if err != nil {
			return err
		}
		c.Enabled = false
		b, _ := json.MarshalIndent(c, "", "  ")
		if err = atomicPrivateFile(edgeConfigPath(*configDir), append(b, '\n')); err != nil {
			return err
		}
		fmt.Fprintln(out, "public entry disabled; restart the server to close its ports")
		return nil
	case "status":
		asJSON := fs.Bool("json", false, "print the raw status")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		c, err := readEdgeConfig(*configDir)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(edgeStatusPath(*dataDir))
		if errors.Is(err, os.ErrNotExist) {
			if c.Enabled {
				fmt.Fprintln(out, "public entry is configured but the server has not started it; restart the server")
			} else {
				fmt.Fprintln(out, "public entry is off; turn it on with: sw edge enable")
			}
			return nil
		}
		if err != nil {
			return err
		}
		if *asJSON {
			_, err = out.Write(b)
			return err
		}
		var st edge.Status
		if err = json.Unmarshal(b, &st); err != nil {
			return err
		}
		return printEdgeStatus(out, st, c.Enabled)
	}
	return fmt.Errorf("unknown sw edge command %q", args[0])
}

func printEdgeStatus(out io.Writer, st edge.Status, enabled bool) error {
	if !enabled {
		fmt.Fprintln(out, "public entry is disabled in the configuration; the state below is from its last run")
	}
	fmt.Fprintf(out, "listening: HTTP %s", st.HTTP)
	if st.HTTPS != "" {
		fmt.Fprintf(out, ", HTTPS %s", st.HTTPS)
	}
	fmt.Fprintln(out)
	if len(st.Routes) == 0 {
		fmt.Fprintf(out, "no Service is published; annotate one with %s=HOST\n", edge.HostAnnotation)
	}
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	if len(st.Routes) > 0 {
		fmt.Fprintln(w, "HOST\tSERVICE\tBACKEND\tCERTIFICATE")
	}
	for _, r := range st.Routes {
		cert := "-"
		if st.HTTPS != "" {
			cs := st.Certs[r.Host]
			switch {
			case cs.Ready && !cs.NotAfter.After(time.Now()):
				cert = "EXPIRED " + cs.NotAfter.Format("2006-01-02")
				if cs.LastError != "" {
					cert += "; renewal failed: " + cs.LastError
				}
			case cs.Ready && cs.LastError != "":
				cert = "valid until " + cs.NotAfter.Format("2006-01-02") + "; renewal failed: " + cs.LastError
			case cs.Ready:
				cert = "valid until " + cs.NotAfter.Format("2006-01-02")
			case cs.LastError != "":
				cert = "failed, next try " + cs.RetryAt.Local().Format("15:04") + ": " + cs.LastError
			default:
				cert = "pending"
			}
		}
		fmt.Fprintf(w, "%s\t%s/%s\t%s\t%s\n", r.Host, r.Namespace, r.Service, r.Backend, cert)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	sort.Strings(st.Problems)
	for _, p := range st.Problems {
		fmt.Fprintln(out, "problem:", p)
	}
	return nil
}

// prepareEdge reads the configuration and opens the public listeners, so a
// busy port or a wrong setting stops the server before it starts any work.
// It returns nil when the public entry is off.
func prepareEdge(st *store.Store, o opsOptions) (*edge.Server, error) {
	c, err := readEdgeConfig(o.ConfigDir)
	if err != nil || !c.Enabled {
		return nil, err
	}
	if err = c.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", edgeConfigPath(o.ConfigDir), err)
	}
	dir := filepath.Join(o.DataDir, "edge")
	if err = os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	srv := &edge.Server{Store: st}
	srv.OnStatus = func(s edge.Status) {
		if err := atomicJSON(edgeStatusPath(o.DataDir), s); err != nil {
			log.Printf("public entry status: %v", err)
		}
		for _, p := range s.Problems {
			log.Printf("public entry: %s", p)
		}
	}
	if srv.HTTP, err = net.Listen("tcp", c.HTTP); err != nil {
		return nil, fmt.Errorf("public HTTP: %w (change it with sw edge enable --http, or turn the entry off with sw edge disable)", err)
	}
	if c.HTTPS == "" {
		return srv, nil
	}
	if srv.HTTPS, err = net.Listen("tcp", c.HTTPS); err != nil {
		srv.HTTP.Close()
		return nil, fmt.Errorf("public HTTPS: %w (change it with sw edge enable --https, or turn the entry off with sw edge disable)", err)
	}
	key, err := edge.LoadAccountKey(filepath.Join(dir, "account.key"))
	if err == nil {
		client := &acme.Client{Directory: c.ACME.Directory, Key: key, Contact: c.ACME.Email}
		if client.Directory == "" {
			client.Directory = acme.LetsEncrypt
		}
		if c.ACME.CA != "" {
			var pemRoots []byte
			if pemRoots, err = os.ReadFile(c.ACME.CA); err == nil {
				pool := x509.NewCertPool()
				if !pool.AppendCertsFromPEM(pemRoots) {
					err = fmt.Errorf("%s holds no PEM certificate", c.ACME.CA)
				}
				client.HTTP = &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
			}
		}
		srv.Certs = &edge.Certs{Dir: filepath.Join(dir, "certs"), Issuer: client}
	}
	if err != nil {
		srv.HTTP.Close()
		srv.HTTPS.Close()
		return nil, fmt.Errorf("public entry: %w", err)
	}
	return srv, nil
}

func runPreparedEdge(ctx context.Context, srv *edge.Server, done chan<- struct{}) {
	defer close(done)
	if srv == nil {
		return
	}
	https := "off"
	if srv.HTTPS != nil {
		https = srv.HTTPS.Addr().String()
	}
	log.Printf("public entry listening: HTTP %s, HTTPS %s", srv.HTTP.Addr(), https)
	if err := srv.Run(ctx); err != nil {
		log.Printf("public entry stopped: %v", err)
	}
}
