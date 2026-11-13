package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"shearwater/internal/apis"
)

type planAction struct {
	Method   string          `json:"method"`
	Path     string          `json:"path"`
	UID      string          `json:"baseUID,omitempty"`
	Revision string          `json:"baseRevision,omitempty"`
	Body     json.RawMessage `json:"body"`
	Before   json.RawMessage `json:"before,omitempty"`
	After    json.RawMessage `json:"after"`
	Warnings []string        `json:"warnings,omitempty"`
}
type deploymentPlan struct {
	Format   int          `json:"format"`
	Created  time.Time    `json:"created"`
	Expires  time.Time    `json:"expires"`
	Listen   string       `json:"listen"`
	Identity string       `json:"identity"` // hash of CA and token; never the credential
	Actions  []planAction `json:"actions"`
}

func planHash(p deploymentPlan) string {
	b, _ := json.Marshal(p)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func connectionIdentity(c *agentClient, config string) (string, error) {
	b, err := os.ReadFile(filepath.Join(config, "ca.crt"))
	if err != nil {
		return "", err
	}
	h := sha256.New()
	h.Write(b)
	h.Write([]byte(c.token))
	return hex.EncodeToString(h.Sum(nil)), nil
}
func validPlanID(id string) bool {
	b, err := hex.DecodeString(id)
	return err == nil && len(b) == 32 && strings.ToLower(id) == id
}
func exclusiveJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err = f.Write(append(b, '\n')); err != nil {
		return err
	}
	return f.Sync()
}
func apiRequest(ctx context.Context, c *agentClient, method, path string, body []byte) ([]byte, int, []string, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.url+path, bytes.NewReader(body))
	if err != nil {
		return nil, 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "shearwater-plan")
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, (8<<20)+1))
	if err != nil {
		return nil, resp.StatusCode, nil, err
	}
	if len(b) > 8<<20 {
		return nil, resp.StatusCode, nil, fmt.Errorf("API response too large")
	}
	if resp.StatusCode >= 300 {
		return b, resp.StatusCode, resp.Header.Values("Warning"), fmt.Errorf("HTTP %d: %s", resp.StatusCode, safeDiagnosticText(string(b)))
	}
	return b, resp.StatusCode, resp.Header.Values("Warning"), nil
}
func objectMap(b []byte) (map[string]any, error) {
	var v map[string]any
	err := json.Unmarshal(b, &v)
	if err == nil && v == nil {
		err = fmt.Errorf("expected JSON object")
	}
	return v, err
}
func objectMeta(v map[string]any) map[string]any {
	m, _ := v["metadata"].(map[string]any)
	if m == nil {
		m = map[string]any{}
		v["metadata"] = m
	}
	return m
}
func textField(v map[string]any, k string) string { s, _ := v[k].(string); return s }
func manifestObjects(b []byte) ([]json.RawMessage, error) {
	if len(b) > 2<<20 {
		return nil, fmt.Errorf("manifest exceeds 2 MiB")
	}
	if fields := apis.DuplicateFields(b); len(fields) > 0 {
		return nil, fmt.Errorf("duplicate manifest fields: %v", fields)
	}
	v, err := objectMap(b)
	if err != nil {
		return nil, fmt.Errorf("manifest must be a JSON object or Kubernetes List: %w", err)
	}
	if textField(v, "kind") != "List" {
		return []json.RawMessage{b}, nil
	}
	var list struct {
		Items []json.RawMessage `json:"items"`
	}
	if err = json.Unmarshal(b, &list); err != nil {
		return nil, err
	}
	if len(list.Items) == 0 || len(list.Items) > 100 {
		return nil, fmt.Errorf("List must contain 1..100 objects")
	}
	return list.Items, nil
}
func preparePlan(ctx context.Context, c *agentClient, config, listen string, b []byte) (deploymentPlan, error) {
	p := deploymentPlan{Format: 1, Created: time.Now().UTC(), Listen: listen}
	p.Expires = p.Created.Add(15 * time.Minute)
	var err error
	p.Identity, err = connectionIdentity(c, config)
	if err != nil {
		return p, err
	}
	objects, err := manifestObjects(b)
	if err != nil {
		return p, err
	}
	seen := map[string]bool{}
	for _, raw := range objects {
		obj, err := objectMap(raw)
		if err != nil {
			return p, err
		}
		kind := textField(obj, "kind")
		version := textField(obj, "apiVersion")
		var res *apis.Resource
		for _, r := range apis.Resources {
			if r.Kind == kind && r.APIVersion() == version {
				res = r
				break
			}
		}
		if res == nil || !res.Create || (kind != "Pod" && kind != "Deployment" && kind != "ReplicaSet" && kind != "Service" && kind != "Namespace") {
			return p, fmt.Errorf("unsupported planned resource %s/%s", version, kind)
		}
		m := objectMeta(obj)
		name := textField(m, "name")
		if !apis.IsDNS1123Subdomain(name) {
			return p, fmt.Errorf("explicit valid metadata.name required")
		}
		ns := textField(m, "namespace")
		if res.Namespaced && ns == "" {
			ns = "default"
			m["namespace"] = ns
		}
		if ns != "" && !apis.IsDNS1123Label(ns) {
			return p, fmt.Errorf("invalid namespace")
		}
		if ns == "kube-system" || kind == "Namespace" && name == "kube-system" {
			return p, fmt.Errorf("kube-system is protected from planned writes")
		}
		ref := resourceRef{res, name}
		path := ref.path(ns)
		if seen[path] {
			return p, fmt.Errorf("duplicate target %s", path)
		}
		seen[path] = true
		current, code, _, err := apiRequest(ctx, c, "GET", path, nil)
		a := planAction{Method: "POST", Path: resourceRef{resource: res}.path(ns)}
		switch {
		case code == 404:
			delete(m, "resourceVersion")
			delete(m, "uid")
		case err != nil:
			return p, err
		default:
			old, err := objectMap(current)
			if err != nil {
				return p, err
			}
			meta := objectMeta(old)
			if meta["deletionTimestamp"] != nil {
				return p, fmt.Errorf("target is being deleted: %s", path)
			}
			a.Method = "PUT"
			a.Path = path
			a.UID = textField(meta, "uid")
			a.Revision = textField(meta, "resourceVersion")
			m["uid"] = a.UID
			m["resourceVersion"] = a.Revision
			a.Before = current
			// ClusterIP is allocated by the server and immutable. An omitted value in
			// a user's declarative Service should preserve that allocation.
			if kind == "Service" {
				spec, _ := obj["spec"].(map[string]any)
				oldSpec, _ := old["spec"].(map[string]any)
				if spec != nil && spec["clusterIP"] == nil {
					spec["clusterIP"] = oldSpec["clusterIP"]
				}
			}
		}
		delete(obj, "status")
		a.Body, err = json.Marshal(obj)
		if err != nil {
			return p, err
		}
		a.After, _, a.Warnings, err = apiRequest(ctx, c, a.Method, a.Path+"?dryRun=All&fieldValidation=Strict", a.Body)
		if err != nil {
			return p, fmt.Errorf("plan validation for %s: %w", path, err)
		}
		p.Actions = append(p.Actions, a)
	}
	return p, nil
}

func loadPlan(dir, id string) (deploymentPlan, error) {
	var p deploymentPlan
	if !validPlanID(id) {
		return p, fmt.Errorf("plan must be its 64-character SHA-256 ID")
	}
	f, err := openPrivate(filepath.Join(dir, id+".json"), os.O_RDONLY)
	if err != nil {
		return p, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, (32<<20)+1))
	if err != nil {
		return p, err
	}
	if len(b) > 32<<20 {
		return p, fmt.Errorf("plan too large")
	}
	if err = json.Unmarshal(b, &p); err != nil {
		return p, err
	}
	if p.Format != 1 || len(p.Actions) < 1 || len(p.Actions) > 100 || planHash(p) != id {
		return p, fmt.Errorf("plan content does not match ID")
	}
	if time.Now().After(p.Expires) {
		return p, fmt.Errorf("plan expired; prepare and review a new plan")
	}
	return p, nil
}
func validatePlanConnection(p deploymentPlan, c *agentClient, config, listen string) error {
	id, err := connectionIdentity(c, config)
	if err != nil {
		return err
	}
	if p.Listen != listen || p.Identity != id {
		return fmt.Errorf("plan belongs to a different server identity")
	}
	return nil
}
func checkPlanTarget(ctx context.Context, c *agentClient, a planAction) error {
	path := planTarget(a)
	b, code, _, err := apiRequest(ctx, c, "GET", path, nil)
	if a.Method == "POST" && code == 404 {
		return nil
	}
	if err != nil {
		return err
	}
	if a.Method == "POST" {
		return fmt.Errorf("CONFLICT: planned creation now exists: %s", path)
	}
	v, err := objectMap(b)
	if err != nil {
		return err
	}
	m := objectMeta(v)
	if textField(m, "uid") != a.UID || textField(m, "resourceVersion") != a.Revision {
		return fmt.Errorf("CONFLICT: %s changed after the plan; make a new plan", path)
	}
	return nil
}

// POST is sent to a collection URL, but an audit receipt must identify the
// actual object. Otherwise two creations would both say just /.../pods.
func planTarget(a planAction) string {
	if a.Method != "POST" {
		return a.Path
	}
	obj, err := objectMap(a.Body)
	if err != nil {
		return a.Path // invalid content will be refused by dry-run before writes
	}
	return a.Path + "/" + textField(objectMeta(obj), "name")
}

func planPreview(id string, p deploymentPlan) ([]byte, error) {
	v := map[string]any{"plan": id, "expires": p.Expires, "actions": p.Actions,
		"approval": "Review this exact plan, then use sw approve --plan=HASH --confirm=HASH locally. MCP cannot approve it."}
	b, err := json.Marshal(v)
	if err != nil || len(b) <= 256<<10 {
		return b, err
	}
	// Keep a valid JSON reply and its plan ID even when the before/after diff is
	// larger than an MCP reply. A human can read the entire immutable plan with
	// the CLI before approving; a clipped diff is not enough for review.
	targets := make([]string, 0, len(p.Actions))
	for _, a := range p.Actions {
		targets = append(targets, a.Method+" "+planTarget(a))
	}
	return json.Marshal(map[string]any{"plan": id, "expires": p.Expires, "targets": targets,
		"fullPreview": false, "reviewCommand": "sw plan --plan=" + id,
		"approval": "Full before/after exceeds 256 KiB. Review the saved plan with the command above and the same --config-dir, --listen and --plan-dir before local approval."})
}

type applyReceipt struct {
	Plan    string    `json:"plan"`
	At      time.Time `json:"at"`
	State   string    `json:"state"`
	Applied []string  `json:"applied"`
	Error   string    `json:"error,omitempty"`
}

func executePlan(ctx context.Context, c *agentClient, config, listen, dir, id string, out io.Writer) error {
	p, err := loadPlan(dir, id)
	if err != nil {
		return err
	}
	if err = validatePlanConnection(p, c, config, listen); err != nil {
		return err
	}
	lock, err := fileLock(dir, "apply.lock")
	if err != nil {
		return err
	}
	defer lock.Close()
	var approval struct {
		Plan string `json:"plan"`
	}
	if err = readPrivateJSON(filepath.Join(dir, id+".approved.json"), &approval); err != nil || approval.Plan != id {
		return fmt.Errorf("exact plan needs local approval: sw approve --plan=%s --confirm=%s (same plan directory)", id, id)
	}
	// Check every target before the first write. Each update also carries its
	// UID/revision in the server transaction, closing the GET-to-PUT race.
	for _, a := range p.Actions {
		if err = checkPlanTarget(ctx, c, a); err != nil {
			return err
		}
		if _, _, _, err = apiRequest(ctx, c, a.Method, a.Path+"?dryRun=All&fieldValidation=Strict", a.Body); err != nil {
			return err
		}
	}
	receipt := applyReceipt{Plan: id, At: time.Now().UTC(), State: "attempted"}
	receiptPath := filepath.Join(dir, id+".receipt.json")
	// Reserve execution before sending a write. A lost response is not safe to
	// retry; the immutable receipt tells the operator to inspect actual state.
	if err = exclusiveJSON(receiptPath, receipt); err != nil {
		return fmt.Errorf("plan already attempted (or cannot record attempt); inspect its receipt: %w", err)
	}
	for _, a := range p.Actions {
		_, _, _, err = apiRequest(ctx, c, a.Method, a.Path+"?fieldValidation=Strict", a.Body)
		if err != nil {
			receipt.State = "uncertain-or-partial"
			receipt.Error = err.Error()
			saveErr := atomicJSON(receiptPath, receipt)
			fmt.Fprintf(out, "Partial/uncertain result; applied=%v; receipt=%s\n", receipt.Applied, receiptPath)
			return errors.Join(err, saveErr)
		}
		receipt.Applied = append(receipt.Applied, planTarget(a))
		if err = atomicJSON(receiptPath, receipt); err != nil {
			return err
		}
	}
	receipt.State = "complete"
	if err = atomicJSON(receiptPath, receipt); err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(receipt)
}
func runPlanCommand(command string, args []string, out io.Writer) error {
	f := flag.NewFlagSet(command, flag.ContinueOnError)
	config := f.String("config-dir", defaultConfigDir, "server credentials")
	listen := f.String("listen", defaultListen, "local HTTPS API")
	dir := f.String("plan-dir", "", "private plan directory (default CONFIG/plans)")
	input := f.String("f", "", "JSON manifest file (plan only)")
	id := f.String("plan", "", "exact plan SHA-256")
	confirm := f.String("confirm", "", "repeat the reviewed hash (approve only)")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return fmt.Errorf("unexpected arguments")
	}
	if *dir == "" {
		*dir = filepath.Join(*config, "plans")
	}
	if !filepath.IsAbs(*dir) {
		return fmt.Errorf("plan-dir must be absolute")
	}
	if err := privateDir(*dir, true); err != nil {
		return err
	}
	switch command {
	case "approve":
		p, err := loadPlan(*dir, *id)
		if err != nil {
			return err
		}
		if *confirm != *id {
			return fmt.Errorf("review the plan then repeat its hash with --confirm")
		}
		c, err := localAPI(*config, *listen)
		if err != nil {
			return err
		}
		if err = validatePlanConnection(p, c, *config, *listen); err != nil {
			return err
		}
		if err = exclusiveJSON(filepath.Join(*dir, *id+".approved.json"), map[string]string{"plan": *id}); err != nil {
			return err
		}
		fmt.Fprintln(out, "Approved exact plan", *id)
		return nil
	case "plan", "apply":
		c, err := localAPI(*config, *listen)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if command == "apply" {
			return executePlan(ctx, c, *config, *listen, *dir, *id, out)
		}
		if *id != "" {
			if *input != "" {
				return fmt.Errorf("use either -f to create a plan or --plan to review a saved plan")
			}
			p, err := loadPlan(*dir, *id)
			if err != nil {
				return err
			}
			if err = validatePlanConnection(p, c, *config, *listen); err != nil {
				return err
			}
			enc := json.NewEncoder(out)
			enc.SetIndent("", "  ")
			return enc.Encode(p)
		}
		file, err := os.Open(*input)
		if err != nil {
			return err
		}
		defer file.Close()
		b, err := io.ReadAll(io.LimitReader(file, (2<<20)+1))
		if err != nil {
			return err
		}
		p, err := preparePlan(ctx, c, *config, *listen, b)
		if err != nil {
			return err
		}
		id := planHash(p)
		if err = exclusiveJSON(filepath.Join(*dir, id+".json"), p); err != nil {
			return err
		}
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]any{"plan": id, "expires": p.Expires, "actions": p.Actions, "next": "review before/after and warnings, then sw approve --plan=HASH --confirm=HASH; sw apply --plan=HASH"})
	default:
		return fmt.Errorf("unknown plan command")
	}
}
