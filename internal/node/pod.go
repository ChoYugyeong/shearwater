package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"slices"
	"strings"
	"syscall"
	"time"

	"shearwater/internal/apis"
	"shearwater/internal/objects"
	"shearwater/internal/podlog"
	"shearwater/internal/runtime"
	"shearwater/internal/store"
)

const (
	backoffBase  = 10 * time.Second
	backoffMax   = 300 * time.Second
	backoffReset = 10 * time.Minute // a run this long clears the delay (design 6)
	killRetry    = time.Second
	idPrefix     = "containerd://"
)

// backoff is the wait before a start that follows n failures in a row: none
// after the first, then 10 s doubling up to 300 s.
func backoff(n int) time.Duration {
	if n <= 0 {
		return 0
	}
	d := backoffBase
	for i := 1; i < n && d < backoffMax; i++ {
		d *= 2
	}
	return min(d, backoffMax)
}

// loadPod returns the pod with this uid if it is bound to this node.
func (a *Agent) loadPod(uid string) (*apis.Pod, bool, error) {
	a.mu.Lock()
	key, known := a.keys[uid]
	a.mu.Unlock()
	if known {
		rec, err := a.Store.Get(key)
		if err == nil && rec.UID == uid {
			pod := &apis.Pod{}
			if err := json.Unmarshal(rec.Data, pod); err != nil {
				return nil, false, fmt.Errorf("stored pod %s is not readable: %v", key, err)
			}
			return pod, true, nil
		}
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return nil, false, err
		}
	}
	// Not where we thought, or never seen: the list is the authority. Tearing
	// a pod down because of a stale index would be the worst mistake here.
	recs, _, err := a.Store.List("", podsRes.Plural, "")
	if err != nil {
		return nil, false, err
	}
	for _, rec := range recs {
		if rec.UID != uid {
			continue
		}
		pod := &apis.Pod{}
		if err := json.Unmarshal(rec.Data, pod); err != nil {
			return nil, false, fmt.Errorf("stored pod %s is not readable: %v", rec.Key, err)
		}
		if pod.Spec.NodeName != a.NodeName {
			return nil, false, nil
		}
		a.mu.Lock()
		a.keys[uid] = rec.Key
		a.mu.Unlock()
		return pod, true, nil
	}
	a.mu.Lock()
	delete(a.keys, uid)
	a.mu.Unlock()
	return nil, false, nil
}

// SyncPod looks at one pod uid once and does what is due now. retry is when
// to look again without waiting for an event (0: no need).
func (a *Agent) SyncPod(ctx context.Context, uid string) (retry time.Duration, err error) {
	a.init()
	if !runtime.IsUID(uid) {
		return 0, nil // not something we made
	}
	pod, found, err := a.loadPod(uid)
	if err != nil {
		return failureRetry, err
	}
	// Reality first (design 6): what containerd has, not what status says.
	actual, err := a.Runtime.Containers(ctx, uid)
	if err != nil {
		return failureRetry, fmt.Errorf("reading containers: %w", err)
	}
	slices.SortFunc(actual, func(x, y runtime.Container) int { return x.Attempt - y.Attempt })
	switch {
	case !found:
		return a.cleanup(ctx, uid, actual, false)
	case pod.Metadata.DeletionTimestamp != nil:
		return a.terminate(ctx, pod, actual)
	}
	// Make in-flight pulls and task creation interruptible by a delete event.
	startCtx, cancel := context.WithCancel(ctx)
	a.mu.Lock()
	w := a.work[uid]
	if w != nil {
		w.cancelStart = cancel
	}
	a.mu.Unlock()
	defer func() {
		cancel()
		if w != nil {
			a.mu.Lock()
			w.cancelStart = nil
			a.mu.Unlock()
		}
	}()
	// Re-read after installing the cancellation hook to cover deletes that
	// happened while the actual container state was being read.
	pod, found, err = a.loadPod(uid)
	if err != nil {
		return failureRetry, err
	}
	if !found {
		return a.cleanup(ctx, uid, actual, false)
	}
	if pod.Metadata.DeletionTimestamp != nil {
		return a.terminate(ctx, pod, actual)
	}
	return a.reconcile(startCtx, pod, actual)
}

// cleanup removes what is left of a pod that has no object any more: the pod
// was deleted without grace, or something was left behind half-made.
func (a *Agent) cleanup(ctx context.Context, uid string, actual []runtime.Container, keepIP bool) (time.Duration, error) {
	for _, c := range runtime.RemovalOrder(actual) {
		if err := a.Runtime.Remove(ctx, c.ID); err != nil {
			return failureRetry, err
		}
		a.mu.Lock()
		delete(a.termSent, c.ID)
		a.mu.Unlock()
	}
	if err := a.Net.Teardown(uid); err != nil {
		return failureRetry, err
	}
	a.removePodFiles(uid)
	if err := a.Runtime.PodGone(ctx, uid); err != nil {
		a.Logf("pod %s: %v", uid, err)
	}
	// The address goes back last: only when no object, container or namespace
	// can still be using it (design 6).
	if keepIP {
		return 0, nil
	}
	if err := a.Store.ReleaseIP(uid); err != nil {
		return failureRetry, err
	}
	return 0, nil
}

// terminate handles a pod with a deletionTimestamp: TERM, wait for the grace
// period, KILL what is left, clean up, and only then let the object go.
func (a *Agent) terminate(ctx context.Context, pod *apis.Pod, actual []runtime.Container) (time.Duration, error) {
	uid, now := pod.Metadata.UID, a.Now()
	deadline := pod.Metadata.DeletionTimestamp.Time
	var running []runtime.Container
	for _, c := range actual {
		if c.State == runtime.StateRunning {
			running = append(running, c)
		}
	}
	if len(running) > 0 {
		expired := !now.Before(deadline)
		for _, c := range running {
			a.mu.Lock()
			sent := a.termSent[c.ID]
			a.mu.Unlock()
			sig := syscall.SIGTERM
			if expired {
				sig = syscall.SIGKILL
			} else if sent {
				continue
			}
			if err := a.Runtime.Signal(ctx, c.ID, sig); err != nil {
				return killRetry, err
			}
			if sig == syscall.SIGTERM {
				a.mu.Lock()
				a.termSent[c.ID] = true
				a.mu.Unlock()
			}
		}
		if expired {
			// KILL is asked again until it works; the pod stays Terminating meanwhile.
			return killRetry, nil
		}
		// An exit wakes us sooner; this is only the deadline.
		return min(deadline.Sub(now), 5*time.Second), nil
	}
	if retry, err := a.cleanup(ctx, uid, actual, true); err != nil {
		return retry, err
	}
	a.forgetPod(uid, pod.Spec.Containers)

	key := objects.Key(podsRes, pod.Metadata.Namespace, pod.Metadata.Name)
	err := a.Store.Write(false, func(tx *store.Tx) error {
		rec, err := tx.Get(key)
		if errors.Is(err, store.ErrNotFound) || (err == nil && rec.UID != uid) {
			return nil // already gone
		}
		if err != nil {
			return err
		}
		cur := &apis.Pod{}
		if err := json.Unmarshal(rec.Data, cur); err != nil {
			return err
		}
		// Grace 0 says "the node is done". With finalizers left the object
		// waits for them, and the API server completes the delete then.
		cur.Metadata.DeletionGracePeriodSeconds = new(int64)
		if len(cur.Metadata.Finalizers) == 0 {
			_, err = tx.Delete(key, rec.Rev, objects.Encoder(cur))
			return err
		}
		for i := range cur.Status.ContainerStatuses {
			cs := &cur.Status.ContainerStatuses[i]
			if cs.State.Running != nil {
				cs.State = apis.ContainerState{Terminated: &apis.ContainerStateTerminated{
					ExitCode: 137, Reason: "Killed", StartedAt: cs.State.Running.StartedAt, FinishedAt: apis.Now(), ContainerID: cs.ContainerID}}
				cs.Ready, cs.Started = false, ptr(false)
			}
		}
		_, _, err = tx.Update(objects.Put(podsRes, cur), rec.Rev)
		return err
	})
	if err != nil {
		return failureRetry, err
	}
	return 0, nil
}

// reconcile brings a live pod up: network, files, and each container.
func (a *Agent) reconcile(ctx context.Context, pod *apis.Pod, actual []runtime.Container) (time.Duration, error) {
	uid := pod.Metadata.UID
	if err := a.withdrawReadiness(pod, actual); err != nil {
		return failureRetry, err
	}
	old := pod.Status
	// A Pod started before Service env support can have runtime or status
	// history without a snapshot. Remember that history before network
	// recovery removes any disconnected containers.
	alreadyStarted := len(actual) > 0 || slices.ContainsFunc(old.ContainerStatuses, func(cs apis.ContainerStatus) bool {
		return cs.ContainerID != ""
	})
	st := apis.PodStatus{Phase: old.Phase, HostIP: old.HostIP, StartTime: old.StartTime}
	if st.StartTime == nil {
		st.StartTime = &apis.Time{Time: a.Now().UTC().Truncate(time.Second)}
	}
	st.HostIP = hostIP()

	var podErr error
	ipStr, err := a.Store.AllocIP("pod", uid, a.podAddrs)
	if err != nil {
		podErr = err
	}
	var ip netip.Addr
	if podErr == nil {
		ip, _ = netip.ParseAddr(ipStr)
		if podErr = a.ensureHost(); podErr == nil {
			var created bool
			if created, podErr = a.Net.Setup(uid, ip); podErr == nil {
				actual, err = a.removeDisconnected(ctx, pod, actual, created)
				if err != nil {
					return failureRetry, err
				}
			}
		}
	}
	var mounts map[string]string
	if podErr == nil {
		st.PodIP, st.PodIPs = ipStr, []apis.PodIP{{IP: ipStr}}
		mounts, podErr = a.ensurePodFiles(pod, ipStr)
	}

	// Show "ContainerCreating" before the first pull, which can take minutes.
	if len(old.ContainerStatuses) == 0 && podErr == nil {
		first := st
		first.Phase = "Pending"
		for _, c := range pod.Spec.Containers {
			first.ContainerStatuses = append(first.ContainerStatuses, apis.ContainerStatus{
				Name: c.Name, Image: c.Image, Started: ptr(false),
				State: apis.ContainerState{Waiting: &apis.ContainerStateWaiting{Reason: "ContainerCreating"}}})
		}
		a.setConditions(&first, &old, pod)
		if err := a.writeStatus(pod, first); err != nil {
			return failureRetry, err
		}
		old = first
	}

	var retry time.Duration
	for i := range pod.Spec.Containers {
		c := &pod.Spec.Containers[i]
		var attempts []runtime.Container
		for _, x := range actual {
			if x.Name == c.Name || x.ID == runtime.ContainerID(uid, c.Name, x.Attempt) {
				attempts = append(attempts, x)
			}
		}
		var prev *apis.ContainerStatus
		for j := range old.ContainerStatuses {
			if old.ContainerStatuses[j].Name == c.Name {
				prev = &old.ContainerStatuses[j]
			}
		}
		cs, again := a.syncContainer(ctx, pod, c, attempts, prev, podErr, mounts, alreadyStarted)
		if cs.State.Running != nil {
			cs, again = a.checkProbes(ctx, pod, c, cs, st.PodIP)
		}
		if ctx.Err() != nil {
			return failureRetry, ctx.Err()
		}
		st.ContainerStatuses = append(st.ContainerStatuses, cs)
		if again > 0 && (retry == 0 || again < retry) {
			retry = again
		}
	}
	// Containers that are no longer in the spec cannot exist (the spec's
	// container list is immutable), but a leftover with another name can.
	for _, x := range actual {
		if !slices.ContainsFunc(pod.Spec.Containers, func(c apis.Container) bool {
			return x.Name == c.Name || x.ID == runtime.ContainerID(uid, c.Name, x.Attempt)
		}) {
			a.Runtime.Remove(ctx, x.ID)
		}
	}

	st.Phase = phase(pod, st.ContainerStatuses)
	a.setConditions(&st, &old, pod)
	if err := a.writeStatus(pod, st); err != nil {
		return failureRetry, err
	}
	if podErr != nil {
		return failureRetry, podErr
	}
	return retry, nil
}

// Withdraw Ready as soon as the runtime shows that a container has stopped.
// Pulling its replacement can take minutes. During that time a different
// container in this Pod might still accept TCP connections, so dial failure
// alone would not keep this Pod out of a Service's endpoints.
func (a *Agent) withdrawReadiness(pod *apis.Pod, actual []runtime.Container) error {
	old := pod.Status
	if !slices.ContainsFunc(old.Conditions, func(c apis.PodCondition) bool { return c.Type == "Ready" && c.Status == "True" }) {
		return nil
	}
	st := old
	st.ContainerStatuses = slices.Clone(old.ContainerStatuses)
	changed := len(st.ContainerStatuses) == 0
	for i := range st.ContainerStatuses {
		cs := &st.ContainerStatuses[i]
		if cs.Ready && !slices.ContainsFunc(actual, func(c runtime.Container) bool {
			return c.State == runtime.StateRunning && cs.ContainerID == containerID(c.ID)
		}) {
			cs.Ready = false
			changed = true
		}
	}
	if !changed {
		return nil
	}
	a.setConditions(&st, &old, pod)
	return a.writeStatus(pod, st)
}

func (a *Agent) removeDisconnected(ctx context.Context, pod *apis.Pod, actual []runtime.Container, created bool) ([]runtime.Container, error) {
	uid := pod.Metadata.UID
	for i := range actual {
		c := &actual[i]
		if c.State == runtime.StateRunning {
			inNet, err := a.Net.ContainsTask(uid, c.Pid)
			if err != nil {
				return nil, err
			}
			if created || !inNet {
				a.Logf("pod %s/%s: container %s lost its network namespace; stopping it", pod.Metadata.Namespace, pod.Metadata.Name, c.Name)
				if err := a.Runtime.Remove(ctx, c.ID); err != nil {
					return nil, err
				}
				// Keep the attempt for restart policy and restartCount even if
				// status was never written for this container.
				c.State = runtime.StateUnknown
			}
		}
	}
	return actual, nil
}

// podAddrs yields the addresses pods may get: the range without the network
// address, the gateway (the first address) and the broadcast address.
func (a *Agent) podAddrs(yield func(string) bool) {
	p := a.PodCIDR.Masked()
	gw := p.Addr().Next()
	for ip := gw.Next(); p.Contains(ip); ip = ip.Next() {
		if !p.Contains(ip.Next()) {
			return // the last one is the broadcast address
		}
		if !yield(ip.String()) {
			return
		}
	}
}

func containerID(id string) string { return idPrefix + id }

func attemptOf(status *apis.ContainerStatus) (int, bool) {
	if status == nil || status.ContainerID == "" {
		return 0, false
	}
	_, _, n, ok := runtime.ParseContainerID(strings.TrimPrefix(status.ContainerID, idPrefix))
	return n, ok
}

func apiTime(t time.Time) *apis.Time { return &apis.Time{Time: t.UTC().Truncate(time.Second)} }

// terminated describes how an attempt ended. When containerd no longer knows
// (the task was lost), the first observation invents the time and later ones
// must find the same value, or the restart delay would never run out.
func (a *Agent) terminated(x runtime.Container, prev *apis.ContainerStatus) *apis.ContainerStateTerminated {
	cid := containerID(x.ID)
	if x.State == runtime.StateExited {
		t := &apis.ContainerStateTerminated{ExitCode: x.ExitCode, Reason: "Error", StartedAt: apiTime(x.CreatedAt), FinishedAt: apiTime(x.ExitedAt), ContainerID: cid}
		if x.ExitCode == 0 {
			t.Reason = "Completed"
		}
		if x.ExitedAt.IsZero() {
			t.FinishedAt = apiTime(a.Now())
		}
		return t
	}
	if prev != nil {
		for _, t := range []*apis.ContainerStateTerminated{prev.State.Terminated, prev.LastState.Terminated} {
			if t != nil && t.ContainerID == cid && t.FinishedAt != nil {
				return t
			}
		}
	}
	return &apis.ContainerStateTerminated{ExitCode: 255, Reason: "Unknown", StartedAt: apiTime(x.CreatedAt), FinishedAt: apiTime(a.Now()), ContainerID: cid,
		Message: "the container's task was lost (the node restarted, or it was never started); the exit code is not known"}
}

// syncContainer decides what one container of a live pod needs now and
// returns its status. attempts are what containerd has for it, oldest first.
func (a *Agent) syncContainer(ctx context.Context, pod *apis.Pod, c *apis.Container, attempts []runtime.Container,
	prev *apis.ContainerStatus, podErr error, mounts map[string]string, alreadyStarted bool) (cs apis.ContainerStatus, retry time.Duration) {
	uid, now := pod.Metadata.UID, a.Now()
	key := uid + "/" + c.Name
	cs = apis.ContainerStatus{Name: c.Name, Image: c.Image, Started: ptr(false)}
	if prev != nil {
		cs.ImageID, cs.RestartCount, cs.LastState, cs.ContainerID = prev.ImageID, prev.RestartCount, prev.LastState, prev.ContainerID
	}
	// Older attempts are only history; the newest one is the container.
	for len(attempts) > 1 {
		a.Runtime.Remove(ctx, attempts[0].ID)
		attempts = attempts[1:]
	}

	next := 0 // the attempt a start would be
	var last *apis.ContainerStateTerminated
	switch {
	case len(attempts) == 1 && attempts[0].State == runtime.StateRunning:
		x := attempts[0]
		cs.State = apis.ContainerState{Running: &apis.ContainerStateRunning{StartedAt: apiTime(x.CreatedAt)}}
		cs.Ready, cs.Started, cs.ContainerID = true, ptr(true), containerID(x.ID)
		if int(cs.RestartCount) != x.Attempt {
			// We started it and did not get to write the status. What status
			// has as the current state is the attempt before this one.
			if prev != nil && prev.State.Terminated != nil {
				cs.LastState = prev.State
			}
			cs.RestartCount = int32(x.Attempt)
		}
		return cs, 0
	case len(attempts) == 1:
		x := attempts[0]
		last = a.terminated(x, prev)
		cs.RestartCount, cs.ContainerID = int32(x.Attempt), containerID(x.ID)
		next = x.Attempt + 1
		if !x.CreatedAt.IsZero() && last.FinishedAt.Sub(x.CreatedAt) >= backoffReset {
			a.mu.Lock()
			// This attempt was healthy: its end counts as a first failure again.
			a.resets[key] = x.Attempt
			a.mu.Unlock()
		}
	default:
		// Nothing in containerd. If status says an attempt existed, it was
		// removed behind our back: it counts as ended, how is unknown.
		if n, ok := attemptOf(prev); ok {
			next = n + 1
			last = prev.State.Terminated
			if t := prev.LastState.Terminated; last == nil && t != nil && t.ContainerID == prev.ContainerID {
				last = t // seen gone before; keep the time we gave it then
			}
			if last == nil {
				last = &apis.ContainerStateTerminated{ExitCode: 255, Reason: "Unknown", FinishedAt: apiTime(now), ContainerID: prev.ContainerID,
					Message: "the container disappeared from the runtime; the exit code is not known"}
				if r := prev.State.Running; r != nil {
					last.StartedAt = r.StartedAt
				}
			}
		}
	}

	if last != nil {
		// A liveness failure remains a failure even if the application's TERM
		// handler exits with code zero. Preserve this reason across sw restarts.
		forced := a.probeStopReason(pod, c, cs.ContainerID)
		if forced != "" {
			last.Reason = forced
		}
		restart := pod.Spec.RestartPolicy == "Always" || (pod.Spec.RestartPolicy == "OnFailure" && (last.ExitCode != 0 || forced != ""))
		if !restart {
			cs.State = apis.ContainerState{Terminated: last}
			return cs, 0
		}
		cs.LastState = apis.ContainerState{Terminated: last}
		a.mu.Lock()
		failures := next - a.resets[key]
		a.mu.Unlock()
		// The first restart is immediate; the delay is counted from the end of
		// the last attempt, so it survives a restart of sw (design 6).
		if wait := last.FinishedAt.Add(backoff(failures - 1)).Sub(now); wait > 0 {
			cs.State = apis.ContainerState{Waiting: &apis.ContainerStateWaiting{Reason: "CrashLoopBackOff",
				Message: fmt.Sprintf("back-off %s restarting failed container=%s pod=%s_%s", backoff(failures-1), c.Name, pod.Metadata.Name, pod.Metadata.Namespace)}}
			return cs, wait
		}
	}

	// Start attempt `next`, unless the last try failed and its delay is still
	// running. (Our own status write wakes us right after a failure; without
	// this the delay would be skipped.)
	a.mu.Lock()
	h, held := a.holds[key]
	a.mu.Unlock()
	if held && now.Before(h.until) {
		w := h.state
		if w.Reason == "ErrImagePull" {
			w.Reason = "ImagePullBackOff"
		}
		cs.State = apis.ContainerState{Waiting: &w}
		return cs, h.until.Sub(now)
	}
	waiting := func(reason string, err error) (apis.ContainerStatus, time.Duration) {
		w := apis.ContainerStateWaiting{Reason: reason, Message: err.Error()}
		a.mu.Lock()
		a.fails[key]++
		wait := backoff(a.fails[key])
		a.holds[key] = hold{until: now.Add(wait), state: w}
		a.mu.Unlock()
		cs.State = apis.ContainerState{Waiting: &w}
		return cs, wait
	}
	if podErr != nil {
		return waiting("CreatePodSandboxError", podErr)
	}
	if ctx.Err() != nil {
		return cs, 0
	}
	imageID, err := a.Runtime.EnsureImage(ctx, c.Image, c.ImagePullPolicy)
	if err != nil {
		if errors.Is(err, runtime.ErrImageNeverPull) {
			return waiting("ErrImageNeverPull", err)
		}
		return waiting("ErrImagePull", err)
	}
	cs.ImageID = imageID
	cfg, err := a.containerConfig(pod, c, next, mounts)
	if err != nil {
		return waiting("CreateContainerConfigError", err)
	}
	if ctx.Err() != nil {
		return cs, 0
	}
	// Snapshot after the image and config are ready. A pull can take long
	// enough for Services to change before the first start is attempted.
	links, err := a.serviceEnvSnapshot(pod, alreadyStarted)
	if err != nil {
		return waiting("CreateContainerConfigError", err)
	}
	cfg.Env = containerEnv(c.Env, links)
	if ctx.Err() != nil {
		return cs, 0
	}
	if err := a.Runtime.Start(ctx, cfg); err != nil {
		return waiting("RunContainerError", err)
	}
	a.mu.Lock()
	delete(a.fails, key)
	delete(a.holds, key)
	a.mu.Unlock()
	if len(attempts) == 1 {
		a.Runtime.Remove(ctx, attempts[0].ID)
	}
	// Keep the logs of this attempt and the one before (design 7).
	if next >= 2 {
		os.Remove(podlog.File(a.LogDir, pod.Metadata.Namespace, pod.Metadata.Name, uid, c.Name, next-2))
	}
	cs.State = apis.ContainerState{Running: &apis.ContainerStateRunning{StartedAt: apiTime(now)}}
	cs.Ready, cs.Started, cs.RestartCount, cs.ContainerID = true, ptr(true), int32(next), containerID(cfg.ID)
	return cs, 0
}

func (a *Agent) containerConfig(pod *apis.Pod, c *apis.Container, attempt int, volumes map[string]string) (runtime.Config, error) {
	uid, m := pod.Metadata.UID, &pod.Metadata
	cfg := runtime.Config{
		ID: runtime.ContainerID(uid, c.Name, attempt), PodUID: uid, PodNamespace: m.Namespace, PodName: m.Name,
		Name: c.Name, Attempt: attempt, Image: runtime.NormalizeImage(c.Image),
		Command: c.Command, Args: c.Args, WorkingDir: c.WorkingDir, Hostname: hostnameOf(pod),
		NetNS: a.Net.Path(uid), CgroupPath: runtime.CgroupPath(uid, c.Name, attempt),
		LogPath: podlog.File(a.LogDir, m.Namespace, m.Name, uid, c.Name, attempt),
	}
	dir := a.podDir(uid)
	cfg.Mounts = []runtime.Mount{
		{Source: dir + "/hosts", Destination: "/etc/hosts"},
		{Source: dir + "/hostname", Destination: "/etc/hostname"},
		{Source: dir + "/resolv.conf", Destination: "/etc/resolv.conf"},
	}
	for _, vm := range c.VolumeMounts {
		src, ok := volumes[vm.Name]
		if !ok {
			return cfg, fmt.Errorf("volume %q is not prepared", vm.Name)
		}
		cfg.Mounts = append(cfg.Mounts, runtime.Mount{Source: src, Destination: vm.MountPath, ReadOnly: vm.ReadOnly})
	}
	if q, ok := c.Resources.Limits["cpu"]; ok {
		milli, err := q.MilliValue()
		if err != nil {
			return cfg, err
		}
		cfg.CPUQuota = max(milli*100, 1000) // of a 100 ms period; the kernel's minimum is 1 ms
	}
	if q, ok := c.Resources.Limits["memory"]; ok {
		v, err := q.Value()
		if err != nil {
			return cfg, err
		}
		cfg.MemoryLimit = v
	}
	return cfg, nil
}

// hostnameOf is the pod's hostname: its name, cut to what a hostname may hold.
func hostnameOf(pod *apis.Pod) string {
	name := pod.Metadata.Name
	if len(name) > 63 {
		name = strings.TrimRight(name[:63], "-.")
	}
	return name
}

// phase follows the Kubernetes rules: Pending until every container has been
// created once, Succeeded or Failed when all have ended for good, else Running.
func phase(pod *apis.Pod, statuses []apis.ContainerStatus) string {
	ended, failed := 0, false
	for i := range statuses {
		cs := &statuses[i]
		switch {
		case cs.State.Terminated != nil:
			ended++
			failed = failed || cs.State.Terminated.ExitCode != 0
		case cs.State.Waiting != nil && cs.ContainerID == "":
			return "Pending"
		}
	}
	if ended == len(statuses) && ended > 0 {
		if failed {
			return "Failed"
		}
		return "Succeeded"
	}
	return "Running"
}

// setConditions fills the pod conditions. A condition keeps its transition
// time while its status stays the same (design 6).
func (a *Agent) setConditions(st, old *apis.PodStatus, pod *apis.Pod) {
	var unready []string
	for _, cs := range st.ContainerStatuses {
		if !cs.Ready {
			unready = append(unready, cs.Name)
		}
	}
	ready := apis.PodCondition{Status: "True"}
	if len(unready) > 0 || len(st.ContainerStatuses) == 0 {
		ready = apis.PodCondition{Status: "False", Reason: "ContainersNotReady", Message: "containers with unready status: [" + strings.Join(unready, " ") + "]"}
		if st.Phase == "Succeeded" {
			ready = apis.PodCondition{Status: "False", Reason: "PodCompleted"}
		}
	}
	conds := []apis.PodCondition{{Type: "Initialized", Status: "True"}, ready, ready, {Type: "PodScheduled", Status: "True"}}
	conds[1].Type, conds[2].Type = "Ready", "ContainersReady"
	now := apiTime(a.Now())
	for i := range conds {
		conds[i].LastTransitionTime = now
		for _, o := range old.Conditions {
			if o.Type == conds[i].Type && o.Status == conds[i].Status && o.LastTransitionTime != nil {
				conds[i].LastTransitionTime = o.LastTransitionTime
			}
		}
	}
	st.Conditions = conds
}

// writeStatus stores the status when it differs from what is stored.
func (a *Agent) writeStatus(pod *apis.Pod, st apis.PodStatus) error {
	want, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if have, _ := json.Marshal(pod.Status); string(have) == string(want) {
		return nil
	}
	key := objects.Key(podsRes, pod.Metadata.Namespace, pod.Metadata.Name)
	return a.Store.Write(false, func(tx *store.Tx) error {
		rec, err := tx.Get(key)
		if errors.Is(err, store.ErrNotFound) || (err == nil && rec.UID != pod.Metadata.UID) {
			return nil // deleted meanwhile; the next look cleans up
		}
		if err != nil {
			return err
		}
		cur := &apis.Pod{}
		if err := json.Unmarshal(rec.Data, cur); err != nil {
			return err
		}
		old := cur.Status
		cur.Status = st
		if _, _, err := tx.Update(objects.Put(podsRes, cur), rec.Rev); err != nil {
			return err
		}
		if !rec.Deleting {
			if err := a.recordTransitions(tx, cur, old); err != nil {
				return err
			}
		}
		pod.Status = st
		return nil
	})
}

func (a *Agent) recordTransitions(tx *store.Tx, pod *apis.Pod, old apis.PodStatus) error {
	for _, cs := range pod.Status.ContainerStatuses {
		i := slices.IndexFunc(old.ContainerStatuses, func(c apis.ContainerStatus) bool { return c.Name == cs.Name })
		var before apis.ContainerStatus
		if i >= 0 {
			before = old.ContainerStatuses[i]
		}
		field := "spec.containers{" + cs.Name + "}"
		// Readiness and periodic observations can change without starting a
		// container. Use its actual runtime ID so adoption emits no new event.
		if cs.State.Running != nil && cs.ContainerID != "" && cs.ContainerID != before.ContainerID {
			if err := objects.PodEvent(tx, pod, a.Now(), "start/"+cs.Name, field, "Started", "Started container "+cs.Name, "shearwater-node", "Normal"); err != nil {
				return err
			}
		}
		if w := cs.State.Waiting; w != nil && w.Reason != "" && w.Reason != "ContainerCreating" {
			prev := before.State.Waiting
			if prev == nil || *prev != *w {
				if err := objects.PodEvent(tx, pod, a.Now(), "warning/"+cs.Name, field, w.Reason, w.Message, "shearwater-node", "Warning"); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func ptr[T any](v T) *T { return &v }
