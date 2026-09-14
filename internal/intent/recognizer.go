// Package intent 提供用户意图识别能力。
// 通过 LLM 分析用户输入，判断其意图类别和置信度，用于 Agent 编排决策。
package intent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"go.uber.org/zap"

	"github.com/enterprise/ai-agent-go/internal/llm"
	"github.com/enterprise/ai-agent-go/internal/model"
)

// 意图类型常量
const (
	IntentChat        = "chat"         // 闲聊
	IntentRAGQuery    = "rag_query"    // 知识库查询
	IntentToolUse     = "tool_use"     // 工具调用
	IntentComplexTask = "complex_task" // 复杂任务（需要多步推理）
)

// 意图识别的 Prompt 模板
const intentPrompt = `你是一个意图识别引擎。请结合历史对话和最新用户输入判断当前意图。

可选意图：
- chat: 日常闲聊、问候、闲谈
- rag_query: 需要从知识库检索信息回答的问题
- tool_use: 需要调用工具（计算、搜索、数据库查询）才能完成的任务
- complex_task: 需要多步推理、规划的复杂任务

请只返回 JSON：
{"intent": "意图类型", "confidence": 0.0-1.0, "entities": {}, "required_tools": []}`

// Recognizer 意图识别器
type Recognizer struct {
	router *llm.Router
	logger *zap.Logger
}

// NewRecognizer 创建意图识别器
func NewRecognizer(router *llm.Router, logger *zap.Logger) *Recognizer {
	return &Recognizer{
		router: router,
		logger: logger,
	}
}

// Recognize 结合最近的会话上下文分析当前用户输入。
// 正常路径由 LLM 决定意图；关键词规则仅在模型不可用时降级使用。
func (r *Recognizer) Recognize(ctx context.Context, userInput string, history []model.LLMMessage) (*model.IntentResult, error) {
	messages := []model.LLMMessage{{Role: "system", Content: intentPrompt}}
	if len(history) > 6 {
		history = history[len(history)-6:]
	}
	for _, message := range history {
		if message.Role != "user" && message.Role != "assistant" {
			continue
		}
		content := []rune(strings.TrimSpace(message.Content))
		if len(content) > 500 {
			content = content[:500]
		}
		messages = append(messages, model.LLMMessage{Role: message.Role, Content: string(content)})
	}
	messages = append(messages, model.LLMMessage{Role: "user", Content: userInput})
	req := &model.LLMRequest{
		Messages:    messages,
		Temperature: 0.1, // 低温度提高确定性
		MaxTokens:   200,
	}

	resp, err := r.router.Chat(ctx, req)
	if err != nil {
		r.logger.Warn("LLM 意图识别失败，使用规则降级", zap.Error(err))
		return r.fallback(userInput), nil
	}

	var result model.IntentResult
	if err := decodeIntent(resp.Content, &result); err != nil {
		r.logger.Warn("解析意图识别结果失败，使用规则降级",
			zap.String("raw", resp.Content),
			zap.Error(err),
		)
		return r.fallback(userInput), nil
	}
	if !validIntent(result.Intent) {
		r.logger.Warn("模型返回未知意图，使用规则降级", zap.String("intent", result.Intent))
		return r.fallback(userInput), nil
	}

	r.logger.Info("意图识别完成",
		zap.String("intent", result.Intent),
		zap.Float64("confidence", result.Confidence),
	)

	return &result, nil
}

// fallback 当 LLM 不可用或解析失败时的兜底策略
func (r *Recognizer) fallback(userInput string) *model.IntentResult {
	input := strings.ToLower(userInput)
	result := &model.IntentResult{Intent: IntentChat, Confidence: 0.5, Entities: make(map[string]string)}

	if containsAny(input, "知识库", "根据文档", "文档中", "内部资料") {
		result.Intent = IntentRAGQuery
		result.Confidence = 0.9
		return result
	}
	if containsAny(input, "搜索", "联网", "实时", "最新", "天气", "新闻", "股价", "汇率", "查一下", "查询一下") {
		result.Intent = IntentToolUse
		result.Confidence = 0.9
		result.RequiredTools = []string{"web_search"}
		return result
	}
	if containsAny(input, "计算", "算一下", "数据库", "sql") {
		result.Intent = IntentToolUse
		result.Confidence = 0.85
		return result
	}
	return result
}

func decodeIntent(content string, result *model.IntentResult) error {
	content = strings.TrimSpace(content)
	start, end := strings.Index(content, "{"), strings.LastIndex(content, "}")
	if start < 0 || end < start {
		return fmt.Errorf("响应中没有 JSON 对象")
	}
	return json.Unmarshal([]byte(content[start:end+1]), result)
}

func validIntent(intentName string) bool {
	switch intentName {
	case IntentChat, IntentRAGQuery, IntentToolUse, IntentComplexTask:
		return true
	default:
		return false
	}
}

func containsAny(input string, keywords ...string) bool {
	for _, keyword := range keywords {
		if strings.Contains(input, keyword) {
			return true
		}
	}
	return false
}
