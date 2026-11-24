package protobuf

// A typed Pod or Deployment can carry a claim in its inline VolumeSource.
var claimVolumeSource = &message{"PersistentVolumeClaimVolumeSource", map[int]field{
	1: str("claimName"), 2: boolean("readOnly"),
}}
