package runtime

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"sync"
	"syscall"
	"time"
)

// Fake is an in-memory Runtime and Network for tests. Containers "run" until
// the test ends them with Exit or Lose.
type Fake struct {
	mu         sync.Mutex
	Now        func() time.Time
	containers map[string]*Container
	configs    map[string]Config
	images     map[string]bool
	nets       map[string]netip.Addr
	exits      chan string

	Down       bool           // the runtime cannot be reached
	PullErr    error          // EnsureImage fails with this
	StartErr   error          // Start fails with this
	IgnoreTERM bool           // SIGTERM does not end containers
	Signals    []string       // "id:signal", in order
	Removed    []string       // container IDs, in order
	GonePods   []string       // PodGone calls
	HostCalls  int            // EnsureHost calls
	Pulls      map[string]int // image -> pulls
	nextPid    uint32
}

func NewFake() *Fake {
	return &Fake{
		Now: time.Now, containers: map[string]*Container{}, configs: map[string]Config{}, images: map[string]bool{},
		nets: map[string]netip.Addr{}, exits: make(chan string, 64), Pulls: map[string]int{}, nextPid: 1000,
	}
}

func (f *Fake) Version(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Down {
		return "", errors.New("fake runtime is down")
	}
	return "fake://1.0", nil
}

func (f *Fake) EnsureImage(_ context.Context, ref, policy string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ref = NormalizeImage(ref)
	if f.Down {
		return "", errors.New("fake runtime is down")
	}
	if policy != "Always" && f.images[ref] {
		return ImageID(ref, "sha256:fake"), nil
	}
	if policy == "Never" {
		return "", ErrImageNeverPull
	}
	if f.PullErr != nil {
		return "", f.PullErr
	}
	f.Pulls[ref]++
	f.images[ref] = true
	return ImageID(ref, "sha256:fake"), nil
}

func (f *Fake) Containers(_ context.Context, podUID string) ([]Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Down {
		return nil, errors.New("fake runtime is down")
	}
	var out []Container
	for _, c := range f.containers {
		if podUID == "" || c.PodUID == podUID {
			out = append(out, *c)
		}
	}
	slices.SortFunc(out, func(a, b Container) int { return cmpString(a.ID, b.ID) })
	return out, nil
}

func cmpString(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func (f *Fake) Start(_ context.Context, cfg Config) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Down {
		return errors.New("fake runtime is down")
	}
	if f.StartErr != nil {
		return f.StartErr
	}
	if _, ok := f.containers[cfg.ID]; ok {
		return fmt.Errorf("container %s already exists", cfg.ID)
	}
	if _, ok := f.nets[cfg.PodUID]; !ok {
		return fmt.Errorf("container %s: the pod has no network namespace", cfg.ID)
	}
	f.nextPid++
	f.containers[cfg.ID] = &Container{
		ID: cfg.ID, PodUID: cfg.PodUID, PodNamespace: cfg.PodNamespace, PodName: cfg.PodName, Name: cfg.Name,
		Attempt: cfg.Attempt, Image: cfg.Image, State: StateRunning, Pid: f.nextPid, CreatedAt: f.Now(),
	}
	f.configs[cfg.ID] = cfg
	return nil
}

func (f *Fake) Signal(_ context.Context, id string, sig syscall.Signal) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Signals = append(f.Signals, fmt.Sprintf("%s:%d", id, int(sig)))
	c := f.containers[id]
	if c == nil || c.State != StateRunning {
		return nil
	}
	if sig == syscall.SIGKILL || (sig == syscall.SIGTERM && !f.IgnoreTERM) {
		f.exitLocked(c, 128+int32(sig))
	}
	return nil
}

func (f *Fake) Remove(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Down {
		return errors.New("fake runtime is down")
	}
	// Model the legacy shared-cgroup failure instead of making Remove always
	// succeed: deleting a stopped task must not discard a live sibling's group.
	if own := f.containers[id]; own != nil && own.State == StateExited {
		group := f.configs[id].CgroupPath
		for otherID, other := range f.containers {
			if group != "" && otherID != id && other.State == StateRunning && f.configs[otherID].CgroupPath == group {
				return fmt.Errorf("cgroup %s is still used by %s", group, otherID)
			}
		}
	}
	if _, ok := f.containers[id]; ok {
		delete(f.containers, id)
		f.Removed = append(f.Removed, id)
	}
	return nil
}

func (f *Fake) PodGone(_ context.Context, podUID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.GonePods = append(f.GonePods, podUID)
	return nil
}

func (f *Fake) Exits() <-chan string { return f.exits }

func (f *Fake) exitLocked(c *Container, code int32) {
	c.State, c.ExitCode, c.ExitedAt, c.Pid = StateExited, code, f.Now(), 0
	select {
	case f.exits <- c.PodUID:
	default:
	}
}

// ---- controls for tests ----

// Exit ends a running container with the given exit code.
func (f *Fake) Exit(id string, code int32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c := f.containers[id]; c != nil && c.State == StateRunning {
		f.exitLocked(c, code)
	}
}

// Lose drops the task but keeps the container record, as a reboot does.
func (f *Fake) Lose(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c := f.containers[id]; c != nil {
		c.State, c.Pid = StateUnknown, 0
	}
}

// Add puts a container in place as if an earlier process had created it.
func (f *Fake) Add(c Container) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.containers[c.ID] = &c
}

// Config returns what a container was started with.
func (f *Fake) Config(id string) (Config, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.configs[id]
	return c, ok
}

// SetDown makes every call fail (or work again).
func (f *Fake) SetDown(down bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Down = down
}

// ---- Network ----

func (f *Fake) EnsureHost() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.HostCalls++
	return nil
}

func (f *Fake) Setup(podUID string, ip netip.Addr) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, had := f.nets[podUID]
	f.nets[podUID] = ip
	return !had, nil
}

func (f *Fake) Teardown(podUID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.nets, podUID)
	return nil
}

func (f *Fake) Path(podUID string) string { return "/fake/netns/" + podUID }

func (f *Fake) ContainsTask(string, uint32) (bool, error) { return true, nil }

func (f *Fake) SweepSnapshots(context.Context) error { return nil }

func (f *Fake) Pods() ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for uid := range f.nets {
		out = append(out, uid)
	}
	slices.Sort(out)
	return out, nil
}

// NetIP returns the address a pod's namespace was set up with.
func (f *Fake) NetIP(podUID string) (netip.Addr, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ip, ok := f.nets[podUID]
	return ip, ok
}

// DropNets forgets every namespace, as a reboot does.
func (f *Fake) DropNets() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nets = map[string]netip.Addr{}
}
