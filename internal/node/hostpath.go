package node

import (
	"errors"
	"fmt"
	"os"

	"shearwater/internal/apis"
)

// hostPath returns the host directory or file to mount for a hostPath volume.
func hostPath(v *apis.HostPathVolumeSource) (string, error) {
	if v.Type == "" {
		return v.Path, nil
	}
	info, err := os.Stat(v.Path)
	if errors.Is(err, os.ErrNotExist) {
		switch v.Type {
		case "DirectoryOrCreate":
			err = os.MkdirAll(v.Path, 0o755)
		case "FileOrCreate":
			var f *os.File
			f, err = os.OpenFile(v.Path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
			if err == nil {
				err = f.Close()
			}
		}
		if err == nil || errors.Is(err, os.ErrExist) {
			info, err = os.Stat(v.Path)
		}
	}
	if err != nil {
		return "", err
	}
	mode := info.Mode()
	var valid bool
	switch v.Type {
	case "Directory", "DirectoryOrCreate":
		valid = mode.IsDir()
	case "File", "FileOrCreate":
		valid = mode.IsRegular()
	case "Socket":
		valid = mode&os.ModeSocket != 0
	case "CharDevice":
		valid = mode&os.ModeDevice != 0 && mode&os.ModeCharDevice != 0
	case "BlockDevice":
		valid = mode&os.ModeDevice != 0 && mode&os.ModeCharDevice == 0
	}
	if !valid {
		return "", fmt.Errorf("hostPath %q does not match type %s", v.Path, v.Type)
	}
	return v.Path, nil
}
