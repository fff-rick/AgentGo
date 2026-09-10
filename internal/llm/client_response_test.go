package llm

import (
	"encoding/json"
	"testing"
)

func TestOpenAIResponseReasoningFallbacks(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"reasoning_content", `{"choices":[{"message":{"reasoning_content":"first"}}]}`, "first"},
		{"reasoning", `{"choices":[{"message":{"reasoning":"second"}}]}`, "second"},
		{"thinking", `{"choices":[{"message":{"thinking":"third"}}]}`, "third"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var response openAIResponse
			if err := json.Unmarshal([]byte(tt.body), &response); err != nil {
				t.Fatal(err)
			}
			if got := response.toLLMResponse().Reasoning; got != tt.want {
				t.Fatalf("reasoning = %q, want %q", got, tt.want)
			}
		})
	}
}
