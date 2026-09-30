package tool

import (
	"context"
	"encoding/json"

	"github.com/enterprise/ai-agent-go/internal/skill"
)

type SkillTool struct {
	registry *skill.Registry
}

func NewSkillTool(registry *skill.Registry) *SkillTool {
	return &SkillTool{registry: registry}
}

func (*SkillTool) Name() string { return LoadSkillName }

func (*SkillTool) Description() string {
	return "按名称加载一个可信 Skill 的完整操作指令。仅当任务匹配 available_skills 中的描述时调用。"
}

func (*SkillTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"name": map[string]interface{}{"type": "string", "description": "available_skills 中的 Skill 名称"},
		},
		"required":             []string{"name"},
		"additionalProperties": false,
	}
}

func (t *SkillTool) Execute(_ context.Context, input string) (*ToolResult, error) {
	var request struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(input), &request); err != nil {
		return NewErrorResult("参数解析失败: " + err.Error()), nil
	}
	candidate, ok := t.registry.Get(request.Name)
	if !ok {
		return NewErrorResult("Skill 未注册: " + request.Name), nil
	}
	return &ToolResult{Success: true, Output: skill.Format(candidate), Trusted: true}, nil
}
