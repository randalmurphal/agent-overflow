package forgeapi

import (
	"context"
	"testing"
)

func TestInteractiveMarker(t *testing.T) {
	t.Parallel()
	base := t.Context()
	if IsInteractive(base) {
		t.Fatal("an unmarked context reads as interactive")
	}
	marked := WithInteractive(base)
	if !IsInteractive(marked) {
		t.Fatal("WithInteractive did not mark the context")
	}
	if IsInteractive(base) {
		t.Fatal("marking a derived context changed its parent")
	}
	child, cancel := context.WithCancel(marked)
	defer cancel()
	if !IsInteractive(child) {
		t.Fatal("a context derived from a marked one lost the mark")
	}
	type otherKey struct{}
	if IsInteractive(context.WithValue(base, otherKey{}, true)) {
		t.Fatal("another package's true value reads as the mark")
	}
}
