package controller

import (
	"math"
	"slices"
	"strconv"
	"strings"

	"shearwater/internal/apis"
	"shearwater/internal/store"
)

// A rollout counts non-terminal Pods against the surge cap until they are
// gone, including terminating Pods. Only active Ready Pods are available.
type rolloutSet struct {
	item     *item
	selector apis.Selector
	pods     []*item
	live     int64
	ready    int64
	target   int64
}

type rolloutSlots struct {
	deploymentUID string
	remaining     int64
}

func (s *rolloutSet) active() int64 { return int64(len(s.pods)) }

func (s *rolloutSet) writeTarget(tx *store.Tx, deploymentDesired int64) error {
	if s.item.rec.Deleting {
		return nil
	}
	rs := s.item.obj.(*apis.ReplicaSet)
	target := int32(s.target)
	if replicas(rs.Spec.Replicas) != target {
		rs.Spec.Replicas = &target
		rs.Metadata.Generation++
	}
	setDesiredReplicas(rs.Meta(), deploymentDesired)
	return s.item.update(tx)
}

func rolloutBudget(strategy apis.DeploymentStrategy, desired int64) (surge, unavailable int64) {
	if strategy.Type == "Recreate" || desired == 0 {
		return 0, 0
	}
	var maxSurge, maxUnavailable *apis.IntOrString
	if r := strategy.RollingUpdate; r != nil {
		maxSurge, maxUnavailable = r.MaxSurge, r.MaxUnavailable
	}
	surge = scaleBudget(maxSurge, desired, true)
	unavailable = min(desired, scaleBudget(maxUnavailable, desired, false))
	// A positive percentage rounded down can give zero with zero surge.
	// Keep the rollout able to progress, as in Kubernetes' ResolveFenceposts.
	if surge == 0 && unavailable == 0 {
		unavailable = 1
	}
	return surge, unavailable
}

func scaleBudget(value *apis.IntOrString, desired int64, roundUp bool) int64 {
	if value != nil && !value.IsString {
		return max(0, int64(value.Int))
	}
	percent := int64(25)
	if value != nil {
		p, err := strconv.ParseInt(strings.TrimSuffix(value.Str, "%"), 10, 64)
		if err != nil || p < 0 {
			return 0
		}
		percent = p
	}
	// Bound the multiplication before rounding, including legacy inputs that
	// accepted percentages much larger than the replica count can represent.
	scaled := min(percent, int64(math.MaxInt32)*100/desired+1) * desired
	if roundUp {
		scaled += 99
	}
	return min(scaled/100, math.MaxInt32)
}

func planRecreate(current *rolloutSet, old []*rolloutSet, desired, orphaned int64) {
	current.target = desired
	for _, s := range old {
		s.target = 0
		orphaned += s.live
	}
	if orphaned > 0 {
		current.target = min(desired, current.active())
	}
}

func planRolling(current *rolloutSet, old []*rolloutSet, desired, slots, unavailable int64) {
	// Only the current ReplicaSet can add Pods. Do not spend slots from Pods
	// about to be marked for deletion until the node agent has removed them.
	current.target = min(desired, current.active()+slots)

	// Account for any current-set scale-down before sharing the available
	// budget across the old sets. Existing unhealthy replicas can be removed
	// without consuming the Ready budget.
	currentReady := current.ready
	if remove := current.active() - current.target; remove > 0 {
		slices.SortStableFunc(current.pods, scaleDownOrder)
		for _, p := range current.pods[:remove] {
			if podReady(p.obj.(*apis.Pod)) {
				currentReady--
			}
		}
	}
	var oldReady, oldActive int64
	for _, s := range old {
		if !s.item.rec.Deleting {
			s.target = s.active()
			oldActive += s.target
			oldReady += s.ready
			slices.SortStableFunc(s.pods, scaleDownOrder)
		}
	}
	minReady := desired - unavailable
	spare := max(0, currentReady+oldReady-minReady)
	// Even an unready old Pod could recover. Hold enough old replicas to
	// reach the availability floor without assuming the new Pods will start.
	drop := max(0, currentReady+oldActive-minReady)
	// Clear unready intermediate revisions first, keeping older serving
	// revisions until the current one has Ready replacements.
	for j := len(old) - 1; j >= 0 && drop > 0; j-- {
		s := old[j]
		if s.item.rec.Deleting {
			continue
		}
		for s.target > 0 && drop > 0 && !podReady(s.pods[s.active()-s.target].obj.(*apis.Pod)) {
			s.target--
			drop--
		}
	}
	for _, s := range old {
		if s.item.rec.Deleting {
			continue
		}
		for s.target > 0 && drop > 0 {
			p := s.pods[s.active()-s.target]
			if podReady(p.obj.(*apis.Pod)) {
				if spare == 0 {
					break
				}
				spare--
			}
			s.target--
			drop--
		}
	}
}

// Pause freezes template reconciliation. A replica count change still scales
// the existing revisions, preserving their current targets on a plain pause.
func planPaused(current *rolloutSet, old []*rolloutSet, desired int64) {
	var sets []*rolloutSet
	var total int64
	for _, s := range withCurrent(current, old) {
		if !s.item.rec.Deleting {
			sets = append(sets, s)
			total += s.target
		}
	}
	if len(sets) == 0 {
		return
	}
	slices.SortStableFunc(sets, compareRevision)
	latest := sets[len(sets)-1]
	previous, err := strconv.ParseInt(latest.item.obj.Meta().Annotations[desiredReplicas], 10, 32)
	if err != nil {
		previous = total // ReplicaSets created before M4 did not record it.
	}
	if previous == desired {
		return
	}
	if total < desired {
		latest.target += desired - total
		return
	}
	// On scale down reduce the newest revision first to keep older Pods
	// serving while the template is paused.
	for j := len(sets) - 1; j >= 0 && total > desired; j-- {
		remove := min(sets[j].target, total-desired)
		sets[j].target -= remove
		total -= remove
	}
}
