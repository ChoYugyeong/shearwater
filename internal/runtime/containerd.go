package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/containerd/errdefs"
	"github.com/opencontainers/runtime-spec/specs-go"
)

// cgroupRoot holds /shearwater/<pod-uid>/<container>_<attempt> leaves.
const cgroupRoot = "/shearwater"

// Containerd talks to the host's containerd through its Go client (design
// decision 2: no CRI). Every call carries the namespace "shearwater"; there is
// no way to point this type at another one.
type Containerd struct {
	Socket string

	mu         sync.Mutex
	snapshotMu sync.RWMutex // exclude snapshot sweeping during create or remove
	c          *client.Client
	watched    map[string]bool // container IDs with an exit waiter
	exits      chan string
	bg         context.Context // for waits that outlive a call
}

func NewContainerd(socket string) *Containerd {
	return &Containerd{
		Socket: socket, watched: map[string]bool{}, exits: make(chan string, 256),
		bg: namespaces.WithNamespace(context.Background(), Namespace),
	}
}

// conn connects on first use and keeps the client. A failed attempt is tried
// again on the next call, so containerd may start after sw does.
func (r *Containerd) conn() (*client.Client, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.c != nil {
		return r.c, nil
	}
	c, err := client.New(r.Socket, client.WithDefaultNamespace(Namespace), client.WithTimeout(5*time.Second))
	if err != nil {
		return nil, fmt.Errorf("connect to containerd at %s: %w", r.Socket, err)
	}
	r.c = c
	return c, nil
}

func (r *Containerd) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.c != nil {
		r.c.Close()
		r.c = nil
	}
}

func inNS(ctx context.Context) context.Context { return namespaces.WithNamespace(ctx, Namespace) }

func (r *Containerd) Version(ctx context.Context) (string, error) {
	c, err := r.conn()
	if err != nil {
		return "", err
	}
	v, err := c.Version(inNS(ctx))
	if err != nil {
		return "", err
	}
	return "containerd://" + strings.TrimPrefix(v.Version, "v"), nil
}

func (r *Containerd) EnsureImage(ctx context.Context, ref, policy string) (string, error) {
	c, err := r.conn()
	if err != nil {
		return "", err
	}
	ctx = inNS(ctx)
	ref = NormalizeImage(ref)
	if policy != "Always" {
		img, err := c.GetImage(ctx, ref)
		switch {
		case err == nil:
			// Present but maybe never unpacked for the snapshotter (an interrupted pull).
			if err := img.Unpack(ctx, ""); err != nil && !errdefs.IsAlreadyExists(err) {
				return "", fmt.Errorf("unpack %s: %w", ref, err)
			}
			return ImageID(ref, img.Target().Digest.String()), nil
		case !errdefs.IsNotFound(err):
			return "", err
		case policy == "Never":
			return "", ErrImageNeverPull
		}
	}
	img, err := c.Pull(ctx, ref, client.WithPullUnpack)
	if err != nil {
		return "", fmt.Errorf("pull %s: %w", ref, err)
	}
	return ImageID(ref, img.Target().Digest.String()), nil
}

func (r *Containerd) Containers(ctx context.Context, podUID string) ([]Container, error) {
	c, err := r.conn()
	if err != nil {
		return nil, err
	}
	ctx = inNS(ctx)
	list, err := c.Containers(ctx)
	if err != nil {
		return nil, err
	}
	var out []Container
	for _, ct := range list {
		info, err := ct.Info(ctx, client.WithoutRefreshedMetadata)
		if err != nil {
			return nil, err
		}
		// Ours by label, or by the naming rule when creation was cut short
		// before the labels were written. Anything else in the namespace is
		// someone's manual container and is left alone.
		uid, name, attempt, named := ParseContainerID(info.ID)
		k := Container{ID: info.ID, Image: info.Image, CreatedAt: info.CreatedAt}
		if l := info.Labels[LabelPodUID]; l != "" {
			k.PodUID, k.PodNamespace, k.PodName, k.Name = l, info.Labels[LabelPodNamespace], info.Labels[LabelPodName], info.Labels[LabelContainer]
			k.Attempt, _ = strconv.Atoi(info.Labels[LabelAttempt])
		} else if named {
			k.PodUID, k.Name, k.Attempt = uid, name, attempt
		} else {
			continue
		}
		if podUID != "" && k.PodUID != podUID {
			continue
		}
		k.State = StateUnknown
		task, err := ct.Task(ctx, nil)
		switch {
		case err == nil:
			st, err := task.Status(ctx)
			if err != nil && !errdefs.IsNotFound(err) {
				return nil, err
			}
			switch st.Status {
			case client.Running, client.Paused, client.Pausing:
				k.State, k.Pid = StateRunning, task.Pid()
				r.watch(k.ID, k.PodUID, task)
			case client.Stopped:
				k.State, k.ExitCode, k.ExitedAt = StateExited, int32(st.ExitStatus), st.ExitTime
			}
		case !errdefs.IsNotFound(err):
			return nil, err
		}
		out = append(out, k)
	}
	return out, nil
}

// watch reports the pod when the task ends. One waiter per container.
func (r *Containerd) watch(id, podUID string, task client.Task) {
	r.mu.Lock()
	if r.watched[id] {
		r.mu.Unlock()
		return
	}
	r.watched[id] = true
	r.mu.Unlock()
	forget := func() {
		r.mu.Lock()
		delete(r.watched, id)
		r.mu.Unlock()
	}
	ch, err := task.Wait(r.bg)
	if err != nil {
		forget()
		return
	}
	go func() {
		<-ch
		forget()
		select {
		case r.exits <- podUID:
		default: // the agent's periodic pass will see it
		}
	}()
}

func (r *Containerd) Exits() <-chan string { return r.exits }

func (r *Containerd) Start(ctx context.Context, cfg Config) (err error) {
	r.snapshotMu.RLock()
	defer r.snapshotMu.RUnlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	c, err := r.conn()
	if err != nil {
		return err
	}
	ctx = inNS(ctx)
	img, err := c.GetImage(ctx, cfg.Image)
	if err != nil {
		return fmt.Errorf("image %s: %w", cfg.Image, err)
	}
	// Kubernetes rules: command replaces the image's ENTRYPOINT, args its CMD;
	// command alone drops the image's CMD.
	opts := []oci.SpecOpts{oci.WithImageConfigArgs(img, cfg.Args)}
	if len(cfg.Command) > 0 {
		opts = append(opts, oci.WithProcessArgs(append(slices.Clone(cfg.Command), cfg.Args...)...))
	}
	if cfg.WorkingDir != "" {
		opts = append(opts, oci.WithProcessCwd(cfg.WorkingDir))
	}
	mounts := make([]specs.Mount, len(cfg.Mounts))
	for i, m := range cfg.Mounts {
		mode := "rw"
		if m.ReadOnly {
			mode = "ro"
		}
		mounts[i] = specs.Mount{Destination: m.Destination, Type: "bind", Source: m.Source, Options: []string{"rbind", mode}}
	}
	opts = append(opts,
		oci.WithEnv(cfg.Env),
		oci.WithHostname(cfg.Hostname),
		oci.WithLinuxNamespace(specs.LinuxNamespace{Type: specs.NetworkNamespace, Path: cfg.NetNS}),
		oci.WithMounts(mounts),
		oci.WithCgroup(cfg.CgroupPath),
	)
	if cfg.MemoryLimit > 0 {
		opts = append(opts, oci.WithMemoryLimit(uint64(cfg.MemoryLimit)))
	}
	if cfg.CPUQuota > 0 {
		opts = append(opts, oci.WithCPUCFS(cfg.CPUQuota, 100000))
	}
	labels := map[string]string{
		LabelPodUID: cfg.PodUID, LabelPodNamespace: cfg.PodNamespace, LabelPodName: cfg.PodName,
		LabelContainer: cfg.Name, LabelAttempt: strconv.Itoa(cfg.Attempt),
	}
	ct, err := c.NewContainer(ctx, cfg.ID,
		client.WithImage(img),
		client.WithNewSnapshot(cfg.ID, img),
		client.WithNewSpec(opts...),
		client.WithContainerLabels(labels))
	if err != nil {
		return fmt.Errorf("create container: %w", err)
	}
	defer func() {
		if err != nil {
			// Whatever the caller's context says, do not leave half a container.
			cleanup, cancel := context.WithTimeout(r.bg, 20*time.Second)
			defer cancel()
			if task, terr := ct.Task(cleanup, nil); terr == nil {
				task.Delete(cleanup, client.WithProcessKill)
			}
			ct.Delete(cleanup, client.WithSnapshotCleanup)
		}
	}()
	if err := os.MkdirAll(filepath.Dir(cfg.LogPath), 0o755); err != nil {
		return err
	}
	// The log file is opened by the shim, not by us: output keeps flowing while
	// sw server is down (docs/m2-first-checks.md, check 1).
	task, err := ct.NewTask(ctx, cio.LogFile(cfg.LogPath))
	if err != nil {
		return fmt.Errorf("create task: %w", err)
	}
	r.watch(cfg.ID, cfg.PodUID, task)
	if err := task.Start(ctx); err != nil {
		return fmt.Errorf("start: %w", err)
	}
	return nil
}

func (r *Containerd) Signal(ctx context.Context, id string, sig syscall.Signal) error {
	c, err := r.conn()
	if err != nil {
		return err
	}
	ctx = inNS(ctx)
	ct, err := c.LoadContainer(ctx, id)
	if err != nil {
		return ignoreGone(err)
	}
	task, err := ct.Task(ctx, nil)
	if err != nil {
		return ignoreGone(err)
	}
	return ignoreGone(task.Kill(ctx, sig))
}

func (r *Containerd) Remove(ctx context.Context, id string) error {
	r.snapshotMu.RLock()
	defer r.snapshotMu.RUnlock()
	c, err := r.conn()
	if err != nil {
		return err
	}
	ctx = inNS(ctx)
	ct, err := c.LoadContainer(ctx, id)
	if err != nil {
		return ignoreGone(err)
	}
	if task, err := ct.Task(ctx, nil); err == nil {
		if _, err := task.Delete(ctx, client.WithProcessKill); err != nil && !errdefs.IsNotFound(err) {
			return fmt.Errorf("delete task %s: %w", id, err)
		}
	} else if ignoreGone(err) != nil {
		return err
	}
	if err := ct.Delete(ctx, client.WithSnapshotCleanup); err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("delete container %s: %w", id, err)
	}
	return nil
}

func (r *Containerd) SweepSnapshots(ctx context.Context) error {
	if !r.snapshotMu.TryLock() {
		return nil // the next resync can sweep after the current start or removal
	}
	defer r.snapshotMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	c, err := r.conn()
	if err != nil {
		return err
	}
	ctx = inNS(ctx)
	containers, err := c.ContainerService().List(ctx)
	if err != nil {
		return err
	}
	used := make(map[string]bool, len(containers))
	for _, ct := range containers {
		used[ct.SnapshotKey] = true
	}
	service := c.SnapshotService("")
	var orphans []string
	if err := service.Walk(ctx, func(_ context.Context, info snapshots.Info) error {
		if _, _, _, named := ParseContainerID(info.Name); named && info.Kind == snapshots.KindActive && !used[info.Name] {
			orphans = append(orphans, info.Name)
		}
		return nil
	}); err != nil {
		return err
	}
	var failures []error
	for _, name := range orphans {
		if err := service.Remove(ctx, name); err != nil && !errdefs.IsNotFound(err) {
			failures = append(failures, fmt.Errorf("remove orphan snapshot %s: %w", name, err))
		}
	}
	return errors.Join(failures...)
}

// ignoreGone: asking a container that is gone, or a task that already
// stopped, to go away is not a failure.
func ignoreGone(err error) error {
	if err == nil || errdefs.IsNotFound(err) || errdefs.IsFailedPrecondition(err) {
		return nil
	}
	return err
}

func (r *Containerd) PodGone(_ context.Context, podUID string) error {
	if !IsUID(podUID) {
		return fmt.Errorf("%q is not a pod uid", podUID)
	}
	return removeEmptyPodCgroups(filepath.Join("/sys/fs/cgroup", cgroupRoot, podUID))
}

// CgroupPath belongs to one execution attempt, not just one container name.
// The agent starts a replacement before removing the stopped attempt. Sharing
// a cgroup made runc's old-task cleanup collide with the live replacement:
// deletion could fail with EBUSY, and limits/process ownership were ambiguous.
// Keep attempts as sibling leaves so runc removes only its own leaf and
// PodGone can still remove the empty Pod directory after all attempts are gone.
// '_' cannot occur in a valid Kubernetes container name. It also separates
// new leaves from legacy name-only leaves (e.g. old "web-1" vs new "web_1").
func CgroupPath(podUID, container string, attempt int) string {
	return cgroupRoot + "/" + podUID + "/" + container + "_" + strconv.Itoa(attempt)
}
