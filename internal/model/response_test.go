package model

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDocumentResponseDoesNotExposeContentHash(t *testing.T) {
	encoded, err := json.Marshal(DocumentResponse{DocID: "doc-1", ContentHash: "secret-internal-hash"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "content_hash") || strings.Contains(string(encoded), "secret-internal-hash") {
		t.Fatalf("internal content hash leaked into API response: %s", encoded)
	}
}
