package agent

import (
	"strings"
	"testing"

	"shearwater/internal/apis"
)

func TestPodFindingsDescribeEvidenceWithoutInventingCause(t *testing.T) {
	for _, reason := range []string{"ErrImagePull", "ImagePullBackOff", "CrashLoopBackOff", "CreateContainerConfigError", "CreatePodSandboxError", "RunContainerError", "ContainerCreating"} {
		t.Run(reason, func(t *testing.T) {
			p := &apis.Pod{Spec: apis.PodSpec{NodeName: "node"}, Status: apis.PodStatus{ContainerStatuses: []apis.ContainerStatus{{Name: "app", State: apis.ContainerState{Waiting: &apis.ContainerStateWaiting{Reason: reason, Message: "observed error"}}}}}}
			f := PodFindings(p)
			if len(f) != 1 || f[0].Code != reason || !strings.Contains(f[0].Message, "observed error") || f[0].Next == "" {
				t.Fatal(f)
			}
		})
	}
	p := &apis.Pod{Spec: apis.PodSpec{NodeName: "node"}, Status: apis.PodStatus{ContainerStatuses: []apis.ContainerStatus{{Name: "app", State: apis.ContainerState{Terminated: &apis.ContainerStateTerminated{ExitCode: 137, Reason: "Error"}}}}}}
	f := PodFindings(p)
	if len(f) != 1 || !strings.Contains(f[0].Message, "exit=137") || strings.Contains(f[0].Message, "OOM") {
		t.Fatal(f)
	}
	p.Status.ContainerStatuses[0].State = apis.ContainerState{Running: &apis.ContainerStateRunning{}}
	f = PodFindings(p)
	if len(f) != 1 || f[0].Code != "NoFailureReported" || !strings.Contains(f[0].Next, "probes are stored") {
		t.Fatal(f)
	}
}
