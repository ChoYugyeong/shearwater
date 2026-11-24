package apis

import (
	"fmt"
	"path"
	"reflect"
	"slices"
	"strconv"
	"strings"
)

const (
	StorageGroup           = "storage.k8s.io"
	DefaultClassAnnotation = "storageclass.kubernetes.io/is-default-class"
	LocalPathProvisioner   = "shearwater.io/local-path"
	LocalVolumeRoot        = "/var/lib/shearwater/volumes"
	PVCProtection          = "kubernetes.io/pvc-protection"
)

type PersistentVolumeClaimVolumeSource struct {
	ClaimName string `json:"claimName"`
	ReadOnly  bool   `json:"readOnly,omitempty"`
}

type LocalVolumeSource struct {
	Path string `json:"path"`
}

type VolumeNodeAffinity struct {
	Required *NodeSelector `json:"required,omitempty"`
}

type PersistentVolume struct {
	TypeMeta
	Metadata ObjectMeta             `json:"metadata"`
	Spec     PersistentVolumeSpec   `json:"spec"`
	Status   PersistentVolumeStatus `json:"status,omitzero"`
}

type PersistentVolumeSpec struct {
	Capacity                      map[string]Quantity   `json:"capacity"`
	AccessModes                   []string              `json:"accessModes"`
	PersistentVolumeReclaimPolicy string                `json:"persistentVolumeReclaimPolicy,omitempty"`
	StorageClassName              string                `json:"storageClassName,omitempty"`
	ClaimRef                      *ObjectReference      `json:"claimRef,omitempty"`
	HostPath                      *HostPathVolumeSource `json:"hostPath,omitempty"`
	Local                         *LocalVolumeSource    `json:"local,omitempty"`
	NodeAffinity                  *VolumeNodeAffinity   `json:"nodeAffinity,omitempty"`
	VolumeMode                    string                `json:"volumeMode,omitempty"`
}

type PersistentVolumeStatus struct {
	Phase PersistentVolumePhase `json:"phase,omitempty"`
}

type PersistentVolumePhase string

// /status decodes status directly, bypassing spec validation.
func (p *PersistentVolumePhase) UnmarshalText(text []byte) error {
	switch s := PersistentVolumePhase(text); s {
	case "", "Available", "Bound", "Released", "Failed":
		*p = s
		return nil
	default:
		return fmt.Errorf("invalid PersistentVolume status.phase %q", text)
	}
}

type PersistentVolumeClaim struct {
	TypeMeta
	Metadata ObjectMeta                  `json:"metadata"`
	Spec     PersistentVolumeClaimSpec   `json:"spec"`
	Status   PersistentVolumeClaimStatus `json:"status,omitzero"`
}

type PersistentVolumeClaimSpec struct {
	AccessModes      []string             `json:"accessModes"`
	Resources        ResourceRequirements `json:"resources"`
	StorageClassName *string              `json:"storageClassName,omitempty"`
	VolumeName       string               `json:"volumeName,omitempty"`
	VolumeMode       string               `json:"volumeMode,omitempty"`
}

type PersistentVolumeClaimStatus struct {
	Phase       PersistentVolumeClaimPhase `json:"phase,omitempty"`
	Capacity    map[string]Quantity        `json:"capacity,omitempty"`
	AccessModes []string                   `json:"accessModes,omitempty"`
}

type PersistentVolumeClaimPhase string

func (p *PersistentVolumeClaimPhase) UnmarshalText(text []byte) error {
	switch s := PersistentVolumeClaimPhase(text); s {
	case "", "Pending", "Bound", "Lost":
		*p = s
		return nil
	default:
		return fmt.Errorf("invalid PersistentVolumeClaim status.phase %q", text)
	}
}

type StorageClass struct {
	TypeMeta
	Metadata             ObjectMeta `json:"metadata"`
	Provisioner          string     `json:"provisioner"`
	ReclaimPolicy        string     `json:"reclaimPolicy,omitempty"`
	VolumeBindingMode    string     `json:"volumeBindingMode,omitempty"`
	AllowVolumeExpansion *bool      `json:"allowVolumeExpansion,omitempty"`
}

func (o *PersistentVolume) Meta() *ObjectMeta      { return &o.Metadata }
func (o *PersistentVolumeClaim) Meta() *ObjectMeta { return &o.Metadata }
func (o *StorageClass) Meta() *ObjectMeta          { return &o.Metadata }

func (o *PersistentVolumeClaim) ClassName() string {
	if o.Spec.StorageClassName == nil {
		return ""
	}
	return *o.Spec.StorageClassName
}

func (o *PersistentVolume) Claims(c *PersistentVolumeClaim) bool {
	r := o.Spec.ClaimRef
	return r != nil && r.UID != "" && r.UID == c.Metadata.UID &&
		r.Name == c.Metadata.Name && r.Namespace == c.Metadata.Namespace
}

func (o *PersistentVolume) MatchesNode(n *Node) bool {
	if n == nil || o.Spec.NodeAffinity == nil || o.Spec.NodeAffinity.Required == nil {
		return false
	}
	for _, term := range o.Spec.NodeAffinity.Required.NodeSelectorTerms {
		if len(term.MatchExpressions)+len(term.MatchFields) == 0 {
			continue
		}
		if !slices.ContainsFunc(term.MatchExpressions, func(r NodeSelectorRequirement) bool {
			v, ok := n.Metadata.Labels[r.Key]
			return !storageRequirementMatches(r, v, ok)
		}) && !slices.ContainsFunc(term.MatchFields, func(r NodeSelectorRequirement) bool {
			return r.Key != "metadata.name" || !storageRequirementMatches(r, n.Metadata.Name, true)
		}) {
			return true
		}
	}
	return false
}

func storageRequirementMatches(r NodeSelectorRequirement, v string, present bool) bool {
	switch r.Operator {
	case "In":
		return present && slices.Contains(r.Values, v)
	case "NotIn":
		return !present || !slices.Contains(r.Values, v)
	case "Exists":
		return present
	case "DoesNotExist":
		return !present
	case "Gt", "Lt":
		if !present || len(r.Values) != 1 {
			return false
		}
		a, err := strconv.ParseInt(v, 10, 64)
		b, other := strconv.ParseInt(r.Values[0], 10, 64)
		return err == nil && other == nil && (r.Operator == "Gt" && a > b || r.Operator == "Lt" && a < b)
	}
	return false
}

func validateStorage(modes []string, capacity map[string]Quantity, field, volumeMode string, errs *ErrorList) {
	if len(modes) == 0 {
		errs.required("spec.accessModes")
	}
	for i, mode := range modes {
		switch mode {
		case "ReadWriteOnce", "ReadOnlyMany", "ReadWriteMany":
			if slices.Contains(modes[:i], mode) {
				errs.duplicate("spec.accessModes", mode)
			}
		default:
			errs.oneOf("spec.accessModes", mode, "ReadWriteOnce", "ReadOnlyMany", "ReadWriteMany")
		}
	}
	q, err := ParseQuantity(string(capacity["storage"]))
	if err != nil || q.Sign() <= 0 {
		errs.invalid(field+".storage", capacity["storage"], "must be a positive storage quantity")
	}
	for k := range capacity {
		if k != "storage" {
			errs.unsupported(field+"."+k, "only storage is supported")
		}
	}
	if volumeMode != "" && volumeMode != "Filesystem" {
		errs.oneOf("spec.volumeMode", volumeMode, "Filesystem")
	}
}

func validateReclaim(policy, field string, errs *ErrorList) {
	if policy != "Delete" && policy != "Retain" {
		errs.oneOf(field, policy, "Delete", "Retain")
	}
}

func storageName(name, field string, errs *ErrorList) {
	if name != "" && !IsDNS1123Subdomain(name) {
		errs.invalid(field, name, nameSubdomain)
	}
}

func validatePV(obj Object) ErrorList {
	o := obj.(*PersistentVolume)
	s := &o.Spec
	var errs ErrorList
	validateMeta(o.Meta(), IsDNS1123Subdomain, nameSubdomain, &errs)
	validateStorage(s.AccessModes, s.Capacity, "spec.capacity", s.VolumeMode, &errs)
	validateReclaim(s.PersistentVolumeReclaimPolicy, "spec.persistentVolumeReclaimPolicy", &errs)
	storageName(s.StorageClassName, "spec.storageClassName", &errs)
	if (s.HostPath == nil) == (s.Local == nil) {
		errs.invalid("spec", "", "exactly one of local or hostPath is required")
	}
	var source, field string
	if s.HostPath != nil {
		source, field = s.HostPath.Path, "spec.hostPath.path"
		switch s.HostPath.Type {
		case "", "Directory", "DirectoryOrCreate":
		default:
			errs.oneOf("spec.hostPath.type", s.HostPath.Type, "", "Directory", "DirectoryOrCreate")
		}
	}
	if s.Local != nil {
		source, field = s.Local.Path, "spec.local.path"
	}
	if field != "" && (!path.IsAbs(source) || slices.Contains(strings.Split(source, "/"), "..")) {
		errs.invalid(field, source, "must be an absolute path without '..' components")
	}
	if s.NodeAffinity == nil || s.NodeAffinity.Required == nil || len(s.NodeAffinity.Required.NodeSelectorTerms) == 0 {
		errs.required("spec.nodeAffinity.required.nodeSelectorTerms")
	} else {
		for i, term := range s.NodeAffinity.Required.NodeSelectorTerms {
			validateNodeSelectorTerm(&term, fmt.Sprintf("spec.nodeAffinity.required.nodeSelectorTerms[%d]", i), &errs)
		}
	}
	if r := s.ClaimRef; r != nil {
		if !IsDNS1123Label(r.Namespace) || !IsDNS1123Subdomain(r.Name) {
			errs.invalid("spec.claimRef", r.Name, "must name a claim and namespace")
		}
	}
	switch o.Status.Phase {
	case "", "Available", "Bound", "Released", "Failed":
	default:
		errs.oneOf("status.phase", string(o.Status.Phase), "Available", "Bound", "Released", "Failed")
	}
	return errs
}

func validatePVC(obj Object) ErrorList {
	o := obj.(*PersistentVolumeClaim)
	var errs ErrorList
	validateMeta(o.Meta(), IsDNS1123Subdomain, nameSubdomain, &errs)
	validateStorage(o.Spec.AccessModes, o.Spec.Resources.Requests, "spec.resources.requests", o.Spec.VolumeMode, &errs)
	if len(o.Spec.Resources.Limits) != 0 {
		errs.unsupported("spec.resources.limits", "use requests.storage to select a volume")
	}
	storageName(o.ClassName(), "spec.storageClassName", &errs)
	storageName(o.Spec.VolumeName, "spec.volumeName", &errs)
	switch o.Status.Phase {
	case "", "Pending", "Bound", "Lost":
	default:
		errs.oneOf("status.phase", string(o.Status.Phase), "Pending", "Bound", "Lost")
	}
	return errs
}

func validateClass(obj Object) ErrorList {
	o := obj.(*StorageClass)
	var errs ErrorList
	validateMeta(o.Meta(), IsDNS1123Subdomain, nameSubdomain, &errs)
	if !IsQualifiedName(o.Provisioner) {
		errs.invalid("provisioner", o.Provisioner, "must be a qualified provisioner name")
	}
	validateReclaim(o.ReclaimPolicy, "reclaimPolicy", &errs)
	if o.VolumeBindingMode != "Immediate" && o.VolumeBindingMode != "WaitForFirstConsumer" {
		errs.oneOf("volumeBindingMode", o.VolumeBindingMode, "Immediate", "WaitForFirstConsumer")
	}
	return errs
}

func init() {
	Register(&Resource{
		Version: "v1", Plural: "persistentvolumes", Singular: "persistentvolume", Kind: "PersistentVolume", ShortNames: []string{"pv"},
		Create: true, Update: true, Delete: true, StatusSub: true, Fields: []string{"status.phase"},
		New:     func() Object { return &PersistentVolume{} },
		Prepare: func(obj Object) { obj.(*PersistentVolume).Status = PersistentVolumeStatus{Phase: "Available"} },
		Default: func(obj Object) {
			s := &obj.(*PersistentVolume).Spec
			if s.VolumeMode == "" {
				s.VolumeMode = "Filesystem"
			}
			if s.PersistentVolumeReclaimPolicy == "" {
				s.PersistentVolumeReclaimPolicy = "Retain"
			}
		},
		Validate: validatePV,
		ValidateUpdate: func(obj, old Object) ErrorList {
			s, prev := obj.(*PersistentVolume).Spec, old.(*PersistentVolume).Spec
			var errs ErrorList
			if !reflect.DeepEqual(s.HostPath, prev.HostPath) || !reflect.DeepEqual(s.Local, prev.Local) ||
				!reflect.DeepEqual(s.NodeAffinity, prev.NodeAffinity) {
				errs.forbidden("spec", "volume source and node affinity are immutable")
			}
			return errs
		},
	})
	Register(&Resource{
		Version: "v1", Plural: "persistentvolumeclaims", Singular: "persistentvolumeclaim", Kind: "PersistentVolumeClaim", ShortNames: []string{"pvc"},
		Namespaced: true, Create: true, Update: true, Delete: true, StatusSub: true, Fields: []string{"status.phase"},
		New: func() Object { return &PersistentVolumeClaim{} },
		Prepare: func(obj Object) {
			o := obj.(*PersistentVolumeClaim)
			o.Status = PersistentVolumeClaimStatus{Phase: "Pending"}
			if !slices.Contains(o.Metadata.Finalizers, PVCProtection) {
				o.Metadata.Finalizers = append(o.Metadata.Finalizers, PVCProtection)
			}
		},
		Default: func(obj Object) {
			s := &obj.(*PersistentVolumeClaim).Spec
			if s.VolumeMode == "" {
				s.VolumeMode = "Filesystem"
			}
		},
		Validate: validatePVC,
		ValidateUpdate: func(obj, old Object) ErrorList {
			o, prev := obj.(*PersistentVolumeClaim), old.(*PersistentVolumeClaim)
			var errs ErrorList
			if prev.Spec.VolumeName != "" && !reflect.DeepEqual(o.Spec, prev.Spec) {
				errs.forbidden("spec", "a named volume and its claim requirements are immutable")
			}
			if slices.Contains(prev.Metadata.Finalizers, PVCProtection) && !slices.Contains(o.Metadata.Finalizers, PVCProtection) {
				errs.forbidden("metadata.finalizers", "the volume controller removes PVC protection when the claim is unused")
			}
			return errs
		},
	})
	Register(&Resource{
		Group: StorageGroup, Version: "v1", Plural: "storageclasses", Singular: "storageclass", Kind: "StorageClass", ShortNames: []string{"sc"},
		Create: true, Update: true, Delete: true, New: func() Object { return &StorageClass{} },
		Default: func(obj Object) {
			o := obj.(*StorageClass)
			if o.ReclaimPolicy == "" {
				o.ReclaimPolicy = "Delete"
			}
			if o.VolumeBindingMode == "" {
				o.VolumeBindingMode = "Immediate"
			}
		},
		Validate: validateClass,
		ValidateUpdate: func(obj, old Object) ErrorList {
			o, prev := obj.(*StorageClass), old.(*StorageClass)
			var errs ErrorList
			if o.Provisioner != prev.Provisioner {
				errs.forbidden("provisioner", "field is immutable")
			}
			if o.ReclaimPolicy != prev.ReclaimPolicy {
				errs.forbidden("reclaimPolicy", "field is immutable")
			}
			if o.VolumeBindingMode != prev.VolumeBindingMode {
				errs.forbidden("volumeBindingMode", "field is immutable")
			}
			return errs
		},
	})
}

// DeleteDataAnnotation marks a released local-path volume whose claim had the
// Delete reclaim policy. Only the node that holds the directory removes it,
// and then the PersistentVolume object. Data is never removed because a
// volume object is missing (for example after restoring an older backup).
const DeleteDataAnnotation = "shearwater.io/delete-data"

// ManagedLocalPath reports whether the volume is a directory the local-path
// provisioner made: pvc-<uid> under LocalVolumeRoot.
func (o *PersistentVolume) ManagedLocalPath() bool {
	return o.Spec.Local != nil && len(o.Metadata.Name) > 4 && o.Metadata.Name[:4] == "pvc-" &&
		o.Spec.Local.Path == LocalVolumeRoot+"/"+o.Metadata.Name
}
