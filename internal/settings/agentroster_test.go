package settings

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The seeds are what an install with no edits reads, and each one passes
// the strict validation a person's own edit must pass.
func TestTheSeededRosterIsValidAndReadByDefault(t *testing.T) {
	t.Parallel()
	got := NewService(t.TempDir()).Get()
	if len(got.AgentProfiles) != len(defaultAgentProfiles) || len(got.AgentNotes) != len(defaultAgentNotes) {
		t.Fatalf("read %d profiles and %d notes, want the %d and %d seeds", len(got.AgentProfiles), len(got.AgentNotes), len(defaultAgentProfiles), len(defaultAgentNotes))
	}
	if got.SubThreadLevels != DefaultSubThreadLevels {
		t.Fatalf("subThreadLevels = %d, want %d", got.SubThreadLevels, DefaultSubThreadLevels)
	}
	seeded := copyDefaults()
	if err := validateAgentRoster(&seeded); err != nil {
		t.Fatalf("the seeds fail validation: %v", err)
	}
	names := map[string]bool{}
	for _, profile := range defaultAgentProfiles {
		names[profile.Name] = true
	}
	for _, name := range []string{"Astra", "Sol", "Terra", "Luna", "Fable", "Opus", "Sonnet", "Haiku"} {
		if !names[name] {
			t.Errorf("no seeded profile for %s", name)
		}
	}
}

// The seeds read correctly on a computer with one provider: each
// provider's profiles are listed together from the lowest tier to the
// highest, and no use-for line points at a profile of the other provider.
// Text that applies to every computer names no profile at all.
func TestTheSeedsShowEachProvidersLadderAndNameNoOtherProvider(t *testing.T) {
	t.Parallel()
	var order []string
	for _, profile := range defaultAgentProfiles {
		order = append(order, profile.Provider+"/"+profile.Name)
	}
	want := []string{
		"codex/Luna", "codex/Terra", "codex/Sol", "codex/Astra",
		"claude/Haiku", "claude/Sonnet", "claude/Opus", "claude/Fable",
	}
	if strings.Join(order, " ") != strings.Join(want, " ") {
		t.Fatalf("seeded order = %v, want each provider's ladder lowest first: %v", order, want)
	}
	for _, profile := range defaultAgentProfiles {
		for _, other := range defaultAgentProfiles {
			if other.Provider != profile.Provider && strings.Contains(profile.UseFor, other.Name) {
				t.Errorf("%s's use-for names %s, which a %s-only computer cannot run", profile.Name, other.Name, profile.Provider)
			}
		}
	}
	for _, note := range defaultAgentNotes {
		for _, profile := range defaultAgentProfiles {
			if strings.Contains(note.Text, profile.Name) {
				t.Errorf("a seeded note names %s: %q", profile.Name, note.Text)
			}
		}
	}
	// Tier and effort are stated together, as the two axes of one choice.
	if !strings.HasPrefix(defaultAgentNotes[0].Text, "Tier and effort are two separate choices.") ||
		!strings.HasPrefix(defaultAgentNotes[1].Text, "Effort: use high by default.") {
		t.Errorf("the first two notes are %q and %q, want the tier statement beside the effort defaults", defaultAgentNotes[0].Text, defaultAgentNotes[1].Text)
	}
}

// An edit made in place on a read value never reaches the seeds.
func TestEditingAReadRosterLeavesTheSeedsAlone(t *testing.T) {
	t.Parallel()
	read := copyDefaults()
	read.AgentProfiles[0].Name = "Changed"
	read.AgentNotes[0].Text = "Changed"
	if defaultAgentProfiles[0].Name == "Changed" || defaultAgentNotes[0].Text == "Changed" {
		t.Fatal("an edit of a read roster wrote through to the seeds")
	}
}

// An edited roster is the person's: an emptied list stays empty, and an
// edited one reads back as edited rather than as the seed.
func TestAnEditedRosterIsStoredAsTheirs(t *testing.T) {
	t.Parallel()
	svc, store := tieredService(t)
	if _, err := svc.Update(map[string]any{
		"agentProfiles": []map[string]any{{"name": " Reviewer ", "useFor": "Reviews.", "provider": "codex", "model": "gpt-5.6-sol"}},
		"agentNotes":    []map[string]any{},
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if _, ok := store.scopes[UserScope]["agentProfiles"]; !ok {
		t.Fatalf("the roster was not written to the user scope: %v", store.scopes[UserScope])
	}
	svc.InvalidateTierCache()
	got := svc.Get()
	if len(got.AgentProfiles) != 1 || got.AgentProfiles[0].Name != "Reviewer" {
		t.Fatalf("profiles = %+v, want the one trimmed edit", got.AgentProfiles)
	}
	if len(got.AgentNotes) != 0 {
		t.Fatalf("notes = %+v, want the emptied list, not the seeds", got.AgentNotes)
	}
}

func TestARosterTheSpawnCouldNotUseIsRefused(t *testing.T) {
	t.Parallel()
	valid := map[string]any{"name": "Ok", "useFor": "Things."}
	cases := map[string]map[string]any{
		"no name":            {"agentProfiles": []map[string]any{{"useFor": "x"}}},
		"no use-for":         {"agentProfiles": []map[string]any{{"name": "A"}}},
		"bad name":           {"agentProfiles": []map[string]any{{"name": "a,b", "useFor": "x"}}},
		"same name":          {"agentProfiles": []map[string]any{valid, {"name": "OK", "useFor": "y"}}},
		"unknown provider":   {"agentProfiles": []map[string]any{{"name": "A", "useFor": "x", "provider": "gemini"}}},
		"model, no provider": {"agentProfiles": []map[string]any{{"name": "A", "useFor": "x", "model": "m"}}},
		"unknown effort":     {"agentProfiles": []map[string]any{{"name": "A", "useFor": "x", "effort": "huge"}}},
		"unknown mode":       {"agentProfiles": []map[string]any{{"name": "A", "useFor": "x", "runtimeMode": "yolo"}}},
		"long instructions":  {"agentProfiles": []map[string]any{{"name": "A", "useFor": "x", "instructions": strings.Repeat("i", MaxAgentProfileInstructionsBytes+1)}}},
		"blank note":         {"agentNotes": []map[string]any{{"text": " "}}},
		"unknown when":       {"agentNotes": []map[string]any{{"text": "x", "when": "sometimes"}}},
		"levels below":       {"subThreadLevels": 0},
		"levels above":       {"subThreadLevels": MaxSubThreadLevels + 1},
	}
	for name, patch := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := NewService(t.TempDir()).Update(patch); err == nil {
				t.Fatalf("Update(%v) succeeded", patch)
			}
		})
	}
}

// The lenient half: a hand-edited file loads with what validation would
// refuse dropped, and the rest kept.
func TestAHandEditedRosterLoadsWithTheUnusableEntriesDropped(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	file := `{"agentProfiles":[{"name":"Good","useFor":"x"},{"name":"","useFor":"x"},{"name":"good","useFor":"dup"}],` +
		`"agentNotes":[{"text":"kept"},{"text":"x","when":"never"}],"subThreadLevels":9}`
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}
	got := NewService(dir).Get()
	if len(got.AgentProfiles) != 1 || got.AgentProfiles[0].Name != "Good" {
		t.Fatalf("profiles = %+v", got.AgentProfiles)
	}
	if len(got.AgentNotes) != 1 || got.AgentNotes[0].Text != "kept" {
		t.Fatalf("notes = %+v", got.AgentNotes)
	}
	if got.SubThreadLevels != DefaultSubThreadLevels {
		t.Fatalf("subThreadLevels = %d, want the default", got.SubThreadLevels)
	}
}

// A roster written as null reads back as empty arrays, so a client never
// takes it for a missing value and shows the seeds in its place.
func TestANullRosterReadsAsEmpty(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"agentProfiles":null,"agentNotes":null}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got := NewService(dir).Get()
	wire, err := json.Marshal(struct {
		Profiles []AgentProfile `json:"agentProfiles"`
		Notes    []AgentNote    `json:"agentNotes"`
	}{got.AgentProfiles, got.AgentNotes})
	if err != nil {
		t.Fatal(err)
	}
	if string(wire) != `{"agentProfiles":[],"agentNotes":[]}` {
		t.Fatalf("the null roster reads as %s", wire)
	}
}
