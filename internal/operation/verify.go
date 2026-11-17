package operation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"shearwater/internal/apis"
	"shearwater/internal/store"
)

type snapshot struct {
	record      Record
	deployment  apis.Deployment
	service     apis.Service
	pods        []apis.Pod
	ports       map[string]int32
	result      Result
	fingerprint string
}

func (m *Manager) snapshot(tx *store.Tx, id, token string, admin bool) (snapshot, error) {
	var s snapshot
	r, e := load(tx, id)
	if e != nil {
		return s, e
	}
	if e = m.authorize(r, token, admin); e != nil {
		return s, e
	}
	s.record = r
	s.result = summary(r)
	if r.Pending != "" || r.UID == "" {
		return s, nil
	}
	d, _, e := current(tx, r)
	if e != nil {
		return s, e
	}
	s.deployment = d
	if d.Metadata.Generation != r.Generation {
		s.result.State = "stale"
		s.result.Next = "operator must review the external change; this operation cannot adopt it"
		return s, nil
	}
	s.service, e = checkRoute(tx, r, d)
	if e != nil {
		return s, e
	}
	records, e := tx.ListObjects(r.Spec.Namespace)
	if e != nil {
		return s, e
	}
	sets := map[string]apis.ReplicaSet{}
	allPods := map[string]apis.Pod{}
	var endpoints []apis.EndpointSlice
	for _, rec := range records {
		switch rec.Key.Resource {
		case "replicasets":
			var rs apis.ReplicaSet
			if e = decode(rec, &rs); e != nil {
				return s, e
			}
			if owner(rs.Metadata, "Deployment", r.Spec.Name, r.UID) {
				sets[rs.Metadata.UID] = rs
			}
		case "pods":
			var p apis.Pod
			if e = decode(rec, &p); e != nil {
				return s, e
			}
			allPods[p.Metadata.UID] = p
		case "endpointslices":
			var sl apis.EndpointSlice
			if e = decode(rec, &sl); e != nil {
				return s, e
			}
			if owner(sl.Metadata, "Service", s.service.Metadata.Name, s.service.Metadata.UID) {
				endpoints = append(endpoints, sl)
			}
		}
	}
	s.ports = map[string]int32{}
	newPods := map[string]apis.Pod{}
	for _, p := range allPods {
		if p.Metadata.DeletionTimestamp != nil || !matches(r.Route.Selector, p.Metadata.Labels) {
			continue
		}
		var own *apis.ReplicaSet
		for _, rs := range sets {
			if owner(p.Metadata, "ReplicaSet", rs.Metadata.Name, rs.Metadata.UID) {
				copy := rs
				own = &copy
				break
			}
		}
		if own == nil {
			return s, fail("FOREIGN_BACKEND", "Service selects a Pod not owned by this app")
		}
		if !templateEqual(own.Spec.Template, d.Spec.Template) || own.Metadata.DeletionTimestamp != nil {
			continue
		}
		if len(p.Spec.Containers) != 1 || p.Spec.Containers[0].Image != d.Spec.Template.Spec.Containers[0].Image || p.Metadata.Annotations[Marker] != d.Spec.Template.Metadata.Annotations[Marker] {
			return s, fail("STALE", "new ReplicaSet Pod does not match selected release")
		}
		if !ready(p) {
			continue
		}
		ip, e := netip.ParseAddr(p.Status.PodIP)
		if e != nil || !ip.Is4() || !apis.PodCIDR.Contains(ip) {
			return s, fail("INVALID", "Pod address is outside the managed Pod network")
		}
		cs := p.Status.ContainerStatuses[0]
		if cs.ContainerID == "" || cs.ImageID == "" {
			continue
		}
		if pinned(p.Spec.Containers[0].Image) {
			_, wanted, _ := strings.Cut(p.Spec.Containers[0].Image, "@")
			_, actual, _ := strings.Cut(cs.ImageID, "@")
			if wanted != actual {
				continue
			}
		}
		port, e := targetPort(r.Route.Port, p.Spec)
		if e != nil {
			return s, e
		}
		newPods[p.Metadata.UID] = p
		s.ports[p.Metadata.UID] = port
	}
	want := *d.Spec.Replicas
	s.result.State = "not_met"
	s.result.Next = "verify again while starting, or diagnose failure; never repeat application to wait"
	s.result.Facts = append(s.result.Facts, fmt.Sprintf("goal generation=%d; ready new-release Pods=%d/%d", r.Generation, len(newPods), want))
	if int32(len(newPods)) != want || d.Status.ObservedGeneration != r.Generation || d.Status.UpdatedReplicas != want || d.Status.ReadyReplicas < want || d.Status.AvailableReplicas < want {
		return s, nil
	}
	covered := map[string]bool{}
	for _, sl := range endpoints {
		if sl.Metadata.DeletionTimestamp != nil || sl.AddressType != "IPv4" || sl.Metadata.Annotations[apis.ServiceGenerationAnnotation] != strconv.FormatInt(s.service.Metadata.Generation, 10) {
			continue
		}
		for _, ep := range sl.Endpoints {
			if ep.Conditions.Ready == nil || !*ep.Conditions.Ready || ep.Conditions.Terminating != nil && *ep.Conditions.Terminating {
				continue
			}
			ref := ep.TargetRef
			if ref == nil {
				return s, fail("FOREIGN_BACKEND", "endpoint has no Pod identity")
			}
			p, ok := newPods[ref.UID]
			if !ok {
				s.result.Facts = append(s.result.Facts, "Service still contains an older or unverified backend")
				return s, nil
			}
			if ref.Kind != "Pod" || ref.Namespace != r.Spec.Namespace || ref.Name != p.Metadata.Name || len(ep.Addresses) != 1 || ep.Addresses[0] != p.Status.PodIP {
				return s, fail("FOREIGN_BACKEND", "endpoint identity/address mismatch")
			}
			portOK := false
			for _, port := range sl.Ports {
				name := ""
				if port.Name != nil {
					name = *port.Name
				}
				if name == r.Route.Port.Name && port.Protocol != nil && *port.Protocol == "TCP" && port.Port != nil && *port.Port == s.ports[p.Metadata.UID] {
					portOK = true
				}
			}
			if portOK {
				covered[p.Metadata.UID] = true
			}
		}
	}
	if len(covered) != len(newPods) {
		s.result.Facts = append(s.result.Facts, "Service endpoints have not converged to every new-release Pod")
		return s, nil
	}
	for _, p := range newPods {
		s.pods = append(s.pods, p)
	}
	slices.SortFunc(s.pods, func(a, b apis.Pod) int { return strings.Compare(a.Metadata.UID, b.Metadata.UID) })
	// A normal heartbeat may change resourceVersion without changing the
	// release. Pin the meaningful route, generation, Pod/IP/container identities
	// instead, and compare them again after network I/O before saving proof.
	var identities []any
	for _, p := range s.pods {
		identities = append(identities, []any{p.Metadata.UID, p.Status.PodIP, p.Status.ContainerStatuses[0].ContainerID, p.Status.ContainerStatuses[0].ImageID, s.ports[p.Metadata.UID]})
	}
	s.fingerprint = digest([]any{r.UID, r.Generation, d.Spec, r.Route, r.GoalChecks, identities})
	s.result.State = "ready_to_probe"
	return s, nil
}

func ready(p apis.Pod) bool {
	if p.Status.Phase != "Running" || len(p.Status.ContainerStatuses) != 1 || !p.Status.ContainerStatuses[0].Ready || p.Status.ContainerStatuses[0].State.Running == nil {
		return false
	}
	for _, c := range p.Status.Conditions {
		if c.Type == "Ready" && c.Status == "True" {
			return true
		}
	}
	return false
}

func (m *Manager) verify(ctx context.Context, id, token string, admin bool) (Result, error) {
	var before snapshot
	err := m.Store.Write(false, func(tx *store.Tx) error { var e error; before, e = m.snapshot(tx, id, token, admin); return e })
	if err != nil || before.result.State != "ready_to_probe" {
		return before.result, err
	}
	probe := m.Probe
	if probe == nil {
		probe = probeHTTP
	}
	proof := Proof{At: m.now(), Generation: before.record.Generation, Image: before.deployment.Spec.Template.Spec.Containers[0].Image, Checks: before.record.GoalChecks}
	proof.ImagePinned = pinned(proof.Image)
	passed := true
	for _, p := range before.pods {
		cs := p.Status.ContainerStatuses[0]
		pp := PodProof{UID: p.Metadata.UID, ContainerID: cs.ContainerID, ImageID: cs.ImageID}
		for _, c := range proof.Checks {
			hp := probe(ctx, p.Status.PodIP, before.ports[p.Metadata.UID], c)
			pp.Checks = append(pp.Checks, hp)
			passed = passed && hp.Passed
		}
		proof.Pods = append(proof.Pods, pp)
	}
	for _, c := range proof.Checks {
		hp := probe(ctx, before.record.Route.IP, before.record.Route.Port.Port, c)
		proof.Service = append(proof.Service, hp)
		passed = passed && hp.Passed
	}
	result := before.result
	err = m.Store.Write(false, func(tx *store.Tx) error {
		after, e := m.snapshot(tx, id, token, admin)
		if e != nil {
			return e
		}
		if after.result.State != "ready_to_probe" || after.fingerprint != before.fingerprint {
			result = after.result
			result.State = "stale"
			result.Next = "state changed during HTTP checks; verify again without applying"
			return nil
		}
		result = after.result
		result.State = "http_failed"
		result.HTTP = map[string]any{"state": "failed", "scope": "new-release Pods directly and bound internal Service"}
		result.ProofID = digest(proof)
		result.CheckedAt = &proof.At
		result.VerifiedPods = len(proof.Pods)
		if admin {
			result.Evidence = &proof
		}
		after.record.LastCheck = &proof
		result.Next = "diagnose the failed check; recovery needs a newly approved plan"
		if passed {
			result.State = "healthy"
			result.VerifiedImage = proof.Image
			result.HTTP = map[string]any{"state": "passed", "scope": "new-release Pods directly and bound internal Service"}
			result.Next = "goal verified; stop calling tools"
			if after.record.StagedImage != "" {
				// A healthy deployed image does not complete a request to update
				// to the staged image. Keep that distinction in machine-readable
				// output and in the next step, not only in model instructions.
				result.Next = "the CURRENT deployed image passed; stagedImage is NOT planned or deployed. If update was requested, call update to SAVE its plan, then stop for approval"
			}
			result.Facts = append(result.Facts, "every selected new-release Pod and the internal Service passed the configured HTTP checks")
			if !proof.ImagePinned {
				result.Unknown = append(result.Unknown, "image tag is mutable; image content is not pinned by the approved reference")
			}
			// Recovery uses the actual resolved immutable image, including for an
			// initially connected tag. Reject heterogeneous digests as a recovery
			// source; never pretend a mutable tag is a durable rollback image.
			resolved := ""
			for _, p := range proof.Pods {
				if !pinned(p.ImageID) || resolved != "" && resolved != p.ImageID {
					resolved = ""
					break
				}
				resolved = p.ImageID
			}
			if resolved != "" {
				good := proof
				good.Image = resolved
				good.ImagePinned = true
				after.record.LastGood = &good
				if e = save(tx, after.record, false); e != nil {
					return e
				}
			} else {
				result.Unknown = append(result.Unknown, "no uniform immutable image digest available for recovery")
			}
		}
		return save(tx, after.record, false)
	})
	return result, err
}

// Probe only already-validated managed IPs. No DNS, environment proxy, cookies,
// credentials, redirects or unbounded response bodies enter this channel.
func probeHTTP(ctx context.Context, ip string, port int32, check Check) HTTPProof {
	r := HTTPProof{Path: check.Path}
	addr, e := netip.ParseAddr(ip)
	if e != nil || !addr.Is4() || (!apis.PodCIDR.Contains(addr) && !apis.ServiceCIDR.Contains(addr)) || port < 1 || port > 65535 {
		r.Reason = "invalid_managed_address"
		return r
	}
	transport := &http.Transport{DisableKeepAlives: true, DialContext: (&net.Dialer{Timeout: time.Second}).DialContext}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, e := http.NewRequestWithContext(ctx, "GET", "http://"+net.JoinHostPort(ip, strconv.Itoa(int(port)))+check.Path, nil)
	if e != nil {
		r.Reason = "invalid_request"
		return r
	}
	resp, e := client.Do(req)
	if e != nil {
		r.Reason = "connection_failed"
		return r
	}
	defer resp.Body.Close()
	r.Status = resp.StatusCode
	b, e := io.ReadAll(io.LimitReader(resp.Body, (64<<10)+1))
	r.Bytes = len(b)
	if e != nil {
		r.Reason = "read_failed"
		return r
	}
	if len(b) > 64<<10 {
		r.Reason = "body_too_large"
		return r
	}
	h := sha256.Sum256(b)
	r.SHA256 = hex.EncodeToString(h[:])
	if r.Status != 200 {
		r.Reason = "unexpected_status"
		return r
	}
	if check.Contains != "" && !strings.Contains(string(b), check.Contains) {
		r.Reason = "expected_content_missing"
		return r
	}
	r.Passed = true
	return r
}

func (m *Manager) diagnose(tx *store.Tx, r Record, result *Result) error {
	if r.UID == "" {
		result.Facts = append(result.Facts, "the selected app has not been deployed")
		return nil
	}
	d, _, e := current(tx, r)
	if e != nil {
		return e
	}
	records, e := tx.ListObjects(r.Spec.Namespace)
	if e != nil {
		return e
	}
	sets := map[string]string{}
	for _, rec := range records {
		if rec.Key.Resource == "replicasets" {
			var rs apis.ReplicaSet
			if e = decode(rec, &rs); e != nil {
				return e
			}
			if owner(rs.Metadata, "Deployment", r.Spec.Name, r.UID) {
				sets[rs.Metadata.UID] = rs.Metadata.Name
			}
		}
	}
	result.Facts = append(result.Facts, fmt.Sprintf("observed generation=%d; intended=%d; ready replicas=%d", d.Metadata.Generation, r.Generation, d.Status.ReadyReplicas))
	count := 0
	for _, rec := range records {
		if rec.Key.Resource != "pods" {
			continue
		}
		var p apis.Pod
		if e = decode(rec, &p); e != nil {
			return e
		}
		owned := false
		for uid, name := range sets {
			if owner(p.Metadata, "ReplicaSet", name, uid) {
				owned = true
			}
		}
		if !owned || count >= 8 {
			continue
		}
		count++
		for _, c := range p.Status.ContainerStatuses {
			reason := "running"
			if c.State.Waiting != nil {
				reason = c.State.Waiting.Reason
			}
			if c.State.Terminated != nil {
				reason = fmt.Sprintf("terminated exit=%d", c.State.Terminated.ExitCode)
			}
			if len(reason) > 80 {
				reason = reason[:80]
			}
			result.Facts = append(result.Facts, fmt.Sprintf("Pod %s: ready=%t restarts=%d state=%q", p.Metadata.Name, c.Ready, c.RestartCount, reason))
		}
	}
	result.Unknown = append(result.Unknown, "runtime failure reasons are observations, not a proven root cause; HTTP is not checked by diagnose")
	if r.LastGood != nil && r.Pending == "" {
		result.Next = "if recovery was requested, recover creates a plan for the last verified immutable image; otherwise verify"
	} else if r.Pending == "" {
		result.Next = "verify for actual HTTP evidence; no last verified recovery image exists"
	}
	return nil
}
