package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// 실제 API 결과를 읽은 뒤 모델이 마지막 문장만 틀리는 경우를 재현한다.
// 출력은 모델의 자유 문장이 아니라 이번 질문의 성공한 운영 함수에서 나와야 한다.
func TestFocusedNativeAnswerDoesNotRetranscribeModelMistakes(t *testing.T) {
	for _, provider := range []string{"ollama", "deepseek"} {
		for _, task := range []string{"inspect", "image"} {
			t.Run(provider+"/"+task, func(t *testing.T) {
				o, _, obj := focusFixture(t, task)
				if task == "inspect" {
					o.Focus.goal.UID = "original-replaced-object"
				}
				s := chatSession{o: chatOptions{mcpOptions: o, Provider: provider, Model: "scripted"}, tools: chatToolsFor(o)}
				s.reset()
				s.call = func(name string, raw json.RawMessage) (string, error) { return mcpCall(o, name, raw) }
				requests := 0
				s.transport = func(context.Context, []byte) ([]byte, error) {
					requests++
					msg := map[string]any{"role": "assistant", "content": ""}
					if requests == 1 {
						name, args := "sw_target_status", map[string]any{}
						if task == "image" {
							name, args = "sw_target_image", map[string]any{"image": "nginx:stable"}
						}
						var wireArgs any = args
						if provider == "deepseek" {
							b, _ := json.Marshal(args)
							wireArgs = string(b)
						}
						msg["tool_calls"] = []any{map[string]any{"id": "read", "type": "function", "function": map[string]any{"name": name, "arguments": wireArgs}}}
					} else if task == "inspect" {
						msg["content"] = `{"state":"stale","uid":"original-replaced-object","generation":1,"http":"not_checked"}`
					} else {
						var p struct {
							Plan string `json:"plan"`
						}
						json.Unmarshal(o.Focus.answer, &p)
						msg["content"] = p.Plan // 실패했던 딥시크 답변처럼 해시만 내놓는다.
					}
					var response any = map[string]any{"done": true, "message": msg}
					if provider == "deepseek" {
						response = map[string]any{"choices": []any{map[string]any{"finish_reason": "stop", "message": msg}}}
					}
					return json.Marshal(response)
				}
				var out bytes.Buffer
				if err := s.turn(context.Background(), "고정한 작업 수행", &out); err != nil {
					t.Fatal(err)
				}
				parts := strings.Split(out.String(), "[Shearwater 확인 결과]\n")
				if len(parts) != 2 {
					t.Fatal(out.String())
				}
				var answer map[string]any
				if json.Unmarshal([]byte(parts[1]), &answer) != nil {
					t.Fatal("not complete JSON", parts[1])
				}
				if task == "inspect" {
					if answer["state"] != "stale" || answer["uid"] != obj["metadata"].(map[string]any)["uid"] || strings.Contains(out.String(), "original-replaced-object") {
						t.Fatal(out.String())
					}
					if len(answer) != 4 {
						t.Fatal(answer)
					}
				} else if len(answer) != 2 || answer["applied"] != false || !focusPlanKnown(o, answer["plan"].(string)) {
					t.Fatal(answer)
				}
				if strings.TrimSpace(parts[1]) != s.messages[len(s.messages)-1].Content {
					t.Fatal("unverified model text remained in history")
				}
				if requests != 2 {
					t.Fatal("unexpected repair/retry", requests)
				}
				// 다음 질문에서 모델만 답하면 앞 질문의 성공을 재사용하면 안 된다.
				s.transport = func(context.Context, []byte) ([]byte, error) {
					if provider == "ollama" {
						return []byte(`{"done":true,"message":{"role":"assistant","content":"met"}}`), nil
					}
					return []byte(`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"met"}}]}`), nil
				}
				out.Reset()
				if err := s.turn(context.Background(), "다시 확인", &out); err == nil || len(o.Focus.answer) != 0 || strings.Contains(out.String(), "[Shearwater 확인 결과]") {
					t.Fatal("old answer reused", err, out.String())
				}
			})
		}
	}
}

func TestFocusedErrorsInvalidatePriorAnswer(t *testing.T) {
	o, _, _ := focusFixture(t, "inspect")
	for _, bad := range []struct{ name, raw string }{{"sw_apply", `{}`}, {"sw_target_status", `{"namespace":"elsewhere"}`}} {
		focusRead(t, o)
		if len(o.Focus.answer) == 0 {
			t.Fatal("missing fixture answer")
		}
		if _, err := mcpCall(o, bad.name, json.RawMessage(bad.raw)); err == nil || len(o.Focus.answer) != 0 {
			t.Fatal("rejected call retained answer", bad)
		}
	}
	// 성공한 조회 직후 마지막 도구가 실패하면 그 조회로 최종 성공을 표시하지 않는다.
	s := chatSession{o: chatOptions{mcpOptions: o, Provider: "ollama", Model: "scripted"}, tools: chatToolsFor(o)}
	s.reset()
	s.call = func(name string, raw json.RawMessage) (string, error) { return mcpCall(o, name, raw) }
	step := 0
	s.transport = func(context.Context, []byte) ([]byte, error) {
		step++
		if step == 1 {
			return []byte(`{"done":true,"message":{"role":"assistant","tool_calls":[{"function":{"name":"sw_target_status","arguments":{}}},{"function":{"name":"sw_target_status","arguments":{"namespace":"elsewhere"}}}]}}`), nil
		}
		return []byte(`{"done":true,"message":{"role":"assistant","content":"all good"}}`), nil
	}
	var out bytes.Buffer
	if err := s.turn(context.Background(), "확인", &out); err == nil {
		t.Fatal(fmt.Sprint("failed last call accepted", out.String()))
	}
	if strings.Contains(out.String(), "[Shearwater 확인 결과]") {
		t.Fatal(out.String())
	}
}
