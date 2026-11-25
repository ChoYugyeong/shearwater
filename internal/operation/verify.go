package operation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"slices"
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
	s.result.Lease, e = m.refreshLease(tx, r)
	if e != nil {
		return s, e
	}
	if r.Pending != "" || r.UID == "" {
		return s, nil
	}
	// A recovery report's retained facts are attached after the observation.
	// Keep the fresh facts separate when recording the recovery's own proof.
	if s.result.Recovery != nil {
		s.result.Facts = nil
	}
	o, e := m.backend().Observe(tx, r)
	var invalid *Error
	if errors.Is(e, store.ErrNotFound) || errors.As(e, &invalid) && (invalid.Code == "STALE" || invalid.Code == "FOREIGN_BACKEND" || invalid.Code == "UNSUPPORTED" || invalid.Code == "INVALID") {
		o.External = true
		o.Facts = append(o.Facts, bounded(e.Error()))
		e = nil
	}
	if e != nil {
		return s, e
	}
	if o.External {
		s.result.State = "stale"
		s.result.Facts = append(s.result.Facts, o.Facts...)
		s.result.Facts = append(s.result.Facts, "the bound release or route changed outside this operation")
		s.result.Next = "operator must review the external change; this operation cannot adopt it"
		return s, nil
	}
	s.result.State = "not_met"
	s.result.Next = "verify again while starting, or diagnose failure; never repeat application to wait"
	s.result.Facts = append(s.result.Facts, o.Facts...)
	if o.Failed {
		s.result.State = "rollout_failed"
		s.result.Next = "inspect the terminal rollout failure and recovery evidence"
		return s, nil
	}
	if !o.Ready {
		return s, nil
	}
	s.obs = o
	s.fingerprint = o.Fingerprint
	s.result.State = "ready_to_probe"
	return s, nil
}

func (m *Manager) verify(ctx context.Context, id, token string, admin bool, expected string) (Result, error) {
	var before snapshot
	var child string
	err := m.Store.WriteAs(actorFor(id, admin), false, func(tx *store.Tx) error {
		var e error
		before, e = m.snapshot(tx, id, token, admin)
		if e != nil {
			return e
		}
		if expected != "" && before.result.LastPlan != expected {
			before.result.State = "superseded"
			return nil
		}
		if p := before.result.Review; p != nil && p.AutoRecoverOf != "" && p.Approved && p.Failure == "" && !before.record.Revoked && m.now().Before(before.record.Spec.Expires) {
			child = p.Hash
		}
		switch before.result.State {
		case "rollout_failed", "stale":
			return m.recordVerification(tx, &before, &before.result, &child)
		case "not_met":
			attachRecovery(before.record, &before.result)
		}
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	if child != "" {
		return m.runRecovery(ctx, id, token, admin, child)
	}
	if before.result.State != "ready_to_probe" {
		return before.result, nil
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
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	// Never let an empty replica set pass vacuously.
	passed = passed && len(proof.Pods) > 0
	// Each backend names what its proof covered (Pods and a Service for the
	// engine, containers and a host port for Compose). Facts reuse it, so a
	// model never reports components the app does not have.
	scope := before.obs.Scope
	if scope == "" {
		scope = "every ready replica directly"
	}
	result := before.result
	err = m.Store.WriteAs(actorFor(id, admin), false, func(tx *store.Tx) error {
		after, e := m.snapshot(tx, id, token, admin)
		if e != nil {
			return e
		}
		if after.result.State != "ready_to_probe" || after.fingerprint != before.fingerprint || after.record.Generation != before.record.Generation || after.result.LastPlan != before.result.LastPlan {
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
		if !passed {
			_, why := proofFacts(&proof)
			result.Facts = append(result.Facts, why...)
		}
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
			result.Facts = append(result.Facts, "the configured HTTP checks passed for "+scope)
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
			} else {
				result.Unknown = append(result.Unknown, "no uniform immutable image digest available for recovery")
			}
		}
		return m.recordVerification(tx, &after, &result, &child)
	})
	if err == nil && child != "" {
		return m.runRecovery(ctx, id, token, admin, child)
	}
	return result, err
}

func (m *Manager) recordVerification(tx *store.Tx, s *snapshot, result *Result, child *string) error {
	if err := m.finishVerification(tx, &s.record, result); err != nil {
		return err
	}
	if err := save(tx, s.record, false); err != nil {
		return err
	}
	attachRecovery(s.record, result)
	if report := result.Recovery; report != nil {
		switch {
		case report.State == "planned" && s.record.Pending == report.Plan:
			*child = report.Plan
		case report.State == "skipped":
			result.Next = "automatic recovery skipped; inspect the update failure and the recorded reason"
		case report.State == "healthy" && result.State == "healthy":
			result.State = "auto_recovered"
			result.Next = "report the failed update and the verified recovery image"
		case report.State == "http_failed" || report.State == "rollout_failed" || report.State == "stale":
			result.Next = "report the failed update and recovery verification; automatic recovery has stopped"
			if result.State == report.State {
				result.State = "auto_recovery_failed"
			}
		}
	}
	var err error
	result.Lease, err = m.refreshLease(tx, s.record)
	return err
}

// reasonText turns a probe reason code into words a person or model can act on.
var reasonText = map[string]string{
	"expected_content_missing": "expected text missing",
	"unexpected_status":        "status was not 200",
	"connection_failed":        "connection failed",
	"read_failed":              "reading the body failed",
	"body_too_large":           "body larger than 64 KiB",
	"invalid_managed_address":  "address is not a managed app address",
	"invalid_request":          "request could not be built",
}

func probeWords(p HTTPProof) string {
	why := reasonText[p.Reason]
	if why == "" {
		why = strings.ReplaceAll(p.Reason, "_", " ")
	}
	if p.Status > 0 {
		return "HTTP " + strconv.Itoa(p.Status) + ", " + why
	}
	return why
}

// proofFacts reports whether every probe in a proof passed and, for each
// check that failed, where it failed and why. It names the path, the status
// and the reason, never the expected text: that may come from a private checks
// file the agent is not shown.
func proofFacts(p *Proof) (bool, []string) {
	passed := len(p.Pods) > 0
	var facts []string
	for i, c := range p.Checks {
		failed, reasons := 0, []string{}
		for _, pod := range p.Pods {
			if i >= len(pod.Checks) || pod.Checks[i].Passed {
				continue
			}
			failed++
			if w := probeWords(pod.Checks[i]); !slices.Contains(reasons, w) {
				reasons = append(reasons, w)
			}
		}
		if failed > 0 {
			passed = false
			facts = append(facts, "check GET "+c.Path+" failed on "+strconv.Itoa(failed)+" of "+strconv.Itoa(len(p.Pods))+" replicas: "+strings.Join(reasons, "; "))
		}
		if i < len(p.Service) && !p.Service[i].Passed {
			passed = false
			facts = append(facts, "check GET "+c.Path+" failed on the app's front address: "+probeWords(p.Service[i]))
		}
	}
	if len(p.Pods) == 0 {
		facts = append(facts, "no ready replica was checked")
	}
	return passed, facts
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
	// The last HTTP check of the current goal is the most useful observation
	// after http_failed. Report it as retained, with its time, not as current.
	if c := r.LastCheck; c != nil && c.Generation == r.Generation {
		if ok, why := proofFacts(c); !ok {
			at := c.At.UTC().Format(time.RFC3339)
			for _, w := range why {
				result.Facts = append(result.Facts, "last HTTP check ("+at+"): "+w)
			}
		}
	}
	if r.LastGood != nil && r.Pending == "" {
		result.Next = "if recovery was requested, recover creates a plan for the last verified immutable image; otherwise verify"
	} else if r.Pending == "" {
		result.Next = "verify for actual HTTP evidence; no last verified recovery image exists"
	}
	return nil
}
