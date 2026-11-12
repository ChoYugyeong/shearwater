package controller

import (
	"fmt"
	"time"

	"shearwater/internal/apis"
	"shearwater/internal/objects"
	"shearwater/internal/store"
)

func (m Manager) schedule(tx *store.Tx, v view, now time.Time) error {
	ready := false
	if v.node != nil && v.node.Metadata.DeletionTimestamp == nil {
		for _, c := range v.node.Status.Conditions {
			if c.Type == "Ready" {
				ready = c.Status == "True"
				break
			}
		}
	}
	for _, i := range v.pods {
		p := i.obj.(*apis.Pod)
		if i.rec.Deleting || p.Spec.NodeName != "" || !active(p) {
			continue
		}
		c := apis.PodCondition{Type: "PodScheduled", Status: "False", Reason: "Unschedulable",
			Message: "The node is not Ready", LastTransitionTime: &apis.Time{Time: now}}
		if ready {
			p.Spec.NodeName = m.NodeName
			p.Metadata.Generation++
			c.Status, c.Reason, c.Message = "True", "", ""
		}
		found := false
		for j := range p.Status.Conditions {
			old := &p.Status.Conditions[j]
			if old.Type == c.Type {
				if old.Status == c.Status {
					c.LastTransitionTime = old.LastTransitionTime
				}
				*old, found = c, true
				break
			}
		}
		if !found {
			p.Status.Conditions = append(p.Status.Conditions, c)
		}
		if err := i.update(tx); err != nil {
			return err
		}
		if ready {
			message := fmt.Sprintf("Assigned %s/%s to %s", p.Metadata.Namespace, p.Metadata.Name, m.NodeName)
			if err := objects.PodEvent(tx, p, now, "schedule", "", "Scheduled", message, "shearwater-scheduler", "Normal"); err != nil {
				return err
			}
		}
	}
	return nil
}
