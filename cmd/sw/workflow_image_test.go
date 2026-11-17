package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"shearwater/internal/apis"
)

func TestStagedWorkflowImageIsCopiedAndCannotBeRetypedOrSwapped(t *testing.T) {
	c, w, o := workflowFixture(t)
	artifact := filepath.Join(t.TempDir(), "image.json")
	image := "example.test/web@sha256:" + strings.Repeat("a", 64)
	data, _ := json.Marshal(map[string]string{"image": image})
	if err := os.WriteFile(artifact, data, 0644); err != nil {
		t.Fatal(err)
	}
	if err := runWorkflow([]string{"stage-image", "--file=" + o.Workflow.Path, "--image-file=" + artifact}, io.Discard); err != nil {
		t.Fatal(err)
	}
	// 빌드가 다음 결과 파일을 써도 이미 선택한 값은 저절로 바뀌지 않는다.
	if err := os.WriteFile(artifact, []byte(`{"image":"another:version"}`), 0644); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadWorkflow(o.Workflow.Path)
	if err != nil || loaded.UpdateImage != image {
		t.Fatal(loaded.UpdateImage, err)
	}
	w = &loaded
	if _, err = executeWorkflow(o, c, w, "deploy", ""); err != nil {
		t.Fatal(err)
	}
	// 첫 계획이 대기 중이라면 선택한 이미지를 바꾸지 못하게 한다.
	if err = runWorkflow([]string{"stage-image", "--file=" + o.Workflow.Path, "--image-file=" + artifact}, io.Discard); err == nil {
		t.Fatal("changed staged input while a plan was pending")
	}
	approveWorkflowFixture(t, w)
	var out bytes.Buffer
	if err = executePlan(context.Background(), c, w.Config, w.Listen, w.Plans, w.Pending, &out); err != nil {
		t.Fatal(err)
	}
	if _, err = w.consumeReceipt(c); err != nil {
		t.Fatal(err)
	}
	if err = atomicJSON(o.Workflow.Path, w); err != nil {
		t.Fatal(err)
	}
	// 같은 길이의 잘못 옮긴 digest도 원문과 다르면 계획을 만들지 못한다.
	wrong := "example.test/web@sha256:" + strings.Repeat("b", 64)
	if _, err = executeWorkflow(o, c, w, "update", wrong); err == nil || w.Pending != "" {
		t.Fatal("accepted a retyped image", err)
	}
	if _, err = executeWorkflow(o, c, w, "update", ""); err != nil {
		t.Fatal(err)
	}
	p, err := loadPlan(w.Plans, w.Pending)
	if err != nil || len(p.Actions) != 1 {
		t.Fatal(p, err)
	}
	var proposed apis.Deployment
	if err = json.Unmarshal(p.Actions[0].Body, &proposed); err != nil || proposed.Spec.Template.Spec.Containers[0].Image != image {
		t.Fatal("plan lost the exact selected image", err)
	}
	actual, err := w.deployment(context.Background(), c)
	if err != nil || actual.Spec.Template.Spec.Containers[0].Image != w.Image {
		t.Fatal("staging or planning applied the new image", err)
	}
}

func TestWorkflowImageArtifactRejectsNonFilesAndUnboundedInput(t *testing.T) {
	root := t.TempDir()
	for _, body := range []string{`{}`, `{"image":""}`, `{"image":"web:1","execute":"anything"}`, `{"image":"web:1\nsecret"}`, strings.Repeat(" ", 4097)} {
		file := filepath.Join(root, "bad.json")
		if err := os.WriteFile(file, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := readWorkflowImageArtifact(file); err == nil {
			t.Fatal("accepted invalid artifact")
		}
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(filepath.Join(root, "bad.json"), link); err != nil {
		t.Fatal(err)
	}
	if _, err := readWorkflowImageArtifact(link); err == nil {
		t.Fatal("followed image artifact symlink")
	}
	fifo := filepath.Join(root, "pipe")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readWorkflowImageArtifact(fifo); err == nil {
		t.Fatal("accepted FIFO artifact")
	}
}
