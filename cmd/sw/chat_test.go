package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
