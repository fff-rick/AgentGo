package agent

import "testing"

func TestUniqueNamesPreservesFirstOccurrence(t *testing.T) {
	got := uniqueNames([]string{"beta", "alpha", "beta", "alpha"})
	if len(got) != 2 || got[0] != "beta" || got[1] != "alpha" {
		t.Fatalf("unique names = %v", got)
	}
}
