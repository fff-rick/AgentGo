package builtin

import "testing"

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
