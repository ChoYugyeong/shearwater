package api

import (
	"crypto/sha256"
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
