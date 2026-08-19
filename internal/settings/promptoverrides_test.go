package settings

import (
	"bytes"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestPromptOverridesRoundTripThroughUpdate(t *testing.T) {
	svc := NewService(t.TempDir())

	updated, err := svc.Update(map[string]any{
		"claudePromptOverrides": []map[string]any{
			{"enabled": true, "models": []string{"claude-opus-5"}, "prompt": "opus prompt"},
			{"enabled": false, "models": []string{"claude-fable-5"}, "prompt": "fable prompt"},
		},
		"codexPromptOverrides": []map[string]any{
			{"enabled": true, "models": []string{"gpt-5.6-sol"}, "prompt": "codex prompt"},
		},
		"claudeDisabledTools": []string{"Workflow", "WebSearch"},
		"codexDisabledTools":  []string{"web_search"},
	})
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if len(updated.ClaudePromptOverrides) != 2 {
		t.Fatalf("claude overrides = %d, want 2", len(updated.ClaudePromptOverrides))
	}
	if got := updated.ClaudePromptOverrides[0]; !got.Enabled || got.Prompt != "opus prompt" {
		t.Fatalf("first claude override = %+v", got)
	}
	if updated.ClaudePromptOverrides[1].Enabled {
		t.Fatal("second claude override must stay disabled")
	}

	// A fresh service over the same file must read back what was written.
	reread := NewService(filepath.Dir(svc.Path())).Get()
	if len(reread.CodexPromptOverrides) != 1 || reread.CodexPromptOverrides[0].Prompt != "codex prompt" {
		t.Fatalf("codex overrides after reload = %+v", reread.CodexPromptOverrides)
	}
	if len(reread.ClaudeDisabledTools) != 2 || reread.ClaudeDisabledTools[0] != "Workflow" {
		t.Fatalf("claude disabled tools after reload = %v", reread.ClaudeDisabledTools)
	}
	if len(reread.CodexDisabledTools) != 1 || reread.CodexDisabledTools[0] != "web_search" {
		t.Fatalf("codex disabled tools after reload = %v", reread.CodexDisabledTools)
	}
}

// The four fields are absent from DefaultSettings, so the sparse writer
// must omit them entirely until the user configures one — otherwise every
// settings file grows four empty keys.
func TestPromptOverridesAreOmittedFromSparseWriteWhenUnset(t *testing.T) {
	svc := NewService(t.TempDir())
	if _, err := svc.Update(map[string]any{"theme": "dark"}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	data, err := os.ReadFile(svc.Path())
	if err != nil {
		t.Fatalf("read settings file: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("decode settings file: %v", err)
	}
	for _, key := range []string{
		"claudePromptOverrides", "codexPromptOverrides",
		"claudeDisabledTools", "codexDisabledTools",
	} {
		if _, present := raw[key]; present {
			t.Errorf("%s written to disk while unset: %s", key, data)
		}
	}
}

func TestPromptOverrideUpdatePreservesUnrelatedFields(t *testing.T) {
	svc := NewService(t.TempDir())
	if _, err := svc.Update(map[string]any{
		"claudeDisabledTools": []string{"Workflow"},
	}); err != nil {
		t.Fatalf("seed Update() error = %v", err)
	}
	// A patch touching only the prompt list must not clear the tool list:
	// applyPatch merges top-level keys, so this is the guard against a
	// caller that sends one field at a time.
	updated, err := svc.Update(map[string]any{
		"codexPromptOverrides": []map[string]any{
			{"enabled": true, "models": []string{"gpt-5.6-sol"}, "prompt": "p"},
		},
	})
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if len(updated.ClaudeDisabledTools) != 1 {
		t.Fatalf("claudeDisabledTools = %v, want the seeded entry preserved", updated.ClaudeDisabledTools)
	}
}

func TestValidatePromptOverridesNormalizesAndRejects(t *testing.T) {
	entries, err := validatePromptOverrides("claudePromptOverrides", []PromptOverride{
		{Enabled: true, Models: []string{" claude-opus-5 ", "claude-opus-5", ""}, Prompt: "  hi  "},
	})
	if err != nil {
		t.Fatalf("validatePromptOverrides() error = %v", err)
	}
	if got := entries[0].Models; len(got) != 1 || got[0] != "claude-opus-5" {
		t.Fatalf("models = %v, want deduped + trimmed", got)
	}
	if entries[0].Prompt != "hi" {
		t.Fatalf("prompt = %q, want trimmed", entries[0].Prompt)
	}

	tooLong := []PromptOverride{{Prompt: strings.Repeat("x", MaxPromptOverrideLen+1)}}
	if _, err := validatePromptOverrides("claudePromptOverrides", tooLong); err == nil {
		t.Fatal("over-length prompt: want error")
	}

	tooMany := make([]PromptOverride, MaxPromptOverrides+1)
	if _, err := validatePromptOverrides("claudePromptOverrides", tooMany); err == nil {
		t.Fatal("over-full list: want error")
	}
}

// The bound is on what gets STORED, and trailing whitespace never does: a
// prompt that fits exactly once trimmed must save, or the editor rejects a
// value it is about to shorten anyway.
func TestValidatePromptOverridesBoundsTheTrimmedPrompt(t *testing.T) {
	atCap := strings.Repeat("x", MaxPromptOverrideLen)
	entries, err := validatePromptOverrides("claudePromptOverrides", []PromptOverride{
		{Enabled: true, Models: []string{"claude-opus-5"}, Prompt: atCap + "\n\n  "},
	})
	if err != nil {
		t.Fatalf("validatePromptOverrides() error = %v, want the trimmed prompt accepted", err)
	}
	if entries[0].Prompt != atCap {
		t.Fatalf("prompt = %d bytes, want the %d-byte trimmed value", len(entries[0].Prompt), len(atCap))
	}
}

// Per-entry caps do not bound the list: 50 × 64 KB would ride in every
// GetSettings payload. The aggregate cap is what makes the wire cost
// finite, so the strict path has to refuse it rather than trim.
func TestValidatePromptOverridesRejectsAnOverSizedListTotal(t *testing.T) {
	prompt := strings.Repeat("x", MaxPromptOverrideLen)
	entries := make([]PromptOverride, 0, MaxPromptOverrides)
	for len(entries)*MaxPromptOverrideLen <= MaxPromptOverridesTotalLen {
		entries = append(entries, PromptOverride{Enabled: true, Models: []string{"m"}, Prompt: prompt})
	}
	if _, err := validatePromptOverrides("claudePromptOverrides", entries); err == nil {
		t.Fatalf("%d × %d bytes: want an aggregate-cap error", len(entries), MaxPromptOverrideLen)
	}

	// One byte under the cap still saves, so the bound is the cap and not
	// the entry count that happens to reach it.
	underCap := entries[:len(entries)-1]
	if _, err := validatePromptOverrides("claudePromptOverrides", underCap); err != nil {
		t.Fatalf("validatePromptOverrides() error = %v for a list under the aggregate cap", err)
	}
}

// The lenient path keeps whole entries: a half-truncated system prompt is
// worse than a missing one, and the rest of the settings file must load.
func TestSanitizePromptOverridesDropsEntriesPastTheAggregateCap(t *testing.T) {
	prompt := strings.Repeat("x", MaxPromptOverrideLen)
	fit := MaxPromptOverridesTotalLen / MaxPromptOverrideLen
	entries := make([]PromptOverride, 0, fit+2)
	for i := 0; i < fit+2; i++ {
		entries = append(entries, PromptOverride{Enabled: true, Models: []string{"m"}, Prompt: prompt})
	}
	got := sanitizePromptOverrides("claudePromptOverrides", entries)
	if len(got) != fit {
		t.Fatalf("kept %d entries, want the %d that fit under %d bytes", len(got), fit, MaxPromptOverridesTotalLen)
	}
	for i, entry := range got {
		if len(entry.Prompt) != MaxPromptOverrideLen {
			t.Fatalf("entry %d is %d bytes — kept entries must be whole", i, len(entry.Prompt))
		}
	}
}

// dedupeTrimmed's cap is silent by itself; on these two lists the drop has
// to reach the log, since the user authored the entries that vanish and
// the UI would otherwise render the shortened list as if it were saved.
func TestSanitizersLogWhenTheyCapAHandAuthoredList(t *testing.T) {
	models := make([]string, MaxPromptOverrideModels+1)
	for i := range models {
		models[i] = "m" + strconv.Itoa(i)
	}
	logged := captureLog(t, func() {
		got := sanitizePromptOverrides("claudePromptOverrides", []PromptOverride{
			{Enabled: true, Models: models, Prompt: "p"},
		})
		if len(got[0].Models) != MaxPromptOverrideModels {
			t.Fatalf("models = %d, want %d", len(got[0].Models), MaxPromptOverrideModels)
		}
	})
	if !strings.Contains(logged, "claudePromptOverrides[0].models") {
		t.Fatalf("capping the model list logged %q, want the field named", logged)
	}

	tools := make([]string, MaxDisabledTools+1)
	for i := range tools {
		tools[i] = "Tool" + strconv.Itoa(i)
	}
	logged = captureLog(t, func() {
		if kept := sanitizeDisabledTools("claudeDisabledTools", tools); len(kept) != MaxDisabledTools {
			t.Fatalf("tools = %d, want %d", len(kept), MaxDisabledTools)
		}
	})
	if !strings.Contains(logged, "claudeDisabledTools") {
		t.Fatalf("capping the tool list logged %q, want the field named", logged)
	}

	// Same rule for the prompt body, which the load path shortens in place.
	logged = captureLog(t, func() {
		got := sanitizePromptOverrides("codexPromptOverrides", []PromptOverride{
			{Enabled: true, Models: []string{"m"}, Prompt: strings.Repeat("x", MaxPromptOverrideLen+1)},
		})
		if len(got[0].Prompt) != MaxPromptOverrideLen {
			t.Fatalf("prompt = %d bytes, want %d", len(got[0].Prompt), MaxPromptOverrideLen)
		}
	})
	if !strings.Contains(logged, "codexPromptOverrides[0].prompt") {
		t.Fatalf("truncating a prompt logged %q, want the field named", logged)
	}

	// A list that fits says nothing: a log line per load would train the
	// reader to ignore the one that matters.
	quiet := captureLog(t, func() {
		sanitizeDisabledTools("claudeDisabledTools", tools[:MaxDisabledTools])
		sanitizePromptOverrides("codexPromptOverrides", []PromptOverride{
			{Enabled: true, Models: []string{"m"}, Prompt: "p"},
		})
	})
	if quiet != "" {
		t.Fatalf("lists within every cap logged %q, want silence", quiet)
	}
}

// captureLog redirects the standard logger for the duration of fn and
// returns what it wrote.
func captureLog(t *testing.T, fn func()) string {
	t.Helper()

	var buf bytes.Buffer
	flags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(os.Stderr)
		log.SetFlags(flags)
	}()
	fn()
	return buf.String()
}

func TestValidateDisabledToolsRejectsFlagShapedAndBlankEntries(t *testing.T) {
	cases := []struct {
		name  string
		tools []string
	}{
		{name: "empty entry", tools: []string{"Workflow", "  "}},
		{name: "leading dash", tools: []string{"--dangerously-skip-permissions"}},
		{name: "whitespace", tools: []string{"Web Search"}},
		{name: "over length", tools: []string{strings.Repeat("T", MaxDisabledToolLen+1)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := validateDisabledTools("claudeDisabledTools", tc.tools); err == nil {
				t.Fatalf("validateDisabledTools(%v): want error", tc.tools)
			}
		})
	}

	got, err := validateDisabledTools("claudeDisabledTools", []string{" Workflow ", "Workflow"})
	if err != nil {
		t.Fatalf("validateDisabledTools() error = %v", err)
	}
	if len(got) != 1 || got[0] != "Workflow" {
		t.Fatalf("tools = %v, want one trimmed entry", got)
	}
}

// A hand-edited file is repaired, not rejected: one bad tool entry must
// not strand the rest of the settings file.
func TestSanitizeLoadedSettingsRepairsPromptOverrideFields(t *testing.T) {
	sanitized := sanitizeLoadedSettings(Settings{
		TimestampFormat:                  DefaultSettings.TimestampFormat,
		ClaudeDisabledTools:              []string{"Workflow", "-bad", "  ", "Workflow"},
		ClaudePromptOverrides:            []PromptOverride{{Enabled: true, Models: []string{" m ", "m"}, Prompt: "  p  "}},
		ClaudeAutoCompactStandardPercent: 90,
	})
	if got := sanitized.ClaudeDisabledTools; len(got) != 1 || got[0] != "Workflow" {
		t.Fatalf("disabled tools = %v, want the one valid entry", got)
	}
	entry := sanitized.ClaudePromptOverrides[0]
	if len(entry.Models) != 1 || entry.Models[0] != "m" || entry.Prompt != "p" {
		t.Fatalf("override = %+v, want normalized", entry)
	}
}

// Both providers are seeded with DIFFERENT values, so the accessors have to
// route rather than merely return something: a per-provider field read is
// indistinguishable from "always the Claude list" against a fixture that
// only fills one side.
func TestOverrideAccessorsRoutePerProviderAndShareTheClaudeListWithClaudeTUI(t *testing.T) {
	s := Settings{
		ClaudePromptOverrides: []PromptOverride{{Enabled: true, Models: []string{"claude-opus-5"}, Prompt: "claude p"}},
		CodexPromptOverrides:  []PromptOverride{{Enabled: true, Models: []string{"gpt-5.6-sol"}, Prompt: "codex p"}},
		ClaudeDisabledTools:   []string{"Workflow"},
		CodexDisabledTools:    []string{"web_search"},
	}
	for _, tc := range []struct {
		provider string
		prompt   string
		tool     string
	}{
		{provider: "claude", prompt: "claude p", tool: "Workflow"},
		{provider: "codex", prompt: "codex p", tool: "web_search"},
		// Same binary, same flags: the interactive TUI honors
		// --system-prompt-file and --disallowedTools exactly as headless
		// does, so it reads the Claude lists rather than none.
		{provider: "claude-tui", prompt: "claude p", tool: "Workflow"},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			got := s.PromptOverridesForProvider(tc.provider)
			if len(got) != 1 || got[0].Prompt != tc.prompt {
				t.Fatalf("%s overrides = %+v, want the %s list", tc.provider, got, tc.provider)
			}
			tools := s.DisabledToolsForProvider(tc.provider)
			if len(tools) != 1 || tools[0] != tc.tool {
				t.Fatalf("%s disabled tools = %v, want the %s list", tc.provider, tools, tc.provider)
			}
		})
	}

	// An unknown provider still gets nothing — the routing is a closed set,
	// not a fallback onto whichever list happens to be first.
	if got := s.PromptOverridesForProvider("nope"); got != nil {
		t.Fatalf("unknown-provider overrides = %v, want nil", got)
	}
	if got := s.DisabledToolsForProvider("nope"); got != nil {
		t.Fatalf("unknown-provider disabled tools = %v, want nil", got)
	}
}

// The nudge toggle routes like the tool lists (claude + claude-tui share
// the answer, codex never sees it), defaults off, survives a reload, and
// stays out of a sparse write while at the default.
func TestClaudeTodoRemindersDisabledRoutesAndRoundTrips(t *testing.T) {
	s := Settings{ClaudeTodoRemindersDisabled: true}
	for provider, want := range map[string]bool{
		"claude":     true,
		"claude-tui": true,
		"codex":      false,
		"nope":       false,
	} {
		if got := s.TodoRemindersDisabledForProvider(provider); got != want {
			t.Errorf("TodoRemindersDisabledForProvider(%q) = %v, want %v", provider, got, want)
		}
	}
	if DefaultSettings.ClaudeTodoRemindersDisabled {
		t.Fatal("DefaultSettings.ClaudeTodoRemindersDisabled = true, want false (nudges keep the vendor default)")
	}

	svc := NewService(t.TempDir())
	if svc.Get().ClaudeTodoRemindersDisabled {
		t.Fatal("fresh settings have reminders disabled, want the vendor default")
	}
	data, err := os.ReadFile(svc.Path())
	if err == nil && bytes.Contains(data, []byte("claudeTodoRemindersDisabled")) {
		t.Fatalf("sparse write carries the default key: %s", data)
	}

	updated, err := svc.Update(map[string]any{"claudeTodoRemindersDisabled": true})
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if !updated.ClaudeTodoRemindersDisabled {
		t.Fatal("ClaudeTodoRemindersDisabled = false after enabling")
	}
	if !NewService(filepath.Dir(svc.Path())).Get().ClaudeTodoRemindersDisabled {
		t.Fatal("reloaded ClaudeTodoRemindersDisabled = false, want true")
	}
}
