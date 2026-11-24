package controller

import (
	"fmt"
	"time"

	"shearwater/internal/apis"
	"shearwater/internal/objects"
	"shearwater/internal/store"
)

// Node lifecycle timing, as in Kubernetes' node lifecycle controller.
const (
	// UnreachableAfter without hearing from a worker marks it NotReady.
	UnreachableAfter = 40 * time.Second
	// DefaultEvictAfter is how long Pods stay on an unreachable node when they
	// carry no toleration for it (Kubernetes' DefaultTolerationSeconds).
	DefaultEvictAfter = 300 * time.Second
	// LifecycleEvery is how often node liveness is looked at while workers exist.
	LifecycleEvery = 5 * time.Second
)

// UnreachableTaint is put on a node that stopped reporting.
const UnreachableTaint = "node.kubernetes.io/unreachable"

// nodeLifecycle marks silent workers NotReady and taints them, takes their
// Pods out of Service endpoints, deletes the Pods that do not tolerate the
// taint after their toleration time so their controllers replace them on other
// nodes, and removes Pods bound to nodes that no longer exist.
// It reports whether it changed anything, so the caller can reload its view.
func (m Manager) nodeLifecycle(tx *store.Tx, v view, now time.Time) (changed bool, err error) {
	byNode := map[string][]*item{}
	for _, i := range v.pods {
		p := i.obj.(*apis.Pod)
		if p.Spec.NodeName != "" {
			byNode[p.Spec.NodeName] = append(byNode[p.Spec.NodeName], i)
		}
	}
	for node, pods := range byNode {
		if _, ok := v.nodeItems[node]; ok || node == m.NodeName {
			continue
		}
		// The node was removed: nothing will stop these Pods or report on
		// them. Kubernetes' Pod garbage collector deletes them too.
		for _, i := range pods {
			if _, err := tx.Delete(i.rec.Key, i.rec.Rev, objects.Encoder(i.obj)); err != nil {
				return changed, err
			}
			changed = true
		}
	}
	if m.Silence == nil {
		return changed, nil
	}
	for _, n := range v.nodes {
		name := n.Metadata.Name
		if name == m.NodeName || n.Metadata.DeletionTimestamp != nil {
			continue // our own node reports in-process
		}
		ni := v.nodeItems[name]
		silent := m.Silence(name) >= UnreachableAfter
		taintAt := -1
		for j, t := range n.Spec.Taints {
			if t.Key == UnreachableTaint && t.Effect == "NoExecute" {
				taintAt = j
			}
		}
		if !silent {
			if taintAt >= 0 {
				n.Spec.Taints = append(n.Spec.Taints[:taintAt], n.Spec.Taints[taintAt+1:]...)
				if err := ni.update(tx); err != nil {
					return changed, err
				}
				changed = true
			}
			continue
		}
		nodeChanged := false
		if taintAt < 0 {
			n.Spec.Taints = append(n.Spec.Taints, apis.Taint{Key: UnreachableTaint, Effect: "NoExecute", TimeAdded: &apis.Time{Time: now}})
			taintAt = len(n.Spec.Taints) - 1
			nodeChanged = true
		}
		ready := apis.NodeCondition{Type: "Ready", Status: "Unknown", Reason: "NodeStatusUnknown",
			Message: "Kubelet stopped posting node status.", LastHeartbeatTime: nil, LastTransitionTime: &apis.Time{Time: now}}
		found := false
		for j := range n.Status.Conditions {
			c := &n.Status.Conditions[j]
			if c.Type != "Ready" {
				continue
			}
			found = true
			if c.Status != "Unknown" {
				ready.LastHeartbeatTime = c.LastHeartbeatTime
				*c = ready
				nodeChanged = true
			}
		}
		if !found {
			n.Status.Conditions = append(n.Status.Conditions, ready)
			nodeChanged = true
		}
		if nodeChanged {
			if err := ni.update(tx); err != nil {
				return changed, err
			}
			changed = true
		}
		taint := n.Spec.Taints[taintAt]
		since := now
		if taint.TimeAdded != nil {
			since = taint.TimeAdded.Time
		}
		for _, i := range byNode[name] {
			p := i.obj.(*apis.Pod)
			if i.rec.Deleting || !nonTerminal(p) {
				continue
			}
			wait := DefaultEvictAfter
			switch s := apis.TolerationSeconds(p.Spec.Tolerations, taint); {
			case s < 0:
				wait = -1 // tolerates it forever
			case s > 0:
				wait = time.Duration(s) * time.Second
			}
			if wait >= 0 && !now.Before(since.Add(wait)) {
				msg := fmt.Sprintf("Node %s has not reported for %s; deleting the Pod so its controller can replace it on another node", name, now.Sub(since).Round(time.Second))
				if err := objects.PodEvent(tx, p, now, "node-lost", "", "TaintManagerEviction", msg, "shearwater-node-lifecycle", "Warning"); err != nil {
					return changed, err
				}
				rec, _, err := objects.Delete(tx, i.res, i.rec, p, nil, now)
				if err != nil {
					return changed, err
				}
				i.rec, changed = rec, true
				continue
			}
			// Take the Pod out of Service endpoints: nobody can tell whether it
			// still answers.
			for j := range p.Status.Conditions {
				c := &p.Status.Conditions[j]
				if c.Type == "Ready" && c.Status == "True" {
					c.Status, c.Reason, c.Message = "False", "NodeNotReady", "the node stopped reporting"
					c.LastTransitionTime = &apis.Time{Time: now}
					if err := i.update(tx); err != nil {
						return changed, err
					}
					changed = true
					break
				}
			}
		}
	}
	return changed, nil
}
