package apis

// Annotations written by the workload controllers.
const (
	DeploymentRevisionAnnotation        = "deployment.kubernetes.io/revision"
	DeploymentRevisionHistoryAnnotation = "deployment.kubernetes.io/revision-history"
	DeploymentDesiredReplicasAnnotation = "deployment.kubernetes.io/desired-replicas"
	PodDeploymentUIDAnnotation          = "shearwater.io/deployment-uid"
)
