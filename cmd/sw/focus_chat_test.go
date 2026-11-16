package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"shearwater/internal/agent"
)

func TestFocusedChatCatalogAndHiddenTools(t *testing.T) {
	for _, provider := range []string{"ollama", "deepseek"} {
		for _, task := range []string{"inspect", "image", "scale"} {
			t.Run(provider+"/"+task, func(t *testing.T) {
				o := mcpOptions{Write: true, Focus: &focusSession{Target: "deploy/demo", Namespace: "default", Task: task, UID: "original", Generation: 7}}
				s := chatSession{o: chatOptions{mcpOptions: o, Provider: provider, Model: "fake"}, tools: chatToolsFor(o)}
				s.reset()
				if strings.Contains(s.messages[0].Content, "sw_observe") {
					t.Fatal("focused instructions mandate an unavailable global tool")
				}
				calls, requests := 0, 0
				s.call = func(name string, raw json.RawMessage) (string, error) {
					calls++
					if name != "sw_target_status" || string(raw) != "{}" {
						t.Fatalf("hidden tool executed: %s %s", name, raw)
					}
					return `{"state":"unknown"}`, nil
				}
				s.transport = func(_ context.Context, body []byte) ([]byte, error) {
					requests++
					var wire struct {
						Tools []struct {
							Function struct {
								Name string `json:"name"`
							} `json:"function"`
						} `json:"tools"`
					}
					if err := json.Unmarshal(body, &wire); err != nil {
						t.Fatal(err)
					}
					want := map[string]bool{"sw_target_status": true}
					if task != "inspect" {
						want["sw_target_"+task] = true
					}
					got := map[string]bool{}
					for _, tool := range wire.Tools {
						got[tool.Function.Name] = true
					}
					if !reflect.DeepEqual(got, want) {
						t.Fatalf("catalog = %v, want %v", got, want)
					}
					args := `{}`
					if provider == "deepseek" {
						args = `"{}"`
					}
					msg := `{"role":"assistant","content":"검증 결과는 미확인이야."}`
					if requests == 1 {
						msg = fmt.Sprintf(`{"role":"assistant","content":"","tool_calls":[{"id":"a","type":"function","function":{"name":"sw_approve","arguments":%s}},{"id":"b","type":"function","function":{"name":"sw_apply","arguments":%s}},{"id":"c","type":"function","function":{"name":"sw_app_plan","arguments":%s}},{"id":"d","type":"function","function":{"name":"sw_target_status","arguments":%s}}]}`, args, args, args, args)
					} else if bytes.Count(body, []byte("tool is not available")) != 3 {
						t.Fatal("hidden approval, apply and raw/global tools were not rejected")
					}
					if provider == "ollama" {
						return []byte(`{"done":true,"message":` + msg + `}`), nil
					}
					return []byte(`{"choices":[{"finish_reason":"stop","message":` + msg + `}]}`), nil
				}
				if err := s.turn(context.Background(), "확인", &bytes.Buffer{}); err != nil {
					t.Fatal(err)
				}
				if requests != 2 || calls != 1 || len(s.approved) != 0 {
					t.Fatalf("requests=%d calls=%d approvals=%v", requests, calls, s.approved)
				}
			})
		}
	}
}

func TestFocusedChatPendingPlanAndResetKeepsPinnedScope(t *testing.T) {
	for _, task := range []string{"image", "scale"} {
		t.Run(task, func(t *testing.T) {
			id := strings.Repeat("a", 64)
			foreign := strings.Repeat("b", 64)
			focus := &focusSession{Target: "deploy/demo", Namespace: "test", Task: task, Container: "web", UID: "original", Generation: 7, plans: map[string]bool{}}
			focus.goal = agent.ReadinessGoal{Namespace: "test", Resource: "deploy/demo", UID: "original", Generation: 7}
			o := mcpOptions{Write: true, Focus: focus}
			s := chatSession{o: chatOptions{mcpOptions: o, Provider: "ollama", Model: "fake"}, tools: chatToolsFor(o)}
			s.reset()
			s.call = func(name string, _ json.RawMessage) (string, error) {
				if name != "sw_target_"+task {
					t.Fatalf("unexpected tool %s", name)
				}
				focus.plans[id] = true
				return `{"plan":"` + id + `"}`, nil
			}
			requests := 0
			s.transport = func(context.Context, []byte) ([]byte, error) {
				requests++
				if requests == 1 {
					args := `{"image":"example:v2"}`
					if task == "scale" {
						args = `{"replicas":2}`
					}
					return []byte(fmt.Sprintf(`{"done":true,"message":{"role":"assistant","content":"","tool_calls":[{"function":{"name":%q,"arguments":%s}}]}}`, "sw_target_"+task, args)), nil
				}
				return []byte(`{"done":true,"message":{"role":"assistant","content":"/review로 계획을 확인해."}}`), nil
			}
			if err := s.turn(context.Background(), "계획", &bytes.Buffer{}); err != nil {
				t.Fatal(err)
			}
			if s.pendingPlan != id || !s.reviewablePlan(id) || s.reviewablePlan(foreign) {
				t.Fatal("pending/review accepted an unknown plan or lost the issued plan")
			}
			before := *focus
			s.approved[id] = true
			s.reset()
			if s.o.Focus != focus || !reflect.DeepEqual(before, *focus) || !s.reviewablePlan(id) {
				t.Fatal("reset changed the pinned scope, goal or issued plans")
			}
			if s.pendingPlan != "" || len(s.approved) != 0 || len(s.messages) != 1 || s.messages[0].Role != "system" {
				t.Fatal("reset retained conversation/approval state")
			}
		})
	}
}

func TestFocusedChatClassicCatalogUnchanged(t *testing.T) {
	for _, write := range []bool{false, true} {
		if !reflect.DeepEqual(chatToolsFor(mcpOptions{Write: write}), chatTools(write)) {
			t.Fatal("classic catalog changed")
		}
	}
}
