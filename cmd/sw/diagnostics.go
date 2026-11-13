// 이 파일은 네 진단 명령의 공통 입구야. 인자를 검사한 뒤 기존 서버에
// 인증된 GET만 보내고, 검증이 끝난 결과를 한 번에 출력해. 런타임이나
// SQLite를 직접 열지 않으므로 CLI 실행 자체가 Pod를 바꾸지 않아.
// 실제 상태 해석은 internal/agent, 로그·이벤트 수집은 diagnostic_evidence.go,
// 여러 근거를 묶는 작업은 diagnostic_why.go에서 맡아.
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"shearwater/internal/agent"
	"shearwater/internal/apis"
)

type diagnosticOptions struct {
	config, listen, namespace, output, container, selector, forObject, since string
	all, full, previous                                                      bool
	tail, bytes, limit                                                       int
	timeout                                                                  time.Duration
	ref                                                                      resourceRef
}

type resourceRef struct {
	resource *apis.Resource
	name     string
}

func parseResourceRef(value string, requireName bool) (resourceRef, error) {
	kind, name, slash := strings.Cut(value, "/")
	for _, r := range apis.Resources {
		if kind != r.Plural && kind != r.Singular && !slices.Contains(r.ShortNames, kind) {
			continue
		}
		if (requireName && !slash) || (slash && !apis.IsDNS1123Subdomain(name)) {
			break
		}
		return resourceRef{r, name}, nil
	}
	return resourceRef{}, opsError("INVALID", "never", fmt.Sprintf("invalid resource reference %q", value), "use a supported TYPE or TYPE/NAME, for example pods or pod/web")
}

func (r resourceRef) path(namespace string) string {
	base := "/api/" + r.resource.Version
	if r.resource.Group != "" {
		base = "/apis/" + r.resource.Group + "/" + r.resource.Version
	}
	if r.resource.Namespaced && namespace != "" {
		base += "/namespaces/" + url.PathEscape(namespace)
	}
	base += "/" + r.resource.Plural
	if r.name != "" {
		base += "/" + url.PathEscape(r.name)
	}
	return base
}

// Go's flag package normally stops at the first positional argument. Accept
// both `get -n demo pod/web` and `get pod/web -n demo`, without guessing which
// words are flag values. Unknown flags still fail before any HTTP request.
func interspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			positional = append(positional, arg)
			continue
		}
		name, _, equals := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		f := fs.Lookup(name)
		if f == nil {
			if name == "h" || name == "help" {
				return nil, flag.ErrHelp
			}
			return nil, fmt.Errorf("unknown flag %q", arg)
		}
		flags = append(flags, arg)
		boolean, isBool := f.Value.(interface{ IsBoolFlag() bool })
		if !equals && !(isBool && boolean.IsBoolFlag()) {
			i++
			if i == len(args) {
				return nil, fmt.Errorf("flag %q needs a value", arg)
			}
			flags = append(flags, args[i])
		}
	}
	return append(append(flags, "--"), positional...), nil
}

func parseDiagnostic(command string, args []string, out io.Writer) (diagnosticOptions, bool, error) {
	var o diagnosticOptions
	fs := flag.NewFlagSet("sw "+command, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&o.config, "config-dir", defaultConfigDir, "directory with the server CA and token (read only)")
	fs.StringVar(&o.listen, "listen", defaultListen, "local HTTPS host:port")
	fs.StringVar(&o.namespace, "n", "default", "namespace (default default)")
	fs.DurationVar(&o.timeout, "timeout", 10*time.Second, "total read deadline, for example 10s")
	if command != "logs" {
		fs.StringVar(&o.output, "o", "text", "output: text or json")
	}
	if command == "get" || command == "events" {
		fs.BoolVar(&o.all, "A", false, "list all namespaces")
		fs.BoolVar(&o.all, "all-namespaces", false, "list all namespaces")
	}
	if command == "get" {
		fs.BoolVar(&o.full, "full", false, "print complete API JSON, including stored-only fields")
		fs.StringVar(&o.selector, "l", "", "label selector, for example app=web")
	}
	if command == "events" {
		fs.StringVar(&o.forObject, "for", "", "current object's events only, for example pod/web (matches UID)")
		fs.StringVar(&o.since, "since", "", "show Event records updated after this resourceVersion, not a timestamp")
	}
	if command == "logs" || command == "why" {
		fs.StringVar(&o.container, "c", "", "container name; logs requires this for multi-container Pods")
		fs.IntVar(&o.tail, "tail", 50, "last 0..10000 lines per excerpt")
		fs.IntVar(&o.bytes, "limit-bytes", 64<<10, "maximum bytes per excerpt (1..1048576)")
	}
	if command == "logs" {
		fs.BoolVar(&o.previous, "previous", false, "read the previous container attempt")
	}
	if command == "events" || command == "why" {
		fs.IntVar(&o.limit, "limit", 20, "maximum recent Event records (1..1000)")
	}
	ordered, err := interspersed(fs, args)
	if err == nil {
		err = fs.Parse(ordered)
	}
	if errors.Is(err, flag.ErrHelp) {
		fmt.Fprintln(out, "Usage: sw "+command+map[string]string{"get": " TYPE[/NAME] [flags]", "logs": " pod/NAME [flags]", "events": " [flags]", "why": " pod/NAME [flags]"}[command])
		fs.SetOutput(out)
		fs.PrintDefaults()
		return o, true, nil
	}
	invalid := func(detail string) (diagnosticOptions, bool, error) {
		return o, false, opsError("INVALID", "never", detail, "run sw "+command+" -h for usage")
	}
	if err != nil {
		return invalid(err.Error())
	}
	if !apis.IsDNS1123Label(o.namespace) {
		return invalid("invalid namespace")
	}
	if o.timeout <= 0 || o.timeout > 5*time.Minute {
		return invalid("--timeout must be greater than zero and at most 5m")
	}
	if command != "logs" && o.output != "text" && o.output != "json" {
		return invalid("-o must be text or json")
	}
	if (command == "logs" || command == "why") && (o.tail < 0 || o.tail > 10000 || o.bytes < 1 || o.bytes > 1<<20) {
		return invalid("--tail must be 0..10000 and --limit-bytes 1..1048576")
	}
	if (command == "events" || command == "why") && (o.limit < 1 || o.limit > 1000) {
		return invalid("--limit must be 1..1000")
	}
	if command == "events" {
		if fs.NArg() != 0 {
			return invalid("events does not take a positional argument; use --for TYPE/NAME")
		}
		if o.forObject != "" {
			o.ref, err = parseResourceRef(o.forObject, true)
		}
		if o.since != "" {
			if _, e := strconv.ParseUint(o.since, 10, 63); e != nil {
				return invalid("--since must be a non-negative resourceVersion")
			}
		}
	} else {
		if fs.NArg() != 1 {
			return invalid("expected one resource reference")
		}
		o.ref, err = parseResourceRef(fs.Arg(0), command != "get")
	}
	if err != nil {
		return o, false, err
	}
	if o.ref.resource != nil && (command == "logs" || command == "why") && o.ref.resource.Plural != "pods" {
		return invalid(command + " currently requires pod/NAME; use get pods -l app=NAME to choose a Pod")
	}
	if o.all && o.ref.name != "" {
		return invalid("a named object requires one namespace; use -n instead of -A")
	}
	if o.selector != "" {
		if o.ref.name != "" {
			return invalid("-l selects a list, not a named object")
		}
		if _, e := apis.ParseSelector(o.selector); e != nil {
			return invalid(e.Error())
		}
	}
	if o.all {
		o.namespace = ""
	}
	return o, false, nil
}

func runDiagnostic(command string, args []string, out io.Writer) error {
	o, help, err := parseDiagnostic(command, args, out)
	if err != nil || help {
		return err
	}
	c, err := localAPI(o.config, o.listen)
	if err != nil {
		return err
	}
	// One command-wide deadline covers all reads, including slow log bodies.
	// Keep the existing local-only dialer, CA verification and redirect refusal.
	c.client.Timeout = 0
	ctx, cancel := context.WithTimeout(context.Background(), o.timeout)
	defer cancel()
	var output []byte
	switch command {
	case "get":
		output, err = diagnosticGet(ctx, c, o)
	case "events":
		output, err = diagnosticEvents(ctx, c, o)
	case "logs":
		output, err = diagnosticLogs(ctx, c, o)
	case "why":
		output, err = diagnosticWhy(ctx, c, o)
	default:
		return opsError("INVALID", "never", "unknown diagnostic command", "use get, logs, events or why")
	}
	if err != nil {
		return err
	}
	// Finite reads are assembled and checked before output. A disappearing Pod
	// must not leave logs labelled with a different Pod's UID in a pipeline.
	if _, err = out.Write(output); err != nil {
		return opsError("INTERNAL", "safe", "cannot write output: "+err.Error(), "check the output destination")
	}
	return nil
}

func diagnosticRead(ctx context.Context, c *agentClient, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	res, err := c.client.Do(req)
	if err != nil {
		var verify *tls.CertificateVerificationError
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return nil, opsError("TIMEOUT", "safe", "diagnostic read timed out", "check sw doctor or increase --timeout")
		case errors.As(err, &verify):
			return nil, opsError("UNAUTHORIZED", "never", "server TLS identity does not match", "check --listen and --config-dir")
		default:
			return nil, opsError("NOT_RUNNING", "never", "cannot reach the local API: "+err.Error(), "check sw status and sw doctor with the same connection flags")
		}
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, (32<<20)+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, opsError("TIMEOUT", "safe", "reading the response timed out", "increase --timeout or inspect the server")
		}
		return nil, opsError("INTERNAL", "safe", "cannot read response: "+err.Error(), "retry the command")
	}
	if len(body) > 32<<20 {
		return nil, opsError("INVALID", "never", "response exceeds 32 MiB", "narrow the namespace or label selector")
	}
	if res.StatusCode != http.StatusOK {
		code, retry := "RUNTIME_ERROR", "safe"
		switch res.StatusCode {
		case 400, 422:
			code, retry = "INVALID", "never"
		case 401, 403:
			code, retry = "UNAUTHORIZED", "never"
		case 404:
			code, retry = "NOT_FOUND", "never"
		case 409, 410:
			code = "CONFLICT"
		case 405, 415, 501:
			code, retry = "UNSUPPORTED", "never"
		}
		detail := fmt.Sprintf("HTTP %d for %s", res.StatusCode, path)
		var status apis.Status
		if json.Unmarshal(body, &status) == nil && status.Message != "" {
			detail += ": " + safeDiagnosticText(status.Message)
		}
		return nil, opsError(code, retry, detail, "check the object name, namespace and server message")
	}
	return body, nil
}

func diagnosticJSON(ctx context.Context, c *agentClient, path string, out any) error {
	b, err := diagnosticRead(ctx, c, path)
	if err != nil {
		return err
	}
	if err = json.Unmarshal(b, out); err != nil {
		return opsError("INTERNAL", "safe", "invalid API JSON: "+err.Error(), "check that --listen points to a Shearwater server")
	}
	return nil
}

func diagnosticObject(ctx context.Context, c *agentClient, r resourceRef, ns string) (apis.Object, error) {
	o := r.resource.New()
	if err := diagnosticJSON(ctx, c, r.path(ns), o); err != nil {
		return nil, err
	}
	if o.Meta().UID == "" || o.Meta().Name != r.name {
		return nil, opsError("INTERNAL", "safe", "API returned a different or unidentified object", "check the API endpoint")
	}
	return o, nil
}

func prettyJSON(v any) ([]byte, error) {
	b, e := json.MarshalIndent(v, "", "  ")
	return append(b, '\n'), e
}

// Replace terminal control characters in metadata/errors. Log excerpts in why
// are quoted separately; `logs` itself returns the original application bytes.
func safeDiagnosticText(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
}

func diagnosticGet(ctx context.Context, c *agentClient, o diagnosticOptions) ([]byte, error) {
	path := o.ref.path(o.namespace)
	if o.selector != "" {
		path += "?" + url.Values{"labelSelector": {o.selector}}.Encode()
	}
	body, err := diagnosticRead(ctx, c, path)
	if err != nil {
		return nil, err
	}
	if !json.Valid(body) {
		return nil, opsError("INTERNAL", "safe", "invalid object JSON", "check the server")
	}
	if o.full || o.output == "json" {
		// RawMessage preserves large integers exactly. Decoding into any would
		// turn JSON numbers into float64 and could silently round a generation.
		return prettyJSON(json.RawMessage(body))
	}
	var objects []json.RawMessage
	if o.ref.name != "" {
		objects = append(objects, body)
	} else {
		var list struct {
			Items []json.RawMessage `json:"items"`
		}
		if err = json.Unmarshal(body, &list); err != nil {
			return nil, err
		}
		objects = list.Items
	}
	typed := make([]apis.Object, 0, len(objects))
	for _, raw := range objects {
		object := o.ref.resource.New()
		if err = json.Unmarshal(raw, object); err != nil {
			return nil, err
		}
		typed = append(typed, object)
	}
	slices.SortFunc(typed, func(a, b apis.Object) int {
		return strings.Compare(a.Meta().Namespace+"/"+a.Meta().Name, b.Meta().Namespace+"/"+b.Meta().Name)
	})
	var b strings.Builder
	if len(typed) == 0 {
		b.WriteString("No resources found.\n")
	}
	for _, obj := range typed {
		m := obj.Meta()
		scope := m.Namespace
		if scope == "" {
			scope = "<cluster>"
		}
		fmt.Fprintf(&b, "%s %s/%s %s rv=%s\n", scope, o.ref.resource.Singular, m.Name, agent.CompactObject(obj), m.ResourceVersion)
		if o.ref.name != "" {
			fmt.Fprintf(&b, "uid=%s\n", m.UID)
		}
	}
	return []byte(b.String()), nil
}
