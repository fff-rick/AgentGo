// Package agent keeps compatibility adapters and optional Agent behaviors.
package agent

import (
	"context"
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
	scope := tool.Scope{SessionID: session.ID}
	if req.Options != nil && req.Options.Tools != nil {
		scope.Restricted = true
		scope.Allowed = uniqueNames(req.Options.Tools)
	}
	result, err := o.harness.Run(ctx, &harness.RunRequest{Session: session, Message: req.Message, Mode: mode, ToolScope: scope})
	if err != nil {
		return nil, err
	}
	return &model.ChatResponse{
		SessionID: req.SessionID, Content: result.Answer, ToolCalls: result.ToolCalls,
		Steps: result.Steps, References: result.References, CreatedAt: time.Now(),
	}, nil
}

func (o *Orchestrator) ValidateAllowedTools(names []string) error {
	return o.harness.ValidateAllowedTools(names)
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
