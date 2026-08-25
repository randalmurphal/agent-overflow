package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/store/storetest"
	"agent-overflow/internal/testutil"
)

// A campaign edits its own prompts between waves: the human reads what the last
// wave produced, sharpens the instruction, and the next wave is supposed to run
// the sharpened one. That only works because a call resolves its target from
// disk per invocation — the caller's frozen snapshot is the caller's, and a
// child is a new run resolved fresh (§8).
//
// The test asserts the property at the definition source, which is where the
// re-resolution lives: `startCall` / `startUnitCall` call `ResolveCall` per
// invocation, and `ResolveCall` reads the workflow and inlines its prompt
// bodies off disk every time.
func TestCallResolutionReadsEditedPromptsPerInvocation(t *testing.T) {
	configRoot := t.TempDir()
	database := storetest.Clone(t)
	projectRow := testutil.EnsureProject(t, database, t.TempDir())
	writeSelfCallingCampaign(t, configRoot, "first wave instructions")

	source := workflowDefinitionSource{
		store:      database,
		configRoot: configRoot,
		profiles:   workflowProfileSource{store: database, configRoot: configRoot},
	}
	first, err := source.ResolveCall(context.Background(), projectRow.ID, "campaign")
	if err != nil {
		t.Fatal(err)
	}
	if body := first.Workflow.Phases[0].Prompt; !strings.Contains(body, "first wave instructions") {
		t.Fatalf("first resolution inlined %q", body)
	}

	// The human edits the prompt between waves. Nothing restarts, nothing is
	// re-registered — the file on disk is the whole mechanism.
	writePromptFile(t, configRoot, "wave.md", "second wave instructions {{goal}}")

	second, err := source.ResolveCall(context.Background(), projectRow.ID, "campaign")
	if err != nil {
		t.Fatal(err)
	}
	body := second.Workflow.Phases[0].Prompt
	if !strings.Contains(body, "second wave instructions") {
		t.Fatalf("the next wave did not pick up the edited prompt: %q", body)
	}
	if strings.Contains(body, "first wave instructions") {
		t.Fatalf("the next wave reused a cached prompt body: %q", body)
	}
	// The already-resolved definition is a value, not a view: the first wave's
	// snapshot is untouched by the edit, which is what freezing means for the run
	// that is already going.
	if got := first.Workflow.Phases[0].Prompt; !strings.Contains(got, "first wave instructions") {
		t.Fatalf("an earlier resolution changed under the edit: %q", got)
	}
}

// writeSelfCallingCampaign writes the campaign shape the re-resolution matters
// for: a fan-out whose units call the campaign back, bounded by max_depth.
func writeSelfCallingCampaign(t *testing.T, configRoot, promptBody string) {
	t.Helper()
	dir := filepath.Join(configRoot, "workflows")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	workflow := `id: campaign
name: Campaign
inputs:
  goal:
    schema:
      type: string
phases:
  - id: plan
    name: Plan
    driver: agent
    provider: claude
    model: claude-opus-4-7
    prompt: wave.md
    access: read-only
    inputs:
      goal:
        schema:
          type: string
    outputs:
      sections:
        schema:
          type: array
          items:
            type: string
    gate:
      routes:
        - to: wave
  - id: wave
    shape: fan-out
    over: plan.sections
    as: section
    unit:
      id: wave-unit
      call: campaign
      max_depth: 120
      args:
        goal: section
    join:
      id: merge
      provider: claude
      model: claude-opus-4-7
      prompt: merge.md
    inputs:
      plan.sections:
        schema:
          type: array
          items:
            type: string
    outputs:
      merged:
        schema:
          type: boolean
    gate:
      routes:
        - to: done
`
	if err := os.WriteFile(filepath.Join(dir, "campaign.yaml"), []byte(workflow), 0o600); err != nil {
		t.Fatal(err)
	}
	writePromptFile(t, configRoot, "wave.md", promptBody+" {{goal}}")
	writePromptFile(t, configRoot, "merge.md", "merge {{units}}")
}

func writePromptFile(t *testing.T, configRoot, name, body string) {
	t.Helper()
	path := filepath.Join(configRoot, "workflows", name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(fmt.Errorf("write prompt %q: %w", path, err))
	}
}
