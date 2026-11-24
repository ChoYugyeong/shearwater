package api

import (
	"slices"
	"testing"
	"time"

	"shearwater/internal/apis"
)

func TestStorageTableCells(t *testing.T) {
	expand := false
	class := "local-path"
	tests := []struct {
		kind string
		obj  apis.Object
		want []any
	}{
		{"PersistentVolume", &apis.PersistentVolume{Metadata: apis.ObjectMeta{Name: "disk"},
			Spec: apis.PersistentVolumeSpec{
				Capacity: map[string]apis.Quantity{"storage": "5Gi"}, AccessModes: []string{"ReadWriteOnce"},
				PersistentVolumeReclaimPolicy: "Retain", StorageClassName: class, VolumeMode: "Filesystem",
				ClaimRef: &apis.ObjectReference{Namespace: "app", Name: "data"}},
			Status: apis.PersistentVolumeStatus{Phase: "Bound"}},
			[]any{"disk", "5Gi", "RWO", "Retain", "Bound", "app/data", class, "<unset>", "", "<unknown>", "Filesystem"}},
		{"PersistentVolumeClaim", &apis.PersistentVolumeClaim{Metadata: apis.ObjectMeta{Name: "data"},
			Spec: apis.PersistentVolumeClaimSpec{StorageClassName: &class, VolumeName: "disk", VolumeMode: "Filesystem"},
			Status: apis.PersistentVolumeClaimStatus{Phase: "Bound", Capacity: map[string]apis.Quantity{"storage": "5Gi"},
				AccessModes: []string{"ReadWriteOnce"}}},
			[]any{"data", "Bound", "disk", "5Gi", "RWO", class, "<unset>", "<unknown>", "Filesystem"}},
		{"StorageClass", &apis.StorageClass{Metadata: apis.ObjectMeta{Name: class, Annotations: map[string]string{apis.DefaultClassAnnotation: "true"}},
			Provisioner: apis.LocalPathProvisioner, ReclaimPolicy: "Delete", VolumeBindingMode: "WaitForFirstConsumer", AllowVolumeExpansion: &expand},
			[]any{"local-path (default)", apis.LocalPathProvisioner, "Delete", "WaitForFirstConsumer", false, "<unknown>"}},
	}
	for _, tc := range tests {
		got := cells(tc.obj, time.Now())
		if len(got) != len(tableColumns[tc.kind]) || !slices.Equal(got, tc.want) {
			t.Errorf("%s: got %v want %v", tc.kind, got, tc.want)
		}
	}
}
