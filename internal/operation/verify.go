package operation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"shearwater/internal/store"
)

type snapshot struct {
	record      Record
	obs         Observation
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
	o, e := m.backend().Observe(tx, r)
	if e != nil {
		return s, e
	}
	if o.External {
		s.result.State = "stale"
		s.result.Next = "operator must review the external change; this operation cannot adopt it"
		return s, nil
	}
	s.result.State = "not_met"
	s.result.Next = "verify again while starting, or diagnose failure; never repeat application to wait"
	s.result.Facts = append(s.result.Facts, o.Facts...)
	if !o.Ready {
		return s, nil
	}
	s.obs = o
	s.fingerprint = o.Fingerprint
	s.result.State = "ready_to_probe"
	return s, nil
}

func (m *Manager) verify(ctx context.Context, id, token string, admin bool) (Result, error) {
	var before snapshot
	err := m.Store.Write(false, func(tx *store.Tx) error { var e error; before, e = m.snapshot(tx, id, token, admin); return e })
	if err != nil || before.result.State != "ready_to_probe" {
		return before.result, err
	}
	probe := m.Probe
	if probe == nil {
		managed := m.backend().Managed
		probe = func(ctx context.Context, ip string, port int32, c Check) HTTPProof {
			return probeHTTP(ctx, managed, ip, port, c)
		}
	}
	proof := Proof{At: m.now(), Generation: before.record.Generation, Image: before.obs.Image, Checks: before.record.GoalChecks}
	proof.ImagePinned = pinned(proof.Image)
	passed := true
	for _, rep := range before.obs.Replicas {
		pp := PodProof{UID: rep.UID, ContainerID: rep.ContainerID, ImageID: rep.ImageID}
		for _, c := range proof.Checks {
			hp := probe(ctx, rep.IP, rep.Port, c)
			pp.Checks = append(pp.Checks, hp)
			passed = passed && hp.Passed
		}
		proof.Pods = append(proof.Pods, pp)
	}
	if before.obs.FrontIP != "" {
		for _, c := range proof.Checks {
			hp := probe(ctx, before.obs.FrontIP, before.obs.FrontPort, c)
			proof.Service = append(proof.Service, hp)
			passed = passed && hp.Passed
		}
	}
	// Never let an empty replica set pass vacuously.
	passed = passed && len(proof.Pods) > 0
	scope := before.obs.Scope
	if scope == "" {
		scope = "new-release Pods directly and bound internal Service"
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
		result.HTTP = map[string]any{"state": "failed", "scope": scope}
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
			result.HTTP = map[string]any{"state": "passed", "scope": scope}
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

// Probe only addresses the backend manages. No DNS, environment proxy, cookies,
// credentials, redirects or unbounded response bodies enter this channel.
func probeHTTP(ctx context.Context, managed func(netip.Addr) bool, ip string, port int32, check Check) HTTPProof {
	r := HTTPProof{Path: check.Path}
	addr, e := netip.ParseAddr(ip)
	if e != nil || !addr.Is4() || managed == nil || !managed(addr) || port < 1 || port > 65535 {
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
	if e := m.backend().Diagnose(tx, r, result); e != nil {
		return e
	}
	result.Unknown = append(result.Unknown, "runtime failure reasons are observations, not a proven root cause; HTTP is not checked by diagnose")
	if r.LastGood != nil && r.Pending == "" {
		result.Next = "if recovery was requested, recover creates a plan for the last verified immutable image; otherwise verify"
	} else if r.Pending == "" {
		result.Next = "verify for actual HTTP evidence; no last verified recovery image exists"
	}
	return nil
}
