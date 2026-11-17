package main

// 모델이 긴 이미지 digest를 한 글자 잘못 옮겨도 다른 배포가 될 수 있다.
// 빌드 도구가 쓴 작은 JSON을 일반 사용자 CLI가 읽어 작업 파일에 복사하면,
// 모델은 문자열을 다시 쓰지 않고 "선택한 이미지로 update"만 요청할 수 있다.
// 이 입력은 계획도 승인도 아니다. 실제 변경은 이후 원문 검토·승인을 거친다.
import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode"
)

func validWorkflowImage(image string) bool {
	return image != "" && len(image) <= 1024 && strings.IndexFunc(image, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}) < 0
}

func readWorkflowImageArtifact(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("image artifact path must be absolute")
	}
	// FIFO로 멈추거나 심볼릭 링크를 따라 예상 밖 파일을 읽지 않는다.
	// 이미지 이름은 비밀이 아니므로 0600은 요구하지 않지만, 저장될 작업
	// 파일은 기존 private-file 규칙(소유자/0600)을 그대로 적용한다.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", fmt.Errorf("open image artifact: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 4096 {
		return "", fmt.Errorf("image artifact must be a regular JSON file of at most 4096 bytes")
	}
	b, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil || len(b) > 4096 {
		return "", fmt.Errorf("image artifact cannot be read within its size limit")
	}
	var artifact struct {
		Image string `json:"image"`
	}
	if err = decodeStrict(b, &artifact); err != nil || !validWorkflowImage(artifact.Image) {
		return "", fmt.Errorf("image artifact requires only one nonempty bounded image reference")
	}
	return artifact.Image, nil
}
