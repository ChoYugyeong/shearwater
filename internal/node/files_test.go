package node

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGeneratedPodFileRepairsPrivateModeWithoutRewriting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts")
	data := []byte("127.0.0.1 localhost\n")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(path)
	if err := writeIfChanged(path, data); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(path)
	if after.Mode().Perm() != 0644 || !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("mode repair rewrote content or left file unreadable")
	}
}
