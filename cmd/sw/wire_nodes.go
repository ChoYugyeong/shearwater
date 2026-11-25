package main

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"shearwater/internal/apis"
	"shearwater/internal/auth"
	"shearwater/internal/ha"
	"shearwater/internal/node"
	"shearwater/internal/nodeapi"
	"shearwater/internal/pki"
	"shearwater/internal/store"
)

// Administrator endpoints for a cluster of several servers
// (docs/design-multinode-tenancy.md 3.2).
const (
	joinTokensPath = "/shearwater/admin/v1/join-tokens"
	nodesPath      = "/shearwater/admin/v1/nodes"
	maxJoinTTL     = 7 * 24 * time.Hour
)

type joinTokenRequest struct {
	Role       string `json:"role,omitempty"`
	TTLSeconds int64  `json:"ttlSeconds"`
}

type joinTokenResponse struct {
	Token   string    `json:"token"`
	CAHash  string    `json:"caHash"`
	Server  string    `json:"server"`
	Expires time.Time `json:"expires"`
	Command string    `json:"command"`
}

func adminFail(w http.ResponseWriter, code int, format string, args ...any) {
	b, _ := json.Marshal(map[string]string{"message": fmt.Sprintf(format, args...)})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	w.Write(b)
}

// wireNodes mounts the node API and the administrator's node endpoints.
// server is the URL workers join ("" when the control plane only listens
// locally, which keeps a single server single).
func wireNodes(w *wiring, server string) *nodeapi.Server {
	ns := &nodeapi.Server{Store: w.Store, Auth: w.Server.Auth, ConfigDir: w.ConfigDir,
		ClusterCIDR: apis.ClusterCIDR, ServiceCIDR: apis.ServiceCIDR, ReservedCIDR: apis.PodCIDR,
		Live: nodeapi.NewLiveness()}
	if w.HA != nil {
		ns.JoinHA, ns.ControlPlanes = w.HA.Join, w.HA.ControlPlanes
	}
	w.Server.NodeAPI = ns

	w.Admin.HandleFunc(joinTokensPath, func(rw http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			adminFail(rw, http.StatusMethodNotAllowed, "use POST")
			return
		}
		if server == "" {
			adminFail(rw, http.StatusConflict, "this control plane listens only on this machine, so no server can join it; "+
				"start it with --advertise-address set to an address the other servers reach (sw up --advertise-address IP)")
			return
		}
		var req joinTokenRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			adminFail(rw, http.StatusBadRequest, "the body is not valid JSON: %v", err)
			return
		}
		var roleFlag string
		switch req.Role {
		case "", ha.RoleWorker:
			req.Role = ha.RoleWorker
		case ha.RoleControlPlane, ha.RoleWitness:
			if w.HA == nil {
				adminFail(rw, http.StatusConflict, "start the control plane with --advertise-address to enable HA")
				return
			}
			if err := w.HA.Enable(); err != nil {
				adminFail(rw, http.StatusServiceUnavailable, "cannot enable HA: %v", err)
				return
			}
			roleFlag = " --" + req.Role
		default:
			adminFail(rw, http.StatusBadRequest, "unknown join role %q", req.Role)
			return
		}
		ttl := 24 * time.Hour
		if req.TTLSeconds > 0 {
			ttl = min(time.Duration(req.TTLSeconds)*time.Second, maxJoinTTL)
		}
		ca, err := os.ReadFile(filepath.Join(w.ConfigDir, "ca.crt"))
		if err != nil {
			adminFail(rw, http.StatusInternalServerError, "%v", err)
			return
		}
		hash, err := pki.CAHash(ca)
		if err != nil {
			adminFail(rw, http.StatusInternalServerError, "%v", err)
			return
		}
		tok, id, h, err := auth.NewToken(auth.KindJoin)
		if err != nil {
			adminFail(rw, http.StatusInternalServerError, "%v", err)
			return
		}
		expires := time.Now().Add(ttl).UTC().Truncate(time.Second)
		data, _ := json.Marshal(map[string]string{"role": req.Role})
		if err := w.Store.PutCredential(store.Credential{ID: id, Kind: auth.KindJoin, Name: "join", Hash: h, Expires: expires, Data: data}); err != nil {
			adminFail(rw, http.StatusInternalServerError, "%v", err)
			return
		}
		writeAdminJSON(rw, joinTokenResponse{Token: tok, CAHash: hash, Server: server, Expires: expires,
			Command: fmt.Sprintf("sudo sw join %s --token %s --ca-hash %s%s", server, tok, hash, roleFlag)})
	})

	w.Admin.HandleFunc(nodesPath+"/", func(rw http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, nodesPath+"/")
		if r.Method != http.MethodDelete || name == "" || strings.Contains(name, "/") {
			adminFail(rw, http.StatusNotFound, "use DELETE %s/NAME", nodesPath)
			return
		}
		if name == w.NodeName {
			adminFail(rw, http.StatusConflict, "%s is the control plane's own node; stop it with sw down instead", name)
			return
		}
		pods, err := removeNode(w.Store, name)
		if errors.Is(err, store.ErrNotFound) {
			adminFail(rw, http.StatusNotFound, "no node %q", name)
			return
		}
		if err != nil {
			adminFail(rw, http.StatusInternalServerError, "%v", err)
			return
		}
		ns.Live.Forget(name)
		writeAdminJSON(rw, map[string]any{"node": name, "podsDeleted": pods})
	})

	if cert, err := pki.EnsureKubeletClient(w.ConfigDir); err == nil {
		if ca, err := os.ReadFile(filepath.Join(w.ConfigDir, "ca.crt")); err == nil {
			w.Server.RemoteLogs = logRelay(w.Store, ca, cert)
		}
	} else {
		fmt.Fprintf(os.Stderr, "sw server: no client certificate for node logs: %v\n", err)
	}
	return ns
}

func writeAdminJSON(w http.ResponseWriter, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		adminFail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(b)
}

// removeNode forgets a server that is gone for good: its Node, its Pods
// (nothing will report on them again), their addresses and its credential.
func removeNode(st *store.Store, name string) (int, error) {
	var cidr netip.Prefix
	deleted := 0
	err := st.Write(false, func(tx *store.Tx) error {
		rec, err := tx.Get(store.Key{Resource: "nodes", Name: name})
		if err != nil {
			return err
		}
		var n apis.Node
		if err := json.Unmarshal(rec.Data, &n); err != nil {
			return err
		}
		cidr, _ = netip.ParsePrefix(n.Spec.PodCIDR)
		if _, err := tx.Delete(rec.Key, rec.Rev, func(int64) ([]byte, error) { return rec.Data, nil }); err != nil {
			return err
		}
		recs, err := tx.Objects()
		if err != nil {
			return err
		}
		for _, rec := range recs {
			if rec.Key.Group != "" || rec.Key.Resource != "pods" {
				continue
			}
			var p apis.Pod
			if json.Unmarshal(rec.Data, &p) == nil && p.Spec.NodeName == name {
				data := rec.Data
				if _, err := tx.Delete(rec.Key, rec.Rev, func(int64) ([]byte, error) { return data, nil }); err != nil {
					return err
				}
				deleted++
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	if cidr.IsValid() {
		ips, err := st.IPs("pod")
		if err != nil {
			return deleted, err
		}
		for uid, ip := range ips {
			if a, err := netip.ParseAddr(ip); err == nil && cidr.Masked().Contains(a) {
				if err := st.ReleaseIP(uid); err != nil {
					return deleted, err
				}
			}
		}
	}
	_, err = st.DeleteCredentials(auth.KindNode, name)
	return deleted, err
}

// logRelay streams a container log from the node agent that has it.
func logRelay(st *store.Store, ca []byte, client tls.Certificate) func(http.ResponseWriter, *http.Request, *apis.Pod, string, int, int64, int64, bool) error {
	return func(w http.ResponseWriter, r *http.Request, pod *apis.Pod, container string, attempt int, tail, limit int64, follow bool) error {
		name := pod.Spec.NodeName
		rec, err := st.Get(store.Key{Resource: "nodes", Name: name})
		if err != nil {
			return fmt.Errorf("node %s of pod %s is not registered: %w", name, pod.Metadata.Name, err)
		}
		var n apis.Node
		if err := json.Unmarshal(rec.Data, &n); err != nil {
			return err
		}
		ip := n.Metadata.Annotations[node.InternalIPAnnotation]
		if ip == "" {
			return fmt.Errorf("node %s has not published its address yet", name)
		}
		cfg, err := pki.PinnedTLS(ca, pki.NodeName+name)
		if err != nil {
			return err
		}
		cfg.Certificates = []tls.Certificate{client}
		hc := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg, ResponseHeaderTimeout: 15 * time.Second}}
		u := fmt.Sprintf("https://%s/containerLogs/%s/%s/%s?uid=%s&attempt=%d&tailLines=%d&limitBytes=%d&follow=%t",
			net.JoinHostPort(ip, fmt.Sprint(node.LogPort)), url.PathEscape(pod.Metadata.Namespace), url.PathEscape(pod.Metadata.Name),
			url.PathEscape(container), url.QueryEscape(pod.Metadata.UID), attempt, tail, limit, follow)
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, u, nil)
		if err != nil {
			return err
		}
		resp, err := hc.Do(req)
		if err != nil {
			return fmt.Errorf("node %s did not answer for the log (is it running, and is TCP %d open between the servers?): %w", name, node.LogPort, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			return fmt.Errorf("node %s: %s: %s", name, resp.Status, strings.TrimSpace(string(msg)))
		}
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		flush := http.NewResponseController(w)
		buf := make([]byte, 32<<10)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				if _, werr := w.Write(buf[:n]); werr != nil {
					return nil
				}
				flush.Flush()
			}
			if err != nil {
				return nil
			}
		}
	}
}

// The control plane's advertised address is remembered in the config
// directory, so sw up, sw down and sw status need not repeat it.
const clusterFile = "cluster.json"

type clusterConfig struct {
	AdvertiseAddress string `json:"advertiseAddress"`
}

func readClusterConfig(dir string) clusterConfig {
	var c clusterConfig
	readPrivateJSON(filepath.Join(dir, clusterFile), &c)
	return c
}

func parseAdvertise(s string) (netip.Addr, error) {
	a, err := netip.ParseAddr(s)
	if err != nil || !a.Is4() || a.IsLoopback() || a.IsUnspecified() {
		return netip.Addr{}, opsError("INVALID", "never", fmt.Sprintf("--advertise-address must be an IPv4 address other servers reach, got %q", s),
			"use this server's LAN or private-network address, for example 192.168.1.10")
	}
	return a, nil
}

func saveAdvertise(dir, s string) error {
	if _, err := parseAdvertise(s); err != nil {
		return err
	}
	if err := privateDir(dir, true); err != nil {
		return err
	}
	return atomicJSON(filepath.Join(dir, clusterFile), clusterConfig{AdvertiseAddress: s})
}
