package api

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"slices"
	"strings"

	"shearwater/internal/apis"
)

// Unaggregated discovery (design 3.1). Verbs and subresources list only what
// is implemented, so they grow with the milestones.

type apiResource struct {
	Name         string   `json:"name"`
	SingularName string   `json:"singularName"`
	Namespaced   bool     `json:"namespaced"`
	Kind         string   `json:"kind"`
	Group        string   `json:"group,omitempty"`
	Version      string   `json:"version,omitempty"`
	Verbs        []string `json:"verbs"`
	ShortNames   []string `json:"shortNames,omitempty"`
	Categories   []string `json:"categories,omitempty"`
}

func verbs(res *apis.Resource) []string {
	v := []string{"get", "list", "watch"}
	if res.Create {
		v = append(v, "create")
	}
	if res.Update {
		v = append(v, "update", "patch")
	}
	if res.Delete {
		v = append(v, "delete")
	}
	slices.Sort(v)
	return v
}

func groupVersions() []string {
	var gvs []string
	for _, r := range apis.Resources {
		if r.Group != "" && !slices.Contains(gvs, r.APIVersion()) {
			gvs = append(gvs, r.APIVersion())
		}
	}
	return gvs
}

func (s *Server) serveAPIVersions(w http.ResponseWriter) {
	writeJSON(w, 200, map[string]any{
		"kind": "APIVersions", "versions": []string{"v1"},
		"serverAddressByClientCIDRs": []map[string]string{{"clientCIDR": "0.0.0.0/0", "serverAddress": "127.0.0.1:6443"}},
	})
}

func groupDoc(gv string) map[string]any {
	group, version, _ := strings.Cut(gv, "/")
	v := map[string]string{"groupVersion": gv, "version": version}
	return map[string]any{"name": group, "versions": []any{v}, "preferredVersion": v}
}

func (s *Server) serveGroupList(w http.ResponseWriter) {
	groups := []any{}
	for _, gv := range groupVersions() {
		groups = append(groups, groupDoc(gv))
	}
	writeJSON(w, 200, map[string]any{"kind": "APIGroupList", "apiVersion": "v1", "groups": groups})
}

func (s *Server) serveGroup(w http.ResponseWriter, group string) {
	for _, gv := range groupVersions() {
		if strings.HasPrefix(gv, group+"/") {
			doc := groupDoc(gv)
			doc["kind"], doc["apiVersion"] = "APIGroup", "v1"
			writeJSON(w, 200, doc)
			return
		}
	}
	writeErr(w, errPath("/apis/"+group))
}

func (s *Server) serveResourceList(w http.ResponseWriter, group, version string) {
	list := []apiResource{}
	gv := version
	if group != "" {
		gv = group + "/" + version
	}
	for _, r := range apis.Resources {
		if r.Group != group || r.Version != version {
			continue
		}
		list = append(list, apiResource{Name: r.Plural, SingularName: r.Singular, Namespaced: r.Namespaced, Kind: r.Kind,
			Verbs: verbs(r), ShortNames: r.ShortNames, Categories: r.Categories})
		if r.StatusSub {
			list = append(list, apiResource{Name: r.Plural + "/status", Namespaced: r.Namespaced, Kind: r.Kind, Verbs: []string{"get", "patch", "update"}})
		}
		if r.ScaleSub {
			scale := apis.ScaleResource
			list = append(list, apiResource{Name: r.Plural + "/scale", Namespaced: r.Namespaced,
				Group: scale.Group, Version: scale.Version, Kind: scale.Kind, Verbs: []string{"get", "patch", "update"}})
		}
		if r.Group == "" && r.Plural == "pods" {
			list = append(list, apiResource{Name: "pods/log", Namespaced: true, Kind: "Pod", Verbs: []string{"get"}})
		}
	}
	if len(list) == 0 {
		writeErr(w, errPath("/"+gv))
		return
	}
	writeJSON(w, 200, map[string]any{"kind": "APIResourceList", "apiVersion": "v1", "groupVersion": gv, "resources": list})
}

// ---- OpenAPI v3: the minimum kubectl needs (design 3.5 item 1) ----
//
// kubectl reads these documents for one thing: whether the PATCH operation of a
// kind accepts the fieldValidation query parameter. If it does, kubectl sends
// fieldValidation=Strict and lets the server validate. So each creatable kind
// gets a path with a patch operation that names the kind and the parameter.
// There are no schemas; `kubectl explain` will not work.

func (s *Server) buildOpenAPI() {
	s.oaDocs = map[string][]byte{}
	paths := map[string]map[string]any{} // gv path -> openapi paths
	for _, r := range apis.Resources {
		if !r.Create {
			continue
		}
		key := "api/" + r.Version
		if r.Group != "" {
			key = "apis/" + r.Group + "/" + r.Version
		}
		if paths[key] == nil {
			paths[key] = map[string]any{}
		}
		p := "/" + key + "/"
		if r.Namespaced {
			p += "namespaces/{namespace}/"
		}
		paths[key][p+r.Plural+"/{name}"] = map[string]any{
			"patch": map[string]any{
				"operationId": "patch" + r.Kind,
				"parameters": []any{
					map[string]any{"name": "fieldValidation", "in": "query", "schema": map[string]string{"type": "string"}},
					map[string]any{"name": "dryRun", "in": "query", "schema": map[string]string{"type": "string"}},
				},
				"responses":                       map[string]any{"200": map[string]string{"description": "OK"}},
				"x-kubernetes-action":             "patch",
				"x-kubernetes-group-version-kind": map[string]string{"group": r.Group, "version": r.Version, "kind": r.Kind},
			},
		}
	}
	root := map[string]map[string]string{}
	for key, p := range paths {
		doc, _ := json.Marshal(map[string]any{
			"openapi": "3.0.0",
			"info":    map[string]string{"title": "Shearwater", "version": GitVersion},
			"paths":   p,
		})
		sum := sha256.Sum256(doc)
		s.oaDocs[key] = doc
		root[key] = map[string]string{"serverRelativeURL": "/openapi/v3/" + key + "?hash=" + hex.EncodeToString(sum[:8])}
	}
	s.oaRoot, _ = json.Marshal(map[string]any{"paths": root})
}

// openAPIV2 serves an empty Swagger 2.0 document. kubectl never asks the server
// to validate a `kind: List` file; it validates such a file on the client
// against OpenAPI v2 and stops with "the server could not find the requested
// resource" when that document is missing. With no definitions, kubectl skips
// its client-side checks for the items, and this server still validates every
// object it receives. Everything else kubectl needs comes from /openapi/v3.
func (s *Server) serveOpenAPIV2(w http.ResponseWriter, r *http.Request) {
	if strings.Contains(r.Header.Get("Accept"), "protobuf") {
		// kubectl asks for the "...spec.v2@v1.0+protobuf" name, but Go's MIME
		// parser rejects '@' in a response; kube-openapi answers with this form.
		w.Header().Set("Content-Type", "application/com.github.proto-openapi.spec.v2.v1.0+protobuf")
		w.WriteHeader(http.StatusOK)
		w.Write(openAPIV2Protobuf())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"swagger": "2.0", "info": map[string]string{"title": "Shearwater", "version": GitVersion}, "paths": map[string]any{}})
}

// openAPIV2Protobuf encodes the same document as a gnostic openapi_v2.Document.
// Field numbers come from gnostic-models v0.7.0 openapiv2/OpenAPIv2.proto:
// Document swagger=1, info=2, paths=8; Info title=1, version=2.
func openAPIV2Protobuf() []byte {
	field := func(b []byte, num int, value []byte) []byte {
		b = append(b, byte(num<<3|2))
		b = binary.AppendUvarint(b, uint64(len(value)))
		return append(b, value...)
	}
	info := field(field(nil, 1, []byte("Shearwater")), 2, []byte(GitVersion))
	doc := field(nil, 1, []byte("2.0"))
	doc = field(doc, 2, info)
	return field(doc, 8, nil) // an empty paths object
}

func (s *Server) serveOpenAPI(w http.ResponseWriter, r *http.Request) {
	s.openapi.Do(s.buildOpenAPI)
	if r.URL.Path == "/openapi/v3" {
		writeRaw(w, 200, s.oaRoot)
		return
	}
	if doc, ok := s.oaDocs[strings.TrimPrefix(r.URL.Path, "/openapi/v3/")]; ok {
		writeRaw(w, 200, doc)
		return
	}
	writeErr(w, errPath(r.URL.Path))
}
