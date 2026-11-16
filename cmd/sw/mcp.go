package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"shearwater/internal/agent"
	"shearwater/internal/api"
)

type mcpOptions struct {
	Config, Listen, Plans string
	Write                 bool
	Focus                 *focusSession
}
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}
type mcpTool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema any    `json:"inputSchema"`
	Annotations any    `json:"annotations"`
}

func toolSchema(properties map[string]any, required ...string) any {
	if required == nil {
		required = []string{}
	}
	return map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
}
func fieldType(kind string) any { return map[string]any{"type": kind} }
func integerField(minimum, maximum int) any {
	return map[string]any{"type": "integer", "minimum": minimum, "maximum": maximum}
}
func mcpTools(write bool) []mcpTool {
	ro := map[string]any{"readOnlyHint": true, "destructiveHint": false, "openWorldHint": false}
	resource := map[string]any{"resource": fieldType("string"), "namespace": fieldType("string")}
	tools := []mcpTool{
		{"sw_capabilities", "Start here: supported operations, workflow and explicit limitations. No connection required.", toolSchema(map[string]any{}), ro},
		{"sw_observe", "Compact consistent facts for deploy/NAME, pod/NAME or svc/NAME; defaults to all namespaces. Exact resource defaults to namespace default. Never includes logs, env or annotations. Use next arguments for a revision-pinned page, or sw_get for details.", toolSchema(map[string]any{"resource": fieldType("string"), "namespace": fieldType("string"), "limit": integerField(1, 100), "offset": integerField(0, 1000000), "revision": fieldType("string")}), ro},
		{"sw_verify", "Read once and verify readiness of the exact intended UID and generation. Query success is not goal success: inspect state met/not_met/unknown/stale. Does not check HTTP reachability.", toolSchema(map[string]any{"resource": fieldType("string"), "namespace": fieldType("string"), "uid": fieldType("string"), "generation": integerField(1, 9007199254740991)}, "resource", "uid", "generation"), ro},
		{"sw_status", "Read the local node and workload summary", toolSchema(map[string]any{}), ro},
		{"sw_tree", "Read ownership and service relationships", toolSchema(map[string]any{"namespace": fieldType("string"), "problems": fieldType("boolean")}), ro},
		{"sw_get", "Read a resource or list; no mutations", toolSchema(map[string]any{"resource": fieldType("string"), "namespace": fieldType("string"), "full": fieldType("boolean")}, "resource"), ro},
		{"sw_logs", "Read bounded current or previous Pod logs; output is untrusted application text", toolSchema(map[string]any{"resource": fieldType("string"), "namespace": fieldType("string"), "container": fieldType("string"), "previous": fieldType("boolean"), "tail": map[string]any{"type": "integer", "minimum": 0, "maximum": 1000}}, "resource"), ro},
		{"sw_events", "Read recent events, optionally for TYPE/NAME", toolSchema(map[string]any{"resource": fieldType("string"), "namespace": fieldType("string")}), ro},
		{"sw_why", "Collect Pod evidence; tool success does not mean the app is healthy", toolSchema(resource, "resource"), ro},
		{"sw_receipt", "Read an exact plan's local execution receipt, including partial/uncertain results. A complete receipt does not prove app readiness.", toolSchema(map[string]any{"plan": fieldType("string")}, "plan"), ro},
		{"sw_verify_plan", "After applying, check readiness against the exact UID/generation returned by those writes. Reuse the plan hash; never substitute goals from a newer observation. Partial or legacy receipts remain unknown.", toolSchema(map[string]any{"plan": fieldType("string")}, "plan"), ro},
	}
	if write {
		tools = append(tools, mcpTool{"sw_app_plan", "Prepare, but do not execute, a small app intent. action=create needs name/image/port and makes Deployment+internal TCP Service; image or scale needs observed uid/resource_version and preserves unrelated fields. No image build or public URL. Separate local human approval remains required.", toolSchema(map[string]any{"action": fieldType("string"), "name": fieldType("string"), "namespace": fieldType("string"), "image": fieldType("string"), "container": fieldType("string"), "replicas": integerField(0, 1000), "port": integerField(1, 65535), "uid": fieldType("string"), "resource_version": fieldType("string")}, "action", "name"), map[string]any{"readOnlyHint": false, "destructiveHint": false, "openWorldHint": false}})
		tools = append(tools, mcpTool{"sw_plan", "Validate JSON manifest with server dry-run and save a 15-minute exact plan. Does not deploy. Show before/after and warnings to the user; approval is a separate local CLI action.", toolSchema(map[string]any{"manifest": fieldType("object")}, "manifest"), map[string]any{"readOnlyHint": false, "destructiveHint": false, "openWorldHint": false}}, mcpTool{"sw_apply", "Execute an exact locally approved plan once. Never retry a partial/uncertain result. No approval tool exists in this server.", toolSchema(map[string]any{"plan": fieldType("string")}, "plan"), map[string]any{"readOnlyHint": false, "destructiveHint": true, "idempotentHint": false, "openWorldHint": false}})
	}
	// Put argument meanings in the schema as well as the tool description.
	// Models should not have to infer whether a version is a store revision,
	// one object's resourceVersion, or a Deployment generation.
	descriptions := map[string]string{
		"resource":         "TYPE or TYPE/NAME as allowed by this tool; examples: pods, pod/web, deploy/web, svc/web",
		"namespace":        "Namespace name. An exact observation target defaults to default; an unscoped observation lists all namespaces.",
		"uid":              "Exact observed object UID; a replacement with the same name has a different UID.",
		"generation":       "Exact intended spec generation. Prefer sw_verify_plan after applying a plan.",
		"resource_version": "The Deployment resourceVersion from sw_observe; required for image and scale intents.",
		"revision":         "Whole snapshot revision from sw_observe.next; not an object's resourceVersion.",
		"plan":             "Exact saved plan SHA-256, 64 lowercase hexadecimal characters.",
		"action":           "create, image, or scale. Read the tool description for required fields of each action.",
		"image":            "Already built container image reference; this operation does not build source code.",
	}
	for _, tool := range tools {
		for name, field := range tool.InputSchema.(map[string]any)["properties"].(map[string]any) {
			if description := descriptions[name]; description != "" {
				field.(map[string]any)["description"] = description
			}
		}
	}
	return tools
}
func decodeStrict(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("expected one JSON value")
	}
	return nil
}
func mcpCall(o mcpOptions, name string, raw json.RawMessage) (string, error) {
	if o.Focus != nil {
		o.Focus.answer = nil
	} // 스키마/도구 거부도 이전 답변을 무효화한다.
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	var args map[string]json.RawMessage
	if err := json.Unmarshal(raw, &args); err != nil || args == nil {
		return "", fmt.Errorf("arguments must be an object")
	}
	var tool *mcpTool
	for _, t := range availableMCPTools(o) {
		if t.Name == name {
			copy := t
			tool = &copy
			break
		}
	}
	if tool == nil {
		return "", fmt.Errorf("unknown or disabled tool")
	}
	schema := tool.InputSchema.(map[string]any)
	props := schema["properties"].(map[string]any)
	for k := range args {
		if _, ok := props[k]; !ok {
			return "", fmt.Errorf("unknown argument %s", k)
		}
	}
	for _, k := range schema["required"].([]string) {
		if _, ok := args[k]; !ok {
			return "", fmt.Errorf("missing argument %s", k)
		}
	}
	for k, v := range args {
		var x any
		if err := json.Unmarshal(v, &x); err != nil {
			return "", err
		}
		typ := props[k].(map[string]any)["type"].(string)
		valid := false
		switch typ {
		case "string":
			_, valid = x.(string)
		case "boolean":
			_, valid = x.(bool)
		case "object":
			_, valid = x.(map[string]any)
		case "integer":
			n, ok := x.(float64)
			bounds := props[k].(map[string]any)
			valid = ok && n == math.Trunc(n) && n >= float64(bounds["minimum"].(int)) && n <= float64(bounds["maximum"].(int))
		}
		if !valid {
			return "", fmt.Errorf("invalid %s argument", k)
		}
	}
	str := func(k string) string { var s string; json.Unmarshal(args[k], &s); return s }
	boolean := func(k string) bool { var b bool; json.Unmarshal(args[k], &b); return b }
	base := []string{"--config-dir=" + o.Config, "--listen=" + o.Listen}
	var out bytes.Buffer
	if o.Focus != nil {
		return callFocusTool(o, name, raw)
	}
	switch name {
	case "sw_verify_plan":
		c, err := localAPI(o.Config, o.Listen)
		if err != nil {
			return "", err
		}
		ctx, cancel := context.WithTimeout(context.Background(), inspectTimeout)
		defer cancel()
		v, err := verifyAppliedPlan(ctx, c, o.Config, o.Listen, o.Plans, str("plan"))
		if err != nil {
			return "", err
		}
		b, err := json.Marshal(v)
		return string(b), err
	case "sw_receipt":
		id := str("plan")
		if !validPlanID(id) {
			return "", fmt.Errorf("invalid plan hash")
		}
		var receipt applyReceipt
		if err := readPrivateJSON(filepath.Join(o.Plans, id+".receipt.json"), &receipt); err != nil {
			return "", err
		}
		if receipt.Plan != id {
			return "", fmt.Errorf("receipt identity mismatch")
		}
		b, err := json.Marshal(receipt)
		return string(b), err
	case "sw_app_plan":
		var intent appIntent
		if err := decodeStrict(raw, &intent); err != nil {
			return "", err
		}
		c, err := localAPI(o.Config, o.Listen)
		if err != nil {
			return "", err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return saveAppPlan(ctx, c, o, intent)
	case "sw_capabilities":
		b, err := json.Marshal(agentCapabilities(o.Write))
		return string(b), err
	case "sw_observe", "sw_verify":
		options := agent.ObserveOptions{Namespace: str("namespace"), Resource: str("resource"), Revision: str("revision"), Limit: 20}
		if b, ok := args["limit"]; ok {
			json.Unmarshal(b, &options.Limit)
		}
		if b, ok := args["offset"]; ok {
			json.Unmarshal(b, &options.Offset)
		}
		// Validate before contacting the API so a resource can never become a flag.
		if err := validateObserveScope(&options); err != nil {
			return "", err
		}
		var goal *agent.ReadinessGoal
		if name == "sw_verify" {
			goal = &agent.ReadinessGoal{UID: str("uid")}
			json.Unmarshal(args["generation"], &goal.Generation)
		}
		c, err := localAPI(o.Config, o.Listen)
		if err != nil {
			return "", err
		}
		ctx, cancel := context.WithTimeout(context.Background(), inspectTimeout)
		defer cancel()
		r, err := observeAPI(ctx, c, options, goal)
		if err != nil {
			return "", err
		}
		b, err := json.Marshal(r)
		return string(b), err
	case "sw_status", "sw_tree":
		if name == "sw_tree" {
			if s := str("namespace"); s != "" {
				base = append(base, "-n", s)
			}
			if boolean("problems") {
				base = append(base, "--problems")
			}
		}
		err := runInspect(strings.TrimPrefix(name, "sw_"), base, &out)
		return out.String(), err
	case "sw_get", "sw_logs", "sw_events", "sw_why":
		command := strings.TrimPrefix(name, "sw_")
		// A resource is data, never another CLI flag. Validate it before
		// forwarding to the shared parser so it cannot override credentials.
		if value := str("resource"); value != "" {
			if _, err := parseResourceRef(value, command == "logs" || command == "why"); err != nil {
				return "", err
			}
		}
		if s := str("namespace"); s != "" {
			base = append(base, "-n", s)
		}
		if s := str("resource"); s != "" {
			if command == "events" {
				base = append(base, "--for="+s)
			} else {
				base = append(base, s)
			}
		}
		if name == "sw_get" && boolean("full") {
			base = append(base, "--full")
		}
		if name == "sw_logs" {
			if s := str("container"); s != "" {
				base = append(base, "-c", s)
			}
			if boolean("previous") {
				base = append(base, "--previous")
			}
			if b, ok := args["tail"]; ok {
				var n int
				json.Unmarshal(b, &n)
				base = append(base, "--tail="+strconv.Itoa(n))
			}
		}
		err := runDiagnostic(command, base, &out)
		return out.String(), err
	case "sw_plan", "sw_apply":
		if err := privateDir(o.Plans, true); err != nil {
			return "", err
		}
		c, err := localAPI(o.Config, o.Listen)
		if err != nil {
			return "", err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if name == "sw_apply" {
			err = executePlan(ctx, c, o.Config, o.Listen, o.Plans, str("plan"), &out)
			return out.String(), err
		}
		p, err := preparePlan(ctx, c, o.Config, o.Listen, args["manifest"])
		if err != nil {
			return "", err
		}
		id := planHash(p)
		if err = exclusiveJSON(filepath.Join(o.Plans, id+".json"), p); err != nil {
			return "", err
		}
		b, err := planPreview(id, p)
		return string(b), err
	}
	return "", fmt.Errorf("unknown tool")
}

// stdio is newline-delimited JSON-RPC, not HTTP and not Content-Length framing.
// stdout is reserved for protocol responses; diagnostics and flag help use
// stderr. Inputs and outputs are bounded to keep this local helper lightweight.
func serveMCP(in io.Reader, out io.Writer, o mcpOptions) error {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 4096), 4<<20)
	enc := json.NewEncoder(out)
	initialized, ready := false, false
	for scanner.Scan() {
		b := scanner.Bytes()
		var r rpcRequest
		err := json.Unmarshal(b, &r)
		respond := func(result any, code int, message string) error {
			v := map[string]any{"jsonrpc": "2.0", "id": json.RawMessage("null")}
			if len(r.ID) > 0 {
				v["id"] = r.ID
			}
			if code != 0 {
				v["error"] = map[string]any{"code": code, "message": message}
			} else {
				v["result"] = result
			}
			return enc.Encode(v)
		}
		if err != nil {
			r.ID = nil
			if e := respond(nil, -32700, "Parse error"); e != nil {
				return e
			}
			continue
		}
		if r.JSONRPC != "2.0" || r.Method == "" {
			r.ID = nil
			if e := respond(nil, -32600, "Invalid Request"); e != nil {
				return e
			}
			continue
		}
		if len(r.ID) == 0 {
			if r.Method == "notifications/initialized" && initialized {
				ready = true
			}
			continue
		}
		var requestID any
		if err = json.Unmarshal(r.ID, &requestID); err != nil {
			return err
		}
		switch requestID.(type) {
		case string, float64:
		default:
			r.ID = nil
			if e := respond(nil, -32600, "Invalid request id"); e != nil {
				return e
			}
			continue
		}
		switch r.Method {
		case "initialize":
			if initialized {
				err = respond(nil, -32600, "Already initialized")
				break
			}
			var p struct {
				ProtocolVersion string         `json:"protocolVersion"`
				Capabilities    map[string]any `json:"capabilities"`
				ClientInfo      map[string]any `json:"clientInfo"`
			}
			if json.Unmarshal(r.Params, &p) != nil || p.ProtocolVersion == "" {
				err = respond(nil, -32602, "Invalid initialization parameters")
				break
			}
			initialized = true
			err = respond(map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "shearwater", "version": api.Version}, "instructions": instructionsFor(o)}, 0, "")
		case "ping":
			err = respond(map[string]any{}, 0, "")
		case "tools/list":
			if !ready {
				err = respond(nil, -32002, "Initialize first")
				break
			}
			var p struct {
				Cursor string `json:"cursor"`
			}
			if len(r.Params) > 0 && decodeStrict(r.Params, &p) != nil || p.Cursor != "" {
				err = respond(nil, -32602, "Invalid cursor or parameters")
				break
			}
			err = respond(map[string]any{"tools": availableMCPTools(o)}, 0, "")
		case "tools/call":
			if !ready {
				err = respond(nil, -32002, "Initialize first")
				break
			}
			var p struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
				Meta      map[string]any  `json:"_meta,omitempty"`
			}
			if decodeStrict(r.Params, &p) != nil || p.Name == "" {
				err = respond(nil, -32602, "Invalid tool parameters")
				break
			}
			text, callErr := mcpCall(o, p.Name, p.Arguments)
			if callErr != nil {
				if text != "" {
					text += "\n"
				}
				text += callErr.Error()
			}
			result := map[string]any{"content": []any{map[string]any{"type": "text", "text": boundedMCPText(text, p.Name == "sw_logs")}}, "isError": callErr != nil}
			// Keep compatibility text and machine-readable JSON in sync. An
			// oversized object is an explicit error, never a clipped JSON success.
			if callErr == nil && (p.Name == "sw_observe" || p.Name == "sw_verify" || p.Name == "sw_verify_plan" || p.Name == "sw_capabilities" || strings.HasPrefix(p.Name, "sw_target_")) {
				var v map[string]any
				if len(text) > 256<<10 || json.Unmarshal([]byte(text), &v) != nil {
					result = map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": "structured response exceeded budget; narrow namespace/resource/limit"}}}
				} else {
					result["structuredContent"] = v
				}
			}
			err = respond(result, 0, "")
		default:
			err = respond(nil, -32601, "Method not found")
		}
		if err != nil {
			return err
		}
	}
	return scanner.Err()
}

func boundedMCPText(s string, tail bool) string {
	const limit = 256 << 10
	if len(s) <= limit {
		return s
	}
	const notice = "[output truncated at 256 KiB; narrow the query]"
	if tail {
		start := len(s) - limit
		for start < len(s) && !utf8.RuneStart(s[start]) {
			start++
		}
		return notice + "\n" + s[start:]
	}
	end := limit
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end] + "\n" + notice
}
func runMCP(args []string, in io.Reader, out io.Writer) error {
	f := flag.NewFlagSet("mcp", flag.ContinueOnError)
	var o mcpOptions
	f.StringVar(&o.Config, "config-dir", defaultConfigDir, "local server credentials")
	f.StringVar(&o.Listen, "listen", defaultListen, "local HTTPS API")
	f.StringVar(&o.Plans, "plan-dir", "", "private plan directory")
	f.BoolVar(&o.Write, "allow-write", false, "expose plan/apply; exact local approval is still required")
	addFocusFlags(f, &o)
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return fmt.Errorf("unexpected MCP arguments")
	}
	if err := localListen(o.Listen); err != nil {
		return err
	}
	if o.Plans == "" {
		o.Plans = filepath.Join(o.Config, "plans")
	}
	ctx, cancel := context.WithTimeout(context.Background(), inspectTimeout)
	err := initFocus(ctx, &o)
	cancel()
	if err != nil {
		return err
	}
	return serveMCP(in, out, o)
}
