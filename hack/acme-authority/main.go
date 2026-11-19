// Command acme-authority runs the in-process test ACME authority as a
// standalone program, for end-to-end checks of the public entry on a private
// test network. It is not a certificate authority for real use: its root is
// created at start and trusted by nothing.
//
// Every domain is validated by connecting to --validate-at (as if DNS pointed
// there) and sending the domain in the Host header.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"shearwater/internal/acme/acmetest"
)

func main() {
	out := flag.String("out", "", "directory for directory-url, root.pem and log.json")
	validateAt := flag.String("validate-at", "127.0.0.1:80", "address every ordered domain resolves to")
	flag.Parse()
	if *out == "" {
		fmt.Fprintln(os.Stderr, "acme-authority: --out is required")
		os.Exit(2)
	}
	a := acmetest.New()
	defer a.Close()
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DisableKeepAlives: true, Proxy: nil}}
	a.Fetch = func(domain, path string) (string, error) {
		req, err := http.NewRequest(http.MethodGet, "http://"+*validateAt+path, nil)
		if err != nil {
			return "", err
		}
		req.Host = domain
		resp, err := client.Do(req)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("HTTP %d from %s", resp.StatusCode, net.JoinHostPort(domain, "80"))
		}
		return string(body), nil
	}
	write := func(name string, data []byte) {
		if err := os.WriteFile(filepath.Join(*out, name), data, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "acme-authority:", err)
			os.Exit(1)
		}
	}
	write("root.pem", a.RootPEM())
	write("directory-url", []byte(a.Directory()))
	fmt.Println("acme-authority: test directory at", a.Directory())
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	tick := time.NewTicker(time.Second)
	for {
		issued, rejected := a.Log()
		b, _ := json.Marshal(map[string]any{"issued": issued, "rejected": rejected})
		write("log.json", b)
		select {
		case <-stop:
			return
		case <-tick.C:
		}
	}
}
