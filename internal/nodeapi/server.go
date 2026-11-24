package nodeapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"shearwater/internal/apis"
	"shearwater/internal/auth"
	"shearwater/internal/objects"
	"shearwater/internal/pki"
	"shearwater/internal/store"
)

// Annotations on a Node that the control plane and the other nodes use.
const (
	// InternalIPAnnotation is the address other nodes reach this node at
	// (VXLAN underlay, log endpoint).
	InternalIPAnnotation = "shearwater.io/internal-ip"
	// VTEPAnnotation is the MAC address of the node's VXLAN device.
	VTEPAnnotation = "shearwater.io/vtep-mac"
)

const maxBody = 4 << 20

// Server is the control plane's side of the node API.
type Server struct {
	Store       *store.Store
	Auth        *auth.Authenticator
	ConfigDir   string       // where the cluster CA lives (signs node serving certificates)
	ClusterCIDR netip.Prefix // pod addresses of the whole cluster, one /24 per node
	ServiceCIDR netip.Prefix
	// ReservedCIDR is the control plane's own node's pod range; join never
	// hands it out.
	ReservedCIDR netip.Prefix
	Live         *Liveness
	Logf         func(format string, args ...any)
}

func (s *Server) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
		return
	}
	fmt.Fprintf(os.Stderr, "node api: "+format+"\n", args...)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		code, b = http.StatusInternalServerError, []byte(`{"code":"Internal","message":"encoding the answer failed"}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	w.Write(b)
}

func fail(w http.ResponseWriter, code int, errCode, format string, args ...any) {
	writeJSON(w, code, errorResponse{Code: errCode, Message: fmt.Sprintf(format, args...)})
}

type statusError struct {
	http int
	code string
	msg  string
}

func (e *statusError) Error() string { return e.msg }

func forbidden(format string, args ...any) error {
	return &statusError{http: http.StatusForbidden, code: codeForbidden, msg: fmt.Sprintf(format, args...)}
}

func invalid(format string, args ...any) error {
	return &statusError{http: http.StatusBadRequest, code: codeInvalid, msg: fmt.Sprintf(format, args...)}
}

var errReadChanged = &statusError{http: http.StatusConflict, code: codeConflict, msg: "an object read by the transaction changed"}

func failErr(w http.ResponseWriter, err error) {
	var se *statusError
	switch {
	case errors.As(err, &se):
		fail(w, se.http, se.code, "%s", se.msg)
	case errors.Is(err, store.ErrNotFound):
		fail(w, http.StatusNotFound, codeNotFound, "%v", err)
	case errors.Is(err, store.ErrExists):
		fail(w, http.StatusConflict, codeExists, "%v", err)
	case errors.Is(err, store.ErrConflict):
		fail(w, http.StatusConflict, codeConflict, "%v", err)
	case errors.Is(err, store.ErrCompacted):
		fail(w, http.StatusGone, codeCompacted, "%v", err)
	default:
		fail(w, http.StatusInternalServerError, codeInternal, "%v", err)
	}
}

func readJSON(r *http.Request, v any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		return err
	}
	if len(body) > maxBody {
		return invalid("the request body is larger than %d bytes", maxBody)
	}
	if err := json.Unmarshal(body, v); err != nil {
		return invalid("the request body is not valid JSON: %v", err)
	}
	return nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	op, ok := strings.CutPrefix(r.URL.Path, Prefix)
	if !ok {
		fail(w, http.StatusNotFound, codeNotFound, "nothing at %s", r.URL.Path)
		return
	}
	switch {
	case op == "ca" && r.Method == http.MethodGet:
		// Public: a joining server compares its hash with the one it was given.
		ca, err := os.ReadFile(filepath.Join(s.ConfigDir, "ca.crt"))
		if err != nil {
			failErr(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/x-pem-file")
		w.Write(ca)
		return
	case op == "join" && r.Method == http.MethodPost:
		s.join(w, r)
		return
	}
	node, ok := s.nodeOf(r)
	if !ok {
		fail(w, http.StatusUnauthorized, codeAuth, "missing or wrong node credential")
		return
	}
	s.Live.Seen(node)
	var err error
	switch {
	case op == "heartbeat" && r.Method == http.MethodPost:
		w.WriteHeader(http.StatusNoContent)
	case op == "objects" && r.Method == http.MethodGet:
		err = s.objects(w, r, node)
	case op == "changes" && r.Method == http.MethodGet:
		err = s.changes(w, r)
	case op == "wait" && r.Method == http.MethodGet:
		err = s.wait(w, r)
	case op == "transact" && r.Method == http.MethodPost:
		err = s.transact(w, r, node)
	case op == "ips" && r.Method == http.MethodGet:
		err = s.ips(w, r, node)
	case op == "ips/alloc" && r.Method == http.MethodPost:
		err = s.alloc(w, r, node)
	case op == "ips/release" && r.Method == http.MethodPost:
		err = s.release(w, r, node)
	case op == "network" && r.Method == http.MethodGet:
		err = s.network(w, r)
	case op == "podatip" && r.Method == http.MethodGet:
		q := r.URL.Query()
		var rec store.Record
		if rec, err = s.Store.PodAtIP(r.Context(), q.Get("namespace"), q.Get("name"), q.Get("ip")); err == nil {
			writeJSON(w, http.StatusOK, toWire(rec))
		}
	default:
		fail(w, http.StatusNotFound, codeNotFound, "no %s %s on the node API", r.Method, r.URL.Path)
	}
	if err != nil {
		failErr(w, err)
	}
}

func (s *Server) nodeOf(r *http.Request) (string, bool) {
	tok, ok := auth.Bearer(r)
	if !ok {
		return "", false
	}
	id, ok := s.Auth.Token(tok)
	if !ok {
		return "", false
	}
	return id.NodeName()
}

// ---- join ----

func (s *Server) join(w http.ResponseWriter, r *http.Request) {
	tok, _ := auth.Bearer(r)
	id, ok := s.Auth.Token(tok)
	if !ok || id.Kind != auth.KindJoin {
		fail(w, http.StatusUnauthorized, codeAuth, "missing, wrong or expired join token")
		return
	}
	var req JoinRequest
	if err := readJSON(r, &req); err != nil {
		failErr(w, err)
		return
	}
	ip := net.ParseIP(req.InternalIP)
	switch {
	case !apis.IsDNS1123Subdomain(req.NodeName):
		fail(w, http.StatusBadRequest, codeInvalid, "node name %q is not a valid name (lowercase letters, digits, '-' and '.')", req.NodeName)
		return
	case ip == nil || ip.To4() == nil || ip.IsLoopback() || ip.IsUnspecified():
		fail(w, http.StatusBadRequest, codeInvalid, "internal IP %q is not an IPv4 address other nodes can reach", req.InternalIP)
		return
	}
	if last, seen := s.Live.LastSeen(req.NodeName); seen && time.Since(last) < UnreachableAfter {
		fail(w, http.StatusConflict, codeExists, "node %q is running and was heard from %s ago; pick another --node-name, or remove it first with `sw node remove %s`",
			req.NodeName, time.Since(last).Round(time.Second), req.NodeName)
		return
	}
	podCIDR, err := s.registerNode(req.NodeName, req.InternalIP)
	if err != nil {
		failErr(w, err)
		return
	}
	cert, err := pki.SignNodeServing(s.ConfigDir, []byte(req.CSR), req.NodeName, []net.IP{ip, net.IPv4(127, 0, 0, 1)})
	if err != nil {
		fail(w, http.StatusBadRequest, codeInvalid, "node certificate request: %v", err)
		return
	}
	// A node has one credential: joining again replaces the old one.
	if _, err := s.Store.DeleteCredentials(auth.KindNode, req.NodeName); err != nil {
		failErr(w, err)
		return
	}
	nodeToken, pub, hash, err := auth.NewToken(auth.KindNode)
	if err == nil {
		err = s.Store.PutCredential(store.Credential{ID: pub, Kind: auth.KindNode, Name: req.NodeName, Hash: hash})
	}
	if err != nil {
		failErr(w, err)
		return
	}
	s.Live.Seen(req.NodeName)
	s.logf("node %s joined from %s with pod range %s (join token %s)", req.NodeName, req.InternalIP, podCIDR, id.CredentialID)
	writeJSON(w, http.StatusOK, JoinResponse{NodeName: req.NodeName, NodeToken: nodeToken, PodCIDR: podCIDR.String(),
		ClusterCIDR: s.ClusterCIDR.String(), ServiceCIDR: s.ServiceCIDR.String(), ServingCert: string(cert)})
}

var nodesRes = apis.Lookup("", "v1", "nodes")

// registerNode creates the Node object with a pod range, or keeps the range
// of a node that joins again under the same name.
func (s *Server) registerNode(name, ip string) (netip.Prefix, error) {
	var podCIDR netip.Prefix
	err := s.Store.Write(false, func(tx *store.Tx) error {
		recs, err := tx.Objects()
		if err != nil {
			return err
		}
		used := map[netip.Prefix]bool{s.ReservedCIDR.Masked(): true}
		var existing *apis.Node
		var existingRec store.Record
		for _, rec := range recs {
			if rec.Key.Group != "" || rec.Key.Resource != "nodes" {
				continue
			}
			n := &apis.Node{}
			if err := json.Unmarshal(rec.Data, n); err != nil {
				return err
			}
			if p, err := netip.ParsePrefix(n.Spec.PodCIDR); err == nil {
				used[p.Masked()] = true
			}
			if rec.Key.Name == name {
				existing, existingRec = n, rec
			}
		}
		if existing != nil {
			if p, err := netip.ParsePrefix(existing.Spec.PodCIDR); err == nil && p != s.ReservedCIDR.Masked() {
				podCIDR = p
			}
		}
		if !podCIDR.IsValid() {
			if podCIDR, err = nextPodCIDR(s.ClusterCIDR, used); err != nil {
				return err
			}
		}
		n := existing
		if n == nil {
			n = &apis.Node{}
			n.Metadata.Name = name
			objects.Stamp(nodesRes, n)
		}
		if n.Metadata.Labels == nil {
			n.Metadata.Labels = map[string]string{}
		}
		n.Metadata.Labels["kubernetes.io/hostname"] = name
		n.Metadata.Labels["kubernetes.io/os"] = "linux"
		if _, ok := n.Metadata.Labels["kubernetes.io/arch"]; !ok {
			n.Metadata.Labels["kubernetes.io/arch"] = goruntime.GOARCH
		}
		if n.Metadata.Annotations == nil {
			n.Metadata.Annotations = map[string]string{}
		}
		n.Metadata.Annotations[InternalIPAnnotation] = ip
		n.Spec.PodCIDR = podCIDR.String()
		if existing == nil {
			_, err = tx.Create(objects.Put(nodesRes, n))
		} else {
			_, _, err = tx.Update(objects.Put(nodesRes, n), existingRec.Rev)
		}
		return err
	})
	return podCIDR, err
}

// nextPodCIDR returns the first /24 of cluster that no node uses.
func nextPodCIDR(cluster netip.Prefix, used map[netip.Prefix]bool) (netip.Prefix, error) {
	if !cluster.Addr().Is4() || cluster.Bits() > 24 {
		return netip.Prefix{}, fmt.Errorf("cluster range %s cannot hold /24 node ranges", cluster)
	}
	base := cluster.Masked().Addr().As4()
	count := 1 << (24 - cluster.Bits())
	for i := range count {
		a := base
		v := uint32(a[0])<<24 | uint32(a[1])<<16 | uint32(a[2])<<8 | uint32(a[3])
		v += uint32(i) << 8
		p := netip.PrefixFrom(netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), 0}), 24)
		if !used[p] {
			return p, nil
		}
	}
	return netip.Prefix{}, &statusError{http: http.StatusConflict, code: codeExists, msg: fmt.Sprintf("the cluster range %s has no free /24 for another node", cluster)}
}

// ---- reads ----

// Nodes read what a node agent, its proxy, DNS and network policy need.
// Secrets are read one at a time, and only those a Pod on the node uses.
var nodeReadable = map[string]bool{
	"/pods": true, "/services": true, "/nodes": true, "/namespaces": true,
	"discovery.k8s.io/endpointslices": true, "networking.k8s.io/networkpolicies": true,
	"/persistentvolumeclaims": true, "/persistentvolumes": true, "storage.k8s.io/storageclasses": true,
}

func readable(group, resource string) bool { return nodeReadable[group+"/"+resource] }

func (s *Server) objects(w http.ResponseWriter, r *http.Request, node string) error {
	q := r.URL.Query()
	group, resource, ns, name := q.Get("group"), q.Get("resource"), q.Get("namespace"), q.Get("name")
	switch {
	case group == "" && resource == "secrets" && name != "":
		if ok, err := s.podUsesSecret(node, ns, name); err != nil {
			return err
		} else if !ok {
			return forbidden("node %s has no Pod that uses Secret %s/%s", node, ns, name)
		}
	case group == "" && resource == "events" && name != "":
		// A node reads back the event it is about to count up.
	case !readable(group, resource):
		return forbidden("nodes cannot read %s", strings.TrimPrefix(group+"/"+resource, "/"))
	}
	if name != "" {
		rec, err := s.Store.Get(store.Key{Group: group, Resource: resource, Namespace: ns, Name: name})
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, listResponse{Rev: rec.Rev, Items: []wireRecord{toWire(rec)}})
		return nil
	}
	recs, rev, err := s.Store.List(group, resource, ns)
	if err != nil {
		return err
	}
	out := listResponse{Rev: rev, Items: make([]wireRecord, len(recs))}
	for i, rec := range recs {
		out.Items[i] = toWire(rec)
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

// podUsesSecret: does a Pod bound to node reference the Secret?
func (s *Server) podUsesSecret(node, ns, name string) (bool, error) {
	recs, _, err := s.Store.List("", "pods", ns)
	if err != nil {
		return false, err
	}
	for _, rec := range recs {
		var pod apis.Pod
		if json.Unmarshal(rec.Data, &pod) != nil || pod.Spec.NodeName != node {
			continue
		}
		for _, c := range pod.Spec.Containers {
			for _, e := range c.Env {
				if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil && e.ValueFrom.SecretKeyRef.Name == name {
					return true, nil
				}
			}
			for _, e := range c.EnvFrom {
				if e.SecretRef != nil && e.SecretRef.Name == name {
					return true, nil
				}
			}
		}
	}
	return false, nil
}

func (s *Server) changes(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	group, resource := q.Get("group"), q.Get("resource")
	if !readable(group, resource) {
		return forbidden("nodes cannot watch %s", strings.TrimPrefix(group+"/"+resource, "/"))
	}
	after, err1 := strconv.ParseInt(q.Get("after"), 10, 64)
	limit, err2 := strconv.Atoi(q.Get("limit"))
	if err1 != nil || err2 != nil || limit <= 0 || limit > 5000 {
		return invalid("after and limit (1..5000) are required numbers")
	}
	changes, next, err := s.Store.Changes(group, resource, q.Get("namespace"), after, limit)
	if err != nil {
		return err
	}
	out := changesResponse{Next: next, Changes: make([]wireChange, len(changes))}
	for i, c := range changes {
		out.Changes[i] = wireChange{Rev: c.Rev, Key: toWireKey(c.Key), Type: c.Type, Data: c.Data, Prev: c.Prev}
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

// wait answers when the store's revision passes after, or at the timeout.
func (s *Server) wait(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	after, err := strconv.ParseInt(q.Get("after"), 10, 64)
	if err != nil {
		return invalid("after must be a revision")
	}
	timeout := 30 * time.Second
	if secs, err := strconv.Atoi(q.Get("timeout")); err == nil && secs > 0 && secs <= 55 {
		timeout = time.Duration(secs) * time.Second
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		changed := s.Store.Changed()
		rev, err := s.Store.Rev()
		if err != nil {
			return err
		}
		if rev > after {
			writeJSON(w, http.StatusOK, waitResponse{Rev: rev})
			return nil
		}
		select {
		case <-changed:
		case <-timer.C:
			writeJSON(w, http.StatusOK, waitResponse{Rev: rev})
			return nil
		case <-r.Context().Done():
			return nil
		}
	}
}

func (s *Server) network(w http.ResponseWriter, r *http.Request) error {
	view, err := s.Store.ServiceNetwork(r.Context())
	if err != nil {
		return err
	}
	conv := func(recs []store.Record) []wireRecord {
		out := make([]wireRecord, len(recs))
		for i, rec := range recs {
			out[i] = toWire(rec)
		}
		return out
	}
	writeJSON(w, http.StatusOK, networkResponse{Services: conv(view.Services), EndpointSlices: conv(view.EndpointSlices),
		Pods: conv(view.Pods), PodIPs: view.PodIPs})
	return nil
}

// ---- writes ----

// nodePodCIDR is the pod range the control plane gave node.
func (s *Server) nodePodCIDR(get func(store.Key) (store.Record, error), node string) (netip.Prefix, error) {
	rec, err := get(store.Key{Resource: "nodes", Name: node})
	if err != nil {
		return netip.Prefix{}, err
	}
	var n apis.Node
	if err := json.Unmarshal(rec.Data, &n); err != nil {
		return netip.Prefix{}, err
	}
	p, err := netip.ParsePrefix(n.Spec.PodCIDR)
	if err != nil {
		return netip.Prefix{}, forbidden("node %s has no pod range", node)
	}
	return p.Masked(), nil
}

func (s *Server) ips(w http.ResponseWriter, r *http.Request, node string) error {
	cidr, err := s.nodePodCIDR(s.Store.Get, node)
	if err != nil {
		return err
	}
	all, err := s.Store.IPs(r.URL.Query().Get("kind"))
	if err != nil {
		return err
	}
	mine := map[string]string{}
	for uid, ip := range all {
		if a, err := netip.ParseAddr(ip); err == nil && cidr.Contains(a) {
			mine[uid] = ip
		}
	}
	writeJSON(w, http.StatusOK, mine)
	return nil
}

func (s *Server) alloc(w http.ResponseWriter, r *http.Request, node string) error {
	var req allocRequest
	if err := readJSON(r, &req); err != nil {
		return err
	}
	if req.Kind != "pod" || req.UID == "" {
		return invalid("nodes allocate pod addresses for a uid")
	}
	cidr, err := s.nodePodCIDR(s.Store.Get, node)
	if err != nil {
		return err
	}
	for _, c := range req.Candidates {
		if a, err := netip.ParseAddr(c); err != nil || !cidr.Contains(a) {
			return forbidden("address %s is not in node %s's pod range %s", c, node, cidr)
		}
	}
	ip, err := s.Store.AllocIP(req.Kind, req.UID, slices.Values(req.Candidates))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, allocResponse{IP: ip})
	return nil
}

func (s *Server) release(w http.ResponseWriter, r *http.Request, node string) error {
	var req releaseRequest
	if err := readJSON(r, &req); err != nil {
		return err
	}
	cidr, err := s.nodePodCIDR(s.Store.Get, node)
	if err != nil {
		return err
	}
	all, err := s.Store.IPs("pod")
	if err != nil {
		return err
	}
	if ip, ok := all[req.UID]; ok {
		if a, err := netip.ParseAddr(ip); err != nil || !cidr.Contains(a) {
			return forbidden("the address of %s (%s) is not in node %s's pod range", req.UID, ip, node)
		}
	}
	if err := s.Store.ReleaseIP(req.UID); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) transact(w http.ResponseWriter, r *http.Request, node string) error {
	var req txRequest
	if err := readJSON(r, &req); err != nil {
		return err
	}
	var out []wireRecord
	err := s.Store.Write(false, func(tx *store.Tx) error {
		out = out[:0]
		for _, rd := range req.Reads {
			cur, err := tx.Get(rd.Key.key())
			switch {
			case errors.Is(err, store.ErrNotFound):
				if rd.Exists {
					return errReadChanged
				}
			case err != nil:
				return err
			case !rd.Exists || cur.Rev != rd.Rev:
				return errReadChanged
			}
		}
		for _, wr := range req.Writes {
			rec, err := s.apply(tx, node, wr)
			if err != nil {
				return err
			}
			out = append(out, toWire(rec))
		}
		return nil
	})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, txResponse{Records: out})
	return nil
}

// apply checks one write against what a node may do (Kubernetes'
// NodeRestriction) and makes it.
func (s *Server) apply(tx *store.Tx, node string, wr txWrite) (store.Record, error) {
	key := wr.Key.key()
	res := apis.Lookup(key.Group, "v1", key.Resource)
	if res == nil {
		return store.Record{}, invalid("unknown resource %s", key)
	}
	obj := res.New()
	if err := json.Unmarshal(wr.Data, obj); err != nil {
		return store.Record{}, invalid("%s: %v", key, err)
	}
	m := obj.Meta()
	if m.Name != key.Name || m.Namespace != key.Namespace {
		return store.Record{}, invalid("the object (%s/%s) is not %s", m.Namespace, m.Name, key)
	}
	switch {
	case key.Group == "" && key.Resource == "nodes":
		if key.Name != node || wr.Op == "delete" {
			return store.Record{}, forbidden("node %s may only create or update its own Node", node)
		}
		n := obj.(*apis.Node)
		if cur, err := tx.Get(key); err == nil {
			var old apis.Node
			if err := json.Unmarshal(cur.Data, &old); err != nil {
				return store.Record{}, err
			}
			if old.Spec.PodCIDR != "" && n.Spec.PodCIDR != old.Spec.PodCIDR {
				return store.Record{}, forbidden("node %s cannot change its pod range from %s", node, old.Spec.PodCIDR)
			}
			// Scheduling settings belong to the administrator.
			n.Spec.Unschedulable, n.Spec.Taints = old.Spec.Unschedulable, old.Spec.Taints
		}
	case key.Group == "" && key.Resource == "pods":
		if wr.Op == "create" {
			return store.Record{}, forbidden("nodes do not create Pods")
		}
		cur, err := tx.Get(key)
		if err != nil {
			return store.Record{}, err
		}
		var old apis.Pod
		if err := json.Unmarshal(cur.Data, &old); err != nil {
			return store.Record{}, err
		}
		if old.Spec.NodeName != node || obj.(*apis.Pod).Spec.NodeName != node {
			return store.Record{}, forbidden("Pod %s/%s is not bound to node %s", key.Namespace, key.Name, node)
		}
	case key.Group == "" && key.Resource == "events":
		if wr.Op == "delete" {
			return store.Record{}, forbidden("nodes do not delete events")
		}
		ref := obj.(*apis.Event).InvolvedObject
		switch ref.Kind {
		case "Node":
			if ref.Name != node {
				return store.Record{}, forbidden("node %s cannot report events of node %s", node, ref.Name)
			}
		case "Pod":
			cur, err := tx.Get(store.Key{Resource: "pods", Namespace: ref.Namespace, Name: ref.Name})
			if err != nil {
				return store.Record{}, forbidden("Pod %s/%s of the event is not bound to node %s", ref.Namespace, ref.Name, node)
			}
			var pod apis.Pod
			if json.Unmarshal(cur.Data, &pod) != nil || pod.Spec.NodeName != node {
				return store.Record{}, forbidden("Pod %s/%s of the event is not bound to node %s", ref.Namespace, ref.Name, node)
			}
		default:
			return store.Record{}, forbidden("nodes report events of Pods and of themselves only")
		}
	default:
		return store.Record{}, forbidden("nodes cannot write %s", key)
	}
	switch wr.Op {
	case "create":
		return tx.Create(objects.Put(res, obj))
	case "update":
		rec, _, err := tx.Update(objects.Put(res, obj), wr.ExpectRev)
		return rec, err
	case "delete":
		return tx.Delete(key, wr.ExpectRev, objects.Encoder(obj))
	}
	return store.Record{}, invalid("unknown operation %q", wr.Op)
}

// Shutdown hooks are not needed: every handler returns when its request ends.
var _ http.Handler = (*Server)(nil)

// background is for tests that need a context.
var background = context.Background
