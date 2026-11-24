// sw is the single Shearwater binary.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
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

	"shearwater/internal/admission"
	"shearwater/internal/api"
	"shearwater/internal/apis"
	"shearwater/internal/auth"
	"shearwater/internal/controller"
	"shearwater/internal/drain"
	"shearwater/internal/node"
	"shearwater/internal/pki"
	"shearwater/internal/proxy"
	"shearwater/internal/runtime"
	"shearwater/internal/store"
)

const usage = `sw - Shearwater ` + api.Version + `

  sw up          check the host and start the server in the background
  sw down        stop the server; --all also stops Pods and removes its network
  sw status      show server, node and Pod health
  sw tree        show how workloads, Pods and Services are related
  sw doctor      diagnose the host and the local server
  sw get         list resources or inspect TYPE/NAME (--full for JSON)
  sw logs        read current or previous Pod container logs
  sw events      show recent events, optionally for one object's UID
  sw why         explain a Pod's state with events and bounded log excerpts
  sw observe     read bounded, structured workload facts for agents
  sw verify      check readiness of an exact resource UID and generation
  sw capabilities show supported agent operations and their limits
  sw server      run the API server in the foreground
  sw kubeconfig  print a kubeconfig for kubectl
  sw node        print a join command, list or remove the servers of this cluster
  sw join        make this server a worker of a cluster (run the command sw node token prints)
  sw user        add, list, remove users or rotate their tokens
  sw version     print the version
  sw service     render/install/manage an automatic systemd service
  sw backup      snapshot the database and credentials, optionally cold volumes
  sw restore     verify a backup and restore into a new directory
  sw plan        prepare a reviewed deployment plan
  sw app-plan    plan creating a web app, changing its image, or scaling it
  sw approve     approve the exact plan hash locally
  sw apply       execute an approved plan, checking for concurrent changes
  sw mcp         serve diagnostics and optional reviewed deployment over stdio
  sw chat        talk through the shared tools using Qwen/Ollama or DeepSeek
  sw connect     check an app connection and create its private AI workflow
  sw setup       check/start a local server and prepare a normal user's credentials
  sw workflow    review/approve/resume the saved app workflow without copying connection flags
  sw task        delegate one app, approve server plans, inspect evidence, or resume with a new token
  sw compose     serve the same delegated operations for an existing Docker Compose project
  sw edge        publish Services on the host's ports 80/443 by host name with automatic HTTPS

Run "sw COMMAND -h" for the flags of a command.
`

const (
	defaultListen    = "127.0.0.1:6443"
	defaultDataDir   = "/var/lib/shearwater"
	defaultConfigDir = "/etc/shearwater"
	defaultLogDir    = "/var/log/shearwater"
	defaultRunDir    = "/run/shearwater"
	defaultSocket    = "/run/containerd/containerd.sock"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch cmd, args := os.Args[1], os.Args[2:]; cmd {
	case "up":
		err = runUp(args)
	case "supervise":
		err = runSupervise(args)
	case "service":
		err = runService(args, os.Stdout)
	case "backup":
		err = runBackup(args, os.Stdout)
	case "restore":
		err = runRestore(args, os.Stdout)
	case "plan", "approve", "apply":
		err = runPlanCommand(cmd, args, os.Stdout)
	case "app-plan":
		err = runAppPlan(args, os.Stdout)
	case "mcp":
		err = runMCP(args, os.Stdin, os.Stdout)
	case "chat":
		err = runChat(args, os.Stdin, os.Stdout)
	case "connect":
		err = runConnect(args, os.Stdout)
	case "setup":
		err = runSetup(args, os.Stdout)
	case "receive-client":
		err = receiveClient(args, os.Stdin, os.Stdout)
	case "workflow":
		err = runWorkflow(args, os.Stdout)
	case "task":
		err = runTask(args, os.Stdout)
	case "compose":
		err = runCompose(args, os.Stdout)
	case "edge":
		err = runEdge(args, os.Stdout)
	case "down":
		err = runDown(args)
	case "status":
		err = runStatus(args)
	case "tree":
		err = runTree(args)
	case "doctor":
		err = runDoctor(args)
	case "get", "logs", "events", "why":
		err = runDiagnostic(cmd, args, os.Stdout)
	case "observe", "verify", "capabilities":
		err = runObserveCommand(cmd, args, os.Stdout)
	case "server":
		err = runServer(args)
	case "kubeconfig":
		err = runKubeconfig(args)
	case "node":
		err = runNode(args)
	case "join":
		err = runJoin(args)
	case "user":
		err = runUser(args)
	case "version", "--version":
		fmt.Println("sw " + api.Version + " (Kubernetes API " + api.GitVersion + ")")
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "sw: unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil && !errors.Is(err, flag.ErrHelp) {
		fmt.Fprintln(os.Stderr, "sw:", err)
		os.Exit(1)
	}
}

func runServer(args []string) error {
	fs := flag.NewFlagSet("sw server", flag.ExitOnError)
	listen := fs.String("listen", defaultListen, "address to serve HTTPS on")
	dataDir := fs.String("data-dir", defaultDataDir, "directory for the database (must be a local filesystem, not /mnt/c)")
	configDir := fs.String("config-dir", defaultConfigDir, "directory for certificates and the admin token")
	trace := fs.String("trace", "", "append every request to this file as JSON lines (development aid)")
	rtFlag := fs.String("runtime", "auto", "container runtime: containerd, none (API only), or auto (containerd when running as root)")
	socket := fs.String("containerd-socket", defaultSocket, "containerd's socket")
	logDir := fs.String("log-dir", defaultLogPath(defaultLogDir, defaultDataDir), "directory for container logs")
	runDir := fs.String("run-dir", defaultRunDir, "directory for pod network namespaces (a tmpfs, emptied by a reboot)")
	nodeName := fs.String("node-name", "", "name of this node (default: the hostname)")
	instance := fs.String("managed", "", "internal instance identifier set by sw up")
	worker := fs.Bool("worker", false, "run as a worker node of the cluster this server joined (sw up sets it)")
	advertise := fs.String("advertise-address", "", "IPv4 address other servers and remote kubectl reach this control plane at (default: the one sw up remembered)")
	fs.Parse(args)

	withAgent := false
	switch *rtFlag {
	case "containerd":
		withAgent = true
	case "none":
	case "auto":
		if withAgent = os.Geteuid() == 0; !withAgent {
			fmt.Fprintln(os.Stderr, "sw server: not running as root, so there is no node agent: pods are stored but not started")
		}
	default:
		return fmt.Errorf("--runtime must be containerd, none or auto, got %q", *rtFlag)
	}
	if *nodeName == "" {
		h, err := os.Hostname()
		if err != nil {
			return fmt.Errorf("no hostname, pass --node-name: %w", err)
		}
		*nodeName = strings.ToLower(h)
	}
	if withAgent && !apis.IsDNS1123Subdomain(*nodeName) {
		return fmt.Errorf("node name %q is not a valid name (lowercase letters, digits, '-' and '.'); pass --node-name", *nodeName)
	}
	o := opsOptions{Listen: *listen, DataDir: *dataDir, ConfigDir: *configDir, RunDir: *runDir, LogDir: *logDir,
		Socket: *socket, NodeName: *nodeName, Runtime: "none"}
	if withAgent {
		o.Runtime = "containerd"
	}
	if *instance != "" || withAgent {
		if err := o.normalize(); err != nil {
			return err
		}
		*listen = o.Listen
	}
	if *worker {
		if !withAgent {
			return fmt.Errorf("a worker runs containers: drop --runtime=none")
		}
		return runWorker(o, *instance)
	}
	if *advertise == "" {
		*advertise = readClusterConfig(*configDir).AdvertiseAddress
	}
	var advertiseIP netip.Addr
	if *advertise != "" {
		a, err := parseAdvertise(*advertise)
		if err != nil {
			return err
		}
		advertiseIP = a
	}
	if *instance != "" {
		if len(*instance) != 32 {
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
	if withAgent {
		if os.Geteuid() != 0 {
			return needRoot("run the node")
		}
		// All root invocations share the containerd namespace and the host
		// bridge. This also excludes a concurrent sw down --all.
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
	}
	if *instance != "" || withAgent {
		lock, err := lockConfig(o)
		if err != nil {
			return err
		}
		defer lock.Close()
	}

	if err := os.MkdirAll(*dataDir, 0o700); err != nil {
		return err
	}
	dbLock, err := fileLock(*dataDir, "database.lock")
	if err != nil {
		return err
	}
	defer dbLock.Close()
	st, err := store.Open(filepath.Join(*dataDir, "state.db"))
	if err != nil {
		return err
	}
	defer st.Close()
	mat, err := pki.Ensure(*configDir)
	if err != nil {
		return err
	}
	if advertiseIP.IsValid() {
		// Remote kubectl reaches the server by this address.
		if err := pki.EnsureServerAddresses(*configDir, []net.IP{net.IP(advertiseIP.AsSlice())}); err != nil {
			return err
		}
		if mat, err = pki.Load(*configDir); err != nil {
			return err
		}
	}
	if *instance != "" {
		// Don't advertise a ready managed server if its kubectl credentials
		// cannot be published (for example, the filesystem is full).
		if err := writeKubeconfig(o, mat); err != nil {
			return err
		}
	}
	if err := st.InitCredentials(); err != nil {
		return err
	}
	adminMux := http.NewServeMux()
	srv := &api.Server{Store: st, Token: mat.Token, Admin: adminMux,
		Auth: &auth.Authenticator{AdminToken: mat.Token, Credentials: st}}
	type loop struct {
		name string
		fn   func(context.Context)
	}
	var loops []loop
	wired := &wiring{Server: srv, Store: st, Admin: adminMux, ConfigDir: *configDir, DataDir: *dataDir,
		Go: func(name string, fn func(context.Context)) { loops = append(loops, loop{name, fn}) }}
	if withAgent {
		wired.NodeName = *nodeName
	}
	volumeFilter, volumeAdmit := wireVolumes(wired)
	plugins := append(wireAccess(wired), wireNetworkServices(wired)...)
	srv.Admit = admission.Chain(append(plugins, volumeAdmit...)...)
	podRes := apis.Lookup("", "v1", "pods")
	controllerID := auth.Identity{Name: "system:serviceaccount:kube-system:replicaset-controller", Kind: auth.KindUser,
		Groups: []string{"system:serviceaccounts", auth.GroupAuthenticated}}
	admitPod := func(tx *store.Tx, p *apis.Pod) error {
		return srv.Admit(admission.Request{Identity: controllerID, Operation: admission.Create, Resource: podRes,
			Namespace: p.Metadata.Namespace, Name: p.Metadata.Name, Object: p, Tx: tx})
	}
	_, listenPort, _ := net.SplitHostPort(*listen)
	joinURL := ""
	if advertiseIP.IsValid() {
		joinURL = "https://" + net.JoinHostPort(advertiseIP.String(), listenPort)
	}
	nodeAPI := wireNodes(wired, joinURL)
	clusterCIDR := netip.Prefix{} // zero: a single server
	if advertiseIP.IsValid() {
		clusterCIDR = apis.ClusterCIDR
	}
	if *trace != "" {
		f, err := os.OpenFile(*trace, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		defer f.Close()
		srv.Trace = f
	}
	if err := srv.Bootstrap(); err != nil {
		return err
	}
	// An existing M4 database may contain Services without reservations.
	// Finish that upgrade before the API and proxy can observe the state.
	if err := api.ReconcileServiceIPs(st); err != nil {
		return fmt.Errorf("prepare Service ClusterIPs: %w", err)
	}

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return fmt.Errorf("%w (is another sw server running? `--listen` picks another address)", err)
	}
	defer ln.Close()
	var clusterLn net.Listener
	if advertiseIP.IsValid() {
		// Workers and remote kubectl connect here; the local address keeps
		// serving the tools on this machine.
		if clusterLn, err = net.Listen("tcp", net.JoinHostPort(advertiseIP.String(), listenPort)); err != nil {
			return fmt.Errorf("listen for other servers on %s: %w", advertiseIP, err)
		}
		defer clusterLn.Close()
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Feature loops (wiring.Go) and node services end before the store closes.
	var background sync.WaitGroup
	defer func() { stop(); background.Wait() }()
	goBackground := func(fn func(context.Context)) {
		background.Add(1)
		go func() { defer background.Done(); fn(ctx) }()
	}
	for _, l := range loops {
		goBackground(l.fn)
	}
	hs := &http.Server{
		Handler:           srv,
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{mat.Server}, MinVersion: tls.VersionTLS12},
		ReadHeaderTimeout: 10 * time.Second,
		// Requests inherit the signal context, so open watches end when the server is told to stop.
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	agentDone := make(chan struct{})
	controllerDone := make(chan struct{})
	proxyDone := make(chan struct{})
	edgeDone := make(chan struct{})
	if withAgent {
		rt := runtime.NewContainerd(*socket)
		defer rt.Close()
		n := runtime.NewNet(*runDir, apis.PodCIDR, apis.ServiceCIDR)
		if advertiseIP.IsValid() {
			n.Overlay, n.ClusterCIDR = true, apis.ClusterCIDR
		}
		// The foreground server must also prove ownership before the
		// node agent's first pass; otherwise it could remove the previous
		// database's surviving Pods as orphans.
		prepare, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if _, _, err := ownedRuntime(prepare, st, rt, n); err != nil {
			return err
		}
		if err := n.CheckHostRanges(prepare); err != nil {
			return err
		}
		prior, err := readNodeOwner()
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		managed := *instance != "" || prior.Managed
		if err := atomicJSON(filepath.Join(defaultRunDir, ownerFile), nodeOwnership{o, managed}); err != nil {
			return err
		}
		if !managed {
			defer releaseIdleNode(st, rt, n)
		}
		if *instance != "" {
			// The launcher only checked the network. The server holds the
			// node lock and prepares it before answering /readyz.
			if err := n.EnsureHost(); err != nil {
				return err
			}
		}
		// Open the public ports before any worker starts: a busy port or a
		// wrong setting must stop the server while nothing needs joining.
		public, err := prepareEdge(st, o)
		if err != nil {
			return err
		}
		draining := &drain.State{}
		forwarding := &proxy.KernelManager{Manager: proxy.Manager{Store: st},
			DataPath: &runtime.KernelServices{PodCIDR: apis.PodCIDR, ServiceCIDR: apis.ServiceCIDR, ClusterCIDR: clusterCIDR}, Drain: draining}
		agent := &node.Agent{
			Store: st, Runtime: rt, Net: n,
			RetirePod: forwarding.RetirePod,
			NodeName:  *nodeName, DataDir: *dataDir, LogDir: *logDir, PodCIDR: apis.PodCIDR, Version: api.GitVersion,
		}
		if advertiseIP.IsValid() {
			agent.NodeIP = advertiseIP.String()
			overlay := &node.Overlay{Store: st, NodeName: *nodeName, NodeIP: advertiseIP, PodCIDR: apis.PodCIDR}
			goBackground(overlay.Run)
		}
		srv.NodeName, srv.LogDir = *nodeName, *logDir
		go func() { agent.Run(ctx); close(agentDone) }()
		go func() {
			defer close(controllerDone)
			controller.Manager{Store: st, NodeName: *nodeName, Drain: draining, Silence: nodeAPI.Live.Silence, VolumeFilter: volumeFilter, AdmitPod: admitPod}.Run(ctx)
		}()
		go func() {
			defer close(proxyDone)
			forwarding.Run(ctx)
		}()
		go runPreparedEdge(ctx, public, edgeDone)
		gateway := n.Gateway()
		goBackground(func(ctx context.Context) {
			runNodeNetworkServices(ctx, st, gateway, func(format string, args ...any) {
				fmt.Fprintf(os.Stderr, "network services: "+format+"\n", args...)
			})
		})
		goBackground(func(ctx context.Context) {
			runNodeNetworkPolicy(ctx, st, apis.PodCIDR, func(format string, args ...any) {
				fmt.Fprintf(os.Stderr, "network policy: "+format+"\n", args...)
			})
		})
	} else {
		close(agentDone)
		if advertiseIP.IsValid() {
			// A control plane without workloads of its own still schedules
			// and replaces Pods on its workers.
			go func() {
				defer close(controllerDone)
				controller.Manager{Store: st, Silence: nodeAPI.Live.Silence, VolumeFilter: volumeFilter, AdmitPod: admitPod}.Run(ctx)
			}()
		} else {
			close(controllerDone)
		}
		close(proxyDone)
		close(edgeDone)
	}
	if *instance != "" {
		if err := writeState(o, *instance); err != nil {
			// Workers may already be using the store. Stop and join them
			// before deferred cleanup closes SQLite and containerd.
			stop()
			<-agentDone
			<-controllerDone
			<-proxyDone
			<-edgeDone
			return err
		}
	}
	done := make(chan error, 2)
	go func() { done <- hs.ServeTLS(ln, "", "") }()
	if clusterLn != nil {
		go func() { done <- hs.ServeTLS(clusterLn, "", "") }()
		fmt.Fprintf(os.Stderr, "sw server: other servers join at %s (sw node token prints the command)\n", joinURL)
	}
	fmt.Fprintf(os.Stderr, "sw server %s listening on https://%s (data %s, config %s)\n", api.Version, ln.Addr(), *dataDir, *configDir)

	select {
	case err := <-done:
		stop()
		<-agentDone
		<-controllerDone
		<-proxyDone
		<-edgeDone
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	shutdownErr := hs.Shutdown(shutdown)
	// Containers keep running: their shims hold them, and the next start of
	// sw server picks them up (design 4).
	<-agentDone
	<-controllerDone
	<-proxyDone
	<-edgeDone
	if shutdownErr != nil && !errors.Is(shutdownErr, context.DeadlineExceeded) {
		return shutdownErr
	}
	fmt.Fprintln(os.Stderr, "sw server stopped")
	return nil
}

func runKubeconfig(args []string) error {
	fs := flag.NewFlagSet("sw kubeconfig", flag.ExitOnError)
	configDir := fs.String("config-dir", defaultConfigDir, "directory with the certificates and the admin token")
	server := fs.String("server", "https://"+defaultListen, "address kubectl should connect to")
	fs.Parse(args)
	mat, err := pki.Load(*configDir)
	if err != nil {
		return err
	}
	fmt.Print(pki.Kubeconfig(mat, *server))
	return nil
}
