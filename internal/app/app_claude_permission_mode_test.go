package app

import (
	"testing"

	"agent-overflow/internal/provider"
)

// Every runtime mode the CLI can move a thread to or from has a name for the
// notice; a missing one would print an empty word.
func TestRuntimeModeLabelsCoverEveryRuntimeMode(t *testing.T) {
	t.Parallel()
	for _, mode := range provider.AllRuntimeModes {
		if runtimeModeLabels[mode] == "" {
			t.Errorf("runtimeModeLabels has no name for %q", mode)
		}
	}
	if len(runtimeModeLabels) != len(provider.AllRuntimeModes) {
		t.Errorf("runtimeModeLabels has %d names for %d runtime modes", len(runtimeModeLabels), len(provider.AllRuntimeModes))
	}
}
