// Package podlog knows where container logs live (design 7). The node agent
// hands these paths to the shim; the API server reads them for `kubectl logs`.
package podlog

import (
	"path/filepath"
	"strconv"
)

// Dir is the log directory of one pod: <root>/pods/<namespace>_<name>_<uid>.
func Dir(root, namespace, name, uid string) string {
	return filepath.Join(root, "pods", namespace+"_"+name+"_"+uid)
}

// File is the log of one attempt of one container.
func File(root, namespace, name, uid, container string, attempt int) string {
	return filepath.Join(Dir(root, namespace, name, uid), container, strconv.Itoa(attempt)+".log")
}
