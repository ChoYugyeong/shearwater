package main

// `sw compose serve` puts Shearwater's operation protocol in front of one
// existing Docker Compose project. The operator keeps docker access and the
// admin token; an AI agent receives only a `sw task` delegation for one service
// and must still stop for the operator's approval of each exact plan.
import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"shearwater/internal/api"
	"shearwater/internal/apis"
	"shearwater/internal/compose"
	"shearwater/internal/operation"
	"shearwater/internal/pki"
	"shearwater/internal/store"
)

type fileList []string

func (f *fileList) String() string     { return strings.Join(*f, ",") }
func (f *fileList) Set(v string) error { *f = append(*f, v); return nil }

func runCompose(args []string, out io.Writer) error {
	if len(args) == 0 || args[0] != "serve" {
		return fmt.Errorf("usage: sw compose serve --file compose.yaml --project NAME [flags]")
	}
	fs := flag.NewFlagSet("compose serve", flag.ContinueOnError)
	var files fileList
	fs.Var(&files, "file", "compose file; repeat in the same order as docker compose -f")
	project := fs.String("project", "", "compose project name; delegations use it as the namespace")
	home, _ := os.UserHomeDir()
	configDir := fs.String("config-dir", filepath.Join(home, ".config", "shearwater-compose"), "operator credentials: CA, server certificate and admin token")
	dataDir := fs.String("data-dir", filepath.Join(home, ".local", "state", "shearwater-compose"), "operation ledger and override files")
	listen := fs.String("listen", "127.0.0.1:16444", "local HTTPS address for sw task and agents")
	socket := fs.String("docker-socket", "/var/run/docker.sock", "Docker Engine API socket")
	command := fs.String("compose-command", "docker compose", "compose command to run")
	timeout := fs.Duration("up-timeout", 5*time.Minute, "limit for one docker compose up, including an image pull")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if len(files) == 0 || !apis.IsDNS1123Label(*project) {
		return fmt.Errorf("give at least one --file and a --project that is a lowercase DNS label")
	}
	if err := localListen(*listen); err != nil {
		return err
	}
	for i, f := range files {
		abs, err := filepath.Abs(f)
		if err != nil {
			return err
		}
		if _, err = os.Stat(abs); err != nil {
			return err
		}
		files[i] = abs
	}
	parts := strings.Fields(*command)
	if len(parts) == 0 {
		return fmt.Errorf("--compose-command is empty")
	}
	for _, dir := range []string{*configDir, *dataDir} {
		if err := privateDir(dir, true); err != nil {
			return err
		}
	}
	lock, err := fileLock(*dataDir, "ledger.lock")
	if err != nil {
		return err
	}
	defer lock.Close()
	st, err := store.Open(filepath.Join(*dataDir, "ledger.db"))
	if err != nil {
		return err
	}
	defer st.Close()
	if err = st.InitOperations(); err != nil {
		return err
	}
	mat, err := pki.Ensure(*configDir)
	if err != nil {
		return err
	}
	backend := &compose.Backend{
		Project:  *project,
		Files:    files,
		StateDir: filepath.Join(*dataDir, "compose"),
		Docker:   compose.NewEngine(*socket),
		Run:      compose.CLI(parts, filepath.Dir(files[0]), []string{"DOCKER_HOST=unix://" + *socket}),
		Timeout:  *timeout,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	check, cancel := context.WithTimeout(ctx, 30*time.Second)
	services, err := backend.Check(check)
	cancel()
	if err != nil {
		return fmt.Errorf("read compose project: %w", err)
	}
	srv := &api.Server{Store: st, Token: mat.Token, Operations: backend}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return fmt.Errorf("%w (is another sw compose serve running? --listen picks another address)", err)
	}
	defer ln.Close()
	hs := &http.Server{
		Handler:           operationsOnly(srv),
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{mat.Server}, MinVersion: tls.VersionTLS12},
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	fmt.Fprintf(out, "sw compose serve: project %s (services: %s) on https://%s\n", *project, strings.Join(services, ", "), *listen)
	fmt.Fprintf(out, "operator: sw task create --config %s --listen %s --namespace %s --name SERVICE ...\n", *configDir, *listen, *project)
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		hs.Shutdown(shutdown)
	}()
	if err = hs.ServeTLS(ln, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// operationsOnly exposes the operation API and health checks. This server
// has no Kubernetes objects, so every other path is refused.
// composeLeasePath lets sw lease list and release reach the app leases that
// delegated operations keep in the Compose ledger: listing a namespace's
// leases, and reading or replacing one app lease. Nothing else of the
// Kubernetes API is served.
func composeLeasePath(method, p string) bool {
	rest, ok := strings.CutPrefix(p, "/apis/coordination.k8s.io/v1/namespaces/")
	if !ok {
		return false
	}
	parts := strings.Split(rest, "/")
	switch {
	case len(parts) == 2 && parts[0] != "" && parts[1] == "leases":
		return method == http.MethodGet
	case len(parts) == 3 && parts[0] != "" && parts[1] == "leases" && strings.HasPrefix(parts[2], "shearwater-app-"):
		return method == http.MethodGet || method == http.MethodPut
	}
	return false
}

func operationsOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p == "/healthz" || p == "/readyz" || p == "/livez" || p == operation.Prefix || strings.HasPrefix(p, operation.Prefix+"/") ||
			composeLeasePath(r.Method, p) {
			next.ServeHTTP(w, r)
			return
		}
		http.Error(w, `{"code":"NOT_FOUND","message":"sw compose serve only serves `+operation.Prefix+`"}`, http.StatusNotFound)
	})
}
