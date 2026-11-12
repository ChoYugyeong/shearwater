package api

import "shearwater/internal/apis"

// Preserve controller-assigned annotations on ordinary API writes. A nil
// previous object means a create, so the controller will populate them later.
func preserveRolloutAnnotations(obj apis.Object, previous *apis.ObjectMeta) {
	var keys []string
	switch obj.(type) {
	case *apis.Deployment:
		keys = []string{apis.DeploymentRevisionAnnotation}
	case *apis.ReplicaSet:
		keys = []string{apis.DeploymentRevisionAnnotation, apis.DeploymentRevisionHistoryAnnotation, apis.DeploymentDesiredReplicasAnnotation}
	case *apis.Pod:
		keys = []string{apis.PodDeploymentUIDAnnotation}
	}
	meta := obj.Meta()
	for _, key := range keys {
		if previous != nil {
			if value, ok := previous.Annotations[key]; ok {
				if meta.Annotations == nil {
					meta.Annotations = make(map[string]string)
				}
				meta.Annotations[key] = value
				continue
			}
		}
		delete(meta.Annotations, key)
	}
}
