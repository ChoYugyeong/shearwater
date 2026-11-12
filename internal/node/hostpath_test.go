package node

import (
	"os"
	"path/filepath"
	"testing"

	"shearwater/internal/apis"
)

func TestHostPathTypes(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "new", "dir")
	file := filepath.Join(root, "file")
	cases := []struct {
		path, kind string
		ok         bool
	}{
		{dir, "Directory", false},
		{dir, "DirectoryOrCreate", true},
		{dir, "Directory", true},
		{dir, "FileOrCreate", false},
		{file, "File", false},
		{file, "FileOrCreate", true},
		{file, "File", true},
		{file, "DirectoryOrCreate", false},
		{file, "Socket", false},
		{file, "CharDevice", false},
		{file, "BlockDevice", false},
		{filepath.Join(root, "absent", "file"), "FileOrCreate", false},
	}
	for _, tc := range cases {
		got, err := hostPath(&apis.HostPathVolumeSource{Path: tc.path, Type: tc.kind})
		if tc.ok && (err != nil || got != tc.path) || !tc.ok && err == nil {
			t.Errorf("hostPath(%q, %q) = %q, %v; want success=%v", tc.path, tc.kind, got, err, tc.ok)
		}
	}
	if err := os.WriteFile(file, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := hostPath(&apis.HostPathVolumeSource{Path: file, Type: "FileOrCreate"}); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(file); err != nil || string(b) != "keep" {
		t.Fatalf("existing file changed: %q, %v", b, err)
	}
}
