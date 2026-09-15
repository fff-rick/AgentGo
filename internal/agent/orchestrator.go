// Package agent keeps compatibility adapters and optional Agent behaviors.
package agent

import (
	"context"
	"time"

	"github.com/enterprise/ai-agent-go/internal/harness"
	"github.com/enterprise/ai-agent-go/internal/model"
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
	result, err := o.harness.Run(ctx, &harness.RunRequest{Session: session, Message: req.Message, Mode: mode})
	if err != nil {
		return nil, err
	}
	return &model.ChatResponse{
		SessionID: req.SessionID, Content: result.Answer, ToolCalls: result.ToolCalls,
		Steps: result.Steps, References: result.References, CreatedAt: time.Now(),
	}, nil
}
