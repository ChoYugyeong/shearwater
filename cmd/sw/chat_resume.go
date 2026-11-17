package main

// Windows의 한 질문 대화에는 /review·/approve를 입력할 터미널이 없다.
// 외부에서 검토·승인한 해시 하나를 실행 인자로 선택하면 그 계획만 이어간다.
// 이 옵션은 승인을 만들지 않는다. 선택과 승인 파일, 만료와 API 신원을 모두
// 확인하고 실제 쓰기 직전 executePlan이 승인·동시 변경 조건을 다시 검사한다.
import (
	"encoding/json"
	"fmt"
	"path/filepath"
)

func validateChatResume(o chatOptions, oneShot bool) error {
	if o.ApprovedPlan == "" {
		return nil
	}
	if !validPlanID(o.ApprovedPlan) || !o.Write || !oneShot || o.Focus != nil || o.Workflow != nil {
		return fmt.Errorf("approved-plan requires an exact plan hash, --allow-write and --prompt; do not combine it with --target")
	}
	return nil
}

func checkChatPlanApproval(o chatOptions, c *agentClient) error {
	p, err := loadPlan(o.Plans, o.ApprovedPlan)
	if err != nil {
		return err
	}
	if err := validatePlanConnection(p, c, o.Config, o.Listen); err != nil {
		return err
	}
	var approval map[string]string
	if err := readPrivateJSON(filepath.Join(o.Plans, o.ApprovedPlan+".approved.json"), &approval); err != nil {
		return fmt.Errorf("plan needs prior local review and exact approval: %w", err)
	}
	if approval["plan"] != o.ApprovedPlan {
		return fmt.Errorf("local approval does not match the selected plan")
	}
	return nil
}

func resumedChatTools(id string) []mcpTool {
	var selected []mcpTool
	for _, t := range chatTools(true) {
		switch t.Name {
		case "sw_apply", "sw_receipt", "sw_verify_plan":
			t.Description += " This session is bound to plan " + id + "; no other plan is allowed."
			t.InputSchema.(map[string]any)["properties"].(map[string]any)["plan"].(map[string]any)["enum"] = []string{id}
			selected = append(selected, t)
		}
	}
	return selected
}

func (s *chatSession) configureExecution(oneShot bool) error {
	if s.o.ApprovedPlan == "" {
		if oneShot {
			// 실행할 수 없는 도구를 숨겨 모델이 같은 거부를 반복하지 않게 한다.
			// 대화형 CLI의 기존 /review·/approve 경로에는 영향을 주지 않는다.
			var selected []mcpTool
			for _, t := range s.tools {
				if t.Name != "sw_apply" {
					selected = append(selected, t)
				}
			}
			s.tools = selected
		}
		return nil
	}
	c, err := localAPI(s.o.Config, s.o.Listen)
	if err != nil {
		return err
	}
	if err := checkChatPlanApproval(s.o, c); err != nil {
		return err
	}
	s.tools = resumedChatTools(s.o.ApprovedPlan)
	s.approved[s.o.ApprovedPlan] = true
	s.messages[0].Content += " The operator selected one already locally approved plan: " + s.o.ApprovedPlan + ". Apply at most once, then use its receipt and verify_plan. A complete write is not readiness or HTTP success. Do not retry partial or uncertain writes."
	return nil
}

func (s *chatSession) checkResumedCall(name string, raw json.RawMessage) error {
	var a struct {
		Plan string `json:"plan"`
	}
	if decodeStrict(raw, &a) != nil || a.Plan != s.o.ApprovedPlan {
		return fmt.Errorf("this chat is bound to the operator-selected approved plan")
	}
	if name != "sw_apply" && name != "sw_receipt" && name != "sw_verify_plan" {
		return fmt.Errorf("tool is not available for the selected plan")
	}
	if name == "sw_apply" {
		if !s.approved[a.Plan] {
			return fmt.Errorf("apply was already attempted in this chat; inspect the receipt and do not retry an uncertain write")
		}
		// 응답 유실도 실패처럼 보일 수 있다. 성공 응답을 기다린 뒤가 아니라
		// 실제 호출 전에 소비해서 같은 대화의 두 번째 쓰기를 항상 막는다.
		delete(s.approved, a.Plan)
	}
	return nil
}
