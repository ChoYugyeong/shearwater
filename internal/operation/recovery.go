package operation

import (
	"context"
	"errors"
	"slices"

	"shearwater/internal/store"
)

// Only the immediately linked child may be resumed by a duplicate continue
// of its parent. An unrelated later approval must never be substituted.
func continuationPlan(r Record, hash string) string {
	if len(r.Plans) > 0 && hash != "" {
		p := r.Plans[len(r.Plans)-1]
		if p.AutoRecoverOf == hash && (r.Pending == p.Hash || p.Applied) {
			return p.Hash
		}
	}
	return hash
}

func linkedRecovery(r Record, hash string) *Recovery {
	if hash != "" {
		for i := range r.Plans {
			if r.Plans[i].Hash == hash {
				return r.Plans[i].Recovery
			}
		}
	}
	return nil
}

func attachRecovery(r Record, result *Result) {
	if len(r.Plans) == 0 {
		return
	}
	p := r.Plans[len(r.Plans)-1]
	report := p.Recovery
	if p.AutoRecoverOf != "" {
		report = linkedRecovery(r, p.AutoRecoverOf)
	}
	if report != nil {
		describeRecovery(report, result)
	}
}

func describeRecovery(report *Recovery, result *Result) {
	result.Recovery = report
	result.Facts = append(result.Facts, "update to "+report.UpdateImage+" failed verification ("+report.UpdateState+")")
	for _, fact := range report.UpdateFacts {
		result.Facts = append(result.Facts, "failed update: "+fact)
	}
	switch report.State {
	case "verifying", "healthy", "http_failed", "rollout_failed", "stale":
		result.Facts = append(result.Facts, "server automatically returned to "+report.Image+"; recovery verification: "+report.State)
	case "planned":
		result.Facts = append(result.Facts, "server planned automatic recovery to "+report.Image)
	default:
		result.Facts = append(result.Facts, "automatic recovery: "+report.State)
	}
	for _, fact := range report.Facts {
		result.Facts = append(result.Facts, "recovery: "+fact)
	}
}

// finishVerification closes the first definitive observation of an applied
// plan. Failure facts, the parent/child link, the approved recovery plan and
// its lease commit with the proof, before any external execution starts.
func (m *Manager) finishVerification(tx *store.Tx, r *Record, result *Result) error {
	if len(r.Plans) == 0 || r.Pending != "" {
		return nil
	}
	i := len(r.Plans) - 1
	p := &r.Plans[i]
	if !p.Applied || p.Generation != r.Generation || p.Verification != "" {
		return nil
	}
	p.Verification = result.State
	if report := linkedRecovery(*r, p.AutoRecoverOf); report != nil {
		report.State, report.ProofID = result.State, result.ProofID
		// Keep only the recovery generation's fresh facts here.
		report.Facts = slices.Clone(result.Facts)
		return nil
	}
	if result.State == "healthy" || p.Action != "update" || !p.Approved || !r.Spec.AutoRecover {
		return nil
	}
	report := &Recovery{UpdatePlan: p.Hash, UpdateImage: p.Image, UpdateState: result.State, UpdateProof: result.ProofID, UpdateFacts: slices.Clone(result.Facts), State: "skipped"}
	p.Recovery = report
	switch {
	case r.Revoked || !m.now().Before(r.Spec.Expires) || !slices.Contains(r.Spec.Allow, "recover"):
		report.Facts = []string{"delegation no longer permits a recovery"}
	case result.State == "stale":
		report.Facts = []string{"app identity, release or route changed; operator review required"}
	case r.LastGood == nil:
		report.Facts = []string{"no previously HTTP-verified immutable image is available"}
	default:
		l, err := appLease(tx, *r)
		if err != nil {
			return err
		}
		if l.Metadata.UID != p.Lease || l.Spec.HolderIdentity != "task:"+r.ID {
			report.Facts = []string{"app lease was released or taken over"}
			break
		}
		err = m.plan(tx, r, Request{Action: "recover"}, p.Hash)
		if err != nil {
			var rejected *Error
			if !errors.As(err, &rejected) && !errors.Is(err, store.ErrNotFound) {
				return err
			}
			report.Facts = []string{bounded(err.Error())}
			break
		}
		child := r.Plans[len(r.Plans)-1]
		report.State, report.Plan, report.Image = "planned", child.Hash, child.Image
	}
	return nil
}

func (m *Manager) runRecovery(ctx context.Context, id, token string, admin bool, hash string) (Result, error) {
	result, err := m.Act(ctx, id, token, admin, Request{Action: "continue", Plan: hash})
	if err == nil || result.Recovery == nil || result.Recovery.Plan != hash {
		return result, err
	}
	switch result.Recovery.State {
	case "execution_uncertain":
		result.State = "auto_recovery_interrupted"
	case "execution_failed", "expired":
		result.State = "auto_recovery_failed"
	default:
		return result, err
	}
	result.Next = "inspect the automatic recovery receipt and execution error before further action"
	result.Facts = append(result.Facts, bounded(err.Error()))
	return result, nil
}
