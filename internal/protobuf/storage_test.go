package protobuf_test

import "testing"

func TestPodProtobufClaimVolume(t *testing.T) {
	claim := cat(sfield(1, "disk"), vfield(2, 1))
	volume := cat(sfield(1, "data"), bfield(2, bfield(10, claim)))
	pod := mustDecode(t, envelope("v1", "Pod", bfield(2, bfield(1, volume))))
	if at(pod, "spec", "volumes", 0, "persistentVolumeClaim", "claimName") != "disk" ||
		at(pod, "spec", "volumes", 0, "persistentVolumeClaim", "readOnly") != true {
		t.Fatalf("claim volume lost in typed Pod: %v", pod)
	}
}
