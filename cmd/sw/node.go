package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"shearwater/internal/apis"
	"shearwater/internal/ha"
	"shearwater/internal/nodeapi"
	"shearwater/internal/pki"
)

const nodeUsage = `usage:
  sw node token [--ttl 24h] [--control-plane | --witness]   print the command that joins another server to this cluster
  sw node list                list the servers of this cluster
  sw node remove NAME         forget a server that is gone for good (its Pods are deleted)

On the server that joins:
  sw join https://CONTROL-PLANE:6443 --token TOKEN --ca-hash sha256:HASH [--node-name NAME] [--node-ip IP] [--control-plane | --witness] [--force]`

func joinRole(controlPlane, witness bool) (string, error) {
	switch {
	case controlPlane && witness:
		return "", opsError("INVALID", "never", "--control-plane and --witness are mutually exclusive", "choose one join role")
	case controlPlane:
		return ha.RoleControlPlane, nil
	case witness:
		return ha.RoleWitness, nil
	default:
		return ha.RoleWorker, nil
	}
}

func runNode(args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fmt.Println(nodeUsage)
		return nil
	}
	action, args := args[0], args[1:]
	fs := flag.NewFlagSet("sw node "+action, flag.ContinueOnError)
	config := fs.String("config-dir", defaultConfigDir, "administrator certificate and token directory")
	listen := fs.String("listen", defaultListen, "administrator API address")
	ttl := fs.Duration("ttl", 24*time.Hour, "how long the join token works (at most 7 days)")
	var controlPlane, witness bool
	if action == "token" {
		fs.BoolVar(&controlPlane, "control-plane", false, "join a standby control plane")
		fs.BoolVar(&witness, "witness", false, "join a witness")
	}
	var name string
	if action == "remove" {
		if len(args) == 0 || !apis.IsDNS1123Subdomain(args[0]) {
			return errors.New(nodeUsage)
		}
		name, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New(nodeUsage)
	}
	role, err := joinRole(controlPlane, witness)
	if err != nil {
		return err
	}
	if err := savedClient(fs, config, listen); err != nil {
		return err
	}
	c, err := localAPI(*config, *listen)
	if err != nil {
		return err
	}
	ctx := context.Background()
	switch action {
	case "token":
		body, _ := json.Marshal(joinTokenRequest{Role: role, TTLSeconds: int64(ttl.Seconds())})
		data, status, _, err := apiRequest(ctx, c, "POST", joinTokensPath, body)
		if status != 0 && status != 200 {
			return adminError(data, status)
		}
		if err != nil {
			return opsError("UNAVAILABLE", "safe", err.Error(), "check the control plane address and sw status")
		}
		var resp joinTokenResponse
		if err := json.Unmarshal(data, &resp); err != nil {
			return err
		}
		if role == ha.RoleWitness {
			fmt.Printf("Run this on the witness (valid until %s):\n\n  %s\n\nAllow TCP %d from both control planes.\n",
				resp.Expires.Local().Format(time.RFC3339), resp.Command, ha.WitnessPort)
			return nil
		}
		fmt.Printf("Run this on the server that joins (valid until %s):\n\n  %s\n\n"+
			"It needs TCP 6443 to reach this server, and UDP %d and TCP 10250 open between all servers.\n",
			resp.Expires.Local().Format(time.RFC3339), resp.Command, 8472)
		return nil
	case "remove":
		data, status, _, err := apiRequest(ctx, c, "DELETE", nodesPath+"/"+name, nil)
		if err != nil {
			return err
		}
		if status != 200 {
			return adminError(data, status)
		}
		var resp struct {
			PodsDeleted int `json:"podsDeleted"`
		}
		json.Unmarshal(data, &resp)
		fmt.Printf("node %s removed; %d Pod(s) bound to it deleted, their controllers recreate them elsewhere\n", name, resp.PodsDeleted)
		return nil
	case "list":
		var list struct {
			Items []apis.Node `json:"items"`
		}
		if err := c.get(ctx, "/api/v1/nodes", &list); err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tSTATUS\tPOD-CIDR\tINTERNAL-IP\tTAINTS")
		for _, n := range list.Items {
			status := "Unknown"
			for _, cnd := range n.Status.Conditions {
				if cnd.Type == "Ready" {
					status = map[string]string{"True": "Ready", "False": "NotReady"}[cnd.Status]
					if status == "" {
						status = "Unknown"
					}
				}
			}
			if n.Spec.Unschedulable {
				status += ",SchedulingDisabled"
			}
			var taints []string
			for _, t := range n.Spec.Taints {
				taints = append(taints, t.Key+":"+t.Effect)
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", n.Metadata.Name, status, n.Spec.PodCIDR,
				n.Metadata.Annotations["shearwater.io/internal-ip"], strings.Join(taints, ","))
		}
		return tw.Flush()
	}
	return errors.New(nodeUsage)
}

func adminError(data []byte, status int) error {
	var e struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(data, &e) != nil || e.Message == "" {
		e.Message = strings.TrimSpace(string(data))
	}
	if status == http.StatusServiceUnavailable {
		return opsError("UNAVAILABLE", "safe", e.Message, "retry against the active control plane")
	}
	return opsError("REFUSED", "never", fmt.Sprintf("HTTP %d: %s", status, e.Message), "see sw node -h")
}

// runJoin enrolls this host and starts it immediately for worker joins.
func runJoin(args []string) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return errors.New(nodeUsage)
	}
	server, args := strings.TrimRight(args[0], "/"), args[1:]
	fs := flag.NewFlagSet("sw join", flag.ContinueOnError)
	var o opsOptions
	o.flags(fs)
	controlPlane := fs.Bool("control-plane", false, "join as the standby control plane")
	witness := fs.Bool("witness", false, "join as a witness")
	force := fs.Bool("force", false, "replace existing join configuration")
	token := fs.String("token", "", "join token from `sw node token`")
	caHash := fs.String("ca-hash", "", "sha256:... hash of the cluster CA from `sw node token`")
	nodeIP := fs.String("node-ip", "", "address the other servers reach this one at (default: the one used to reach the control plane)")
	if err := parseOps(fs, args, &o); err != nil {
		return err
	}
	role, err := joinRole(*controlPlane, *witness)
	if err != nil {
		return err
	}
	u, err := url.Parse(server)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return opsError("INVALID", "never", fmt.Sprintf("%q is not https://HOST:PORT", server), "copy the command printed by sw node token")
	}
	if *token == "" || !strings.HasPrefix(*caHash, "sha256:") {
		return opsError("INVALID", "never", "--token and --ca-hash are required", "copy the command printed by sw node token")
	}
	if role != ha.RoleWitness && o.Runtime != "containerd" {
		return opsError("INVALID", "never", "a worker runs containers", "drop --runtime")
	}
	if role != ha.RoleWitness && os.Geteuid() != 0 {
		return needRoot("join a cluster")
	}
	if err := privateDir(o.ConfigDir, true); err != nil {
		return localConfigError(o.ConfigDir, err)
	}
	lock, err := fileLock(o.ConfigDir, "config.lock")
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := checkJoinFiles(o.ConfigDir, *force); err != nil {
		return err
	}
	ip := *nodeIP
	if ip == "" {
		conn, err := net.Dial("udp", u.Host) // picks the route; sends nothing
		if err != nil {
			return opsError("INVALID", "never", "cannot find a route to "+u.Host+": "+err.Error(), "pass --node-ip")
		}
		ip = conn.LocalAddr().(*net.UDPAddr).IP.String()
		conn.Close()
	}
	if a, err := netip.ParseAddr(ip); err != nil || !a.Is4() || a.IsLoopback() || a.IsUnspecified() {
		return opsError("INVALID", "never", fmt.Sprintf("node address %q is not an IPv4 address other servers can reach", ip), "pass --node-ip")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ca, err := nodeapi.FetchCA(ctx, server, *caHash)
	if err != nil {
		return opsError("UNAUTHORIZED", "never", err.Error(), "check the address and copy the command printed by sw node token again")
	}
	var keyPEM, csrPEM []byte
	if role != ha.RoleWitness {
		keyPEM, csrPEM, err = pki.NewNodeKey(o.NodeName)
		if err != nil {
			return opsError("INTERNAL", "safe", err.Error(), "retry generating the node key")
		}
	}
	resp, err := nodeapi.Join(ctx, server, *token, ca, nodeapi.JoinRequest{Role: role, NodeName: o.NodeName, InternalIP: ip, CSR: string(csrPEM)})
	if err != nil {
		var reply *nodeapi.Error
		if errors.As(err, &reply) && reply.Status == http.StatusServiceUnavailable {
			return opsError("NOT_LEADER", "safe", reply.Msg, "retry the join against the active control plane")
		}
		return opsError("REFUSED", "never", err.Error(), "check the requested role and get a fresh sw node token on the control plane")
	}
	if err := writeJoinFiles(o.ConfigDir, role, server, ip, ca, keyPEM, resp, *force); err != nil {
		return opsError("JOIN_FILES", "never", err.Error(), "check the config directory and request a fresh join token before retrying")
	}
	nextConfig := ""
	if o.ConfigDir != defaultConfigDir {
		nextConfig = " --config-dir " + shellQuote(o.ConfigDir)
	}
	switch role {
	case ha.RoleWitness:
		fmt.Printf("joined %s as witness %s\nnext: sw ha witness%s\n", server, o.NodeName, nextConfig)
	case ha.RoleControlPlane:
		fmt.Printf("joined %s as standby %s\nnext: sw up%s\n", server, resp.NodeName, nextConfig)
	default:
		fmt.Printf("joined %s as node %s (pods %s, reached at %s)\n", server, resp.NodeName, resp.PodCIDR, ip)
		lock.Close() // sw up takes the config lock
		return upWith(o)
	}
	return nil
}

// Keep the role markers last. A forced rejoin also retires credentials and
// derived certificates from the old role. The durable witness vote is kept.
var joinFiles = []string{
	"ca.crt", "ca.key", "admin.token", "node.token", "node.crt", "node.key",
	"witness.crt", "witness.key", "server.crt", "server.key",
	"apiserver-kubelet-client.crt", "apiserver-kubelet-client.key", "kubeconfig",
	ha.PromoteFile, ha.StatusFile, clusterFile, ha.ConfigFile, ha.WitnessFile, nodeConfigFile,
}

func checkJoinFiles(dir string, force bool) error {
	for _, name := range append(slices.Clone(joinFiles), ha.WitnessStateFile) {
		path := filepath.Join(dir, name)
		info, err := os.Lstat(path)
		switch {
		case errors.Is(err, os.ErrNotExist):
		case err != nil:
			return localConfigError(dir, err)
		case !info.Mode().IsRegular():
			return opsError("INVALID", "never", path+" is not a regular file", "choose a private config directory")
		case !force:
			return opsError("CONFLICT", "never", path+" already exists", "choose another --config-dir or use --force to replace this join")
		}
	}
	return nil
}

type witnessConfig struct {
	Config ha.Config `json:"config"`
	Listen string    `json:"listen"`
}

func stageJoin(dir, role, server, ip string, ca, key []byte, r nodeapi.JoinResponse) error {
	if len(ca) == 0 {
		return errors.New("joining requires the pinned cluster CA")
	}
	files := map[string][]byte{"ca.crt": ca}
	b := r.HA
	switch role {
	case ha.RoleControlPlane:
		if b == nil || b.Config.Role != ha.RoleStandby || b.Config.Self != r.NodeName || b.CAKey == "" || b.AdminToken == "" {
			return errors.New("missing standby configuration or credentials")
		}
		files["ca.key"], files["admin.token"] = []byte(b.CAKey), []byte(strings.TrimSpace(b.AdminToken)+"\n")
		if err := b.Config.Save(dir); err != nil {
			return err
		}
		if err := saveAdvertise(dir, ip); err != nil {
			return err
		}
	case ha.RoleWitness:
		if b == nil || b.Config.Token == "" || b.Listen == "" || b.ServingCert == "" || b.ServingKey == "" {
			return errors.New("missing witness configuration or credentials")
		}
		files["witness.crt"], files["witness.key"] = []byte(b.ServingCert), []byte(b.ServingKey)
		if err := atomicJSON(filepath.Join(dir, ha.WitnessFile), witnessConfig{Config: b.Config, Listen: b.Listen}); err != nil {
			return err
		}
	case ha.RoleWorker:
	default:
		return fmt.Errorf("unknown join role %q", role)
	}
	if role != ha.RoleWitness {
		if r.NodeName == "" || r.NodeToken == "" || r.ServingCert == "" || len(key) == 0 {
			return errors.New("missing node credentials")
		}
		files["node.token"], files["node.key"], files["node.crt"] = []byte(r.NodeToken+"\n"), key, []byte(r.ServingCert)
		servers := r.ControlPlanes
		if len(servers) == 0 {
			servers = []string{server}
		}
		cfg := nodeConfig{Server: server, Servers: servers, NodeName: r.NodeName, NodeIP: ip, PodCIDR: r.PodCIDR,
			ClusterCIDR: r.ClusterCIDR, ServiceCIDR: r.ServiceCIDR, Joined: time.Now().UTC()}
		if err := atomicJSON(filepath.Join(dir, nodeConfigFile), cfg); err != nil {
			return err
		}
	}
	for name, data := range files {
		mode := os.FileMode(0o600)
		if name == "ca.crt" || name == "node.crt" {
			mode = 0o644
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, mode); err != nil {
			return err
		}
	}
	return nil
}

// Stage the whole response before touching the previous join. Keep each
// replaced file until publication finishes so an I/O error can be rolled back.
func writeJoinFiles(dir, role, server, ip string, ca, key []byte, r nodeapi.JoinResponse, force bool) error {
	if err := checkJoinFiles(dir, force); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(dir, ".join-*")
	if err != nil {
		return err
	}
	keepBackup := false
	defer func() {
		if !keepBackup {
			os.RemoveAll(stage)
		}
	}()
	if err := stageJoin(stage, role, server, ip, ca, key, r); err != nil {
		return err
	}
	var undo []func() error
	for _, name := range joinFiles {
		dest, src := filepath.Join(dir, name), filepath.Join(stage, name)
		backup := src + ".old"
		if err = os.Rename(dest, backup); err == nil {
			undo = append(undo, func() error { return os.Rename(backup, dest) })
		} else if !errors.Is(err, os.ErrNotExist) {
			break
		}
		if err = os.Rename(src, dest); err == nil {
			undo = append(undo, func() error { return os.Remove(dest) })
		} else if !errors.Is(err, os.ErrNotExist) {
			break
		}
		err = nil
	}
	if err != nil {
		for i := len(undo) - 1; i >= 0; i-- {
			if restoreErr := undo[i](); restoreErr != nil {
				keepBackup = true
				err = errors.Join(err, fmt.Errorf("restore previous join from %s: %w", stage, restoreErr))
			}
		}
	}
	return err
}
