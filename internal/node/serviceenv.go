package node

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"shearwater/internal/apis"
)

// serviceEnvSnapshot freezes one namespace view for the lifetime of the Pod
// UID. The Pod directory already exists when a container is ready to start.
// Pod cleanup removes the file with the directory. An empty snapshot is also
// saved so a later container start sees the original empty environment.
func (a *Agent) serviceEnvSnapshot(pod *apis.Pod, alreadyStarted bool) (map[string]string, error) {
	path := filepath.Join(a.podDir(pod.Metadata.UID), "service-env.json")
	env, err := readServiceEnv(path)
	if !errors.Is(err, os.ErrNotExist) {
		return env, err
	}
	// Containers started by a version without Service env had an empty set
	// of injected variables. Preserve that environment on their next start.
	env = map[string]string{}
	if !alreadyStarted {
		env, err = a.serviceEnv(pod.Metadata.Namespace)
		if err != nil {
			return nil, err
		}
	}
	if err := writeServiceEnv(path, env); err != nil {
		if errors.Is(err, os.ErrExist) {
			return readServiceEnv(path)
		}
		return nil, fmt.Errorf("saving Service environment snapshot: %w", err)
	}
	return env, nil
}

func readServiceEnv(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var env map[string]string
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("reading Service environment snapshot %s: %w", path, err)
	}
	return env, nil
}

func writeServiceEnv(path string, env map[string]string) error {
	data, err := json.Marshal(env)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".service-env-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// Publish a complete file atomically before Runtime.Start. Link creates
	// the final name only once, so a racing caller reads the first snapshot.
	return os.Link(tmp.Name(), path)
}

func (a *Agent) serviceEnv(namespace string) (map[string]string, error) {
	records, _, err := a.Store.List("", "services", namespace)
	if err != nil {
		return nil, fmt.Errorf("listing Services for Pod environment: %w", err)
	}
	env := make(map[string]string)
	for _, rec := range records {
		if rec.Deleting || rec.Key.Namespace != namespace {
			continue
		}
		var svc apis.Service
		if err := json.Unmarshal(rec.Data, &svc); err != nil {
			return nil, fmt.Errorf("decoding Service %s for Pod environment: %w", rec.Key, err)
		}
		if svc.Metadata.DeletionTimestamp != nil || svc.Spec.Type != "ClusterIP" ||
			len(svc.Spec.Ports) == 0 || !serviceEnvPort(svc.Spec.Ports[0]) {
			continue
		}
		// A ClusterIP is usable once it has been allocated in the Service
		// range. Headless and still-unallocated Services have no address here.
		ip, err := netip.ParseAddr(svc.Spec.ClusterIP)
		if err != nil || !ip.Is4() || !apis.ServiceCIDR.Contains(ip) || ip == apis.ServiceCIDR.Masked().Addr() {
			continue
		}
		prefix := serviceEnvName(rec.Key.Name) + "_SERVICE_"
		env[prefix+"HOST"] = ip.String()
		// Use the Service's listening port in declaration order. The resolved
		// target port is the proxy's concern and can vary between Pods.
		env[prefix+"PORT"] = strconv.Itoa(int(svc.Spec.Ports[0].Port))
		for _, port := range svc.Spec.Ports {
			if port.Name != "" && serviceEnvPort(port) {
				env[prefix+"PORT_"+serviceEnvName(port.Name)] = strconv.Itoa(int(port.Port))
			}
		}
	}
	return env, nil
}

func serviceEnvName(name string) string {
	return strings.ReplaceAll(strings.ToUpper(name), "-", "_")
}

func serviceEnvPort(port apis.ServicePort) bool {
	return port.Port >= 1 && port.Port <= 65535 && (port.Protocol == "TCP" || port.Protocol == "")
}

func containerEnv(explicit []apis.EnvVar, links map[string]string) []string {
	var env []string
	explicitNames := make(map[string]bool, len(explicit))
	for _, e := range explicit {
		env = append(env, e.Name+"="+e.Value)
		explicitNames[e.Name] = true
	}
	// An explicit empty value overrides an injected value as well. Build a
	// fresh list for each container so its overrides stay local to it.
	for _, name := range slices.Sorted(maps.Keys(links)) {
		if !explicitNames[name] {
			env = append(env, name+"="+links[name])
		}
	}
	return env
}
