package main

import "shearwater/internal/admission"

// wireAccess attaches access control to the server: the RBAC authorizer,
// user management under api.AdminPrefix, and the admission checks for RBAC
// escalation, resource quota and pod security
// (docs/design-multinode-tenancy.md 2). Owned by the access-control work.
func wireAccess(w *wiring) []admission.Func {
	return nil
}
