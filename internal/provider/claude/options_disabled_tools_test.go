package claude

import (
	"slices"
	"testing"

	"agent-overflow/internal/provider"
)

// The settings-level list and the read-only strip are two independent
// reasons to remove a tool; one must never displace the other.
func TestConfigFromOptionsUnionsSettingsToolsWithTheReadOnlyStrip(t *testing.T) {
	cfg := ConfigFromOptions(provider.SessionOptions{
		Provider:      "claude",
		RuntimeMode:   provider.RuntimeReadOnly,
		DisabledTools: []string{"Workflow", "Edit", "WebSearch"},
	})
	// Mode entries first in their fixed order, then the user's in theirs,
	// deduped — a stable order is what keeps PlanLiveUpdate's equality
	// check from flapping between two spellings of the same set.
	want := []string{"Write", "Edit", "NotebookEdit", "Workflow", "WebSearch"}
	if !slices.Equal(cfg.DisallowedTools, want) {
		t.Fatalf("DisallowedTools = %v, want %v", cfg.DisallowedTools, want)
	}
}

func TestConfigFromOptionsAppliesSettingsToolsOutsideReadOnly(t *testing.T) {
	for _, mode := range provider.AllRuntimeModes {
		if mode == provider.RuntimeReadOnly {
			continue
		}
		cfg := ConfigFromOptions(provider.SessionOptions{
			Provider:      "claude",
			RuntimeMode:   mode,
			DisabledTools: []string{"Workflow"},
		})
		if !slices.Equal(cfg.DisallowedTools, []string{"Workflow"}) {
			t.Errorf("mode %q: DisallowedTools = %v, want [Workflow]", mode, cfg.DisallowedTools)
		}
	}
}

func TestMergeDisallowedToolsDropsBlanksAndDuplicates(t *testing.T) {
	got := mergeDisallowedTools([]string{"Write"}, []string{" Workflow ", "", "Workflow", "Write"})
	if !slices.Equal(got, []string{"Write", "Workflow"}) {
		t.Fatalf("mergeDisallowedTools() = %v", got)
	}
	if got := mergeDisallowedTools(nil, []string{"  "}); got != nil {
		t.Fatalf("mergeDisallowedTools() = %v, want nil when nothing survives", got)
	}
}

// The argv boundary is defense in depth behind Settings validation: a name
// that cannot be ONE safe CLI argument is dropped here regardless of how
// Config was built, because the alternative is a name that splits into two
// arguments or parses as a flag.
func TestMergeDisallowedToolsDropsArgvUnsafeNames(t *testing.T) {
	unsafe := []string{
		"Web Search",              // splits into two arguments
		"--permission-mode",       // parses as a flag
		"-Workflow",               // same, short form
		"Workflow\tExtra",         // any whitespace, not just spaces
		"With\nNewline",           // ditto
		"  --allowedTools  Bash ", // survives TrimSpace as a flag
	}
	for _, name := range unsafe {
		got := mergeDisallowedTools([]string{"Write"}, []string{name})
		if !slices.Equal(got, []string{"Write"}) {
			t.Errorf("mergeDisallowedTools(settings=%q) = %v, want the unsafe name dropped", name, got)
		}
	}
	// A legal name beside an unsafe one still lands: dropping is per entry,
	// never the whole list.
	got := mergeDisallowedTools(nil, []string{"bad name", "Workflow"})
	if !slices.Equal(got, []string{"Workflow"}) {
		t.Fatalf("mergeDisallowedTools() = %v, want only the safe name", got)
	}
}

// Tool removal is spawn-only on Claude — no control_request adds or drops a
// tool — so a settings change must fall out of PlanLiveUpdate as a restart.
func TestPlanLiveUpdateRequiresRestartWhenDisabledToolsChange(t *testing.T) {
	prev := provider.SessionOptions{Provider: "claude", Model: "claude-opus-5"}
	next := prev
	next.DisabledTools = []string{"Workflow"}
	if _, ok := PlanLiveUpdate(prev, next); ok {
		t.Fatal("PlanLiveUpdate() ok = true for a disabled-tool change; it is spawn-only")
	}
}

// The rendered argv is the only thing that actually removes a schema.
func TestBuildArgsRendersSettingsDisabledTools(t *testing.T) {
	cfg := ConfigFromOptions(provider.SessionOptions{
		Provider:      "claude",
		RuntimeMode:   provider.RuntimeFullAccess,
		DisabledTools: []string{"Workflow"},
	})
	args := buildArgs(cfg, "")
	idx := slices.Index(args, "Workflow")
	if idx <= 0 || args[idx-1] != "--disallowedTools" {
		t.Fatalf("args missing --disallowedTools Workflow: %v", args)
	}
}
