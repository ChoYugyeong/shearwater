package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"shearwater/internal/apis"
	"shearwater/internal/podlog"
)

// podLog serves GET .../pods/NAME/log (kubectl logs). The file is the one the
// shim writes (design 7): plain container output, no timestamps.
func (s *Server) podLog(w http.ResponseWriter, r *http.Request, res *apis.Resource, ri reqInfo) error {
	q := r.URL.Query()
	rec, err := s.Store.Get(keyOf(res, ri.namespace, ri.name))
	if err != nil {
		return err
	}
	pod := &apis.Pod{}
	if err := json.Unmarshal(rec.Data, pod); err != nil {
		return fmt.Errorf("stored %s is not readable: %v", rec.Key, err)
	}
	if s.LogDir == "" {
		return errUnsupported(http.StatusNotImplemented, "NotImplemented", "this server runs without a node agent, so there are no container logs").
			withHint("run `sw server` as root so that it starts containers")
	}
	for _, p := range []string{"timestamps", "sinceSeconds", "sinceTime"} {
		if v := q.Get(p); v != "" && v != "false" {
			return errUnsupported(http.StatusBadRequest, "BadRequest", "the log option %s is not supported: stage 1 logs carry no timestamps", p).
				withHint("use --tail=N or --limit-bytes=N to cut the output")
		}
	}

	container := q.Get("container")
	var names []string
	for _, c := range pod.Spec.Containers {
		names = append(names, c.Name)
	}
	switch {
	case container == "" && len(names) == 1:
		container = names[0]
	case container == "":
		return errBadRequest("a container name must be specified for pod %s, choose one of: [%s]", ri.name, strings.Join(names, " "))
	case !contains(names, container):
		return errBadRequest("container %s is not valid for pod %s", container, ri.name)
	}
	current := func(p *apis.Pod) *apis.ContainerStatus {
		for i := range p.Status.ContainerStatuses {
			if cs := &p.Status.ContainerStatuses[i]; cs.Name == container {
				return cs
			}
		}
		return nil
	}
	cs := current(pod)
	if cs == nil || cs.ContainerID == "" {
		why := "ContainerCreating"
		if cs != nil && cs.State.Waiting != nil && cs.State.Waiting.Reason != "" {
			why = cs.State.Waiting.Reason
		}
		return errBadRequest("container %q in pod %q is waiting to start: %s", container, ri.name, why)
	}
	// restartCount is the attempt number of the container that status describes.
	attempt := int(cs.RestartCount)
	if q.Get("previous") == "true" {
		if attempt--; attempt < 0 {
			return errBadRequest("previous terminated container %q in pod %q not found", container, ri.name)
		}
	}
	tail, err := intParam(q.Get("tailLines"))
	if err != nil {
		return errBadRequest("tailLines must be a non-negative integer, got %q", q.Get("tailLines"))
	}
	limit, err := intParam(q.Get("limitBytes"))
	if err != nil || limit == 0 {
		return errBadRequest("limitBytes must be a positive integer, got %q", q.Get("limitBytes"))
	}
	follow := q.Get("follow") == "true"

	f, err := os.Open(podlog.File(s.LogDir, ri.namespace, ri.name, pod.Metadata.UID, container, attempt))
	if errors.Is(err, os.ErrNotExist) && q.Get("previous") == "true" {
		return errBadRequest("previous terminated container %q in pod %q not found", container, ri.name)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(200)
	if f == nil {
		return nil // started, nothing written yet
	}
	defer f.Close()
	if tail >= 0 {
		off, err := tailOffset(f, tail)
		if err != nil {
			return nil
		}
		f.Seek(off, io.SeekStart)
	}
	var out io.Writer = w
	left := limit // -1: no limit
	flush := http.NewResponseController(w)
	send := func() (more bool) {
		var src io.Reader = f
		if left >= 0 {
			src = io.LimitReader(f, left)
		}
		n, err := io.Copy(out, src)
		if left >= 0 {
			left -= n
		}
		flush.Flush()
		return err == nil && left != 0
	}
	if !send() || !follow {
		return nil
	}
	// Follow: the file grows while this attempt runs. It is over when status
	// moves on (the container ended or was replaced) and the rest has been sent.
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return nil
		case <-tick.C:
		}
		over := true
		if rec, err := s.Store.Get(keyOf(res, ri.namespace, ri.name)); err == nil && rec.UID == pod.Metadata.UID {
			now := &apis.Pod{}
			if json.Unmarshal(rec.Data, now) == nil {
				if cs := current(now); cs != nil && int(cs.RestartCount) == attempt && cs.State.Running != nil {
					over = false
				}
			}
		}
		if !send() || over {
			return nil
		}
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// intParam parses an optional non-negative number; -1 means "not given".
func intParam(v string) (int64, error) {
	if v == "" {
		return -1, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("not a non-negative integer")
	}
	return n, nil
}

// tailOffset returns where the last n lines of f start.
func tailOffset(f *os.File, n int64) (int64, error) {
	st, err := f.Stat()
	if err != nil {
		return 0, err
	}
	size := st.Size()
	if n == 0 {
		return size, nil
	}
	const chunk = 64 << 10
	buf := make([]byte, chunk)
	pos, seen := size, int64(0)
	for pos > 0 {
		step := min(pos, chunk)
		pos -= step
		if _, err := f.ReadAt(buf[:step], pos); err != nil && err != io.EOF {
			return 0, err
		}
		b := buf[:step]
		for {
			i := bytes.LastIndexByte(b, '\n')
			if i < 0 {
				break
			}
			// The newline that ends the file ends the last line; it starts nothing.
			if pos+int64(i) != size-1 {
				if seen++; seen == n {
					return pos + int64(i) + 1, nil
				}
			}
			b = b[:i]
		}
	}
	return 0, nil
}
