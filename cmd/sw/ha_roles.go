package main

// Which role a control plane starts in, how it changes role, and the standby
// mode (docs/design-agent-first-ha.md 3.6).

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"shearwater/internal/apis"
	"shearwater/internal/auth"
	"shearwater/internal/ha"
	"shearwater/internal/nodeapi"
	"shearwater/internal/store"
)

// haNonceEnv carries the nonce of a won election into the process started
// next; only that process starts as the leader.
const haNonceEnv = "SW_HA_NONCE"

// standbyDir holds the worker credentials a control plane uses while it is a
// standby (the leader prepares its own when high availability is enabled; a
// joined standby has them in the config directory itself).
func standbyDir(configDir string) string {
	if _, err := os.Stat(filepath.Join(configDir, "standby", nodeConfigFile)); err == nil {
		return filepath.Join(configDir, "standby")
	}
	return configDir
}

// startsAsLeader decides the role of a control plane with ha.json. A process
// started for a won election (the nonce matches) leads, and so does a lone
// member. Otherwise it starts as a standby, except a leader without a witness
// whose peer does not lead a later epoch: nobody else takes over without a
// witness, so it continues.
func startsAsLeader(configDir string, cfg ha.Config) bool {
	nonce := os.Getenv(haNonceEnv)
	os.Unsetenv(haNonceEnv)
	switch {
	case cfg.Role == ha.RoleLeader && cfg.Nonce != "" && nonce == cfg.Nonce:
		return true
	case len(cfg.Members) < 2:
		return true
	case cfg.Role == ha.RoleLeader && cfg.Witness == "":
		return !peerLeadsLater(configDir, cfg)
	}
	return false
}

// peerLeadsLater asks the other control plane whether it leads a later epoch
// (an operator promoted it while this one was away).
func peerLeadsLater(configDir string, cfg ha.Config) bool {
	p, ok := cfg.Peer()
	if !ok {
		return false
	}
	c, err := haHTTPClient(configDir)
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(p.URL, "/")+ha.Prefix+"status", nil)
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	res, err := c.Do(req)
	if err != nil {
		return false
	}
	defer res.Body.Close()
	var st ha.Status
	if res.StatusCode != http.StatusOK || json.NewDecoder(res.Body).Decode(&st) != nil {
		return false
	}
	return st.Role == ha.RoleLeader && st.Epoch > cfg.Epoch
}

// reexec starts this program again in place (same PID, arguments and
// managed instance), handing over nonce. Containers keep running.
func reexec(nonce string) error {
	env := []string{}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, haNonceEnv+"=") {
			env = append(env, kv)
		}
	}
	if nonce != "" {
		env = append(env, haNonceEnv+"="+nonce)
	}
	// /proc/self/exe still names the running binary if its file was replaced.
	return syscall.Exec("/proc/self/exe", os.Args, env)
}

// promoteSelf records that this control plane won epoch and starts it again
// as the leader.
func promoteSelf(configDir string, cfg ha.Config, epoch int64) error {
	// The standby saves members and a witness it learns from the leader;
	// keep those rather than what it started with.
	if cur, err := ha.Load(configDir); err == nil {
		cfg = cur
	}
	b := make([]byte, 16)
	rand.Read(b)
	cfg.Role, cfg.Epoch, cfg.Nonce = ha.RoleLeader, epoch, hex.EncodeToString(b)
	if err := cfg.Save(configDir); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "sw: this control plane leads epoch %d; starting again as the leader\n", epoch)
	return reexec(cfg.Nonce)
}

// ownPodCIDR is the pod range of this control plane's node: the one recorded
// on its Node (a standby that took over keeps the range it had as a worker),
// or the control plane's first range.
func ownPodCIDR(st *store.Store, nodeName string) netip.Prefix {
	rec, err := st.Get(store.Key{Resource: "nodes", Name: nodeName})
	if err != nil {
		return apis.PodCIDR
	}
	var n struct {
		Spec struct {
			PodCIDR string `json:"podCIDR"`
		} `json:"spec"`
	}
	if json.Unmarshal(rec.Data, &n) != nil {
		return apis.PodCIDR
	}
	p, err := netip.ParsePrefix(n.Spec.PodCIDR)
	if err != nil || !apis.ClusterCIDR.Contains(p.Addr()) {
		return apis.PodCIDR
	}
	return p
}

// prepareSelfWorker gives the leader the credentials it needs to run its node
// as a worker of the other control plane after stepping down: a node token
// (stored, so it replicates), a node serving certificate and node.json in
// configDir/standby.
func (h *haWiring) prepareSelfWorker() error {
	if h.w.NodeName == "" {
		return nil // no workloads here: a standby without a node agent
	}
	if _, err := os.Stat(filepath.Join(standbyDir(h.w.ConfigDir), nodeConfigFile)); err == nil {
		return nil // a joined standby that took over has its worker files
	}
	dir := filepath.Join(h.w.ConfigDir, "standby")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	certPEM, keyPEM, err := signServing(h.w.ConfigDir, h.w.NodeName, h.advertise)
	if err != nil {
		return err
	}
	token, pub, hash, err := auth.NewToken(auth.KindNode)
	if err != nil {
		return err
	}
	if _, err := h.w.Store.DeleteCredentials(auth.KindNode, h.w.NodeName); err != nil {
		return err
	}
	if err := h.w.Store.PutCredential(store.Credential{ID: pub, Kind: auth.KindNode, Name: h.w.NodeName, Hash: hash}); err != nil {
		return err
	}
	ca, err := os.ReadFile(filepath.Join(h.w.ConfigDir, "ca.crt"))
	if err != nil {
		return err
	}
	cfg := nodeConfig{Server: h.url(h.advertise.String()), NodeName: h.w.NodeName, NodeIP: h.advertise.String(),
		PodCIDR: ownPodCIDR(h.w.Store, h.w.NodeName).String(), ClusterCIDR: apis.ClusterCIDR.String(),
		ServiceCIDR: apis.ServiceCIDR.String(), Joined: time.Now().UTC()}
	for name, data := range map[string][]byte{"ca.crt": ca, "node.crt": certPEM, "node.key": keyPEM, "node.token": []byte(token + "\n")} {
		if err := atomicPrivateFile(filepath.Join(dir, name), data); err != nil {
			return err
		}
	}
	return atomicJSON(filepath.Join(dir, nodeConfigFile), cfg)
}

// runStandby runs a control plane as the standby: a worker of the leader that
// keeps a replica in dataDir/state.db and passes API connections on its
// addresses to the leader. Winning an election starts the process again as
// the leader.
func runStandby(o opsOptions, instance string, cfg ha.Config, advertise netip.Addr) error {
	if err := os.MkdirAll(o.DataDir, 0o700); err != nil {
		return err
	}
	dbLock, err := fileLock(o.DataDir, "database.lock")
	if err != nil {
		return err
	}
	defer dbLock.Close()
	client, err := haHTTPClient(o.ConfigDir)
	if err != nil {
		return err
	}
	logf := func(format string, a ...any) { fmt.Fprintf(os.Stderr, "standby: "+format+"\n", a...) }
	sb := &ha.Standby{Config: cfg, ConfigDir: o.ConfigDir, DataDir: o.DataDir, Client: client, Logf: logf,
		CampaignNow: cfg.Role == ha.RoleLeader,
		Promote:     func(epoch int64) error { return promoteSelf(o.ConfigDir, cfg, epoch) }}
	if cfg.Witness != "" {
		sb.Witness = &ha.WitnessClient{URL: cfg.Witness, Token: cfg.Token, Client: client}
	}
	if cfg.Role == ha.RoleLeader {
		c := cfg
		c.Role, c.Nonce = ha.RoleStandby, ""
		if err := c.Save(o.ConfigDir); err != nil {
			return err
		}
		sb.Config = c
	}
	_, port, _ := net.SplitHostPort(o.Listen)
	x := workerExtras{Role: "standby", FilesDir: standbyDir(o.ConfigDir),
		Local: func(ctx context.Context, ln net.Listener) error {
			ha.Forward(ctx, ln, sb.LeaderAddr, logf)
			return nil
		},
		Sidecar: func(ctx context.Context, _ *nodeapi.Client) {
			go sb.WriteStatus(ctx)
			if advertise.IsValid() {
				ln, err := net.Listen("tcp", net.JoinHostPort(advertise.String(), port))
				if err != nil {
					logf("cannot listen on %s for other servers: %v", advertise, err)
				} else {
					go ha.Forward(ctx, ln, sb.LeaderAddr, logf)
				}
			}
			if err := sb.Run(ctx); err != nil && !errors.Is(err, context.Canceled) && ctx.Err() == nil {
				// A worker that no longer follows or takes over is half a
				// standby: stop, so the service manager starts it again.
				logf("stopped following: %v; exiting", err)
				os.Exit(1)
			}
		}}
	return runWorkerWith(o, instance, x)
}
