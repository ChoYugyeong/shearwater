package controller

import (
	"cmp"
	"math"
	"strconv"
	"strings"
	"time"

	"shearwater/internal/apis"
	"shearwater/internal/objects"
	"shearwater/internal/store"
)

const (
	revision        = apis.DeploymentRevisionAnnotation
	revisionHistory = apis.DeploymentRevisionHistoryAnnotation
	desiredReplicas = apis.DeploymentDesiredReplicasAnnotation
)

func revisionOf(meta *apis.ObjectMeta) int64 {
	n, err := strconv.ParseInt(meta.Annotations[revision], 10, 64)
	if err != nil {
		return 0
	}
	return max(0, n)
}

func compareRevision(a, b *rolloutSet) int {
	am, bm := a.item.obj.Meta(), b.item.obj.Meta()
	if c := cmp.Compare(replicaSetRevision(a), replicaSetRevision(b)); c != 0 {
		return c
	}
	if am.CreationTimestamp != nil && bm.CreationTimestamp != nil {
		if c := am.CreationTimestamp.Compare(bm.CreationTimestamp.Time); c != 0 {
			return c
		}
	}
	return cmp.Compare(am.Name, bm.Name)
}

func replicaSetRevision(s *rolloutSet) int64 {
	n := revisionOf(s.item.obj.Meta())
	if n > s.item.rec.Rev {
		return 0 // Not a controller-issued revision, including legacy writable annotations.
	}
	return n
}

func deploymentRevision(previous int64, current *rolloutSet, old []*rolloutSet) (int64, bool) {
	var maxOld int64
	for _, s := range old {
		maxOld = max(maxOld, replicaSetRevision(s))
	}
	// The store preserves the last allocated number independently of the
	// Deployment annotation. Existing M3 ReplicaSets seed it on upgrade.
	latest := max(maxOld, previous)
	if current != nil {
		if n := replicaSetRevision(current); n >= latest && n > maxOld {
			return n, true
		}
	}
	if latest == math.MaxInt64 {
		return 0, false
	}
	return latest + 1, true
}

func annotate(meta *apis.ObjectMeta, key, value string) {
	if meta.Annotations == nil {
		meta.Annotations = make(map[string]string)
	}
	meta.Annotations[key] = value
}

func setRevision(meta *apis.ObjectMeta, n int64) {
	annotate(meta, revision, strconv.FormatInt(n, 10))
}

func setDesiredReplicas(meta *apis.ObjectMeta, n int64) {
	annotate(meta, desiredReplicas, strconv.FormatInt(n, 10))
}

func syncRevision(rs, deployment *apis.ObjectMeta, previous, n int64) {
	if previous > 0 && previous != n {
		history := rs.Annotations[revisionHistory]
		number := strconv.FormatInt(previous, 10)
		if history != "" {
			number = history + "," + number
		}
		// Keep the history annotation bounded when the same template is reused.
		for len(number) > 2000 {
			_, number, _ = strings.Cut(number, ",")
		}
		annotate(rs, revisionHistory, number)
	}
	setRevision(rs, n)
	for k, v := range deployment.Annotations {
		switch k {
		case revision, revisionHistory, desiredReplicas, "deployment.kubernetes.io/max-replicas",
			"kubectl.kubernetes.io/last-applied-configuration", "deprecated.deployment.rollback.to":
			continue
		}
		annotate(rs, k, v)
	}
}

func pruneHistory(tx *store.Tx, d *apis.Deployment, old []*rolloutSet, now time.Time) error {
	limit := int32(10)
	if d.Spec.RevisionHistoryLimit != nil {
		limit = *d.Spec.RevisionHistoryLimit
	}
	remaining := -int64(limit)
	for _, s := range old {
		if !s.item.rec.Deleting {
			remaining++
		}
	}
	for _, s := range old {
		if remaining <= 0 {
			break
		}
		i := s.item
		if i.rec.Deleting || s.live != 0 || s.target != 0 {
			continue
		}
		if _, _, err := objects.Delete(tx, i.res, i.rec, i.obj, "", nil, now); err != nil {
			return err
		}
		// The ReplicaSet loop shares this view. Skip this set later in the pass
		// whether deletion was immediate or a finalizer holds it.
		i.rec.Deleting = true
		remaining--
	}
	return nil
}
