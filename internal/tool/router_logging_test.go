package tool

import (
	"strings"
	"testing"
)

func TestDatabaseInputLogOmitsSQLConstants(t *testing.T) {
	for _, input := range []string{`{"sql":"SELECT 'private-value'"}`, `{"sql":"private-value"`} {
		logged := loggableInput("database_query", input)
		if strings.Contains(logged, "private-value") || !strings.Contains(logged, "input_bytes") {
			t.Fatalf("unsafe log: %s", logged)
		}
	}
}
