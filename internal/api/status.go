package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"shearwater/internal/apis"
	"shearwater/internal/store"
)

// apiError is an error that knows its Status body. The message starts with the
// agent-facing line "CODE retry=safe|unsafe|never: text" (design 8.3), followed
// by an optional "hint:" line.
type apiError struct {
	http    int
	reason  string // Kubernetes StatusReason
	code    string // Shearwater error code
	retry   string
	text    string
	hint    string
	details *apis.StatusDetails
}

func (e *apiError) Error() string { return e.text }

func (e *apiError) withHint(format string, a ...any) *apiError {
	e.hint = fmt.Sprintf(format, a...)
	return e
}

func (e *apiError) status() *apis.Status {
	msg := e.code + " retry=" + e.retry + ": " + e.text
	if e.hint != "" {
		msg += "\nhint: " + e.hint
	}
	return &apis.Status{
		TypeMeta: apis.TypeMeta{Kind: "Status", APIVersion: "v1"},
		Status:   "Failure", Message: msg, Reason: e.reason, Details: e.details, Code: e.http,
	}
}

func newErr(httpCode int, reason, code, retry, format string, a ...any) *apiError {
	return &apiError{http: httpCode, reason: reason, code: code, retry: retry, text: fmt.Sprintf(format, a...)}
}

func resourceName(res *apis.Resource) string {
	if res.Group == "" {
		return res.Plural
	}
	return res.Plural + "." + res.Group
}

func details(res *apis.Resource, name string) *apis.StatusDetails {
	return &apis.StatusDetails{Name: name, Group: res.Group, Kind: res.Plural}
}

func errNotFound(res *apis.Resource, name string) *apiError {
	e := newErr(http.StatusNotFound, "NotFound", "NOT_FOUND", "never", "%s %q not found", resourceName(res), name)
	e.details = details(res, name)
	return e
}

func errAlreadyExists(res *apis.Resource, name string) *apiError {
	e := newErr(http.StatusConflict, "AlreadyExists", "ALREADY_EXISTS", "never", "%s %q already exists", resourceName(res), name)
	e.details = details(res, name)
	return e
}

func errConflict(res *apis.Resource, name, why string) *apiError {
	e := newErr(http.StatusConflict, "Conflict", "CONFLICT", "safe", "%s %q: %s", resourceName(res), name, why)
	e.details = details(res, name)
	return e.withHint("read the object again and retry with its current resourceVersion")
}

func errBadRequest(format string, a ...any) *apiError {
	return newErr(http.StatusBadRequest, "BadRequest", "INVALID", "never", format, a...)
}

func errUnsupported(httpCode int, reason, format string, a ...any) *apiError {
	return newErr(httpCode, reason, "UNSUPPORTED", "never", format, a...)
}

func errInvalid(res *apis.Resource, name string, list apis.ErrorList) *apiError {
	code := "UNSUPPORTED"
	parts := make([]string, len(list))
	d := details(res, name)
	d.Kind = res.Kind
	for i, fe := range list {
		parts[i] = fe.String()
		d.Causes = append(d.Causes, apis.StatusCause{Reason: fe.Reason, Message: fe.Detail, Field: fe.Field})
		if !fe.Unsupported {
			code = "INVALID"
		}
	}
	e := newErr(http.StatusUnprocessableEntity, "Invalid", code, "never", "%s %q is invalid: %s", res.Kind, name, strings.Join(parts, "; "))
	e.details = d
	return e
}

func errInternal(err error) *apiError {
	return newErr(http.StatusInternalServerError, "InternalError", "INTERNAL", "unsafe", "%v", err).
		withHint("check whether the change was applied before retrying")
}

// toAPIError turns anything a handler returns into an apiError.
func toAPIError(err error, res *apis.Resource, name string) *apiError {
	var ae *apiError
	switch {
	case errors.As(err, &ae):
		return ae
	case errors.Is(err, store.ErrNotFound):
		return errNotFound(res, name)
	case errors.Is(err, store.ErrExists):
		return errAlreadyExists(res, name)
	case errors.Is(err, store.ErrConflict):
		return errConflict(res, name, "the object has been modified")
	}
	return errInternal(err)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		code, b = http.StatusInternalServerError, []byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","code":500}`)
	}
	writeRaw(w, code, b)
}

func writeRaw(w http.ResponseWriter, code int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	w.Write(body)
}

func writeErr(w http.ResponseWriter, e *apiError) {
	writeJSON(w, e.http, e.status())
}
