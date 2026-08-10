package runner

import (
	"strings"
	"testing"

	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/memory"
)

const testDigest = "<campaign-memory>\n3 of 9 notes, newest first. The whole log is /data/workflow-memory/root/notes.ndjson — grep or read it for anything not below.\n</campaign-memory>"

// The CHANNEL follows access; READING does not. A read-only session restricts
// writes, not reads (Claude strips Write/Edit/NotebookEdit; Codex's read-only
// sandbox permits reads filesystem-wide), so the log's absolute path is legible
// to both and both branches carry the same digest naming it.
func TestPromptSuffixChannelFollowsAccessAndBothBranchesCarryTheLog(t *testing.T) {
	narrative := "/data/workflow-runs/item/implement.1/narrative.md"

	writing, err := PromptSuffix(PromptContext{NarrativePath: narrative, Access: def.AccessWrite, Memory: testDigest})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(writing, "agent-overflow memory add --kind") {
		t.Fatalf("a writing element was not given the CLI verb:\n%s", writing)
	}
	if !strings.Contains(writing, "Leave the envelope's `memory` field null") {
		t.Fatalf("a writing element was not told which channel is not its:\n%s", writing)
	}
	if !strings.Contains(writing, testDigest) {
		t.Fatalf("a writing element got no digest:\n%s", writing)
	}

	readOnly, err := PromptSuffix(PromptContext{NarrativePath: narrative, Access: def.AccessReadOnly, Memory: testDigest})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readOnly, "`memory` field of your final envelope") {
		t.Fatalf("a read-only element was not given the envelope channel:\n%s", readOnly)
	}
	if strings.Contains(readOnly, "agent-overflow memory add --kind <") {
		t.Fatalf("a read-only element was told to run a command it cannot run:\n%s", readOnly)
	}
	if !strings.Contains(readOnly, "not available to you") {
		t.Fatalf("a read-only element was not told why the verb is not its channel:\n%s", readOnly)
	}
	if !strings.Contains(readOnly, testDigest) {
		t.Fatalf("a read-only element got no digest:\n%s", readOnly)
	}

	// Both branches name the whole vocabulary: a kind outside it is refused, and
	// an element that has to guess the four names burns its envelope retry.
	for _, suffix := range []string{writing, readOnly} {
		for _, kind := range memory.Kinds {
			if !strings.Contains(suffix, kind) {
				t.Fatalf("suffix does not name kind %q:\n%s", kind, suffix)
			}
		}
		if !strings.Contains(suffix, "NO context") {
			t.Fatalf("suffix does not state the format contract:\n%s", suffix)
		}
	}
}

// A run whose tree could not be resolved gets no section at all. A contract
// naming a channel that collects nothing is worse than never asking.
func TestPromptSuffixOmitsTheMemorySectionWithoutADigest(t *testing.T) {
	for _, access := range []def.Access{def.AccessWrite, def.AccessReadOnly} {
		suffix, err := PromptSuffix(PromptContext{NarrativePath: "/data/n.md", Access: access})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(suffix, "campaign") || strings.Contains(suffix, "memory add") {
			t.Fatalf("access=%q produced a memory section with no digest:\n%s", access, suffix)
		}
	}
	// Whitespace is not a digest either.
	if suffix, err := PromptSuffix(PromptContext{NarrativePath: "/data/n.md", Access: def.AccessWrite, Memory: "  \n "}); err != nil {
		t.Fatal(err)
	} else if strings.Contains(suffix, "memory add") {
		t.Fatalf("a blank digest produced a memory section:\n%s", suffix)
	}
}

// Every agent-backed element gets the section: a phase, a unit, a join, and a
// takeover finalize all run turns that learn things.
func TestEveryPromptBuilderCarriesTheDigest(t *testing.T) {
	phase, err := BuildPrompt(def.Phase{ID: "implement", Prompt: "do it", Access: def.AccessWrite},
		nil, PromptContext{NarrativePath: "/data/n.md", Memory: testDigest})
	if err != nil {
		t.Fatal(err)
	}
	unit, err := BuildUnitPrompt(def.Unit{ID: "lens", Provider: "claude", Prompt: "review"},
		nil, nil, PromptContext{NarrativePath: "/data/n.md", Memory: testDigest})
	if err != nil {
		t.Fatal(err)
	}
	takeover, err := BuildTakeoverFinalizePrompt(PromptContext{
		NarrativePath: "/data/n.md", Access: def.AccessWrite, Memory: testDigest,
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, prompt := range map[string]string{"phase": phase, "unit": unit, "takeover": takeover} {
		if !strings.Contains(prompt, testDigest) {
			t.Errorf("%s prompt carries no digest:\n%s", name, prompt)
		}
	}
}
