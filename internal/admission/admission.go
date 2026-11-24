// Package admission is the hook between validation and the write: checks
// that need the requester's identity or other stored objects (resource quota,
// pod security, RBAC escalation, host name claims) run here, inside the same
// write transaction as the change (docs/design-multinode-tenancy.md 2.4).
package admission

import (
	"fmt"
	"net/http"

	"shearwater/internal/apis"
	"shearwater/internal/auth"
	"shearwater/internal/store"
)

// Operations.
const (
	Create = "CREATE"
	Update = "UPDATE"
)

// Request is one create or update that passed validation.
type Request struct {
	Identity  auth.Identity
	Operation string
	Resource  *apis.Resource
	Namespace string
	Name      string
	Object    apis.Object // as it will be stored (may be changed by a plugin, for example defaults)
	Old       apis.Object // nil on create
	Tx        *store.Tx   // read other objects consistently with the write
	DryRun    bool
}

// Func refuses a request by returning an error. Return a *Denied for a clear
// answer; any other error becomes an internal error.
type Func func(Request) error

// Denied is a refusal with an HTTP status and a Kubernetes reason.
type Denied struct {
	Code    int    // HTTP status, for example 403 or 422
	Reason  string // Kubernetes StatusReason, for example "Forbidden"
	ErrCode string // Shearwater error code, for example "QUOTA_EXCEEDED"
	Message string
	Hint    string
}

func (d *Denied) Error() string { return d.Message }

// Forbid returns a 403 refusal.
func Forbid(errCode, hint, format string, a ...any) *Denied {
	return &Denied{Code: http.StatusForbidden, Reason: "Forbidden", ErrCode: errCode, Message: fmt.Sprintf(format, a...), Hint: hint}
}

// Conflict returns a 409 refusal (for example a host name another namespace owns).
func Conflict(errCode, hint, format string, a ...any) *Denied {
	return &Denied{Code: http.StatusConflict, Reason: "Conflict", ErrCode: errCode, Message: fmt.Sprintf(format, a...), Hint: hint}
}

// Chain runs plugins in order and stops at the first refusal.
func Chain(plugins ...Func) Func {
	return func(r Request) error {
		for _, p := range plugins {
			if p == nil {
				continue
			}
			if err := p(r); err != nil {
				return err
			}
		}
		return nil
	}
}
