package main

// Chat is a client, never part of the root server. The model proposes tool calls;
// the same dispatcher used by MCP validates and executes them. It has no shell,
// credential-reading tool or approval tool. Conversation memory lasts one process.
import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"golang.org/x/sys/unix"
)

type chatCall struct {
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}
type chatMessage struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	Calls      []chatCall `json:"tool_calls,omitempty"`
	ToolName   string     `json:"tool_name,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}
type chatOptions struct {
	mcpOptions
	Provider, Model, Endpoint, KeyEnv string
	ModelStdio                        bool
}
type chatTransport func(context.Context, []byte) ([]byte, error)
type chatSession struct {
	o           chatOptions
	tools       []mcpTool
	messages    []chatMessage
	transport   chatTransport
	call        func(string, json.RawMessage) (string, error)
	approved    map[string]bool
	pendingPlan string
}

// Use a small operation vocabulary. Advanced raw manifests remain available in
// MCP/CLI; a first-use chat should not have to invent a whole Kubernetes object.
func chatTools(write bool) []mcpTool {
	var selected []mcpTool
	for _, t := range mcpTools(write) {
		switch t.Name {
		case "sw_capabilities", "sw_observe", "sw_verify_plan", "sw_why", "sw_get", "sw_logs", "sw_events", "sw_app_plan", "sw_apply", "sw_receipt":
			selected = append(selected, t)
		}
	}
	return selected
}

func chatToolsFor(o mcpOptions) []mcpTool {
	if o.Focus != nil {
		return availableMCPTools(o)
	}
	return chatTools(o.Write)
}

func (s *chatSession) reset() {
	instructions := instructionsFor(s.o.mcpOptions) + " Speak Korean."
	if s.o.Focus == nil {
		instructions += " Use sw_observe for current facts before making a diagnosis."
	}
	instructions += " Never claim a tool was called unless its response is in the conversation. Built-in approval commands can only be entered by the human, not emitted as a tool call."
	s.messages = []chatMessage{{Role: "system", Content: instructions}}
	s.approved = map[string]bool{}
	s.pendingPlan = ""
	// 대화만 지운다. 처음 고정한 대상·목표와 이 세션에서 만든 계획 목록은 유지한다.
}

func (s *chatSession) reviewablePlan(id string) bool {
	return validPlanID(id) && (s.o.Focus == nil || focusPlanKnown(s.o.mcpOptions, id))
}

func printableChat(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return ' '
		}
		return r
	}, s)
}

func (s *chatSession) turn(ctx context.Context, text string, out io.Writer) error {
	s.messages = append(s.messages, chatMessage{Role: "user", Content: text})
	for step := 0; step < 8; step++ {
		var definitions []any
		for _, t := range s.tools {
			definitions = append(definitions, map[string]any{"type": "function", "function": map[string]any{"name": t.Name, "description": t.Description, "parameters": t.InputSchema}})
		}
		// Ollama uses object arguments; DeepSeek uses JSON encoded as a string.
		// Normalize only at this boundary, keeping the core tool contract identical.
		wire := make([]chatMessage, len(s.messages))
		copy(wire, s.messages)
		if s.o.Provider == "deepseek" {
			for j := range wire {
				wire[j].ToolName = ""
				wire[j].Calls = append([]chatCall(nil), wire[j].Calls...)
				for k := range wire[j].Calls {
					b, _ := json.Marshal(string(wire[j].Calls[k].Function.Arguments))
					wire[j].Calls[k].Function.Arguments = b
				}
			}
		}
		request := map[string]any{"model": s.o.Model, "messages": wire, "tools": definitions, "stream": false}
		if s.o.Provider == "ollama" {
			request["options"] = map[string]any{"num_ctx": 8192, "num_predict": 1024, "temperature": 0}
			request["keep_alive"] = "0"
		} else {
			request["max_tokens"] = 1024
			request["thinking"] = map[string]any{"type": "disabled"}
		}
		b, err := json.Marshal(request)
		if err != nil {
			return err
		}
		// A byte budget is not a token measurement. Stop explicitly rather than
		// silently losing the goal, approvals or the evidence behind an action.
		if len(b) > 24<<10 {
			return fmt.Errorf("chat context exceeded 24 KiB; use /reset and a narrower task")
		}
		fmt.Fprintf(out, "[Shearwater] %s 응답을 기다리는 중 (%d/8)\n", s.o.Provider, step+1)
		body, err := s.transport(ctx, b)
		if err != nil {
			return err
		}
		var msg chatMessage
		if s.o.Provider == "ollama" {
			var r struct {
				Message chatMessage `json:"message"`
				Done    bool        `json:"done"`
				Error   string      `json:"error"`
			}
			if err = json.Unmarshal(body, &r); err != nil {
				return err
			}
			if r.Error != "" || !r.Done {
				return fmt.Errorf("model returned an incomplete or failed response")
			}
			msg = r.Message
		} else {
			var r struct {
				Choices []struct {
					Message      chatMessage `json:"message"`
					FinishReason string      `json:"finish_reason"`
				} `json:"choices"`
			}
			if err = json.Unmarshal(body, &r); err != nil {
				return err
			}
			if len(r.Choices) != 1 || r.Choices[0].FinishReason == "length" {
				return fmt.Errorf("model returned no complete choice")
			}
			msg = r.Choices[0].Message
			for k := range msg.Calls {
				var encoded string
				if json.Unmarshal(msg.Calls[k].Function.Arguments, &encoded) != nil {
					return fmt.Errorf("invalid model tool arguments")
				}
				msg.Calls[k].Function.Arguments = json.RawMessage(encoded)
			}
		}
		if msg.Role != "assistant" || len(msg.Calls) > 4 {
			return fmt.Errorf("unexpected model role or too many tool calls")
		}
		if msg.Content != "" {
			fmt.Fprintln(out, "[모델 답변 · 사실 여부는 아래 도구 결과로 확인]\n"+printableChat(msg.Content))
		}
		s.messages = append(s.messages, msg)
		if len(msg.Calls) == 0 {
			if msg.Content == "" {
				return fmt.Errorf("model returned an empty answer")
			}
			return nil
		}
		for _, call := range msg.Calls {
			allowed := false
			for _, t := range s.tools {
				if t.Name == call.Function.Name {
					allowed = true
				}
			}
			var result string
			var callErr error
			if !allowed {
				callErr = fmt.Errorf("tool is not available in this chat")
			} else if call.Function.Name == "sw_apply" {
				var a struct {
					Plan string `json:"plan"`
				}
				if decodeStrict(call.Function.Arguments, &a) != nil || !s.approved[a.Plan] {
					callErr = fmt.Errorf("this chat requires local /review HASH then /approve HASH before applying")
				}
			}
			if callErr == nil {
				result, callErr = s.call(call.Function.Name, call.Function.Arguments)
			}
			if callErr == nil && (call.Function.Name == "sw_app_plan" || s.o.Focus != nil && (call.Function.Name == "sw_target_image" || call.Function.Name == "sw_target_scale")) {
				var saved struct {
					Plan string `json:"plan"`
				}
				if json.Unmarshal([]byte(result), &saved) == nil && s.reviewablePlan(saved.Plan) {
					s.pendingPlan = saved.Plan
				}
			}
			if callErr != nil {
				result = "Tool error: " + callErr.Error() + "\n" + result
			}
			// Do not silently truncate JSON/plan details. The model can narrow its
			// next read; a human can review the complete saved plan via /review.
			if len(result) > 12<<10 {
				result = "Tool result exceeds chat budget. Narrow the read, or review the saved plan with sw plan --plan=HASH. Do not infer success from missing output."
			}
			// Rejected tool names still come from the model. Sanitize the header
			// too, so an invalid name cannot control the terminal or clipboard.
			fmt.Fprintf(out, "[Shearwater 도구 %s]\n%s\n", printableChat(call.Function.Name), printableChat(result))
			s.messages = append(s.messages, chatMessage{Role: "tool", Content: result, ToolName: call.Function.Name, ToolCallID: call.ID})
		}
	}
	return fmt.Errorf("model reached 8 steps; stopped without assuming success")
}

func modelTransport(o chatOptions) (chatTransport, error) {
	if o.Provider != "ollama" && o.Provider != "deepseek" {
		return nil, fmt.Errorf("provider must be ollama or deepseek")
	}
	if o.Model == "" {
		return nil, fmt.Errorf("pass --model with an installed Ollama model or a DeepSeek API model")
	}
	u, err := url.Parse(o.Endpoint)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" && u.Path != "/" {
		return nil, fmt.Errorf("endpoint must be an origin URL without credentials, path or query")
	}
	host := u.Hostname()
	ip := net.ParseIP(host)
	if u.Scheme != "https" && (u.Scheme != "http" || o.Provider != "ollama" || !(host == "localhost" || ip != nil && ip.IsLoopback())) {
		return nil, fmt.Errorf("use HTTPS; plain HTTP is allowed only for loopback Ollama")
	}
	key := ""
	if o.Provider == "deepseek" {
		key = os.Getenv(o.KeyEnv)
		if key == "" {
			return nil, fmt.Errorf("set %s in this process for DeepSeek; existing chat logins are not API credentials", o.KeyEnv)
		}
	}
	endpoint := strings.TrimRight(o.Endpoint, "/") + "/api/chat"
	if o.Provider == "deepseek" {
		endpoint = strings.TrimRight(o.Endpoint, "/") + "/chat/completions"
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	client := &http.Client{Transport: transport, Timeout: 90 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return fmt.Errorf("model redirects are disabled") }}
	return func(ctx context.Context, b []byte) ([]byte, error) {
		req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(b))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("model connection failed; check endpoint and model availability")
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("model HTTP status %d; check model name and credentials", resp.StatusCode)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, (256<<10)+1))
		if err != nil {
			return nil, err
		}
		if len(body) > 256<<10 {
			return nil, fmt.Errorf("model response exceeds 256 KiB")
		}
		return body, nil
	}, nil
}

// The Windows launcher owns the local model HTTP connection and starts sw in
// WSL. Every stdout byte is framed JSON, including model/log text. Treating a
// magic line in plain text as a request would allow a log to forge a request.
type chatFrameWriter struct{ out io.Writer }

func (w chatFrameWriter) Write(p []byte) (int, error) {
	err := json.NewEncoder(w.out).Encode(map[string]any{"type": "output", "text": string(p)})
	if err != nil {
		return 0, err
	}
	return len(p), nil
}

func stdioModelTransport(in io.Reader, out io.Writer) chatTransport {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 4096), 256<<10)
	return func(ctx context.Context, b []byte) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := json.NewEncoder(out).Encode(map[string]any{"type": "model_request", "body": json.RawMessage(b)}); err != nil {
			return nil, err
		}
		if !scanner.Scan() {
			return nil, fmt.Errorf("model bridge closed or exceeded response limit")
		}
		body := append([]byte(nil), scanner.Bytes()...)
		if !json.Valid(body) {
			return nil, fmt.Errorf("invalid model bridge JSON")
		}
		return body, nil
	}
}

func runChat(args []string, in *os.File, out io.Writer) error {
	f := flag.NewFlagSet("chat", flag.ContinueOnError)
	o := chatOptions{}
	f.StringVar(&o.Config, "config-dir", defaultConfigDir, "user-owned delegated server credentials")
	f.StringVar(&o.Listen, "listen", defaultListen, "local HTTPS API")
	f.StringVar(&o.Plans, "plan-dir", "", "private plan directory")
	f.BoolVar(&o.Write, "allow-write", false, "allow planning and locally approved apply")
	addFocusFlags(f, &o.mcpOptions)
	f.StringVar(&o.Provider, "provider", "ollama", "ollama (Qwen/local DeepSeek) or deepseek (API)")
	f.StringVar(&o.Model, "model", "", "explicit model name")
	f.StringVar(&o.Endpoint, "endpoint", "", "model origin URL")
	f.StringVar(&o.KeyEnv, "key-env", "DEEPSEEK_API_KEY", "environment variable holding DeepSeek API key")
	f.BoolVar(&o.ModelStdio, "model-stdio", false, "framed local model transport for the Windows launcher (requires --prompt)")
	prompt := f.String("prompt", "", "one read/plan conversation and exit; approvals require an interactive terminal")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return fmt.Errorf("unexpected arguments")
	}
	if os.Geteuid() == 0 {
		return fmt.Errorf("run chat as an ordinary user with delegated config; the model loop must not run as root")
	}
	if err := localListen(o.Listen); err != nil {
		return err
	}
	if o.Endpoint == "" {
		o.Endpoint = "http://127.0.0.1:11434"
		if o.Provider == "deepseek" {
			o.Endpoint = "https://api.deepseek.com"
		}
	}
	if o.Plans == "" {
		o.Plans = filepath.Join(o.Config, "plans")
	}
	// 모델에 첫 요청을 보내기 전에 대상과 목표를 한 번만 고정한다.
	focusCtx, cancelFocus := context.WithTimeout(context.Background(), inspectTimeout)
	focusErr := initFocus(focusCtx, &o.mcpOptions)
	cancelFocus()
	if focusErr != nil {
		return focusErr
	}
	var transport chatTransport
	var err error
	if o.ModelStdio {
		if *prompt == "" || o.Provider != "ollama" || o.Model == "" || o.Endpoint != "http://127.0.0.1:11434" {
			return fmt.Errorf("model-stdio requires a one-shot prompt and local Ollama model")
		}
		transport = stdioModelTransport(in, out)
		out = chatFrameWriter{out}
	} else {
		transport, err = modelTransport(o)
		if err != nil {
			return err
		}
	}
	s := chatSession{o: o, tools: chatToolsFor(o.mcpOptions), transport: transport, approved: map[string]bool{}}
	s.call = func(name string, raw json.RawMessage) (string, error) { return mcpCall(o.mcpOptions, name, raw) }
	s.reset()
	fmt.Fprintf(out, "[Shearwater] %s / %s. 질문과 선택한 도구 결과가 이 모델로 전달돼. 대화는 파일에 저장하지 않아.\n", o.Provider, printableChat(o.Model))
	if o.Focus != nil {
		fmt.Fprintln(out, "[Shearwater 집중 모드] "+printableChat(focusLabel(o.mcpOptions))+". 새 목표는 새 세션에서 시작해.")
	}
	if *prompt != "" {
		return s.turn(context.Background(), *prompt, out)
	}
	fmt.Fprintln(out, "/reset 새 대화 · /review 최근 계획 원문(또는 /review HASH) · /approve 검토한 계획 승인 · /quit 종료")
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 4096), 16<<10)
	reviewed := ""
	for {
		fmt.Fprint(out, "> ")
		if !scanner.Scan() {
			return scanner.Err()
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		switch {
		case line == "/quit":
			return nil
		case line == "/reset":
			s.reset()
			reviewed = ""
			continue
		case line == "/review" || strings.HasPrefix(line, "/review "):
			id := s.pendingPlan
			if line != "/review" {
				id = strings.TrimSpace(strings.TrimPrefix(line, "/review "))
			}
			reviewed = ""
			if !validPlanID(id) {
				err = fmt.Errorf("no plan selected; first create a plan or use /review HASH")
				break
			}
			if !s.reviewablePlan(id) {
				err = fmt.Errorf("focused chat can review only plans created in this session")
				break
			}
			err = runPlanCommand("plan", []string{"--config-dir=" + o.Config, "--listen=" + o.Listen, "--plan-dir=" + o.Plans, "--plan=" + id}, out)
			if err == nil {
				reviewed = id
				fmt.Fprintln(out, "[Shearwater] 검토 대상: "+id+". 원문을 확인했다면 /approve 입력. 여러 객체는 한 번에 취소되는 트랜잭션이 아니야.")
			}
		case line == "/approve" || strings.HasPrefix(line, "/approve "):
			id := reviewed
			if line != "/approve" {
				id = strings.TrimSpace(strings.TrimPrefix(line, "/approve "))
			}
			_, ttyErr := unix.IoctlGetTermios(int(in.Fd()), unix.TCGETS)
			if !o.Write || ttyErr != nil || id != reviewed || !s.reviewablePlan(id) {
				err = fmt.Errorf("interactive write mode and /review of this exact plan are required")
				break
			}
			err = runPlanCommand("approve", []string{"--config-dir=" + o.Config, "--listen=" + o.Listen, "--plan-dir=" + o.Plans, "--plan=" + id, "--confirm=" + id}, out)
			if err == nil {
				reviewed = ""
				if o.Focus != nil {
					// 사람의 정확한 계획 승인을 CLI로 직접 실행한다. 모델에는 적용 도구를 열지 않는다.
					// apply가 실제 쓰기 영수증으로 verifyAppliedPlan을 호출하고 검증 JSON을 출력한다.
					s.pendingPlan = ""
					err = runPlanCommand("apply", []string{"--config-dir=" + o.Config, "--listen=" + o.Listen, "--plan-dir=" + o.Plans, "--plan=" + id}, out)
				} else {
					s.approved[id] = true
					err = s.turn(context.Background(), "The user locally reviewed and approved plan "+id+". Apply it once, then observe its targets. Report actual readiness separately from execution.", out)
				}
			}
		default:
			reviewed = "" // a new conversation turn may propose a different change
			err = s.turn(context.Background(), line, out)
		}
		if err != nil {
			fmt.Fprintln(out, "[Shearwater 오류] "+printableChat(err.Error()))
			err = nil
		}
	}
}
