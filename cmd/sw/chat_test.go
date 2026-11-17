package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestChatUsesSameToolsAndRejectsModelApproval(t *testing.T) {
	for _, provider := range []string{"ollama", "deepseek"} {
		t.Run(provider, func(t *testing.T) {
			requests, calls := 0, 0
			s := chatSession{o: chatOptions{Provider: provider, Model: "mock"}, tools: chatTools(true), approved: map[string]bool{}}
			s.call = func(name string, raw json.RawMessage) (string, error) {
				calls++
				if name != "sw_observe" {
					t.Fatalf("unauthorized call %s", name)
				}
				return `{"state":"unknown","reachability":"not_checked"}`, nil
			}
			s.transport = func(ctx context.Context, b []byte) ([]byte, error) {
				requests++
				var wire map[string]any
				if json.Unmarshal(b, &wire) != nil {
					t.Fatal("bad request")
				}
				if requests == 1 {
					args := `{}`
					if provider == "deepseek" {
						args = `"{}"`
					}
					msg := fmt.Sprintf(`{"role":"assistant","content":"","tool_calls":[{"id":"a","type":"function","function":{"name":"sw_approve","arguments":%s}},{"id":"b","type":"function","function":{"name":"sw_apply","arguments":%s}},{"id":"c","type":"function","function":{"name":"sw_observe","arguments":%s}}]}`, args, args, args)
					if provider == "ollama" {
						return []byte(`{"done":true,"message":` + msg + `}`), nil
					}
					return []byte(`{"choices":[{"finish_reason":"tool_calls","message":` + msg + `}]}`), nil
				}
				if !bytes.Contains(b, []byte("tool is not available")) || !bytes.Contains(b, []byte("requires local")) {
					t.Fatal("guard outcome not returned to model")
				}
				msg := `{"role":"assistant","content":"미확인 상태야."}`
				if provider == "ollama" {
					return []byte(`{"done":true,"message":` + msg + `}`), nil
				}
				return []byte(`{"choices":[{"finish_reason":"stop","message":` + msg + `}]}`), nil
			}
			var out bytes.Buffer
			if err := s.turn(context.Background(), "상태 확인", &out); err != nil {
				t.Fatal(err)
			}
			if calls != 1 || requests != 2 || !strings.Contains(out.String(), "미확인") {
				t.Fatal(calls, requests, out.String())
			}
		})
	}
}

func TestChatBudgetsAndTransportBoundaries(t *testing.T) {
	s := chatSession{o: chatOptions{Provider: "ollama"}, transport: func(context.Context, []byte) ([]byte, error) { t.Fatal("oversized context sent"); return nil, nil }}
	if err := s.turn(context.Background(), strings.Repeat("x", 25<<10), &bytes.Buffer{}); err == nil {
		t.Fatal("missing context bound")
	}
	for _, o := range []chatOptions{{Provider: "unknown", Model: "x"}, {Provider: "ollama", Model: "x", Endpoint: "http://external.example"}, {Provider: "ollama", Model: "x", Endpoint: "http://user:password@localhost"}, {Provider: "deepseek", Model: "x", Endpoint: "http://localhost"}} {
		if _, err := modelTransport(o); err == nil {
			t.Fatal("invalid transport accepted", o)
		}
	}
	t.Setenv("SW_TEST_NO_KEY", "")
	if _, err := modelTransport(chatOptions{Provider: "deepseek", Model: "x", Endpoint: "https://api.deepseek.com", KeyEnv: "SW_TEST_NO_KEY"}); err == nil {
		t.Fatal("missing credential accepted")
	}
	text := strings.Repeat("가", 100000) + "LATEST"
	for _, tail := range []bool{false, true} {
		got := boundedMCPText(text, tail)
		if !utf8.ValidString(got) {
			t.Fatal("UTF-8 clipped")
		}
		if tail && !strings.HasSuffix(got, "LATEST") {
			t.Fatal("latest log lost")
		}
	}
	if strings.Contains(printableChat("hello\x1b[2J\rbye"), "\x1b") {
		t.Fatal("terminal control emitted")
	}
}

func TestChatRejectedToolNameCannotControlTerminal(t *testing.T) {
	s := chatSession{o: chatOptions{Provider: "ollama", Model: "mock"}, tools: chatTools(false), approved: map[string]bool{}}
	s.call = func(string, json.RawMessage) (string, error) {
		t.Fatal("unrecognized model tool must not execute")
		return "", nil
	}
	requests := 0
	s.transport = func(context.Context, []byte) ([]byte, error) {
		requests++
		if requests == 1 {
			return []byte(`{"done":true,"message":{"role":"assistant","content":"","tool_calls":[{"function":{"name":"sw_observe\u001b]52;c;fake\u0007","arguments":{}}}]}}`), nil
		}
		return []byte(`{"done":true,"message":{"role":"assistant","content":"미확인"}}`), nil
	}
	var out bytes.Buffer
	if err := s.turn(context.Background(), "관찰", &out); err != nil {
		t.Fatal(err)
	}
	if requests != 2 || strings.ContainsAny(out.String(), "\x1b\x07") || !strings.Contains(out.String(), "tool is not available") {
		t.Fatalf("rejected tool header was unsafe: %q", out.String())
	}
}

func TestChatRetriesOneEmptyResponseBeforeAnyToolOnly(t *testing.T) {
	for _, repeated := range []bool{false, true} {
		t.Run(fmt.Sprint(repeated), func(t *testing.T) {
			w := &workflowSession{}
			s := chatSession{o: chatOptions{Provider: "ollama", Model: "fake", mcpOptions: mcpOptions{Workflow: w}}, tools: workflowTools(false)}
			requests, calls := 0, 0
			s.call = func(name string, args json.RawMessage) (string, error) {
				calls++
				if name != "sw_workflow" || string(args) != `{"action":"verify"}` {
					t.Fatal("repair invented a different action", name, string(args))
				}
				w.answer = json.RawMessage(`{"state":"healthy","proofID":"observed"}`)
				return string(w.answer), nil
			}
			s.transport = func(_ context.Context, request []byte) ([]byte, error) {
				requests++
				if requests == 2 && !bytes.Contains(request, []byte("neither an answer nor a native tool call")) {
					t.Fatal("missing bounded response repair")
				}
				if requests == 2 && !repeated {
					return []byte(`{"done":true,"message":{"role":"assistant","content":"","tool_calls":[{"function":{"name":"sw_workflow","arguments":{"action":"verify"}}}]}}`), nil
				}
				return []byte(`{"done":true,"message":{"role":"assistant","content":""}}`), nil
			}
			err := s.turn(context.Background(), "실제 접속 확인", &bytes.Buffer{})
			if repeated {
				if err == nil || requests != 2 || calls != 0 {
					t.Fatal("repeated empty responses were accepted or looped", err, requests, calls)
				}
			} else if err != nil || requests != 3 || calls != 1 {
				t.Fatal("tool evidence was lost or a tool repeated", err, requests, calls)
			}
		})
	}
}

func TestChatCompletionRequiresAppliedHTTPProofAndStopsQueuedCalls(t *testing.T) {
	plan := strings.Repeat("a", 64)
	base := map[string]any{"format": "shearwater.workflow-result/v1", "state": "healthy", "uid": "selected-app", "goalGeneration": 2, "verifiedImage": "example/app:checked", "http": map[string]any{"state": "passed"}}
	for _, delegated := range []bool{false, true} {
		t.Run(fmt.Sprint(delegated), func(t *testing.T) {
			good := maps.Clone(base)
			if delegated {
				good["operation"], good["plan"], good["lastPlan"] = strings.Repeat("b", 32), plan, plan
				good["proofID"], good["verifiedPods"], good["applied"], good["approved"] = strings.Repeat("c", 64), 1, true, true
			}
			raw, _ := json.Marshal(good)
			if !workflowAppliedAndHealthy([]byte(`{"action":"continue"}`), raw) || workflowAppliedAndHealthy([]byte(`{"action":"verify"}`), raw) {
				t.Fatal("a preflight observation was confused with completed application")
			}
			bad := []map[string]any{{"state": "approved_pending"}, {"stagedImage": "example/next"}, {"uid": ""}, {"goalGeneration": 0}, {"http": map[string]any{"state": "failed"}}}
			if delegated {
				bad = append(bad, map[string]any{"applied": false}, map[string]any{"approved": false}, map[string]any{"lastPlan": strings.Repeat("d", 64)}, map[string]any{"proofID": "invented"}, map[string]any{"verifiedPods": 0})
			}
			for _, fields := range bad {
				v := maps.Clone(good)
				maps.Copy(v, fields)
				b, _ := json.Marshal(v)
				if workflowAppliedAndHealthy([]byte(`{"action":"continue"}`), b) {
					t.Fatal("incomplete or different goal was accepted", fields)
				}
			}
			w := &workflowSession{}
			s := chatSession{o: chatOptions{Provider: "ollama", Model: "fake", mcpOptions: mcpOptions{Workflow: w}}, tools: workflowTools(true)}
			requests, calls := 0, 0
			s.call = func(_ string, args json.RawMessage) (string, error) {
				calls++
				if string(args) != `{"action":"continue"}` {
					t.Fatal("another mutation executed after completion", string(args))
				}
				w.answer = raw
				return string(raw), nil
			}
			s.transport = func(context.Context, []byte) ([]byte, error) {
				requests++
				return []byte(`{"done":true,"message":{"role":"assistant","content":"","tool_calls":[{"function":{"name":"sw_workflow","arguments":{"action":"continue"}}},{"function":{"name":"sw_workflow","arguments":{"action":"update","image":"unexpected"}}}]}}`), nil
			}
			var out bytes.Buffer
			if e := s.turn(context.Background(), "승인한 계획 적용과 접속 확인", &out); e != nil {
				t.Fatal(e)
			}
			if requests != 1 || calls != 1 || !strings.Contains(out.String(), string(raw)) || strings.Contains(s.messages[len(s.messages)-1].Content, "unexpected") {
				t.Fatal("model/calls continued after authoritative completion", requests, calls)
			}
		})
	}
}
