package api

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"mime"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"shearwater/internal/apis"
	"shearwater/internal/objects"
	"shearwater/internal/patch"
	"shearwater/internal/protobuf"
	"shearwater/internal/store"
)

func keyOf(res *apis.Resource, namespace, name string) store.Key {
	return objects.Key(res, namespace, name)
}

// put builds the store write for obj.
func put(res *apis.Resource, obj apis.Object) store.Put { return objects.Put(res, obj) }

func encoder(obj apis.Object) func(int64) ([]byte, error) { return objects.Encoder(obj) }

// stamp fills in what the server owns on a new object.
func stamp(res *apis.Resource, obj apis.Object) { objects.Stamp(res, obj) }

const nameSuffixChars = "bcdfghjklmnpqrstvwxz2456789"

func nameSuffix() string {
	var b [5]byte
	rand.Read(b[:])
	for i := range b {
		b[i] = nameSuffixChars[int(b[i])%len(nameSuffixChars)]
	}
	return string(b[:])
}

func dryRun(q url.Values) (bool, error) {
	switch v := q.Get("dryRun"); v {
	case "":
		return false, nil
	case "All":
		return true, nil
	default:
		return false, errBadRequest("dryRun must be \"All\" or absent, got %q", v)
	}
}

// writeParams checks the query parameters every create, update and patch
// takes. fieldValidation is checked here, whether or not the body turns out to
// have unknown fields: a wrong value must not pass just because nothing needed
// the rule this time.
func writeParams(q url.Values) (dry bool, err error) {
	switch v := q.Get("fieldValidation"); v {
	case "", "Strict", "Warn", "Ignore":
	default:
		return false, errBadRequest("fieldValidation must be Strict, Warn or Ignore, got %q", v)
	}
	return dryRun(q)
}

// warn adds Warning headers; kubectl prints each as "Warning: text".
func warn(w http.ResponseWriter, texts []string) {
	for _, t := range texts {
		t = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", " ").Replace(t)
		w.Header().Add("Warning", `299 - "`+t+`"`)
	}
}

func readAll(r *http.Request) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		return nil, errBadRequest("reading the request body: %v", err)
	}
	if len(body) > maxBody {
		return nil, newErr(http.StatusRequestEntityTooLarge, "RequestEntityTooLarge", "INVALID", "never", "request body is larger than %d bytes", maxBody)
	}
	return body, nil
}

const manifestHint = "write a manifest and use `kubectl apply -f FILE` or `kubectl create -f FILE`"

// readBody returns the body of a create or update as JSON. A Protobuf body
// (what `kubectl create deployment` and its relatives send) is translated, so
// everything after this point sees one format.
func readBody(r *http.Request) ([]byte, error) {
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	switch ct {
	case "application/json", "", protobuf.ContentType:
	default:
		return nil, errUnsupported(http.StatusUnsupportedMediaType, "UnsupportedMediaType",
			"content type %q is not supported; send application/json", ct)
	}
	body, err := readAll(r)
	if err != nil || ct != protobuf.ContentType {
		return body, err
	}
	_, _, js, err := protobuf.Decode(body)
	if err != nil {
		return nil, newErr(http.StatusBadRequest, "BadRequest", "UNSUPPORTED", "never", "the protobuf body cannot be read: %v", err).withHint(manifestHint)
	}
	return js, nil
}

// judgeFields applies the fieldValidation rule to what is wrong with the
// members of a body: fields the type does not know, and fields that appear
// twice (where JSON decoding keeps the last value). Strict refuses, Warn (the
// default) answers with warnings, Ignore says nothing. subject starts the
// error message.
func judgeFields(q url.Values, subject string, unknown, duplicate []string) (warnings []string, err error) {
	if len(unknown)+len(duplicate) == 0 {
		return nil, nil
	}
	var problems []string
	for _, u := range unknown {
		problems = append(problems, fmt.Sprintf("unknown field %q", u))
		warnings = append(warnings, fmt.Sprintf("unknown field %q (dropped)", u))
	}
	for _, d := range duplicate {
		problems = append(problems, fmt.Sprintf("duplicate field %q", d))
		warnings = append(warnings, fmt.Sprintf("duplicate field %q (the last value is used)", d))
	}
	switch v := q.Get("fieldValidation"); v {
	case "Strict":
		return nil, errBadRequest("%s: strict decoding error: %s", subject, strings.Join(problems, ", ")).
			withHint("remove the unknown field or the repeated one, or pass --validate=warn to get a warning instead")
	case "", "Warn":
		return warnings, nil
	case "Ignore":
		return nil, nil
	default:
		return nil, errBadRequest("fieldValidation must be Strict, Warn or Ignore, got %q", v)
	}
}

// decodeObject turns a JSON body into a new object of the resource's type and
// applies the fieldValidation rule to members the type does not know and to
// members that appear twice. The warnings are for the response.
func decodeObject(q url.Values, res *apis.Resource, body []byte) (obj apis.Object, warnings []string, err error) {
	obj = res.New()
	// status is never taken from a write to the main resource, so its content is not checked either.
	unknown, err := apis.Decode(body, obj, "status")
	if err != nil {
		return nil, nil, errBadRequest("%s in version %q cannot be handled as a %s: %v", res.Kind, res.APIVersion(), res.Kind, err)
	}
	subject := fmt.Sprintf("%s in version %q cannot be handled as a %s", res.Kind, res.APIVersion(), res.Kind)
	if warnings, err = judgeFields(q, subject, unknown, apis.DuplicateFields(body, "status")); err != nil {
		return nil, nil, err
	}
	if t := obj.Type(); (t.Kind != "" && t.Kind != res.Kind) || (t.APIVersion != "" && t.APIVersion != res.APIVersion()) {
		return nil, nil, errBadRequest("the body is %s %s, but this endpoint takes %s %s", t.APIVersion, t.Kind, res.APIVersion(), res.Kind)
	}
	return obj, warnings, nil
}

func (s *Server) get(w http.ResponseWriter, r *http.Request, res *apis.Resource, ri reqInfo) error {
	tbl, aerr := negotiate(r)
	if aerr != nil {
		return aerr
	}
	rec, err := s.Store.Get(keyOf(res, ri.namespace, ri.name))
	if err != nil {
		return err
	}
	if tbl == nil {
		writeRaw(w, 200, rec.Data)
		return nil
	}
	t := newTable(res, strconv.FormatInt(rec.Rev, 10), true)
	row, err := tableRowFor(res, rec.Data, tbl, timeNow())
	if err != nil {
		return err
	}
	t.Rows = append(t.Rows, row)
	writeJSON(w, 200, t)
	return nil
}

func (s *Server) list(w http.ResponseWriter, r *http.Request, res *apis.Resource, ri reqInfo) error {
	q := r.URL.Query()
	tbl, aerr := negotiate(r)
	if aerr != nil {
		return aerr
	}
	f, err := parseFilter(q, res)
	if err != nil {
		return err
	}
	if v := q.Get("watch"); v == "1" || v == "true" {
		return s.watch(w, r, res, ri, f, tbl)
	}
	recs, rev, err := s.Store.List(res.Group, res.Plural, ri.namespace)
	if err != nil {
		return err
	}
	// A list is always the current state. That satisfies every resourceVersion
	// request except "exactly an older one".
	if rv := q.Get("resourceVersion"); q.Get("resourceVersionMatch") == "Exact" && rv != strconv.FormatInt(rev, 10) {
		return newErr(http.StatusGone, "Expired", "EXPIRED", "safe", "only the current revision (%d) can be listed, not %s", rev, rv).
			withHint("list without resourceVersionMatch=Exact")
	}
	var matched [][]byte
	for _, rec := range recs {
		ok, err := f.matches(rec.Data)
		if err != nil {
			return err
		}
		if ok {
			matched = append(matched, rec.Data)
		}
	}
	if tbl != nil {
		t := newTable(res, strconv.FormatInt(rev, 10), true)
		now := timeNow()
		for _, data := range matched {
			row, err := tableRowFor(res, data, tbl, now)
			if err != nil {
				return err
			}
			t.Rows = append(t.Rows, row)
		}
		writeJSON(w, 200, t)
		return nil
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, `{"kind":"%sList","apiVersion":%q,"metadata":{"resourceVersion":"%d"},"items":[`, res.Kind, res.APIVersion(), rev)
	b.Write(bytes.Join(matched, []byte{','}))
	b.WriteString("]}")
	writeRaw(w, 200, b.Bytes())
	return nil
}

func (s *Server) create(w http.ResponseWriter, r *http.Request, res *apis.Resource, ri reqInfo) error {
	if !res.Create || (res.Namespaced && ri.namespace == "") {
		return errMethod(r.Method)
	}
	q := r.URL.Query()
	dry, err := writeParams(q)
	if err != nil {
		return err
	}
	body, err := readBody(r)
	if err != nil {
		return err
	}
	obj, warnings, err := decodeObject(q, res, body)
	if err != nil {
		return err
	}
	m := obj.Meta()
	switch {
	case !res.Namespaced:
		m.Namespace = ""
	case m.Namespace == "":
		m.Namespace = ri.namespace
	case m.Namespace != ri.namespace:
		return errBadRequest("the object's namespace %q does not match the request's namespace %q", m.Namespace, ri.namespace)
	}
	if m.Name == "" && m.GenerateName != "" {
		m.Name = m.GenerateName + nameSuffix()
	}
	stamp(res, obj)
	preserveRolloutAnnotations(obj, nil)
	if errs := res.Validate(obj); len(errs) > 0 {
		return errInvalid(res, m.Name, errs)
	}
	var rec store.Record
	err = s.Store.Write(dry, func(tx *store.Tx) error {
		if res.Namespaced {
			nsRes := apis.Lookup("", "v1", "namespaces")
			ns, err := tx.Get(keyOf(nsRes, "", m.Namespace))
			if err != nil {
				return toAPIError(err, nsRes, m.Namespace).withHint("create it first: `kubectl create namespace %s`", m.Namespace)
			}
			if ns.Deleting {
				return newErr(http.StatusForbidden, "Forbidden", "FORBIDDEN", "never", "namespace %q is being deleted; nothing can be created in it", m.Namespace)
			}
		}
		if svc, ok := obj.(*apis.Service); ok && svc.Spec.ClusterIP != "None" {
			if err := allocateNewServiceIP(tx, res, svc); err != nil {
				return err
			}
		}
		rec, err = tx.Create(put(res, obj))
		return err
	})
	if err != nil {
		return toAPIError(err, res, m.Name)
	}
	warn(w, append(warnings, apis.NewlyStored(obj, nil)...))
	writeRaw(w, http.StatusCreated, rec.Data)
	return nil
}

// checkUpdateTarget checks the URL identity and optional write preconditions
// against the current record in the write transaction.
func checkUpdateTarget(res *apis.Resource, ri reqInfo, cur store.Record, m *apis.ObjectMeta) error {
	if m.Name == "" {
		m.Name = ri.name
	}
	if m.Name != ri.name || (m.Namespace != "" && m.Namespace != ri.namespace) {
		return errBadRequest("the object in the body (%s/%s) is not the one in the URL (%s/%s)", m.Namespace, m.Name, ri.namespace, ri.name)
	}
	if rv := m.ResourceVersion; rv != "" && rv != strconv.FormatInt(cur.Rev, 10) {
		return errConflict(res, ri.name, fmt.Sprintf("the object has been modified (you sent resourceVersion %s, it is now %d)", rv, cur.Rev))
	}
	if m.UID != "" && m.UID != cur.UID {
		return errConflict(res, ri.name, fmt.Sprintf("the uid in the body (%s) is not the stored object's (%s); it was deleted and created again", m.UID, cur.UID))
	}
	return nil
}

// replace commits body (the whole object as the client wants it, as JSON) over
// cur, inside a write transaction. PUT and PATCH both end here: read, change,
// default, validate and the conditional write are one transaction (design 3.6).
// With the status subresource only status is taken from body.
func (s *Server) replace(tx *store.Tx, q url.Values, res *apis.Resource, ri reqInfo, cur store.Record, body []byte) (rec store.Record, warnings []string, err error) {
	old := res.New()
	if err := json.Unmarshal(cur.Data, old); err != nil {
		return rec, nil, fmt.Errorf("stored %s is not readable: %v", cur.Key, err)
	}
	om := old.Meta()
	var obj apis.Object
	if ri.sub == "status" {
		// Only status is taken from the body, so the rest is decoded leniently.
		obj = res.New()
		if err := json.Unmarshal(body, obj); err != nil {
			return rec, nil, errBadRequest("the body is not a valid %s: %v", res.Kind, err)
		}
		// Only what is taken is judged: a repeated key under status.
		dups := slices.DeleteFunc(apis.DuplicateFields(body), func(p string) bool {
			return p != "status" && !strings.HasPrefix(p, "status.") && !strings.HasPrefix(p, "status[")
		})
		if warnings, err = judgeFields(q, "the status of "+res.Kind+" "+ri.name, nil, dups); err != nil {
			return rec, nil, err
		}
	} else if obj, warnings, err = decodeObject(q, res, body); err != nil {
		return rec, nil, err
	}
	m := obj.Meta()
	if err := checkUpdateTarget(res, ri, cur, m); err != nil {
		return rec, nil, err
	}

	if ri.sub == "status" {
		// /status replaces status only and never moves generation.
		apis.CopyStatus(old, obj)
		rec, _, err = tx.Update(put(res, old), cur.Rev)
		return rec, warnings, err
	}

	*obj.Type() = *old.Type()
	m.Namespace, m.UID, m.CreationTimestamp, m.Generation = om.Namespace, om.UID, om.CreationTimestamp, om.Generation
	m.DeletionTimestamp, m.DeletionGracePeriodSeconds = om.DeletionTimestamp, om.DeletionGracePeriodSeconds
	preserveRolloutAnnotations(obj, om)
	apis.CopyStatus(obj, old)
	if res.Default != nil {
		res.Default(obj)
	}
	errs := res.Validate(obj)
	if res.ValidateUpdate != nil {
		errs = append(errs, res.ValidateUpdate(obj, old)...)
	}
	if om.DeletionTimestamp != nil {
		// An object on its way out only loses finalizers. A new one would hold
		// back a delete that someone already asked for and is waiting on.
		var added []string
		for _, f := range m.Finalizers {
			if !slices.Contains(om.Finalizers, f) {
				added = append(added, f)
			}
		}
		if len(added) > 0 {
			errs = append(errs, apis.FieldError{Reason: "FieldValueForbidden", Field: "metadata.finalizers",
				Detail: fmt.Sprintf("Forbidden: no new finalizers can be added while the object is being deleted, found new finalizers %q", added)})
		}
	}
	if len(errs) > 0 {
		return rec, nil, errInvalid(res, ri.name, errs)
	}
	if !bytes.Equal(apis.SpecJSON(obj), apis.SpecJSON(old)) ||
		(res.AnnotationsMoveGeneration && !maps.Equal(m.Annotations, om.Annotations)) {
		m.Generation++
	}
	warnings = append(warnings, apis.NewlyStored(obj, old)...)
	if g := m.DeletionGracePeriodSeconds; m.DeletionTimestamp != nil && len(m.Finalizers) == 0 && (g == nil || *g == 0) {
		// The last finalizer is gone: the pending delete completes. (A grace
		// period above 0 means the node agent is still stopping the pod; it
		// sets 0 when it is done.)
		rec, err = tx.Delete(cur.Key, cur.Rev, encoder(obj))
		return rec, warnings, err
	}
	rec, _, err = tx.Update(put(res, obj), cur.Rev)
	return rec, warnings, err
}

func (s *Server) update(w http.ResponseWriter, r *http.Request, res *apis.Resource, ri reqInfo) error {
	if !res.Update {
		return errMethod(r.Method)
	}
	q := r.URL.Query()
	dry, err := writeParams(q)
	if err != nil {
		return err
	}
	body, err := readBody(r)
	if err != nil {
		return err
	}
	var rec store.Record
	var warnings []string
	err = s.Store.Write(dry, func(tx *store.Tx) error {
		cur, err := tx.Get(keyOf(res, ri.namespace, ri.name))
		if err != nil {
			return err
		}
		rec, warnings, err = s.replace(tx, q, res, ri, cur, body)
		return err
	})
	if err != nil {
		return err
	}
	warn(w, warnings)
	writeRaw(w, 200, rec.Data)
	return nil
}

const (
	mergePatchType     = "application/merge-patch+json"
	strategicPatchType = "application/strategic-merge-patch+json"
	jsonPatchType      = "application/json-patch+json"
	applyPatchType     = "application/apply-patch+yaml"
)

func errPatch(err error) *apiError {
	return newErr(http.StatusUnprocessableEntity, "Invalid", "INVALID", "never", "the patch cannot be applied: %v", err)
}

type patchRequest struct {
	contentType string
	body        []byte
	fields      map[string]any
	dry         bool
	warnings    []string
}

func readPatch(r *http.Request, q url.Values) (*patchRequest, error) {
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	switch ct {
	case mergePatchType, strategicPatchType, jsonPatchType:
	case applyPatchType:
		return nil, errUnsupported(http.StatusUnsupportedMediaType, "UnsupportedMediaType", "server-side apply is not supported").
			withHint("use `kubectl apply` without --server-side; it sends a strategic merge patch")
	default:
		return nil, errUnsupported(http.StatusUnsupportedMediaType, "UnsupportedMediaType",
			"patch content type %q is not supported; use %s, %s or %s", ct, strategicPatchType, mergePatchType, jsonPatchType)
	}
	dry, err := writeParams(q)
	if err != nil {
		return nil, err
	}
	body, err := readAll(r)
	if err != nil {
		return nil, err
	}
	var fields map[string]any
	if ct != jsonPatchType {
		parsed, err := patch.Parse(body)
		if err != nil {
			return nil, errBadRequest("the patch is %v", err)
		}
		var ok bool
		if fields, ok = parsed.(map[string]any); !ok {
			return nil, errBadRequest("the patch must be a JSON object")
		}
	}
	// A key that appears twice in the patch document itself follows the same
	// rule as in a whole object. (The patched result cannot have one.)
	patchWarnings, err := judgeFields(q, "the patch", nil, apis.DuplicateFields(body))
	if err != nil {
		return nil, err
	}
	return &patchRequest{contentType: ct, body: body, fields: fields, dry: dry, warnings: patchWarnings}, nil
}

// apply returns the replacement JSON for the current representation.
func (p *patchRequest) apply(data []byte, key store.Key, res *apis.Resource) ([]byte, error) {
	parsed, err := patch.Parse(data)
	doc, ok := parsed.(map[string]any)
	if err != nil || !ok {
		return nil, fmt.Errorf("stored %s is not readable: %v", key, err)
	}
	var out any
	switch p.contentType {
	case mergePatchType:
		out = patch.Merge(doc, p.fields)
	case jsonPatchType:
		out, err = patch.JSONPatch(doc, p.body)
	case strategicPatchType:
		out, err = patch.Strategic(doc, p.fields, reflect.TypeOf(res.New()).Elem())
	}
	if err != nil {
		return nil, errPatch(err)
	}
	if _, ok := out.(map[string]any); !ok {
		return nil, errPatch(fmt.Errorf("the result is not an object"))
	}
	return json.Marshal(out)
}

// patch applies the request to the current object and runs the PUT checks in
// the same transaction.
func (s *Server) patch(w http.ResponseWriter, r *http.Request, res *apis.Resource, ri reqInfo) error {
	if !res.Update {
		return errMethod(r.Method)
	}
	q := r.URL.Query()
	p, err := readPatch(r, q)
	if err != nil {
		return err
	}

	var rec store.Record
	var warnings []string
	err = s.Store.Write(p.dry, func(tx *store.Tx) error {
		cur, err := tx.Get(keyOf(res, ri.namespace, ri.name))
		if err != nil {
			return err
		}
		js, err := p.apply(cur.Data, cur.Key, res)
		if err != nil {
			return err
		}
		rec, warnings, err = s.replace(tx, q, res, ri, cur, js)
		return err
	})
	if err != nil {
		return err
	}
	warn(w, append(p.warnings, warnings...))
	writeRaw(w, 200, rec.Data)
	return nil
}

type deleteOptions struct {
	GracePeriodSeconds *int64  `json:"gracePeriodSeconds"`
	PropagationPolicy  *string `json:"propagationPolicy"`
	Preconditions      *struct {
		UID             *string `json:"uid"`
		ResourceVersion *string `json:"resourceVersion"`
	} `json:"preconditions"`
	DryRun []string `json:"dryRun"`
}

func readDeleteOptions(r *http.Request) (opts deleteOptions, dry bool, err error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		return opts, false, errBadRequest("reading the request body: %v", err)
	}
	if len(bytes.TrimSpace(body)) > 0 {
		if ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); ct != "application/json" && ct != "" {
			return opts, false, errUnsupported(http.StatusUnsupportedMediaType, "UnsupportedMediaType", "delete options must be application/json, got %q", ct)
		}
		if err := json.Unmarshal(body, &opts); err != nil {
			return opts, false, errBadRequest("delete options are not valid JSON: %v", err)
		}
	}
	q := r.URL.Query()
	if v := q.Get("propagationPolicy"); v != "" {
		opts.PropagationPolicy = &v
	}
	if v := q.Get("gracePeriodSeconds"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return opts, false, errBadRequest("gracePeriodSeconds must be an integer, got %q", v)
		}
		opts.GracePeriodSeconds = &n
	}
	if p := opts.PropagationPolicy; p != nil && *p != "Background" && *p != "Foreground" && *p != "Orphan" {
		return opts, false, errBadRequest("propagationPolicy must be Background, Foreground or Orphan, got %q", *p)
	}
	if g := opts.GracePeriodSeconds; g != nil && (*g < 0 || *g > apis.MaxGraceSeconds) {
		return opts, false, errBadRequest("gracePeriodSeconds must be between 0 and %d", apis.MaxGraceSeconds)
	}
	if dry, err = dryRun(q); err != nil {
		return opts, false, err
	}
	for _, d := range opts.DryRun {
		if d != "All" {
			return opts, false, errBadRequest("dryRun must be \"All\", got %q", d)
		}
		dry = true
	}
	return opts, dry, nil
}

var protectedNamespaces = []string{"default", "kube-system"}

func (s *Server) delete(w http.ResponseWriter, r *http.Request, res *apis.Resource, ri reqInfo) error {
	if !res.Delete {
		return errMethod(r.Method)
	}
	opts, dry, err := readDeleteOptions(r)
	if err != nil {
		return err
	}
	var rec store.Record
	pending := false
	err = s.Store.Write(dry, func(tx *store.Tx) error {
		cur, err := tx.Get(keyOf(res, ri.namespace, ri.name))
		if err != nil {
			return err
		}
		if p := opts.Preconditions; p != nil {
			if p.UID != nil && *p.UID != cur.UID {
				return errConflict(res, ri.name, fmt.Sprintf("precondition failed: uid is %s, not %s", cur.UID, *p.UID))
			}
			if p.ResourceVersion != nil && *p.ResourceVersion != strconv.FormatInt(cur.Rev, 10) {
				return errConflict(res, ri.name, fmt.Sprintf("precondition failed: resourceVersion is %d, not %s", cur.Rev, *p.ResourceVersion))
			}
		}
		if res.Plural == "namespaces" && res.Group == "" {
			for _, p := range protectedNamespaces {
				if ri.name == p {
					return newErr(http.StatusForbidden, "Forbidden", "FORBIDDEN", "never", "namespace %q cannot be deleted", p)
				}
			}
			empty, err := tx.NamespaceEmpty(ri.name)
			if err != nil {
				return err
			}
			if !empty {
				e := errConflict(res, ri.name, "the namespace still contains objects; namespaces are deleted only when empty")
				e.retry = "never"
				return e.withHint("delete what is in it first; `kubectl get all -n %s` lists most of it", ri.name)
			}
		}
		obj := res.New()
		if err := json.Unmarshal(cur.Data, obj); err != nil {
			return fmt.Errorf("stored %s is not readable: %v", cur.Key, err)
		}
		if opts.PropagationPolicy != nil {
			switch *opts.PropagationPolicy {
			case "Foreground":
				if s.NodeName != "" && !slices.Contains(obj.Meta().Finalizers, objects.ForegroundFinalizer) {
					obj.Meta().Finalizers = append(obj.Meta().Finalizers, objects.ForegroundFinalizer)
				}
			case "Orphan":
				if err := objects.Orphan(tx, obj.Meta().UID); err != nil {
					return err
				}
			}
		}
		rec, pending, err = objects.Delete(tx, res, cur, obj, s.NodeName, opts.GracePeriodSeconds, timeNow())
		return err
	})
	if err != nil {
		return err
	}
	if pending {
		writeRaw(w, 200, rec.Data)
		return nil
	}
	// The uid lets kubectl's wait tell this object from a new one with the same name.
	writeJSON(w, 200, &apis.Status{
		TypeMeta: apis.TypeMeta{Kind: "Status", APIVersion: "v1"},
		Status:   "Success", Code: 200,
		Details: &apis.StatusDetails{Name: ri.name, Group: res.Group, Kind: res.Plural, UID: rec.UID},
	})
	return nil
}
