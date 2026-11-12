package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"shearwater/internal/agent"
	"shearwater/internal/apis"
)

const (
	inspectTimeout  = 5 * time.Second
	inspectAttempts = 5
)

func runStatus(args []string) error { return runInspect("status", args, os.Stdout) }
func runTree(args []string) error   { return runInspect("tree", args, os.Stdout) }

func runInspect(command string, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("sw "+command, flag.ContinueOnError)
	// flag normally prints its own errors. Use the same code and hint format
	// as the other operations and only print usage when help was requested.
	fs.SetOutput(io.Discard)
	configDir := fs.String("config-dir", defaultConfigDir, "directory with the server CA and admin token")
	listen := fs.String("listen", defaultListen, "local HTTPS host:port (127.0.0.1, localhost, or [::1])")
	tree := command == "tree"
	var options agent.TreeOptions
	if tree {
		fs.StringVar(&options.Namespace, "n", "", "namespace to show (default all)")
		fs.StringVar(&options.Root, "root", "", "show one subtree (deploy/NAME, rs/NAME, pod/NAME, or svc/NAME)")
		fs.BoolVar(&options.Problems, "problems", false, "show problems and their ancestors")
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fs.SetOutput(out)
			fs.Usage()
			return nil
		}
		return opsError("INVALID", "never", err.Error(), "run "+fs.Name()+" -h for the flags")
	}
	if fs.NArg() != 0 {
		return opsError("INVALID", "never", fmt.Sprintf("unexpected positional arguments %q", fs.Args()), "run "+fs.Name()+" -h for the flags")
	}
	if ns := options.Namespace; ns != "" && !apis.IsDNS1123Label(ns) {
		return opsError("INVALID", "never", fmt.Sprintf("invalid namespace %q", ns), "pass a namespace name such as -n default")
	}
	if options.Root != "" {
		root, err := inspectRoot(options.Root)
		if err != nil {
			return err
		}
		options.Root = root
	}

	client, err := localAPI(*configDir, *listen)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), inspectTimeout)
	defer cancel()
	snapshot, err := readInspection(ctx, client.get, tree)
	if err != nil {
		return err
	}

	var output string
	if tree {
		output, err = snapshot.Tree(options)
		switch {
		case errors.Is(err, agent.ErrTreeNotFound):
			return opsError("NOT_FOUND", "never", err.Error(), "run sw tree with the same connection flags to list namespaces and objects")
		case errors.Is(err, agent.ErrTreeAmbiguous):
			return opsError("INVALID", "never", err.Error(), "pass -n NS to select the root's namespace")
		case err != nil:
			return err
		}
	} else {
		output = snapshot.Summary() + "\n"
	}
	// Wait for a complete, consistent view before printing any of it.
	if _, err := io.WriteString(out, output); err != nil {
		return opsError("INTERNAL", "safe", fmt.Sprintf("cannot write %s output: %v", command, err), "check where standard output is directed")
	}
	return nil
}

func inspectRoot(root string) (string, error) {
	kind, name, _ := strings.Cut(root, "/")
	switch kind {
	case "deploy", "deployment", "deployments":
		kind = "deploy"
	case "rs", "replicaset", "replicasets":
		kind = "rs"
	case "po", "pod", "pods":
		kind = "pod"
	case "svc", "service", "services":
		kind = "svc"
	default:
		kind = ""
	}
	if kind == "" || !apis.IsDNS1123Subdomain(name) {
		return "", opsError("INVALID", "never", fmt.Sprintf("invalid tree root %q", root), "pass --root deploy/NAME, rs/NAME, pod/NAME, or svc/NAME")
	}
	return kind + "/" + name, nil
}

type inspectList[T any] struct {
	Metadata struct {
		Revision string `json:"resourceVersion"`
	} `json:"metadata"`
	Items []T `json:"items"`
}

func (l *inspectList[T]) revision() string { return l.Metadata.Revision }

type inspectGET func(context.Context, string, any) error

func readInspection(ctx context.Context, get inspectGET, tree bool) (agent.Snapshot, error) {
	for attempt := range inspectAttempts {
		snapshot, matched, err := readInspectionOnce(ctx, get, tree)
		if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
			return agent.Snapshot{}, inspectTimeoutError()
		}
		if err != nil || matched {
			return snapshot, err
		}
		if attempt+1 == inspectAttempts {
			break
		}
		select {
		case <-ctx.Done():
			return agent.Snapshot{}, inspectTimeoutError()
		case <-time.After((20 * time.Millisecond) << attempt):
		}
	}
	return agent.Snapshot{}, opsError("CONFLICT", "safe", "cluster revision kept changing while reading the lists", "retry the command after the cluster has settled")
}

func inspectTimeoutError() error {
	return opsError("TIMEOUT", "safe", "timed out reading cluster state", "check sw doctor and retry the command")
}

func readInspectionOnce(ctx context.Context, get inspectGET, tree bool) (agent.Snapshot, bool, error) {
	var nodes inspectList[apis.Node]
	var pods inspectList[apis.Pod]
	var namespaces inspectList[apis.Namespace]
	var deployments inspectList[apis.Deployment]
	var replicaSets inspectList[apis.ReplicaSet]
	var services inspectList[apis.Service]
	var endpoints inspectList[apis.EndpointSlice]
	type source struct {
		path string
		list interface{ revision() string }
	}
	lists := []source{{"/api/v1/nodes", &nodes}, {"/api/v1/pods", &pods}}
	if tree {
		lists = append(lists,
			source{"/api/v1/namespaces", &namespaces},
			source{"/apis/apps/v1/deployments", &deployments},
			source{"/apis/apps/v1/replicasets", &replicaSets},
			source{"/api/v1/services", &services},
			source{"/apis/discovery.k8s.io/v1/endpointslices", &endpoints},
		)
	}
	// The server reads each list and its store-wide resourceVersion in one
	// SQLite read transaction. Matching list revisions mean the same committed
	// state, even when individual objects were last changed at different times.
	// If a write landed between lists, discard the whole attempt and start over.
	var revision string
	for _, list := range lists {
		if err := get(ctx, list.path, list.list); err != nil {
			var op *operationError
			if errors.As(err, &op) || errors.Is(err, context.DeadlineExceeded) {
				return agent.Snapshot{}, false, err
			}
			return agent.Snapshot{}, false, opsError("INTERNAL", "safe", fmt.Sprintf("cannot read %s: %v", list.path, err), "check that --listen points to a Shearwater server")
		}
		got := list.list.revision()
		if _, err := strconv.ParseUint(got, 10, 63); err != nil {
			return agent.Snapshot{}, false, opsError("INTERNAL", "safe", fmt.Sprintf("invalid list revision %q from %s", got, list.path), "check that --listen points to a Shearwater server")
		}
		if revision != "" && revision != got {
			return agent.Snapshot{}, false, nil
		}
		revision = got
	}
	return agent.Snapshot{
		Revision: revision, Nodes: nodes.Items, Pods: pods.Items, Namespaces: namespaces.Items,
		Deployments: deployments.Items, ReplicaSets: replicaSets.Items, Services: services.Items, EndpointSlices: endpoints.Items,
	}, true, nil
}
