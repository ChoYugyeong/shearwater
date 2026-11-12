package controller

import (
	"cmp"
	"maps"
	"slices"
	"strings"
	"time"

	"shearwater/internal/apis"
	"shearwater/internal/objects"
	"shearwater/internal/store"
)

var podResource = apis.Lookup("", "v1", "pods")

func nonTerminal(p *apis.Pod) bool {
	return p.Status.Phase != "Succeeded" && p.Status.Phase != "Failed"
}

func active(p *apis.Pod) bool { return p.Metadata.DeletionTimestamp == nil && nonTerminal(p) }

func replicas(n *int32) int32 {
	if n == nil {
		return 1
	}
	return *n
}

func childPrefix(name string, length int) string {
	return strings.TrimRight(name[:min(len(name), length)], ".-")
}

func (m Manager) replicaSet(tx *store.Tx, i *item, v view, budget *int, now time.Time) error {
	rs := i.obj.(*apis.ReplicaSet)
	slots := v.rolloutSlots[i.rec.UID]
	sel, err := apis.SelectorFrom(rs.Spec.Selector)
	if err != nil {
		return err
	}
	var owned []*item
	for _, candidate := range v.pods {
		p := candidate.obj.(*apis.Pod)
		if p.Metadata.Namespace != rs.Metadata.Namespace {
			continue
		}
		controlled := controlledBy(candidate, i)
		if !active(p) {
			if slots != nil && controlled && nonTerminal(p) && setPodDeployment(p, slots) {
				if err := candidate.update(tx); err != nil {
					return err
				}
			}
			continue
		}
		matches := sel.Matches(p.Metadata.Labels)
		if controlled {
			if matches {
				if setPodDeployment(p, slots) {
					if err := candidate.update(tx); err != nil {
						return err
					}
				}
				owned = append(owned, candidate)
			} else {
				p.Metadata.OwnerReferences = slices.DeleteFunc(p.Metadata.OwnerReferences, func(ref apis.OwnerReference) bool {
					return references(candidate, ref, i)
				})
				setPodDeployment(p, nil)
				if err := candidate.update(tx); err != nil {
					return err
				}
			}
		} else if _, hasOwner := controllerRef(&p.Metadata); !hasOwner && matches {
			ref := ownerRef(i)
			p.Metadata.OwnerReferences = slices.DeleteFunc(p.Metadata.OwnerReferences, func(r apis.OwnerReference) bool { return r.UID == ref.UID })
			p.Metadata.OwnerReferences = append(p.Metadata.OwnerReferences, ref)
			setPodDeployment(p, slots)
			if err := candidate.update(tx); err != nil {
				return err
			}
			owned = append(owned, candidate)
		}
	}

	status := apis.ReplicaSetStatus{Replicas: int32(len(owned)), ObservedGeneration: rs.Metadata.Generation}
	for _, p := range owned {
		pod := p.obj.(*apis.Pod)
		if podReady(pod) {
			status.ReadyReplicas++
		}
		fullyLabeled := true
		for key, val := range rs.Spec.Template.Metadata.Labels {
			if got, ok := pod.Metadata.Labels[key]; !ok || got != val {
				fullyLabeled = false
				break
			}
		}
		if fullyLabeled {
			status.FullyLabeledReplicas++
		}
	}
	status.AvailableReplicas = status.ReadyReplicas
	rs.Status = status
	if err := i.update(tx); err != nil {
		return err
	}
	desired := int(replicas(rs.Spec.Replicas))
	if excess := len(owned) - desired; excess > 0 {
		slices.SortStableFunc(owned, scaleDownOrder)
		for _, child := range owned[:excess] {
			rec, _, err := objects.Delete(tx, child.res, child.rec, child.obj, m.NodeName, nil, now)
			if err != nil {
				return err
			}
			child.rec = rec
			// A second ReplicaSet must not adopt a Pod removed earlier in this pass.
			if child.obj.Meta().DeletionTimestamp == nil {
				child.obj.Meta().DeletionTimestamp = &apis.Time{Time: now}
			}
		}
	}
	allowed := *budget
	if slots != nil {
		allowed = int(min(int64(allowed), slots.remaining))
	}
	for n := min(desired-len(owned), allowed); n > 0; n-- {
		p := &apis.Pod{Spec: rs.Spec.Template.Spec, Metadata: apis.ObjectMeta{
			Namespace: rs.Metadata.Namespace, Labels: rs.Spec.Template.Metadata.Labels,
			Annotations: maps.Clone(rs.Spec.Template.Metadata.Annotations), GenerateName: childPrefix(rs.Metadata.Name, 53) + "-",
			OwnerReferences: []apis.OwnerReference{ownerRef(i)},
		}}
		objects.Stamp(podResource, p)
		setPodDeployment(p, slots)
		p.Metadata.Name = p.Metadata.GenerateName + p.Metadata.UID[:8]
		if _, err := tx.Create(objects.Put(podResource, p)); err != nil {
			return err
		}
		*budget--
		if slots != nil {
			slots.remaining--
		}
	}
	return nil
}

func setPodDeployment(p *apis.Pod, slots *rolloutSlots) bool {
	const key = apis.PodDeploymentUIDAnnotation
	prev, exists := p.Metadata.Annotations[key]
	if slots == nil {
		delete(p.Metadata.Annotations, key)
		return exists
	}
	if prev == slots.deploymentUID {
		return false
	}
	annotate(p.Meta(), key, slots.deploymentUID)
	return true
}

func scaleDownOrder(a, b *item) int {
	pa, pb := a.obj.(*apis.Pod), b.obj.(*apis.Pod)
	if (pa.Spec.NodeName == "") != (pb.Spec.NodeName == "") {
		if pa.Spec.NodeName == "" {
			return -1
		}
		return 1
	}
	if podReady(pa) != podReady(pb) {
		if podReady(pa) {
			return 1
		}
		return -1
	}
	if a, b := pa.Metadata.CreationTimestamp, pb.Metadata.CreationTimestamp; a != nil && b != nil {
		if order := b.Compare(a.Time); order != 0 {
			return order
		}
	}
	return cmp.Compare(pa.Metadata.Name, pb.Metadata.Name)
}
