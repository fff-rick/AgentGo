package builtin

import (
	"context"
	"github.com/enterprise/ai-agent-go/internal/config"
	"go.uber.org/zap"
	"strings"
	"testing"
)

func TestValidateReadOnlyQuery(t *testing.T) {
	for _, query := range []string{"SELECT 1", " select * from documents; "} {
		if _, err := validateReadOnlyQuery(query); err != nil {
			t.Fatalf("validateReadOnlyQuery(%q) error = %v", query, err)
		}
	}
	for _, query := range []string{"", "DELETE FROM documents", "SELECT 1; DROP TABLE documents"} {
		if _, err := validateReadOnlyQuery(query); err == nil {
			t.Fatalf("validateReadOnlyQuery(%q) unexpectedly succeeded", query)
		}
	}
}

func TestDatabaseToolRequiresDedicatedConfig(t *testing.T) {
	for _, cfg := range []config.DatabaseToolConfig{
		{}, {DSN: "postgres://reader@localhost/reports"},
		{DSN: "postgres://reader:secret-sensitive@localhost/%zz", AllowedViews: []string{"tool_api.products"}},
	} {
		got, err := NewDatabaseTool(context.Background(), cfg, zap.NewNop())
		if err == nil {
			got.Close()
			t.Fatal("invalid dedicated configuration accepted")
		}
		if strings.Contains(err.Error(), "secret-sensitive") {
			t.Fatal("DSN credential leaked")
		}
	}
}
