package etl

import "testing"

func TestDocumentAndChunkIDsAreStable(t *testing.T) {
	docID := DocumentID("markdown", "# AgentGo\ncontent")
	if docID != DocumentID("markdown", "# AgentGo\ncontent") {
		t.Fatal("identical documents produced different IDs")
	}
	if docID == DocumentID("markdown", "# AgentGo\nchanged") {
		t.Fatal("different documents produced the same ID")
	}
	if stableChunkID(docID, 0) != stableChunkID(docID, 0) || stableChunkID(docID, 0) == stableChunkID(docID, 1) {
		t.Fatal("chunk IDs are not stable and unique by index")
	}
}
