package node

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"shearwater/internal/apis"
	"shearwater/internal/objects"
	"shearwater/internal/store"
)

type probeSample struct {
	Next             time.Time
	Success, Failure int32
	Healthy          bool
	Message          string
}

// Only startup completion and a requested stop survive a server restart.
// Consecutive sample counts deliberately restart at zero. This prevents a
// restarted observer from killing an app using a half-observed failure run.
type probeState struct {
	ID           string                  `json:"id"`
	Started      bool                    `json:"started"`
	StopReason   string                  `json:"stopReason,omitempty"`
	StopDeadline time.Time               `json:"stopDeadline,omitempty"`
	Samples      map[string]*probeSample `json:"-"`
}

func (a *Agent) probeFile(p *apis.Pod, c *apis.Container) string {
	return filepath.Join(a.podDir(p.Metadata.UID), "probe-"+c.Name+".json")
}
func (a *Agent) loadProbe(p *apis.Pod, c *apis.Container, id string) *probeState {
	key := p.Metadata.UID + "/" + c.Name
	a.mu.Lock()
	defer a.mu.Unlock()
	if v := a.probes[key]; v != nil && v.ID == id {
		return v
	}
	v := &probeState{ID: id}
	b, err := os.ReadFile(a.probeFile(p, c))
	if err == nil {
		var old probeState
		if json.Unmarshal(b, &old) == nil && old.ID == id {
			v = &old
		}
	}
	v.Samples = map[string]*probeSample{}
	a.probes[key] = v
	return v
}
func (a *Agent) saveProbe(p *apis.Pod, c *apis.Container, v *probeState) error {
	path := a.probeFile(p, c)
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	// Rename in the same filesystem: interrupted writes cannot leave a half JSON
	// record which falsely claims that a startup probe completed.
	f, err := os.CreateTemp(filepath.Dir(path), ".probe-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err = f.Write(b); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
func (a *Agent) probeStopReason(p *apis.Pod, c *apis.Container, id string) string {
	if id == "" {
		return ""
	}
	return a.loadProbe(p, c, id).StopReason
}
func (a *Agent) probeEvent(p *apis.Pod, c *apis.Container, kind, reason, message string) error {
	return a.Store.Write(false, func(tx *store.Tx) error {
		rec, err := tx.Get(objects.Key(podsRes, p.Metadata.Namespace, p.Metadata.Name))
		if err != nil {
			return err
		}
		if rec.UID != p.Metadata.UID || rec.Deleting {
			return nil
		}
		return objects.PodEvent(tx, p, a.Now(), "probe/"+c.Name+"/"+kind, "spec.containers{"+c.Name+"}", reason, message, "shearwater-node", "Warning")
	})
}

func (a *Agent) checkProbes(ctx context.Context, pod *apis.Pod, c *apis.Container, cs apis.ContainerStatus, ip string) (apis.ContainerStatus, time.Duration) {
	raws := map[string]json.RawMessage{"startup": c.StartupProbe, "readiness": c.ReadinessProbe, "liveness": c.LivenessProbe}
	anyProbe := false
	for _, v := range raws {
		if len(v) > 0 && string(v) != "null" && string(v) != "{}" {
			anyProbe = true
		}
	}
	if !anyProbe {
		return cs, 0
	}
	v := a.loadProbe(pod, c, cs.ContainerID)
	now := a.Now()
	ps := map[string]*apis.Probe{}
	for kind, raw := range raws {
		q, err := apis.ParseProbe(raw, kind, c)
		if err != nil {
			cs.Ready = false
			cs.Started = ptr(false)
			a.Logf("invalid stored %s probe: %v", kind, err)
			return cs, failureRetry
		}
		ps[kind] = q
	}
	stop := func() (apis.ContainerStatus, time.Duration) {
		cs.Ready = false
		cs.Started = ptr(v.Started)
		sig := syscall.SIGTERM
		if !now.Before(v.StopDeadline) {
			sig = syscall.SIGKILL
		}
		if err := a.Runtime.Signal(ctx, strings.TrimPrefix(cs.ContainerID, idPrefix), sig); err != nil {
			a.Logf("probe termination: %v", err)
		}
		return cs, time.Second
	}
	if v.StopReason != "" {
		return stop()
	}
	// Conservative adoption: a readiness probe must succeed in this observer
	// before traffic returns; startup completion is persisted for slow starters.
	cs.Started = ptr(ps["startup"] == nil || v.Started)
	cs.Ready = *cs.Started && ps["readiness"] == nil
	retry := time.Duration(0)
	for _, kind := range []string{"startup", "readiness", "liveness"} {
		q := ps[kind]
		if q == nil || (kind == "startup" && v.Started) {
			continue
		}
		if kind != "startup" && !*cs.Started {
			continue
		}
		s := v.Samples[kind]
		if s == nil {
			s = &probeSample{}
			v.Samples[kind] = s
		}
		due := s.Next
		if cs.State.Running.StartedAt != nil {
			initial := cs.State.Running.StartedAt.Add(time.Duration(q.InitialDelaySeconds) * time.Second)
			if initial.After(due) {
				due = initial
			}
		}
		if !now.Before(due) {
			actionCtx, cancel := context.WithTimeout(ctx, time.Duration(q.TimeoutSeconds)*time.Second)
			run := a.ProbeRun
			if run == nil {
				run = a.runProbe
			}
			err := run(actionCtx, q, c, ip, strings.TrimPrefix(cs.ContainerID, idPrefix))
			cancel()
			if ctx.Err() != nil {
				return cs, time.Second
			}
			s.Next = now.Add(time.Duration(q.PeriodSeconds) * time.Second)
			if err == nil {
				s.Success++
				s.Failure = 0
				if s.Success >= q.SuccessThreshold {
					s.Healthy = true
				}
				s.Message = ""
			} else {
				s.Failure++
				s.Success = 0
				msg := err.Error()
				if len(msg) > 512 {
					msg = msg[:512]
				}
				if s.Message != msg {
					if e := a.probeEvent(pod, c, kind, "Unhealthy", kind+" probe failed: "+msg); e != nil {
						a.Logf("probe event: %v", e)
					}
					s.Message = msg
				}
				if s.Failure >= q.FailureThreshold {
					s.Healthy = false
				}
			}
			if kind == "startup" && s.Healthy {
				v.Started = true
				if err := a.saveProbe(pod, c, v); err != nil {
					v.Started = false
					a.Logf("persist startup probe: %v", err)
					return cs, failureRetry
				}
				cs.Started = ptr(true)
				cs.Ready = ps["readiness"] == nil
			}
			if kind != "readiness" && s.Failure >= q.FailureThreshold {
				v.StopReason = kind + "ProbeFailed"
				grace := int64(30)
				if pod.Spec.TerminationGracePeriodSeconds != nil {
					grace = *pod.Spec.TerminationGracePeriodSeconds
				}
				if q.TerminationGracePeriodSeconds != nil {
					grace = *q.TerminationGracePeriodSeconds
				}
				v.StopDeadline = now.Add(time.Duration(grace) * time.Second)
				if err := a.saveProbe(pod, c, v); err != nil {
					v.StopReason = ""
					a.Logf("persist probe stop: %v", err)
					return cs, failureRetry
				}
				return stop()
			}
			due = s.Next
		}
		if kind == "readiness" {
			cs.Ready = *cs.Started && s.Healthy
		}
		wait := due.Sub(now)
		if wait < time.Millisecond {
			wait = time.Millisecond
		}
		if retry == 0 || wait < retry {
			retry = wait
		}
	}
	return cs, retry
}

func (a *Agent) runProbe(ctx context.Context, p *apis.Probe, c *apis.Container, ip, id string) error {
	if p.Exec != nil {
		r, ok := a.Runtime.(interface {
			ProbeExec(context.Context, string, []string) error
		})
		if !ok {
			return errors.New("runtime does not support exec probes")
		}
		return r.ProbeExec(ctx, id, p.Exec.Command)
	}
	addr, err := netip.ParseAddr(ip)
	if err != nil || !addr.Is4() || !a.PodCIDR.Contains(addr) {
		return errors.New("probe target is not an allocated Pod address")
	}
	port := 0
	if p.HTTPGet != nil {
		port, err = apis.ProbePort(p.HTTPGet.Port, c)
	} else {
		port, err = apis.ProbePort(p.TCPSocket.Port, c)
	}
	if err != nil {
		return err
	}
	target := net.JoinHostPort(ip, fmt.Sprint(port))
	if p.TCPSocket != nil {
		d := net.Dialer{}
		conn, err := d.DialContext(ctx, "tcp", target)
		if err == nil {
			conn.Close()
		}
		return err
	}
	h := p.HTTPGet
	// Kubernetes HTTPS probes test application reachability, not PKI. The target
	// is still restricted to the Pod IP, with no proxy or redirect to another host.
	transport := &http.Transport{Proxy: nil, TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, "GET", strings.ToLower(h.Scheme)+"://"+target+h.Path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "shearwater-probe")
	req.Header.Set("Accept", "*/*")
	for _, v := range h.HTTPHeaders {
		if strings.EqualFold(v.Name, "Host") {
			req.Host = v.Value
		} else {
			req.Header.Add(v.Name, v.Value)
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP status %d", resp.StatusCode)
	}
	return nil
}
