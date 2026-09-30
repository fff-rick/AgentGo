// Package agent keeps compatibility adapters and optional Agent behaviors.
package agent

import (
	"context"
	"fmt"
	"time"

	"github.com/enterprise/ai-agent-go/internal/harness"
	"github.com/enterprise/ai-agent-go/internal/model"
	"github.com/enterprise/ai-agent-go/internal/tool"
)

// Orchestrator is a compatibility adapter. Agent lifecycle and decisions live
// in AgentHarness and AgentLoop respectively.
type Orchestrator struct {
	harness *harness.AgentHarness
}

func NewOrchestrator(h *harness.AgentHarness) *Orchestrator {
	return &Orchestrator{harness: h}
}

func (o *Orchestrator) ProcessMessage(ctx context.Context, req *model.ChatRequest, session *model.Session) (*model.ChatResponse, error) {
	mode := ""
	if req.Options != nil {
		mode = req.Options.Mode
	}
	scope := tool.Scope{SessionID: session.ID, UserID: session.UserID}
	if req.Options != nil && req.Options.Tools != nil {
		scope.Restricted = true
		scope.Allowed = uniqueNames(req.Options.Tools)
	}
	var requiredTools []string
	var runtimeInstructions []string
	if req.Options != nil && len(req.Options.ApprovedProposals) > 0 {
		scope.ApprovedProposals = uniqueNames(req.Options.ApprovedProposals)
		requiredTools = []string{tool.FileEditApplyName}
		runtimeInstructions = []string{fmt.Sprintf("用户已明确批准文件修改 proposal %q；调用 file_edit_apply 提交这些 ID，不要提交其他 ID。", scope.ApprovedProposals)}
	}
	var skillNames []string
	skillsDisabled := false
	if req.Options != nil && req.Options.Skills != nil {
		skillNames = uniqueNames(req.Options.Skills)
		skillsDisabled = len(req.Options.Skills) == 0
	}
	result, err := o.harness.Run(ctx, &harness.RunRequest{
		Session: session, Message: req.Message, Mode: mode, ToolScope: scope,
		SkillNames: skillNames, SkillsDisabled: skillsDisabled,
		RequiredTools: requiredTools, RuntimeInstructions: runtimeInstructions,
	})
	if err != nil {
		return nil, err
	}
	return &model.ChatResponse{
		SessionID: req.SessionID, Content: result.Answer, ToolCalls: result.ToolCalls,
		Steps: result.Steps, References: result.References, Usage: result.Usage, CreatedAt: time.Now(),
	}, nil
}

func (o *Orchestrator) ValidateAllowedTools(names []string) error {
	return o.harness.ValidateAllowedTools(names)
}

func (o *Orchestrator) ValidateSkills(names []string) error {
	return o.harness.ValidateSkills(names)
}

func uniqueNames(names []string) []string {
	result := make([]string, 0, len(names))
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		result = append(result, name)
	}
	return result
}
