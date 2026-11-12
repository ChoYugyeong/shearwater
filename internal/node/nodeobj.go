package node

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	goruntime "runtime"
	"strconv"
	"strings"
	"time"

	"shearwater/internal/apis"
	"shearwater/internal/objects"
	"shearwater/internal/store"
)

var nodesRes = apis.Lookup("", "v1", "nodes")

// syncNode checks the runtime and keeps the Node object true. The object is
// written only when something changed: there is no heartbeat, so an idle node
// writes nothing to disk (design 6).
func (a *Agent) syncNode(ctx context.Context) {
	a.init()
	check, cancel := context.WithTimeout(ctx, 5*time.Second)
	version, err := a.Runtime.Version(check)
	cancel()
	reason, message := "KubeletReady", "the node agent is running and the container runtime answers"
	if err != nil {
		reason, message = "RuntimeNotReady", err.Error()
	} else {
		a.mu.Lock()
		if !a.nodeReady {
			a.hostOK = false // the runtime is (back) up: Docker may have dropped our rules
		}
		a.mu.Unlock()
		if err = a.ensureHost(); err != nil {
			reason, message = "NetworkNotReady", err.Error()
		}
	}
	ready := err == nil
	if ctx.Err() != nil {
		return
	}

	werr := a.Store.Write(false, func(tx *store.Tx) error {
		key := objects.Key(nodesRes, "", a.NodeName)
		node := &apis.Node{}
		rec, err := tx.Get(key)
		isNew := errors.Is(err, store.ErrNotFound)
		switch {
		case isNew:
			node.Metadata.Name = a.NodeName
			objects.Stamp(nodesRes, node)
		case err != nil:
			return err
		default:
			if err := json.Unmarshal(rec.Data, node); err != nil {
				return err
			}
		}
		a.fillNode(node, version, ready, reason, message)
		if isNew {
			_, err = tx.Create(objects.Put(nodesRes, node))
			return err
		}
		_, _, err = tx.Update(objects.Put(nodesRes, node), rec.Rev)
		return err
	})
	if werr != nil {
		a.Logf("writing node %s: %v", a.NodeName, werr)
	}
	a.mu.Lock()
	was := a.nodeReady
	a.nodeReady = ready
	a.mu.Unlock()
	if ready != was {
		a.Logf("node %s is %s: %s", a.NodeName, map[bool]string{true: "Ready", false: "NotReady"}[ready], message)
	}
}

func (a *Agent) fillNode(n *apis.Node, runtimeVersion string, ready bool, reason, message string) {
	if n.Metadata.Labels == nil {
		n.Metadata.Labels = map[string]string{}
	}
	n.Metadata.Labels["kubernetes.io/hostname"] = a.NodeName
	n.Metadata.Labels["kubernetes.io/os"] = "linux"
	n.Metadata.Labels["kubernetes.io/arch"] = goruntime.GOARCH
	n.Spec.PodCIDR = a.PodCIDR.String()

	pods := 110
	if room := 1<<(32-a.PodCIDR.Bits()) - 3; room < pods {
		pods = max(room, 0)
	}
	res := map[string]apis.Quantity{"cpu": apis.Quantity(strconv.Itoa(goruntime.NumCPU())), "pods": apis.Quantity(strconv.Itoa(pods))}
	if kb := memTotalKB(); kb > 0 {
		res["memory"] = apis.Quantity(strconv.FormatInt(kb, 10) + "Ki")
	}
	n.Status.Capacity, n.Status.Allocatable = res, res

	n.Status.Addresses = nil
	if ip := hostIP(); ip != "" {
		n.Status.Addresses = append(n.Status.Addresses, apis.NodeAddress{Type: "InternalIP", Address: ip})
	}
	n.Status.Addresses = append(n.Status.Addresses, apis.NodeAddress{Type: "Hostname", Address: a.NodeName})

	info := &n.Status.NodeInfo
	info.KernelVersion = readTrim("/proc/sys/kernel/osrelease")
	info.OSImage = osImage()
	info.KubeletVersion = a.Version
	info.OperatingSystem, info.Architecture = "linux", goruntime.GOARCH
	if runtimeVersion != "" {
		info.ContainerRuntimeVersion = runtimeVersion // keep the last known one while it is down
	}

	cond := apis.NodeCondition{Type: "Ready", Status: "False", Reason: reason, Message: message}
	if ready {
		cond.Status = "True"
	}
	now := apiTime(a.Now())
	cond.LastTransitionTime, cond.LastHeartbeatTime = now, now
	for _, o := range n.Status.Conditions {
		// Same state as before: keep the times, and with them the stored bytes.
		if o.Type == "Ready" && o.Status == cond.Status && o.Reason == cond.Reason && o.Message == cond.Message {
			cond.LastTransitionTime, cond.LastHeartbeatTime = o.LastTransitionTime, o.LastHeartbeatTime
		} else if o.Type == "Ready" && o.Status == cond.Status {
			cond.LastTransitionTime = o.LastTransitionTime
		}
	}
	n.Status.Conditions = []apis.NodeCondition{cond}
}

func readTrim(path string) string {
	b, _ := os.ReadFile(path)
	return strings.TrimSpace(string(b))
}

func memTotalKB() int64 {
	for _, line := range strings.Split(readTrim("/proc/meminfo"), "\n") {
		if f := strings.Fields(line); len(f) >= 2 && f[0] == "MemTotal:" {
			v, _ := strconv.ParseInt(f[1], 10, 64)
			return v
		}
	}
	return 0
}

func osImage() string {
	for _, line := range strings.Split(readTrim("/etc/os-release"), "\n") {
		if v, ok := strings.CutPrefix(line, "PRETTY_NAME="); ok {
			return strings.Trim(v, `"`)
		}
	}
	return "Linux"
}
