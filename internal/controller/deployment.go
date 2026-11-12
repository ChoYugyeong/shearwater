package controller

import (
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"maps"
	"math"
	"reflect"
	"slices"
	"time"

	"shearwater/internal/apis"
	"shearwater/internal/objects"
	"shearwater/internal/store"
)

const templateHash = "pod-template-hash"

var rsResource = apis.Lookup("apps", "v1", "replicasets")

func (m Manager) deployment(tx *store.Tx, i *item, v view, now time.Time) error {
	d := i.obj.(*apis.Deployment)
	match, err := apis.SelectorFrom(d.Spec.Selector)
	if err != nil {
		return err
	}
	template, selector, hash, err := deploymentTemplate(d, match)
	if err != nil {
		return err
	}
	current, old, orphaned, err := deploymentReplicaSets(i, v, match, template, selector)
	if err != nil {
		return err
	}
	desired := int64(replicas(d.Spec.Replicas))
	surge, unavailable := rolloutBudget(d.Spec.Strategy, desired)
	slots := &rolloutSlots{deploymentUID: i.rec.UID, remaining: desired + surge - orphaned}
	for _, s := range withCurrent(current, old) {
		slots.remaining -= s.live
	}
	slots.remaining = max(0, slots.remaining)
	reason := ""

	if d.Spec.Paused {
		planPaused(current, old, desired)
	} else {
		previous, err := tx.DeploymentRevision(i.rec.UID)
		if err != nil {
			return err
		}
		num, ok := deploymentRevision(previous, current, old)
		if !ok {
			reason = "RevisionLimitReached"
		} else {
			if current == nil {
				current = &rolloutSet{}
			}
			if d.Spec.Strategy.Type == "Recreate" {
				planRecreate(current, old, desired, orphaned)
			} else {
				planRolling(current, old, desired, slots.remaining, unavailable)
			}
			if current.item == nil {
				current.item, err = createDeploymentReplicaSet(tx, i, template, selector, hash, int32(current.target), num)
				if err != nil {
					return err
				}
			} else {
				syncRevision(current.item.obj.Meta(), &d.Metadata, replicaSetRevision(current), num)
			}
			setRevision(&d.Metadata, num)
			if num != previous {
				if err := tx.SetDeploymentRevision(i.rec.UID, num); err != nil {
					return err
				}
			}
		}
	}

	// A new set is first seen by the ReplicaSet loop on the next pass.
	for _, s := range withCurrent(current, old) {
		v.rolloutSlots[s.item.rec.UID] = slots
		if err := s.writeTarget(tx, desired); err != nil {
			return err
		}
	}
	if updateDeploymentStatus(d, current, old, desired, unavailable, orphaned, reason, now) && !d.Spec.Paused {
		if err := pruneHistory(tx, d, old, now); err != nil {
			return err
		}
	}
	return i.update(tx)
}

func deploymentTemplate(d *apis.Deployment, match apis.Selector) (apis.PodTemplateSpec, *apis.LabelSelector, string, error) {
	template := d.Spec.Template
	data, err := json.Marshal(template)
	if err != nil {
		return template, nil, "", err
	}
	h := fnv.New32a()
	h.Write(data)
	hash := fmt.Sprintf("%08x", h.Sum32())
	template.Metadata.Labels = withHash(template.Metadata.Labels, hash)
	selector := *d.Spec.Selector
	if match.Matches(template.Metadata.Labels) {
		selector.MatchLabels = withHash(selector.MatchLabels, hash)
	} else {
		// The user's selector can already constrain the hash label.
		template = d.Spec.Template
	}
	return template, &selector, hash, nil
}

func withHash(labels map[string]string, hash string) map[string]string {
	out := make(map[string]string, len(labels)+1)
	maps.Copy(out, labels)
	out[templateHash] = hash
	return out
}

func deploymentReplicaSets(i *item, v view, match apis.Selector,
	template apis.PodTemplateSpec, selector *apis.LabelSelector) (*rolloutSet, []*rolloutSet, int64, error) {
	var current *rolloutSet
	var old []*rolloutSet
	groups := make(map[string]*rolloutSet)
	for _, child := range v.sets {
		rs := child.obj.(*apis.ReplicaSet)
		if !controlledBy(child, i) || !match.Matches(rs.Spec.Template.Metadata.Labels) {
			continue
		}
		podSelector, err := apis.SelectorFrom(rs.Spec.Selector)
		if err != nil {
			return nil, nil, 0, err
		}
		set := &rolloutSet{item: child, selector: podSelector, target: int64(replicas(rs.Spec.Replicas))}
		groups[child.rec.UID] = set
		if !child.rec.Deleting && reflect.DeepEqual(rs.Spec.Template, template) && reflect.DeepEqual(rs.Spec.Selector, selector) &&
			(current == nil || replicaSetRevision(set) > replicaSetRevision(current)) {
			if current != nil {
				old = append(old, current)
			}
			current = set
		} else {
			old = append(old, set)
		}
	}
	var orphaned int64
	for _, child := range v.pods {
		pod := child.obj.(*apis.Pod)
		ref, ok := controllerRef(pod.Meta())
		if !ok || !nonTerminal(pod) {
			continue
		}
		set := groups[ref.UID]
		if set == nil {
			// Background deletion can remove the ReplicaSet before the
			// node agent has finished stopping its Pods.
			if child.rec.Deleting && v.byUID[ref.UID] == nil &&
				ref.APIVersion == rsResource.APIVersion() && ref.Kind == rsResource.Kind &&
				pod.Metadata.Namespace == i.rec.Key.Namespace && pod.Metadata.Annotations[apis.PodDeploymentUIDAnnotation] == i.rec.UID {
				orphaned++
			}
			continue
		}
		if !references(child, ref, set.item) {
			continue
		}
		set.live++
		if active(pod) && set.selector.Matches(pod.Metadata.Labels) {
			set.pods = append(set.pods, child)
			if podReady(pod) {
				set.ready++
			}
		}
	}
	slices.SortStableFunc(old, compareRevision)
	return current, old, orphaned, nil
}

func withCurrent(current *rolloutSet, old []*rolloutSet) []*rolloutSet {
	if current == nil {
		return old
	}
	return append(slices.Clone(old), current)
}

func createDeploymentReplicaSet(tx *store.Tx, parent *item, template apis.PodTemplateSpec,
	selector *apis.LabelSelector, hash string, target int32, num int64) (*item, error) {
	d := parent.obj.(*apis.Deployment)
	rs := &apis.ReplicaSet{
		Metadata: apis.ObjectMeta{Namespace: d.Metadata.Namespace, Labels: maps.Clone(template.Metadata.Labels),
			OwnerReferences: []apis.OwnerReference{ownerRef(parent)}},
		Spec: apis.ReplicaSetSpec{Replicas: &target, Selector: selector, Template: template},
	}
	objects.Stamp(rsResource, rs)
	syncRevision(rs.Meta(), d.Meta(), 0, num)
	setDesiredReplicas(rs.Meta(), int64(replicas(d.Spec.Replicas)))
	rs.Metadata.Name = childPrefix(d.Metadata.Name, 54) + "-" + hash
	rec, err := tx.Create(objects.Put(rsResource, rs))
	if errors.Is(err, store.ErrExists) {
		rs.Metadata.Name = childPrefix(d.Metadata.Name, 45) + "-" + hash + "-" + rs.Metadata.UID[:8]
		rec, err = tx.Create(objects.Put(rsResource, rs))
	}
	if err != nil {
		return nil, err
	}
	return &item{res: rsResource, rec: rec, obj: rs}, nil
}

func updateDeploymentStatus(d *apis.Deployment, current *rolloutSet, old []*rolloutSet, desired, unavailable, orphaned int64,
	reason string, now time.Time) bool {
	var total, ready, updated int64
	oldLive := orphaned
	for _, s := range old {
		total += s.active()
		ready += s.ready
		oldLive += s.live
	}
	if current != nil {
		updated = current.active()
		total += updated
		ready += current.ready
	}
	status := apis.DeploymentStatus{
		ObservedGeneration: d.Metadata.Generation,
		Replicas:           int32(min(total, math.MaxInt32)), UpdatedReplicas: int32(min(updated, math.MaxInt32)),
		ReadyReplicas: int32(min(ready, math.MaxInt32)), AvailableReplicas: int32(min(ready, math.MaxInt32)),
		UnavailableReplicas: int32(max(0, desired-ready)),
	}
	available := apis.DeploymentCondition{Type: "Available", Status: "False", Reason: "MinimumReplicasUnavailable"}
	if ready >= desired-unavailable {
		available.Status, available.Reason = "True", "MinimumReplicasAvailable"
	}
	complete := current != nil && updated == desired && total == desired && ready == desired && oldLive == 0
	progress := apis.DeploymentCondition{Type: "Progressing", Status: "True", Reason: "ReplicaSetUpdated"}
	switch {
	case d.Spec.Paused:
		progress.Status, progress.Reason = "Unknown", "DeploymentPaused"
	case reason != "":
		progress.Status, progress.Reason = "False", reason
	case complete:
		progress.Reason = "NewReplicaSetAvailable"
	}
	status.Conditions = []apis.DeploymentCondition{
		deploymentCondition(d.Status.Conditions, available, now),
		deploymentCondition(d.Status.Conditions, progress, now),
	}
	d.Status = status
	return complete && reason == ""
}

func deploymentCondition(previous []apis.DeploymentCondition, c apis.DeploymentCondition, now time.Time) apis.DeploymentCondition {
	c.LastUpdateTime = &apis.Time{Time: now}
	c.LastTransitionTime = c.LastUpdateTime
	for _, before := range previous {
		if before.Type == c.Type && before.Status == c.Status {
			c.LastTransitionTime = before.LastTransitionTime
			if before.Reason == c.Reason {
				c.LastUpdateTime = before.LastUpdateTime
			}
			break
		}
	}
	return c
}
