package handler

import (
	"strings"
	"testing"

	"github.com/enterprise/ai-agent-go/internal/model"
	"github.com/enterprise/ai-agent-go/internal/tool"
)

func TestValidateFileApprovals(t *testing.T) {
	validID := "123e4567-e89b-12d3-a456-426614174000"
	for _, test := range []struct {
		name    string
		options *model.ChatOptions
		wantErr string
	}{
		{name: "none", options: nil},
		{name: "valid", options: &model.ChatOptions{ApprovedProposals: []string{validID}}},
		{name: "invalid id", options: &model.ChatOptions{ApprovedProposals: []string{"bad"}}, wantErr: "无效 ID"},
		{name: "planner", options: &model.ChatOptions{Mode: model.ExecutionModePlanner, ApprovedProposals: []string{validID}}, wantErr: "agent 模式"},
		{name: "excluded tool", options: &model.ChatOptions{Tools: []string{"file_inspect"}, ApprovedProposals: []string{validID}}, wantErr: tool.FileEditApplyName},
		{name: "allowed tool", options: &model.ChatOptions{Tools: []string{tool.FileEditApplyName}, ApprovedProposals: []string{validID}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateFileApprovals(test.options)
			if test.wantErr == "" && err != nil {
				t.Fatal(err)
			}
			if test.wantErr != "" && (err == nil || !strings.Contains(err.Error(), test.wantErr)) {
				t.Fatalf("err=%v, want %q", err, test.wantErr)
			}
		})
	}
}
