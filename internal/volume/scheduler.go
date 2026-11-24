package volume

import (
	"fmt"
	"slices"
	"sync"

	"shearwater/internal/apis"
	"shearwater/internal/objects"
	"shearwater/internal/store"
)

const (
	noVolume = "node(s) didn't find available persistent volumes to bind"
	conflict = "node(s) had volume node affinity conflict"
)

// SchedulerFilter is the scheduler's last filter: a Pod that passes on at
// least one node will be placed in this transaction. Let one consumer choose
// a delayed claim's node at a time. On the following commit its placement is
// visible to every other consumer, even if the binder hasn't run yet.
func SchedulerFilter(st store.Reader) func(*apis.Pod, *apis.Node) string {
	var mu sync.Mutex
	var changed <-chan struct{}
	var index *state
	var selecting map[store.Key]string
	return func(p *apis.Pod, n *apis.Node) string {
		if !slices.ContainsFunc(p.Spec.Volumes, func(v apis.Volume) bool { return v.PersistentVolumeClaim != nil }) {
			return ""
		}
		mu.Lock()
		defer mu.Unlock()
		epoch := st.Changed()
		if index == nil || changed != epoch {
			next := newState()
			for _, res := range []*apis.Resource{pvcRes, pvRes, classRes, apis.Lookup("", "v1", "pods")} {
				records, _, err := st.List(res.Group, res.Plural, "")
				if err != nil {
					return noVolume
				}
				for _, rec := range records {
					if err := next.add(rec); err != nil {
						return noVolume
					}
				}
			}
			index, changed, selecting = next, epoch, make(map[store.Key]string)
		}
		var delayed []store.Key
		for _, v := range p.Spec.Volumes {
			if v.PersistentVolumeClaim == nil {
				continue
			}
			key := objects.Key(pvcRes, p.Metadata.Namespace, v.PersistentVolumeClaim.ClaimName)
			c := index.claims[key]
			if c == nil {
				return fmt.Sprintf("persistentvolumeclaim %q not found", key.Name)
			}
			if c.Metadata.DeletionTimestamp != nil || c.Status.Phase == "Lost" {
				return noVolume
			}
			if c.Status.Phase == "Bound" {
				pv := index.volumes[c.Spec.VolumeName]
				if pv == nil || pv.Metadata.DeletionTimestamp != nil || pv.Status.Phase != "Bound" || !pv.Claims(c) {
					return noVolume
				}
				if !pv.MatchesNode(n) {
					return conflict
				}
				continue
			}
			sc := index.classes[c.ClassName()]
			if sc == nil || sc.VolumeBindingMode != "WaitForFirstConsumer" || c.Spec.VolumeName != "" {
				return noVolume // immediate and prebound claims finish binding before scheduling
			}
			if selected := index.users[key]; selected != "" && selected != n.Metadata.Name {
				return conflict
			}
			if uid, ok := selecting[key]; ok && uid != p.Metadata.UID {
				return noVolume
			}
			delayed = append(delayed, key)
		}
		for _, key := range delayed {
			selecting[key] = p.Metadata.UID
		}
		return ""
	}
}
