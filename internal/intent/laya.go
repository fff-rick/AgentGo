package intent

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"

	"go.uber.org/zap"

	"github.com/enterprise/ai-agent-go/internal/laya"
	"github.com/enterprise/ai-agent-go/internal/metrics"
	"github.com/enterprise/ai-agent-go/internal/model"
)

const (
	questionExecutionMode = "execution_mode"
	questionSource        = "information_source"
)

var errLayaLowConfidence = errors.New("Laya 意图识别置信度不足")

// LayaShadow observes intent without changing AgentGo execution.
type LayaShadow struct {
	client        *laya.Client
	minConfidence float64
	logger        *zap.Logger
}

func NewLayaShadow(client *laya.Client, minConfidence float64, logger *zap.Logger) *LayaShadow {
	return &LayaShadow{client: client, minConfidence: minConfidence, logger: logger}
}

func (s *LayaShadow) Recognize(ctx context.Context, input string, history []model.LLMMessage) (*model.IntentResult, error) {
	response, err := s.client.Predict(ctx, "intent_recognition", laya.Request{
		State: map[string]string{"scenario": intentScenario(history, input), "request": input},
		Questions: map[string]laya.Question{
			questionExecutionMode: {
				Type:         "choice",
				Instructions: "Choose how this request should be executed. Tool use alone does not require planner mode.",
				Criteria: map[string]string{
					model.ExecutionModeAgent:   "A direct response, one tool call, or a short independent sequence is sufficient.",
					model.ExecutionModePlanner: "Multiple dependent actions require an explicit plan and coordinated intermediate results.",
				},
			},
			questionSource: {
				Type:         "choice",
				Instructions: "Choose the primary information source or capability needed. Choose none when no tool is needed.",
				Criteria: map[string]string{
					"none":               "No external information or capability is needed.",
					"internal_knowledge": "Private documents or the internal knowledge base.",
					"web":                "Current or public internet information.",
					"database":           "Structured records queried from a database.",
					"file":               "Local file inspection or modification.",
					"calculator":         "Exact arithmetic or numeric calculation.",
				},
			},
		},
		MinConfidence: &s.minConfidence,
	})
	if err != nil {
		return nil, err
	}
	mode, modeConfidence, err := layaChoice(response, questionExecutionMode, model.ExecutionModeAgent, model.ExecutionModePlanner)
	if err != nil {
		return nil, err
	}
	source, sourceConfidence, err := layaChoice(response, questionSource, "none", "internal_knowledge", "web", "database", "file", "calculator")
	if err != nil {
		return nil, err
	}

	intentName := IntentChat
	if mode == model.ExecutionModePlanner {
		intentName = IntentComplexTask
	} else if source == "internal_knowledge" {
		intentName = IntentRAGQuery
	} else if source != "none" {
		intentName = IntentToolUse
	}
	return &model.IntentResult{
		Intent: intentName, Confidence: min(modeConfidence, sourceConfidence), Entities: map[string]string{},
	}, nil
}

func (s *LayaShadow) Observe(ctx context.Context, input string, history []model.LLMMessage) {
	result, err := s.Recognize(ctx, input, history)
	if err != nil {
		reason := "error"
		if errors.Is(err, errLayaLowConfidence) {
			reason = "low_confidence"
		}
		metrics.Default.LayaFallbacks.WithLabelValues("intent_recognition", reason).Inc()
		s.logger.Warn("Laya 意图识别影子调用未采用", zap.String("reason", reason), zap.String("task_sha256", intentTaskSHA256(input)), zap.Error(err))
		return
	}
	s.logger.Info("Laya 意图识别影子结果",
		zap.String("task_sha256", intentTaskSHA256(input)),
		zap.String("intent", result.Intent),
		zap.Float64("confidence", result.Confidence),
	)
}

func layaChoice(response *laya.Response, question string, choices ...string) (string, float64, error) {
	answer, ok := response.Answers[question]
	if !ok || answer.Abstention != "passed" || answer.LowConfidence != nil && *answer.LowConfidence {
		return "", 0, errLayaLowConfidence
	}
	valid := false
	for _, choice := range choices {
		if answer.Choice == choice {
			valid = true
			break
		}
	}
	if !valid {
		return "", 0, fmt.Errorf("Laya 对 %s 返回未知选项 %q", question, answer.Choice)
	}
	confidence := answer.Confidence
	if answer.AnswerConfidence != nil {
		confidence = answer.AnswerConfidence
	}
	if confidence == nil {
		return "", 0, fmt.Errorf("Laya 对 %s 未返回置信度", question)
	}
	return answer.Choice, *confidence, nil
}

func intentScenario(history []model.LLMMessage, current string) string {
	if n := len(history); n > 0 && history[n-1].Role == "user" && history[n-1].Content == current {
		history = history[:n-1]
	}
	if len(history) > 6 {
		history = history[len(history)-6:]
	}
	var scenario strings.Builder
	for _, message := range history {
		if message.Role != "user" && message.Role != "assistant" {
			continue
		}
		content := []rune(strings.TrimSpace(message.Content))
		if len(content) > 500 {
			content = content[:500]
		}
		fmt.Fprintf(&scenario, "%s: %s\n", message.Role, string(content))
	}
	return strings.TrimSpace(scenario.String())
}

func intentTaskSHA256(input string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(input))) }
