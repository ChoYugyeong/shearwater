package node

import (
	"context"
	"crypto/tls"
	"crypto/x509"
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
	"shearwater/internal/pki"
	"shearwater/internal/podlog"
	"shearwater/internal/store"
)

// LogPort is where a node answers the control plane's log requests (the
// kubelet's port).
const LogPort = 10250

// LogServer streams container logs of this node's Pods to the control plane,
// which relays `kubectl logs` for Pods on other servers. Only a client with
// the control plane's certificate (signed by the cluster CA) may ask.
type LogServer struct {
	Agent *Agent
	Cert  tls.Certificate // this node's serving certificate
	CA    []byte          // cluster CA (PEM)
	Addr  string          // NODE-IP:10250
}

// Run serves until ctx ends.
func (l *LogServer) Run(ctx context.Context) error {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(l.CA) {
		return errors.New("log server: no cluster CA")
	}
	cfg := &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{l.Cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
		VerifyPeerCertificate: func(_ [][]byte, chains [][]*x509.Certificate) error {
			if len(chains) == 0 || len(chains[0]) == 0 || chains[0][0].Subject.CommonName != pki.KubeletClientName {
				return errors.New("only the control plane may read logs here")
			}
			return nil
		},
	}
	ln, err := tls.Listen("tcp", l.Addr, cfg)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: l, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// ServeHTTP answers GET /containerLogs/NAMESPACE/POD/CONTAINER?uid=&attempt=&tailLines=&limitBytes=&follow=.
// The control plane has already checked the request against the Pod's status;
// this side only checks that the Pod is still the one bound here.
func (l *LogServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/containerLogs/"), "/")
	if r.Method != http.MethodGet || !strings.HasPrefix(r.URL.Path, "/containerLogs/") || len(parts) != 3 {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	ns, name, container := parts[0], parts[1], parts[2]
	q := r.URL.Query()
	uid := q.Get("uid")
	attempt, err1 := strconv.Atoi(q.Get("attempt"))
	tail, err2 := strconv.ParseInt(q.Get("tailLines"), 10, 64)
	limit, err3 := strconv.ParseInt(q.Get("limitBytes"), 10, 64)
	if err1 != nil || err2 != nil || err3 != nil || attempt < 0 {
		http.Error(w, "uid, attempt, tailLines and limitBytes are required", http.StatusBadRequest)
		return
	}
	a := l.Agent
	key := store.Key{Resource: "pods", Namespace: ns, Name: name}
	rec, err := a.Store.Get(key)
	var pod apis.Pod
	if err != nil || rec.UID != uid || json.Unmarshal(rec.Data, &pod) != nil || pod.Spec.NodeName != a.NodeName {
		http.Error(w, fmt.Sprintf("pod %s/%s with uid %s is not on node %s", ns, name, uid, a.NodeName), http.StatusNotFound)
		return
	}
	f, err := os.Open(podlog.File(a.LogDir, ns, name, uid, container, attempt))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	if f == nil {
		return
	}
	defer f.Close()
	if tail >= 0 {
		if off, err := tailOffset(f, tail); err == nil {
			f.Seek(off, io.SeekStart)
		}
	}
	left := limit
	flush := http.NewResponseController(w)
	send := func() bool {
		var src io.Reader = f
		if left >= 0 {
			src = io.LimitReader(f, left)
		}
		n, err := io.Copy(w, src)
		if left >= 0 {
			left -= n
		}
		flush.Flush()
		return err == nil && left != 0
	}
	if !send() || q.Get("follow") != "true" {
		return
	}
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
		}
		over := true
		if rec, err := a.Store.Get(key); err == nil && rec.UID == uid {
			var now apis.Pod
			if json.Unmarshal(rec.Data, &now) == nil {
				for _, cs := range now.Status.ContainerStatuses {
					if cs.Name == container && int(cs.RestartCount) == attempt && cs.State.Running != nil {
						over = false
					}
				}
			}
		}
		if !send() || over {
			return
		}
	}
}

// tailOffset finds where the last n lines of f start.
func tailOffset(f *os.File, n int64) (int64, error) {
	end, err := f.Seek(0, io.SeekEnd)
	if err != nil || n == 0 {
		return end, err
	}
	buf := make([]byte, 32<<10)
	pos, lines := end, int64(0)
	for pos > 0 {
		size := min(int64(len(buf)), pos)
		pos -= size
		if _, err := f.ReadAt(buf[:size], pos); err != nil {
			return 0, err
		}
		for i := size - 1; i >= 0; i-- {
			if buf[i] != '\n' || pos+i == end-1 {
				continue
			}
			if lines++; lines == n {
				return pos + i + 1, nil
			}
		}
	}
	return 0, nil
}
