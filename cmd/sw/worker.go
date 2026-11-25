package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"shearwater/internal/api"
	"shearwater/internal/drain"
	"shearwater/internal/node"
	"shearwater/internal/nodeapi"
	"shearwater/internal/proxy"
	"shearwater/internal/runtime"
)

// A worker node keeps what joining gave it in its config directory
// (docs/design-multinode-tenancy.md 3.2).
const nodeConfigFile = "node.json"

type nodeConfig struct {
	Server      string    `json:"server"`
	NodeName    string    `json:"nodeName"`
	NodeIP      string    `json:"nodeIP"`
	PodCIDR     string    `json:"podCIDR"`
	ClusterCIDR string    `json:"clusterCIDR"`
	ServiceCIDR string    `json:"serviceCIDR"`
	Joined      time.Time `json:"joined"`
}

func isWorker(configDir string) bool {
	_, err := os.Stat(filepath.Join(configDir, nodeConfigFile))
	return err == nil
}

type workerFiles struct {
	cfg                  nodeConfig
	token                string
	ca                   []byte
	cert                 tls.Certificate
	podCIDR, cluster, sv netip.Prefix
	nodeIP               netip.Addr
}

func readWorkerFiles(dir string) (workerFiles, error) {
	var f workerFiles
	if err := readPrivateJSON(filepath.Join(dir, nodeConfigFile), &f.cfg); err != nil {
		return f, err
	}
	tok, err := os.ReadFile(filepath.Join(dir, "node.token"))
	if err != nil {
		return f, err
	}
	f.token = strings.TrimSpace(string(tok))
	if f.ca, err = os.ReadFile(filepath.Join(dir, "ca.crt")); err != nil {
		return f, err
	}
	if f.cert, err = tls.LoadX509KeyPair(filepath.Join(dir, "node.crt"), filepath.Join(dir, "node.key")); err != nil {
		return f, err
	}
	var errs []error
	var e error
	f.podCIDR, e = netip.ParsePrefix(f.cfg.PodCIDR)
	errs = append(errs, e)
	f.cluster, e = netip.ParsePrefix(f.cfg.ClusterCIDR)
	errs = append(errs, e)
	f.sv, e = netip.ParsePrefix(f.cfg.ServiceCIDR)
	errs = append(errs, e)
	f.nodeIP, e = netip.ParseAddr(f.cfg.NodeIP)
	errs = append(errs, e)
	if err := errors.Join(errs...); err != nil {
		return f, fmt.Errorf("%s is damaged: %w", filepath.Join(dir, nodeConfigFile), err)
	}
	return f, nil
}

// runWorker runs this server as a worker node: node agent, service proxy,
// cluster DNS and the overlay, with the cluster state read from the control
// plane. There is no database and no Kubernetes API here; the local listener
// only answers health checks for sw up and sw status.
// workerExtras lets a standby control plane run beside the node agent
// (docs/design-agent-first-ha.md 3.2): it is a worker of the leader that also
// keeps a replica and passes API connections to the leader.
type workerExtras struct {
	// Sidecar runs for the worker's lifetime with its control plane client.
	Sidecar func(ctx context.Context, client *nodeapi.Client)
	// Local serves o.Listen instead of the local health endpoints.
	Local func(ctx context.Context, ln net.Listener) error
	// Role is reported in the start line ("worker" when empty).
	Role string
	// FilesDir holds node.json and the node credentials (default
	// o.ConfigDir; a control plane keeps its own in a subdirectory).
	FilesDir string
}

func runWorker(o opsOptions, instance string) error {
	return runWorkerWith(o, instance, workerExtras{})
}

func runWorkerWith(o opsOptions, instance string, x workerExtras) error {
	dir := o.ConfigDir
	if x.FilesDir != "" {
		dir = x.FilesDir
	}
	f, err := readWorkerFiles(dir)
	if err != nil {
		return fmt.Errorf("this server has not joined a cluster: %w", err)
	}
	if os.Geteuid() != 0 {
		return needRoot("run the node")
	}
	if instance != "" {
		if len(instance) != 32 {
			return fmt.Errorf("invalid internal instance identifier")
		}
		if err := privateDir(o.RunDir, false); err != nil {
			return err
		}
		lock, err := fileLock(o.RunDir, "instance.lock")
		if err != nil {
			return err
		}
		defer lock.Close()
	}
	lock, err := lockNode()
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := checkNodeOwner(o); err != nil {
		return err
	}
	for _, dir := range []string{o.DataDir, o.RunDir, o.LogDir} {
		if err := privateDir(dir, true); err != nil {
			return err
		}
	}
	client, err := nodeapi.NewClient(f.cfg.Server, f.token, f.ca, f.podCIDR)
	if err != nil {
		return err
	}
	logf := func(prefix string) func(string, ...any) {
		return func(format string, args ...any) { fmt.Fprintf(os.Stderr, prefix+format+"\n", args...) }
	}
	client.Logf = logf("control plane: ")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	rt := runtime.NewContainerd(o.Socket)
	defer rt.Close()
	n := runtime.NewNet(o.RunDir, f.podCIDR, f.sv)
	n.Overlay, n.ClusterCIDR = true, f.cluster
	prepare, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := n.CheckHostRanges(prepare); err != nil {
		return err
	}
	prior, err := readNodeOwner()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := atomicJSON(filepath.Join(defaultRunDir, ownerFile), nodeOwnership{o, instance != "" || prior.Managed}); err != nil {
		return err
	}
	if err := n.EnsureHost(); err != nil {
		return err
	}

	draining := &drain.State{}
	forwarding := &proxy.KernelManager{Manager: proxy.Manager{Store: client},
		DataPath: &runtime.KernelServices{PodCIDR: f.podCIDR, ServiceCIDR: f.sv, ClusterCIDR: f.cluster}, Drain: draining}
	agent := &node.Agent{Store: client, Runtime: rt, Net: n, RetirePod: forwarding.RetirePod,
		NodeName: f.cfg.NodeName, NodeIP: f.cfg.NodeIP, DataDir: o.DataDir, LogDir: o.LogDir, PodCIDR: f.podCIDR,
		Version: api.GitVersion}
	overlay := &node.Overlay{Store: client, NodeName: f.cfg.NodeName, NodeIP: f.nodeIP, PodCIDR: f.podCIDR, Logf: logf("overlay: ")}
	logs := &node.LogServer{Agent: agent, Cert: f.cert, CA: f.ca, Addr: net.JoinHostPort(f.cfg.NodeIP, fmt.Sprint(node.LogPort))}

	var wg sync.WaitGroup
	start := func(fn func(context.Context)) {
		wg.Add(1)
		go func() { defer wg.Done(); fn(ctx) }()
	}
	start(client.Run)
	start(agent.Run)
	start(forwarding.Run)
	start(overlay.Run)
	start(func(ctx context.Context) {
		if err := logs.Run(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "node logs: %v\n", err)
		}
	})
	gateway := n.Gateway()
	start(func(ctx context.Context) { runNodeNetworkServices(ctx, client, gateway, logf("network services: ")) })
	start(func(ctx context.Context) { runNodeNetworkPolicy(ctx, client, f.podCIDR, logf("network policy: ")) })
	if x.Sidecar != nil {
		start(func(ctx context.Context) { x.Sidecar(ctx, client) })
	}

	// Local health for sw up and sw status: ready while the control plane answers.
	ln, err := net.Listen("tcp", o.Listen)
	if err != nil {
		stop()
		wg.Wait()
		return fmt.Errorf("%w (is another sw running? `--listen` picks another address)", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if last := client.LastContact(); last.IsZero() || time.Since(last) > 30*time.Second {
			http.Error(w, "the control plane at "+f.cfg.Server+" has not answered", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"gitVersion": api.GitVersion, "role": "worker",
			"node": f.cfg.NodeName, "controlPlane": f.cfg.Server})
	})
	hs := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second,
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{f.cert}, MinVersion: tls.VersionTLS12}}
	served := make(chan error, 1)
	if x.Local != nil {
		go func() { served <- x.Local(ctx, ln) }()
	} else {
		go func() { served <- hs.ServeTLS(ln, "", "") }()
	}
	if instance != "" {
		if err := writeState(o, instance); err != nil {
			stop()
			hs.Close()
			wg.Wait()
			return err
		}
	}
	role := x.Role
	if role == "" {
		role = "worker"
	}
	fmt.Fprintf(os.Stderr, "sw %s %s: node %s (pods %s) of %s, local address https://%s\n",
		role, api.Version, f.cfg.NodeName, f.podCIDR, f.cfg.Server, ln.Addr())
	select {
	case err = <-served:
	case <-ctx.Done():
	}
	stop()
	shutdown, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelShutdown()
	hs.Shutdown(shutdown)
	// Containers keep running; the next start picks them up.
	wg.Wait()
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	fmt.Fprintln(os.Stderr, "sw worker stopped")
	return nil
}

// workerReady asks the local worker whether it reaches the control plane.
func workerReady(ctx context.Context, o opsOptions) error {
	ca, err := os.ReadFile(filepath.Join(o.ConfigDir, "ca.crt"))
	if err != nil {
		return err
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca)
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+o.Listen+"/readyz", nil)
	if err != nil {
		return err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("worker not ready: %s", resp.Status)
	}
	return nil
}

func printWorker(o opsOptions, state string) error {
	f, err := readWorkerFiles(o.ConfigDir)
	if err != nil {
		return err
	}
	fmt.Printf("sw worker %s: node %s (pods %s) of control plane %s\n"+
		"use kubectl against the control plane; on it: kubectl get nodes -o wide\n", state, f.cfg.NodeName, f.cfg.PodCIDR, f.cfg.Server)
	return nil
}
