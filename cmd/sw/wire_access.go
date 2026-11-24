package main

import (
	"log"

	"shearwater/internal/admission"
	"shearwater/internal/podsecurity"
	"shearwater/internal/quota"
	"shearwater/internal/rbac"
)

// wireAccess attaches access control to the server: the RBAC authorizer,
// user management under api.AdminPrefix, and the admission checks for RBAC
// escalation, resource quota and pod security
// (docs/design-multinode-tenancy.md 2). Owned by the access-control work.
func wireAccess(w *wiring) []admission.Func {
	if err := rbac.Bootstrap(w.Store); err != nil {
		log.Fatalf("sw server: bootstrap RBAC roles: %v", err)
	}
	policy := rbac.New(w.Store)
	w.Server.Authorizer = policy
	registerUsers(w)
	quotas := quota.New(w.Store)
	w.Go("resource-quota", quotas.Run)
	return []admission.Func{policy.Admit, podsecurity.Admit, quotas.Admit}
}
