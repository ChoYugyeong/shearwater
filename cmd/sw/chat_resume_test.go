package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestChatResumeRequiresExactExistingApprovalAndIdentity(t *testing.T) {
	for _, scenario := range []string{"valid", "missing", "different approval", "different server", "expired", "tampered"} {
		t.Run(scenario, func(t *testing.T) {
			c, root, _ := planTestServer(t)
			p, err := prepareAppPlan(context.Background(), c, root, "127.0.0.1:6443", appIntent{Action: "create", Name: "resume", Image: "nginx:alpine", Port: 80})
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "expired" {
				p.Expires = time.Now().Add(-time.Minute)
			}
			id := planHash(p)
			if scenario == "tampered" {
				p.Actions[0].Path += "/another"
			}
			if err := exclusiveJSON(filepath.Join(root, id+".json"), p); err != nil {
				t.Fatal(err)
			}
			if scenario != "missing" {
				approved := id
				if scenario == "different approval" {
					approved = strings.Repeat("a", 64)
				}
				if err := exclusiveJSON(filepath.Join(root, id+".approved.json"), map[string]string{"plan": approved}); err != nil {
					t.Fatal(err)
				}
			}
			o := chatOptions{mcpOptions: mcpOptions{Config: root, Plans: root, Listen: "127.0.0.1:6443", Write: true}, ApprovedPlan: id}
			if scenario == "different server" {
				o.Listen = "127.0.0.1:6444"
			}
			err = checkChatPlanApproval(o, c)
			if (err == nil) != (scenario == "valid") {
				t.Fatalf("%s: %v", scenario, err)
			}
			if _, err := os.Stat(filepath.Join(root, id+".receipt.json")); !os.IsNotExist(err) {
				t.Fatal("preflight executed a plan")
			}
		})
	}
}

func TestChatResumeRejectsInvalidModesAndKeepsOrdinaryOneShotReadPlanOnly(t *testing.T) {
	id := strings.Repeat("a", 64)
	valid := chatOptions{mcpOptions: mcpOptions{Write: true}, ApprovedPlan: id}
	if err := validateChatResume(valid, true); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*chatOptions){func(o *chatOptions) { o.Write = false }, func(o *chatOptions) { o.ApprovedPlan = "not-a-hash" }, func(o *chatOptions) { o.Focus = &focusSession{} }} {
		o := valid
		change(&o)
		if validateChatResume(o, true) == nil {
			t.Fatal("invalid resume accepted")
		}
	}
	if validateChatResume(valid, false) == nil {
		t.Fatal("interactive resume must not regrant after reset")
	}
	s := chatSession{o: chatOptions{}, tools: chatTools(true)}
	if err := s.configureExecution(true); err != nil {
		t.Fatal(err)
	}
	for _, tool := range s.tools {
		if tool.Name == "sw_apply" {
			t.Fatal("unusable apply advertised")
		}
	}
}

func TestResumedChatCannotSwitchPlanOrRetryUncertainApply(t *testing.T) {
	id, other := strings.Repeat("a", 64), strings.Repeat("b", 64)
	s := chatSession{o: chatOptions{ApprovedPlan: id, Provider: "ollama"}, tools: resumedChatTools(id), approved: map[string]bool{id: true}}
	calls := []string{}
	s.call = func(name string, raw json.RawMessage) (string, error) {
		calls = append(calls, name)
		return `{"state":"unknown"}`, nil
	}
	step := 0
	s.transport = func(context.Context, []byte) ([]byte, error) {
		step++
		if step == 1 {
			return []byte(`{"done":true,"message":{"role":"assistant","content":"","tool_calls":[{"function":{"name":"sw_apply","arguments":{"plan":"` + other + `"}}},{"function":{"name":"sw_apply","arguments":{"plan":"` + id + `"}}},{"function":{"name":"sw_apply","arguments":{"plan":"` + id + `"}}},{"function":{"name":"sw_verify_plan","arguments":{"plan":"` + id + `"}}}]}}`), nil
		}
		return []byte(`{"done":true,"message":{"role":"assistant","content":"실행 결과는 아직 미확인이야."}}`), nil
	}
	var out bytes.Buffer
	if err := s.turn(context.Background(), "apply", &out); err != nil {
		t.Fatal(err)
	}
	if strings.Join(calls, ",") != "sw_apply,sw_verify_plan" {
		t.Fatal(calls)
	}
	if !strings.Contains(out.String(), "operator-selected") || !strings.Contains(out.String(), "already attempted") {
		t.Fatal(out.String())
	}
	s.reset()
	if err := s.checkResumedCall("sw_apply", json.RawMessage(`{"plan":"`+id+`"}`)); err == nil {
		t.Fatal("reset restored approval")
	}
}
