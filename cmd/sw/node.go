package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"shearwater/internal/apis"
	"shearwater/internal/nodeapi"
	"shearwater/internal/pki"
)

const nodeUsage = `usage:
  sw node token [--ttl 24h]   print the command that joins another server to this cluster
  sw node list                list the servers of this cluster
  sw node remove NAME         forget a server that is gone for good (its Pods are deleted)

On the server that joins (as root):
  sw join https://CONTROL-PLANE:6443 --token TOKEN --ca-hash sha256:HASH [--node-name NAME] [--node-ip IP]`

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
		body, _ := json.Marshal(joinTokenRequest{TTLSeconds: int64(ttl.Seconds())})
		data, status, _, err := apiRequest(ctx, c, "POST", joinTokensPath, body)
		if err != nil {
			return err
		}
		if status != 200 {
			return adminError(data, status)
		}
		var resp joinTokenResponse
		if err := json.Unmarshal(data, &resp); err != nil {
			return err
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
	return opsError("REFUSED", "never", fmt.Sprintf("HTTP %d: %s", status, e.Message), "see sw node -h")
}

// runJoin makes this server a worker of the cluster at the given address and
// starts it like sw up.
func runJoin(args []string) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return errors.New(nodeUsage)
	}
	server, args := strings.TrimRight(args[0], "/"), args[1:]
	fs := flag.NewFlagSet("sw join", flag.ContinueOnError)
	var o opsOptions
	o.flags(fs)
	token := fs.String("token", "", "join token from `sw node token`")
	caHash := fs.String("ca-hash", "", "sha256:... hash of the cluster CA from `sw node token`")
	nodeIP := fs.String("node-ip", "", "address the other servers reach this one at (default: the one used to reach the control plane)")
	if err := parseOps(fs, args, &o); err != nil {
		return err
	}
	u, err := url.Parse(server)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Path != "" {
		return opsError("INVALID", "never", fmt.Sprintf("%q is not https://HOST:PORT", server), "copy the command printed by sw node token")
	}
	if *token == "" || !strings.HasPrefix(*caHash, "sha256:") {
		return opsError("INVALID", "never", "--token and --ca-hash are required", "copy the command printed by sw node token")
	}
	if o.Runtime != "containerd" {
		return opsError("INVALID", "never", "a worker runs containers", "drop --runtime")
	}
	if os.Geteuid() != 0 {
		return needRoot("join a cluster")
	}
	if isWorker(o.ConfigDir) {
		return opsError("CONFLICT", "never", "this server already joined a cluster ("+filepath.Join(o.ConfigDir, nodeConfigFile)+")",
			"sw up starts it again; to join another cluster, sw down --all and move that directory away first")
	}
	if _, err := os.Stat(filepath.Join(o.ConfigDir, "admin.token")); err == nil {
		return opsError("CONFLICT", "never", o.ConfigDir+" belongs to a control plane on this machine",
			"a server is either a control plane or a worker; pass another --config-dir (and --data-dir, --run-dir) to join")
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
	if a, err := netip.ParseAddr(ip); err != nil || !a.Is4() || a.IsLoopback() {
		return opsError("INVALID", "never", fmt.Sprintf("node address %q is not an IPv4 address other servers can reach", ip), "pass --node-ip")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ca, err := nodeapi.FetchCA(ctx, server, *caHash)
	if err != nil {
		return opsError("UNAUTHORIZED", "never", err.Error(), "check the address and copy the command printed by sw node token again")
	}
	keyPEM, csrPEM, err := pki.NewNodeKey(o.NodeName)
	if err != nil {
		return err
	}
	resp, err := nodeapi.Join(ctx, server, *token, ca, nodeapi.JoinRequest{NodeName: o.NodeName, InternalIP: ip, CSR: string(csrPEM)})
	if err != nil {
		return opsError("REFUSED", "never", err.Error(), "an expired token needs a new `sw node token` on the control plane")
	}
	if err := privateDir(o.ConfigDir, true); err != nil {
		return err
	}
	cfg := nodeConfig{Server: server, NodeName: resp.NodeName, NodeIP: ip, PodCIDR: resp.PodCIDR,
		ClusterCIDR: resp.ClusterCIDR, ServiceCIDR: resp.ServiceCIDR, Joined: time.Now().UTC()}
	for name, data := range map[string][]byte{"node.token": []byte(resp.NodeToken + "\n"), "node.key": keyPEM} {
		if err := atomicPrivateFile(filepath.Join(o.ConfigDir, name), data); err != nil {
			return err
		}
	}
	for name, data := range map[string][]byte{"ca.crt": ca, "node.crt": []byte(resp.ServingCert)} {
		if err := os.WriteFile(filepath.Join(o.ConfigDir, name), data, 0o644); err != nil {
			return err
		}
	}
	if err := atomicJSON(filepath.Join(o.ConfigDir, nodeConfigFile), cfg); err != nil {
		return err
	}
	fmt.Printf("joined %s as node %s (pods %s, reached at %s)\n", server, resp.NodeName, resp.PodCIDR, ip)
	return upWith(o)
}
