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

func (o *Orchestrator) ProcessMessage(ctx context.Context, req *model.ChatRequest) (*model.ChatResponse, error) {
	result, err := o.harness.Run(ctx, &harness.RunRequest{SessionID: req.SessionID, Message: req.Message})
	if err != nil {
		return nil, err
	}
	return &model.ChatResponse{
		SessionID: req.SessionID, Content: result.Answer, ToolCalls: result.ToolCalls,
		Steps: result.Steps, References: result.References, CreatedAt: time.Now(),
	}, nil
}
