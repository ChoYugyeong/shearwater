package controller

import (
	"slices"
	"time"

	"shearwater/internal/apis"
	"shearwater/internal/objects"
	"shearwater/internal/store"
)

func foreground(i *item) bool {
	return i != nil && i.rec.Deleting && slices.Contains(i.obj.Meta().Finalizers, objects.ForegroundFinalizer)
}

func (m Manager) collect(tx *store.Tx, v view, now time.Time) (bool, error) {
	blocking := make(map[string]bool)
	for _, child := range v.all {
		for _, ref := range child.obj.Meta().OwnerReferences {
			if ref.BlockOwnerDeletion != nil && *ref.BlockOwnerDeletion && references(child, ref, v.byUID[ref.UID]) {
				blocking[ref.UID] = true
			}
		}
	}
	// Process one deletion or detachment per pass so every decision sees
	// current ownership and blocking dependents.
	for _, i := range v.all {
		meta := i.obj.Meta()
		if foreground(i) && !blocking[i.rec.UID] {
			meta.Finalizers = slices.DeleteFunc(meta.Finalizers, func(f string) bool { return f == objects.ForegroundFinalizer })
			if g := meta.DeletionGracePeriodSeconds; len(meta.Finalizers) == 0 && (g == nil || *g == 0) {
				_, err := tx.Delete(i.rec.Key, i.rec.Rev, objects.Encoder(i.obj))
				return true, err
			}
			return true, i.update(tx)
		}
		if i.rec.Deleting || len(meta.OwnerReferences) == 0 {
			continue
		}
		var live, waiting bool
		for _, ref := range meta.OwnerReferences {
			owner := v.byUID[ref.UID]
			if references(i, ref, owner) {
				if foreground(owner) {
					waiting = true
				} else {
					live = true
				}
			}
		}
		if live && waiting {
			// A dependent with another live owner survives foreground deletion.
			meta.OwnerReferences = slices.DeleteFunc(meta.OwnerReferences, func(ref apis.OwnerReference) bool {
				owner := v.byUID[ref.UID]
				return references(i, ref, owner) && foreground(owner)
			})
			return true, i.update(tx)
		}
		if live {
			continue
		}
		if waiting && blocking[i.rec.UID] {
			meta.Finalizers = append(meta.Finalizers, objects.ForegroundFinalizer)
		}
		_, _, err := objects.Delete(tx, i.res, i.rec, i.obj, m.NodeName, nil, now)
		return true, err
	}
	return breakForegroundCycle(tx, v)
}

// A cycle of blocking edges between deleting objects cannot make progress.
// Clear one blocking edge in such a cycle and let the next pass continue GC.
func breakForegroundCycle(tx *store.Tx, v view) (bool, error) {
	const (
		visiting = 1
		done     = 2
	)
	state := make(map[string]int)
	var visit func(*item) (bool, error)
	visit = func(child *item) (bool, error) {
		state[child.rec.UID] = visiting
		refs := child.obj.Meta().OwnerReferences
		for j, ref := range refs {
			owner := v.byUID[ref.UID]
			if !foreground(owner) || !references(child, ref, owner) || ref.BlockOwnerDeletion == nil || !*ref.BlockOwnerDeletion {
				continue
			}
			switch state[ref.UID] {
			case visiting:
				no := false
				refs[j].BlockOwnerDeletion = &no
				return true, child.update(tx)
			case 0:
				if changed, err := visit(owner); changed || err != nil {
					return changed, err
				}
			}
		}
		state[child.rec.UID] = done
		return false, nil
	}
	for _, i := range v.all {
		if foreground(i) && state[i.rec.UID] == 0 {
			if changed, err := visit(i); changed || err != nil {
				return changed, err
			}
		}
	}
	return false, nil
}
