package provider

import (
	"os"
	"regexp"
	"slices"
	"testing"

	"agent-overflow/internal/testutil/reporoot"
)

// The frontend's list of the providers whose agents get the tracker tools
// (frontend/src/lib/providers/trackerTools.ts), which Ask your agent
// starts a thread on, is the providers SupportsTrackerMCP names.
func TestTheFrontendsTrackerToolProvidersAreSupportsTrackerMCP(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile(reporoot.Path(t, "frontend/src/lib/providers/trackerTools.ts"))
	if err != nil {
		t.Fatal(err)
	}
	list := regexp.MustCompile(`TRACKER_TOOL_PROVIDERS: readonly ProviderID\[\] = \[([^\]]*)\]`).FindSubmatch(src)
	if list == nil {
		t.Fatal("trackerTools.ts declares no TRACKER_TOOL_PROVIDERS list")
	}
	var frontend []string
	for _, m := range regexp.MustCompile(`'([^']+)'`).FindAllSubmatch(list[1], -1) {
		frontend = append(frontend, string(m[1]))
	}
	var supported []string
	for _, kind := range AllProviders() {
		if CapabilitiesForProvider(string(kind)).SupportsTrackerMCP {
			supported = append(supported, string(kind))
		}
	}
	slices.Sort(supported)
	sorted := slices.Sorted(slices.Values(frontend))
	if !slices.Equal(sorted, supported) {
		t.Fatalf("the frontend lists %v; SupportsTrackerMCP names %v", frontend, supported)
	}
}
