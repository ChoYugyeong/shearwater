// Package runtime is what the node agent drives: containers (containerd,
// namespace "shearwater") and the pod network (network namespaces, veth pairs,
// forwarding rules). Both are interfaces with a fake, so the node logic is
// tested without root and without containerd (design 7).
package runtime

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Namespace is the only containerd namespace Shearwater touches. Docker's
// (moby, moby_history) are never read or written.
const Namespace = "shearwater"

// Labels on every container we create.
const (
	LabelPodUID       = "shearwater/pod-uid"
	LabelPodNamespace = "shearwater/pod-namespace"
	LabelPodName      = "shearwater/pod-name"
	LabelContainer    = "shearwater/container"
	LabelAttempt      = "shearwater/attempt"
)

type State string

const (
	// StateRunning: the task is alive.
	StateRunning State = "running"
	// StateExited: the task ended and containerd still knows how.
	StateExited State = "exited"
	// StateUnknown: the container record is there but no usable task is: it was
	// never started, or the task was lost (a reboot empties /run).
	StateUnknown State = "unknown"
)

// Container is one attempt at running a pod's container, as containerd has it.
type Container struct {
	ID           string
	PodUID       string
	PodNamespace string // empty when the labels are missing (creation was cut short)
	PodName      string
	Name         string // the container's name in the pod; from the label, else from the ID
	Attempt      int
	Image        string
	State        State
	Pid          uint32
	CreatedAt    time.Time
	ExitedAt     time.Time // zero unless StateExited
	ExitCode     int32
}

// RemovalOrder is used only when every listed attempt is being torn down.
// Older sw versions gave all attempts of a container the same cgroup. Stop
// and delete the live/newest owner first; removing an old stopped task while
// its replacement is alive can fail to remove that shared cgroup. New tasks
// have separate cgroups, but shutdown must also handle pre-upgrade tasks.
// Clone the slice: normal reconciliation still needs its oldest-first order.
func RemovalOrder(containers []Container) []Container {
	ordered := slices.Clone(containers)
	slices.SortStableFunc(ordered, func(a, b Container) int {
		if a.State == StateExited && b.State != StateExited {
			return 1
		}
		if b.State == StateExited && a.State != StateExited {
			return -1
		}
		return cmp.Compare(b.Attempt, a.Attempt)
	})
	return ordered
}

type Mount struct {
	Source      string
	Destination string
	ReadOnly    bool
}

// Config is everything needed to start one attempt.
type Config struct {
	ID           string
	PodUID       string
	PodNamespace string
	PodName      string
	Name         string
	Attempt      int
	Image        string // a full reference, see NormalizeImage
	Command      []string
	Args         []string
	WorkingDir   string
	Env          []string // NAME=value, later entries win
	Hostname     string
	NetNS        string // path of the pod's network namespace
	Mounts       []Mount
	CPUQuota     int64 // microseconds per 100 ms period; 0 = no limit
	MemoryLimit  int64 // bytes; 0 = no limit
	CgroupPath   string
	LogPath      string // the shim writes stdout and stderr here
}

var (
	// ErrImageNeverPull: imagePullPolicy is Never and the image is not there.
	ErrImageNeverPull = errors.New("the image is not present and imagePullPolicy is Never")
)

// Runtime runs containers.
type Runtime interface {
	// Version is the runtime's version ("containerd://2.3.3"). An error means
	// the runtime cannot be reached; the node is then NotReady.
	Version(ctx context.Context) (string, error)
	// EnsureImage makes the image available under policy (Always, IfNotPresent,
	// Never) and returns its id (repository@digest).
	EnsureImage(ctx context.Context, ref, policy string) (id string, err error)
	// Containers lists the attempts of one pod, or of all pods when podUID is
	// empty. It finds containers by label and by the ID naming rule.
	Containers(ctx context.Context, podUID string) ([]Container, error)
	// Start creates the container with a fresh snapshot, creates its task with
	// the log file attached and starts it. On failure nothing is left behind.
	Start(ctx context.Context, cfg Config) error
	// Signal sends a signal to the container's main process. A container that
	// is not running is not an error.
	Signal(ctx context.Context, id string, sig syscall.Signal) error
	// Remove kills the task if it is alive and deletes task, container and
	// snapshot. Removing what is already gone is not an error.
	Remove(ctx context.Context, id string) error
	// PodGone releases what is kept per pod (the cgroup directory).
	PodGone(ctx context.Context, podUID string) error
	// SweepSnapshots removes leftover writable snapshots without containers.
	SweepSnapshots(ctx context.Context) error
	// Exits delivers the uid of a pod whenever one of its containers ends. It
	// is a hint; the agent re-reads everything every 30 seconds anyway.
	Exits() <-chan string
}

// Network gives each pod a network namespace with one address.
type Network interface {
	// EnsureHost prepares what all pods share: the bridge, forwarding, rules.
	// It is safe to call again (Docker restarts drop rules).
	EnsureHost() error
	// Setup makes the pod's namespace exist with eth0 = ip and the default
	// route. It repairs what is missing. created reports that the namespace
	// itself was made by this call: containers that were running before are
	// then in a namespace nobody can reach any more.
	Setup(podUID string, ip netip.Addr) (created bool, err error)
	// ContainsTask checks whether a running task uses the current namespace.
	ContainsTask(podUID string, pid uint32) (bool, error)
	// Teardown removes the namespace and with it the veth pair.
	Teardown(podUID string) error
	// Path is where the pod's namespace is bound.
	Path(podUID string) string
	// Pods lists the uids that have a namespace.
	Pods() ([]string, error)
}

// maxID is containerd's limit on identifiers.
const maxID = 76

// ContainerID builds "<pod-uid>-<container>-<attempt>". containerd limits
// identifiers to 76 characters, so a long container name is cut and gets a hash;
// the real name is in the label.
func ContainerID(podUID, name string, attempt int) string {
	suffix := "-" + strconv.Itoa(attempt)
	room := maxID - len(podUID) - 1 - len(suffix)
	if len(name) > room {
		h := fnv.New32a()
		h.Write([]byte(name))
		name = strings.TrimRight(name[:room-9], "-.") + "-" + fmt.Sprintf("%08x", h.Sum32())
	}
	return podUID + "-" + name + suffix
}

const uidLen = 36 // 8-4-4-4-12

// IsUID reports whether s looks like a pod uid. Paths and interface names are
// built from uids, so nothing else is accepted.
func IsUID(s string) bool {
	if len(s) != uidLen {
		return false
	}
	for i, c := range []byte(s) {
		switch {
		case i == 8 || i == 13 || i == 18 || i == 23:
			if c != '-' {
				return false
			}
		case (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f'):
		default:
			return false
		}
	}
	return true
}

// ParseContainerID splits an ID made by ContainerID. name may be the cut form.
func ParseContainerID(id string) (podUID, name string, attempt int, ok bool) {
	if len(id) < uidLen+4 || id[uidLen] != '-' || !IsUID(id[:uidLen]) {
		return "", "", 0, false
	}
	rest := id[uidLen+1:]
	i := strings.LastIndexByte(rest, '-')
	if i <= 0 {
		return "", "", 0, false
	}
	n, err := strconv.Atoi(rest[i+1:])
	if err != nil || n < 0 {
		return "", "", 0, false
	}
	return id[:uidLen], rest[:i], n, true
}

// NormalizeImage expands a short image name the way Docker does:
// "nginx" -> "docker.io/library/nginx:latest", "foo/bar:1" ->
// "docker.io/foo/bar:1". A first component with a dot, a colon or "localhost"
// is a registry.
func NormalizeImage(ref string) string {
	name, digest, hasDigest := strings.Cut(ref, "@")
	first, _, hasSlash := strings.Cut(name, "/")
	switch {
	case !hasSlash:
		name = "docker.io/library/" + name
	case !strings.ContainsAny(first, ".:") && first != "localhost":
		name = "docker.io/" + name
	}
	if hasDigest {
		return name + "@" + digest
	}
	if last := name[strings.LastIndexByte(name, '/')+1:]; !strings.Contains(last, ":") {
		name += ":latest"
	}
	return name
}

// ImageID is how an image is named in a container status: the repository
// without the tag, and the digest.
func ImageID(ref, digest string) string {
	name, _, _ := strings.Cut(ref, "@")
	if i := strings.LastIndexByte(name, ':'); i > strings.LastIndexByte(name, '/') {
		name = name[:i]
	}
	return name + "@" + digest
}
