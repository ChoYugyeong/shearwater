package volume

import (
	"context"
	"encoding/json"
	"log"
	"path"
	"slices"
	"time"

	"shearwater/internal/apis"
	"shearwater/internal/objects"
	"shearwater/internal/store"
)

type Controller struct {
	Store *store.Store
}

func (c Controller) Run(ctx context.Context) {
	for ctx.Err() == nil {
		changed := c.Store.Changed()
		wait := 5 * time.Minute
		if err := c.Sync(); err != nil {
			log.Printf("persistent volumes: %v", err)
			wait, changed = time.Second, nil
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
		case <-changed:
		case <-timer.C:
		}
		timer.Stop()
	}
}

// Object and pod-use indexes describe one SQLite write transaction. A
// competing claim, a Pod creation and a PVC delete are serialized with bind.
type state struct {
	claims  map[store.Key]*apis.PersistentVolumeClaim
	volumes map[string]*apis.PersistentVolume
	classes map[string]*apis.StorageClass
	nodes   map[string]*apis.Node
	// A key exists while a non-terminal Pod uses the claim (including
	// terminating Pods). Its value is the first live consumer's assigned node.
	users map[store.Key]string
	rev   map[store.Key]int64
}

func newState() *state {
	return &state{
		claims: make(map[store.Key]*apis.PersistentVolumeClaim), volumes: make(map[string]*apis.PersistentVolume),
		classes: make(map[string]*apis.StorageClass), nodes: make(map[string]*apis.Node),
		users: make(map[store.Key]string), rev: make(map[store.Key]int64),
	}
}

func (s *state) add(rec store.Record) error {
	k := rec.Key
	res := apis.Lookup(k.Group, "v1", k.Resource)
	if res == nil {
		return nil
	}
	switch res.Kind {
	case "PersistentVolumeClaim", "PersistentVolume", "StorageClass", "Pod", "Node":
	default:
		return nil
	}
	obj := res.New()
	if err := json.Unmarshal(rec.Data, obj); err != nil {
		return err
	}
	s.rev[k] = rec.Rev
	switch o := obj.(type) {
	case *apis.PersistentVolumeClaim:
		s.claims[k] = o
	case *apis.PersistentVolume:
		s.volumes[k.Name] = o
	case *apis.StorageClass:
		if !rec.Deleting {
			s.classes[k.Name] = o
		}
	case *apis.Node:
		s.nodes[k.Name] = o
	case *apis.Pod:
		if o.Status.Phase != "Succeeded" && o.Status.Phase != "Failed" {
			for _, v := range o.Spec.Volumes {
				if ref := v.PersistentVolumeClaim; ref != nil {
					key := objects.Key(pvcRes, k.Namespace, ref.ClaimName)
					if s.users[key] == "" {
						name := ""
						if o.Metadata.DeletionTimestamp == nil {
							name = o.Spec.NodeName
						}
						s.users[key] = name
					}
				}
			}
		}
	}
	return nil
}

func (s *state) update(tx *store.Tx, res *apis.Resource, o apis.Object) error {
	put := objects.Put(res, o)
	rec, _, err := tx.Update(put, s.rev[put.Key])
	if err == nil {
		s.rev[put.Key] = rec.Rev
	}
	return err
}

func (c Controller) Sync() error {
	return c.Store.Write(false, func(tx *store.Tx) error {
		recs, err := tx.Objects()
		if err != nil {
			return err
		}
		s := newState()
		for _, rec := range recs {
			if err := s.add(rec); err != nil {
				return err
			}
		}
		// Process claims in the store's stable namespace/name order.
		for _, rec := range recs {
			if claim := s.claims[rec.Key]; claim != nil {
				if err := s.syncClaim(tx, rec, claim); err != nil {
					return err
				}
			}
		}
		for _, rec := range recs {
			if rec.Key.Group == "" && rec.Key.Resource == pvRes.Plural {
				if err := s.reclaim(tx, rec, s.volumes[rec.Key.Name]); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func (s *state) syncClaim(tx *store.Tx, rec store.Record, c *apis.PersistentVolumeClaim) error {
	selected, used := s.users[rec.Key]
	if rec.Deleting {
		if used || !slices.Contains(c.Metadata.Finalizers, apis.PVCProtection) {
			return nil
		}
		c.Metadata.Finalizers = slices.DeleteFunc(c.Metadata.Finalizers, func(f string) bool { return f == apis.PVCProtection })
		if len(c.Metadata.Finalizers) == 0 {
			delete(s.claims, rec.Key)
			_, err := tx.Delete(rec.Key, rec.Rev, objects.Encoder(c))
			return err
		}
		return s.update(tx, pvcRes, c)
	}
	if c.Status.Phase == "Bound" || c.Status.Phase == "Lost" {
		pv := s.volumes[c.Spec.VolumeName]
		if pv == nil || !pv.Claims(c) {
			if c.Status.Phase != "Lost" {
				c.Status = apis.PersistentVolumeClaimStatus{Phase: "Lost"}
				return s.update(tx, pvcRes, c)
			}
			return nil
		}
		if c.Status.Phase == "Bound" {
			return nil
		}
		return s.bind(tx, c, pv)
	}
	class := s.classes[c.ClassName()]
	node := s.nodes[selected]
	if selected != "" && node == nil {
		return nil // the node must exist before selecting storage for it
	}
	delayed := class != nil && class.VolumeBindingMode == "WaitForFirstConsumer"
	if delayed && node == nil && c.Spec.VolumeName == "" {
		return nil
	}
	if pv := s.bestVolume(c, node); pv != nil {
		return s.bind(tx, c, pv)
	}
	if !delayed || node == nil || class.Provisioner != apis.LocalPathProvisioner || c.Spec.VolumeName != "" {
		return nil
	}
	name := "pvc-" + c.Metadata.UID
	if s.volumes[name] != nil {
		return nil // never adopt or overwrite another PV at the generated name
	}
	pv := &apis.PersistentVolume{
		Metadata: apis.ObjectMeta{Name: name},
		Spec: apis.PersistentVolumeSpec{
			Capacity: map[string]apis.Quantity{"storage": c.Spec.Resources.Requests["storage"]}, AccessModes: c.Spec.AccessModes,
			StorageClassName: class.Metadata.Name, PersistentVolumeReclaimPolicy: class.ReclaimPolicy,
			Local: &apis.LocalVolumeSource{Path: path.Join(apis.LocalVolumeRoot, name)},
			NodeAffinity: &apis.VolumeNodeAffinity{Required: &apis.NodeSelector{NodeSelectorTerms: []apis.NodeSelectorTerm{
				{MatchExpressions: []apis.NodeSelectorRequirement{{Key: "kubernetes.io/hostname", Operator: "In", Values: []string{node.Metadata.Name}}}},
			}}},
		},
	}
	objects.Stamp(pvRes, pv)
	created, err := tx.Create(objects.Put(pvRes, pv))
	if err != nil {
		return err
	}
	s.rev[created.Key], s.volumes[name] = created.Rev, pv
	return s.bind(tx, c, pv)
}

func (s *state) bestVolume(c *apis.PersistentVolumeClaim, node *apis.Node) *apis.PersistentVolume {
	want, err := apis.ParseQuantity(string(c.Spec.Resources.Requests["storage"]))
	if err != nil {
		return nil
	}
	var best *apis.PersistentVolume
	for _, pv := range s.volumes {
		if pv.Metadata.DeletionTimestamp != nil || pv.Status.Phase != "Available" || pv.Spec.StorageClassName != c.ClassName() ||
			(c.Spec.VolumeName != "" && c.Spec.VolumeName != pv.Metadata.Name) || (node != nil && !pv.MatchesNode(node)) {
			continue
		}
		r := pv.Spec.ClaimRef
		if r != nil && (r.Namespace != c.Metadata.Namespace || r.Name != c.Metadata.Name || r.UID != "" && r.UID != c.Metadata.UID) {
			continue
		}
		if slices.ContainsFunc(c.Spec.AccessModes, func(m string) bool { return !slices.Contains(pv.Spec.AccessModes, m) }) {
			continue
		}
		size, err := apis.ParseQuantity(string(pv.Spec.Capacity["storage"]))
		if err != nil || size.Cmp(want) < 0 {
			continue
		}
		if best != nil {
			// Prefer a reservation, then the smallest fit, then its name.
			switch {
			case r == nil && best.Spec.ClaimRef != nil:
				continue
			case (r == nil) == (best.Spec.ClaimRef == nil):
				b, _ := apis.ParseQuantity(string(best.Spec.Capacity["storage"]))
				if cmp := size.Cmp(b); cmp > 0 || cmp == 0 && pv.Metadata.Name >= best.Metadata.Name {
					continue
				}
			}
		}
		best = pv
	}
	return best
}

func (s *state) bind(tx *store.Tx, c *apis.PersistentVolumeClaim, pv *apis.PersistentVolume) error {
	if !pv.Claims(c) {
		pv.Spec.ClaimRef = &apis.ObjectReference{Kind: "PersistentVolumeClaim", APIVersion: "v1",
			Namespace: c.Metadata.Namespace, Name: c.Metadata.Name, UID: c.Metadata.UID}
		pv.Metadata.Generation++
	}
	pv.Status.Phase = "Bound"
	if err := s.update(tx, pvRes, pv); err != nil {
		return err
	}
	if c.Spec.VolumeName != pv.Metadata.Name {
		c.Spec.VolumeName = pv.Metadata.Name
		c.Metadata.Generation++
	}
	c.Status = apis.PersistentVolumeClaimStatus{Phase: "Bound", Capacity: pv.Spec.Capacity, AccessModes: pv.Spec.AccessModes}
	return s.update(tx, pvcRes, c)
}

func (s *state) reclaim(tx *store.Tx, rec store.Record, pv *apis.PersistentVolume) error {
	r := pv.Spec.ClaimRef
	if rec.Deleting || pv.Status.Phase != "Bound" || r == nil || r.UID == "" {
		return nil
	}
	if c := s.claims[objects.Key(pvcRes, r.Namespace, r.Name)]; c != nil && pv.Claims(c) {
		return nil
	}
	if pv.Spec.PersistentVolumeReclaimPolicy == "Delete" {
		if !pv.ManagedLocalPath() {
			// No deleter for a static directory: the object goes, the data stays.
			_, _, err := objects.Delete(tx, pvRes, rec, pv, nil, time.Now())
			return err
		}
		// The node that holds the directory removes it, then this object.
		if pv.Metadata.Annotations == nil {
			pv.Metadata.Annotations = map[string]string{}
		}
		pv.Metadata.Annotations[apis.DeleteDataAnnotation] = "true"
	}
	pv.Status.Phase = "Released"
	return s.update(tx, pvRes, pv)
}
