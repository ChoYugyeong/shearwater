package api

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"shearwater/internal/operation"
)

func TestDelegatedTokenCannotBypassMCPThroughRawHTTP(t *testing.T) {
	c := newClient(t)
	spec := operation.Spec{Name: "web", Namespace: "default", Image: "nginx:alpine", Port: 80, Checks: []operation.Check{{Path: "/"}}, Allow: []string{"deploy"}, Expires: time.Now().Add(time.Hour)}
	b, _ := json.Marshal(spec)
	created := c.want(c.do("POST", operation.Prefix, string(b)), 200)
	token := created.at("token").(string)
	id := created.at("task", "operation").(string)
	authorization := "Bearer " + token
	c.want(c.do("GET", operation.Prefix+"/"+id, "", "Authorization", authorization), 200)
	for _, path := range []string{pods, deploys, "/api", "/apis", "/openapi/v3"} {
		c.want(c.do("GET", path, "", "Authorization", authorization), 401)
	}
	c.want(c.do("POST", operation.Prefix, string(b), "Authorization", authorization), 403)
	for _, action := range []string{"approve", "discard", "stage", "revoke"} {
		body := `{"action":"` + action + `"}`
		c.want(c.do("POST", operation.Prefix+"/"+id, body, "Authorization", authorization), 403)
	}
	c.want(c.do("POST", operation.Prefix+"/"+id+"/renew", `{"expires":"2099-01-01T00:00:00Z"}`, "Authorization", authorization), 403)
	c.want(c.do("POST", operation.Prefix+"/"+id, `{"action":"update","image":"bad"}`, "Authorization", authorization), 403)
	c.want(c.do("POST", operation.Prefix+"/"+id, `{"action":"deploy","name":"other"}`, "Authorization", authorization), 400)
	c.want(c.do("POST", operation.Prefix+"/"+id, `{"action":"status","action":"deploy"}`, "Authorization", authorization), 400)
	plan := c.want(c.do("POST", operation.Prefix+"/"+id, `{"action":"deploy"}`, "Authorization", authorization), 200).at("plan").(string)
	c.want(c.do("POST", operation.Prefix+"/"+id, `{"action":"continue","plan":"`+plan+`"}`, "Authorization", authorization), 409)
	if len(strings.TrimSpace(c.want(c.do("GET", deploys, ""), 200).raw)) == 0 {
		t.Fatal("missing API response")
	}
	c.want(c.do("POST", operation.Prefix+"/"+id, `{"action":"revoke"}`), 200)
	c.want(c.do("GET", operation.Prefix+"/"+id, "", "Authorization", authorization), 403)
}

func TestOperationRequestBoundsAndNoSecretTrace(t *testing.T) {
	c := newClient(t)
	c.want(c.do("POST", operation.Prefix, strings.Repeat("x", (16<<10)+1)), 400)
	c.want(c.do("GET", operation.Prefix+"/../api/v1/pods", "", "Authorization", "Bearer swop_bad"), 404)
	c.want(c.do("POST", operation.Prefix, `{"name":"web","namespace":"default","image":"nginx","port":80,"allow":["deploy"],"checks":[{"path":"http://outside/"}],"expires":"2099-01-01T00:00:00Z"}`), 400)
}
