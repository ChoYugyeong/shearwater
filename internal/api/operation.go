package api

// Delegated agents never enter the Kubernetes request router. This small API
// receives operation intents, checks the token against ONE server-owned record,
// and performs all authorization again inside the write transaction. Hiding an
// MCP tool is useful UX; it is not the security boundary.
import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"shearwater/internal/apis"
	"shearwater/internal/operation"
	"shearwater/internal/store"
)

func operationBody(r *http.Request, v any) error {
	b, e := io.ReadAll(io.LimitReader(r.Body, (16<<10)+1))
	if e != nil {
		return e
	}
	if len(b) > 16<<10 {
		return errBadRequest("operation request exceeds 16 KiB")
	}
	if len(apis.DuplicateFields(b)) > 0 {
		return errBadRequest("duplicate operation fields")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if e = d.Decode(v); e != nil {
		return errBadRequest("invalid operation request: %v", e)
	}
	if e = d.Decode(new(any)); e != io.EOF {
		return errBadRequest("expected one operation JSON value")
	}
	return nil
}

func (s *Server) serveOperation(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	admin := s.authorized(r)
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !admin && !strings.HasPrefix(token, "swop_") {
		writeErr(w, newErr(401, "Unauthorized", "UNAUTHORIZED", "never", "operator or delegated bearer token required"))
		return
	}
	s.operationInit.Do(func() { s.operationErr = s.Store.InitOperations() })
	if s.operationErr != nil {
		writeErr(w, newErr(500, "InternalError", "INTERNAL", "never", "operation store unavailable"))
		return
	}
	m := operation.Manager{Store: s.Store, Backend: s.Operations}
	path := strings.TrimPrefix(r.URL.Path, operation.Prefix)
	var value any
	var err error
	switch {
	case path == "" && r.Method == http.MethodPost:
		if !admin {
			err = &operation.Error{Code: "FORBIDDEN", Message: "only an operator can delegate"}
			break
		}
		var spec operation.Spec
		if err = operationBody(r, &spec); err != nil {
			break
		}
		var result operation.Result
		var secret string
		result, secret, err = m.Create(spec)
		value = map[string]any{"task": result, "token": secret}
	case strings.Count(path, "/") == 1 && operation.ValidID(strings.TrimPrefix(path, "/")):
		id := strings.TrimPrefix(path, "/")
		q := operation.Request{Action: "status"}
		if r.Method == http.MethodPost {
			if err = operationBody(r, &q); err != nil {
				break
			}
		} else if r.Method != http.MethodGet {
			err = errMethod(r.Method)
			break
		}
		value, err = m.Act(r.Context(), id, token, admin, q)
	case strings.HasSuffix(path, "/renew") && strings.Count(path, "/") == 2 && r.Method == http.MethodPost:
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/"), "/renew")
		if !admin {
			err = &operation.Error{Code: "FORBIDDEN", Message: "only an operator can renew"}
			break
		}
		if !operation.ValidID(id) {
			err = errBadRequest("invalid operation id")
			break
		}
		var q struct {
			Expires time.Time `json:"expires"`
		}
		if err = operationBody(r, &q); err != nil {
			break
		}
		var result operation.Result
		var secret string
		result, secret, err = m.Renew(id, q.Expires)
		value = map[string]any{"task": result, "token": secret}
	default:
		err = errPath(r.URL.Path)
	}
	if err != nil {
		var oe *operation.Error
		switch {
		case errors.As(err, &oe):
			code := 409
			if oe.Code == "FORBIDDEN" {
				code = 403
			} else if oe.Code == "INVALID" || oe.Code == "UNSUPPORTED" || oe.Code == "AMBIGUOUS" {
				code = 400
			}
			writeErr(w, newErr(code, "OperationRejected", oe.Code, oe.Retry(), "%s", oe.Message))
		case errors.Is(err, store.ErrNotFound):
			writeErr(w, errPath(r.URL.Path))
		case errors.Is(err, store.ErrConflict), errors.Is(err, store.ErrExists):
			writeErr(w, newErr(409, "Conflict", "CONFLICT", "never", "operation target changed"))
		default:
			writeErr(w, toAPIError(err, nil, ""))
		}
		return
	}
	writeJSON(w, 200, value)
}
