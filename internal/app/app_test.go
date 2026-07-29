package app

import "testing"

// TestPlaceholder exists so that `make test` has something to run before the
// first real component lands. Replace it, do not add to it.
func TestPlaceholder(t *testing.T) {
	t.Parallel()
	if got := "forktower"; got == "" {
		t.Fatal("unreachable")
	}
}
