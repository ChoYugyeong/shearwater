package main

import (
	"context"
	"net/http"

	"shearwater/internal/api"
	"shearwater/internal/store"
)

// wiring is what a feature gets to attach itself to `sw server`
// (docs/design-multinode-tenancy.md 1.3). Each feature owns one wire_*.go
// file and does its setup there, so features do not all edit runServer.
type wiring struct {
	Server    *api.Server
	Store     *store.Store
	Admin     *http.ServeMux // served under api.AdminPrefix, administrators only
	ConfigDir string
	DataDir   string
	NodeName  string // the control plane's own node ("" when it runs no workloads)
	// Go runs fn in the background for the life of the server. The server
	// waits for every fn to return before it closes the store.
	Go func(name string, fn func(ctx context.Context))
	// HA is the control plane's high availability (wire_ha.go); nil when the
	// server cannot have a second control plane (no advertised address).
	HA *haWiring
}
