package llm

import (
	"context"
	"io"
	"strings"
	"testing"
)

func TestReadSSEStreamParsesReasoningVariants(t *testing.T) {
	body := strings.Join([]string{
		`data: {"choices":[{"delta":{"reasoning_content":"a"}}]}`,
		`data: {"choices":[{"delta":{"reasoning":"b","content":"答案"}}]}`,
		`data: {"choices":[{"delta":{"thinking":"c"},"finish_reason":"stop"}]}`,
		"",
	}, "\n")
	client := &HTTPClient{}
	ch := make(chan StreamEvent, 8)
	client.readSSEStream(context.Background(), io.NopCloser(strings.NewReader(body)), ch)

	var reasoning, content string
	var done bool
	for event := range ch {
		reasoning += event.Reasoning
		content += event.Content
		done = done || event.Done
		if event.Err != nil {
			t.Fatal(event.Err)
		}
	}
	if reasoning != "abc" || content != "答案" || !done {
		t.Fatalf("reasoning=%q content=%q done=%v", reasoning, content, done)
	}
}
