package claude

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/testutil/mockexec"
)

const getSettingsFullPayload = `{
  "effective": {"model": "claude-fable-5", "effortLevel": "high"},
  "sources": [
    {"source": "userSettings", "settings": {"effortLevel": "medium"}},
    {"source": "projectSettings", "settings": {"model": "claude-sonnet-5", "effortLevel": "low"}},
    {"source": "flagSettings", "settings": {"model": "claude-fable-5"}}
  ],
  "applied": {"model": "claude-fable-5", "effort": "high", "advisor": null, "ultracode": false},
  "errors": [{"file": "/repo/.claude/settings.local.json", "path": "permissions", "message": "unexpected token"}]
}`

func TestParseSettingsSnapshot(t *testing.T) {
	snapshot, err := ParseSettingsSnapshot(json.RawMessage(getSettingsFullPayload))
	if err != nil {
		t.Fatalf("ParseSettingsSnapshot: %v", err)
	}
	if snapshot.Applied == nil {
		t.Fatal("applied is nil, want the decoded object")
	}
	if snapshot.Applied.Model != "claude-fable-5" || snapshot.Applied.Effort != "high" {
		t.Fatalf("applied = %+v, want model claude-fable-5 / effort high", *snapshot.Applied)
	}
	if snapshot.Applied.Advisor != "" || snapshot.Applied.Ultracode {
		t.Fatalf("applied optional fields = %+v, want zero values for null/false", *snapshot.Applied)
	}
	if len(snapshot.Sources) != 3 || snapshot.Sources[1].Source != "projectSettings" {
		t.Fatalf("sources = %+v, want three in wire order", snapshot.Sources)
	}
	if len(snapshot.Errors) != 1 || snapshot.Errors[0].File != "/repo/.claude/settings.local.json" {
		t.Fatalf("errors = %+v, want the one parse error", snapshot.Errors)
	}
}

// TestParseSettingsSnapshotNullEffortIsAValue pins the difference between
// "this model has no effort tier" (explicit null — a real answer) and "this
// CLI cannot tell us what it resolved" (absent `applied` — a fallback
// condition). Collapsing the two would make an effortless model look like an
// unsupported CLI and silently re-enable the text-parse path.
func TestParseSettingsSnapshotNullEffortIsAValue(t *testing.T) {
	snapshot, err := ParseSettingsSnapshot(json.RawMessage(
		`{"effective":{},"sources":[],"applied":{"model":"claude-haiku-5","effort":null}}`))
	if err != nil {
		t.Fatalf("ParseSettingsSnapshot: %v", err)
	}
	if snapshot.Applied == nil {
		t.Fatal("applied is nil for an explicit null effort")
	}
	if snapshot.Applied.Effort != "" {
		t.Fatalf("effort = %q, want the empty string for null", snapshot.Applied.Effort)
	}

	older, err := ParseSettingsSnapshot(json.RawMessage(`{"effective":{},"sources":[]}`))
	if err != nil {
		t.Fatalf("ParseSettingsSnapshot(no applied): %v", err)
	}
	if older.Applied != nil {
		t.Fatalf("applied = %+v for a response without the key, want nil", older.Applied)
	}
}

// TestSettingsSnapshotProjectOverrides pins what counts as a project
// overriding AO's intent: only the two WORKSPACE-scoped layers, only keys AO
// itself requested, and never a `[1m]` context marker reading as a different
// model.
func TestSettingsSnapshotProjectOverrides(t *testing.T) {
	snapshot, err := ParseSettingsSnapshot(json.RawMessage(getSettingsFullPayload))
	if err != nil {
		t.Fatalf("ParseSettingsSnapshot: %v", err)
	}
	now := time.Now()

	notices := snapshot.ProjectOverrides("claude-fable-5", "high", nil, now)
	if len(notices) != 2 {
		t.Fatalf("notices = %+v, want the project model + effortLevel pair", notices)
	}
	for _, notice := range notices {
		if notice.Source != "projectSettings" {
			t.Fatalf("notice source = %q, want only workspace-scoped layers (userSettings/flagSettings must not report)", notice.Source)
		}
	}

	// The extended-context marker is AO's own spelling of the same model.
	marked := snapshot.ProjectOverrides("claude-sonnet-5[1m]", "low", nil, now)
	if len(marked) != 0 {
		t.Fatalf("notices = %+v, want none when the project agrees modulo the [1m] marker", marked)
	}

	// No stated intent, nothing to disagree with.
	if got := snapshot.ProjectOverrides("", "", nil, now); len(got) != 0 {
		t.Fatalf("notices = %+v, want none when AO requested nothing", got)
	}
}

// TestSettingsSnapshotProjectOverridesReadsAliasesFromCLIList pins that a
// project alias means whatever the CLI's own model list says it resolves to,
// and that an alias the list does not resolve is compared as written.
func TestSettingsSnapshotProjectOverridesReadsAliasesFromCLIList(t *testing.T) {
	snapshot, err := ParseSettingsSnapshot(json.RawMessage(
		`{"effective":{},"sources":[{"source":"projectSettings","settings":{"model":"opus[1m]"}}]}`))
	if err != nil {
		t.Fatalf("ParseSettingsSnapshot: %v", err)
	}
	now := time.Now()
	if got := snapshot.ProjectOverrides("claude-opus-5-5[1m]", "", map[string]string{"opus": "claude-opus-5-5"}, now); len(got) != 0 {
		t.Fatalf("notices = %+v, want none when the CLI resolves opus to the requested model", got)
	}
	for name, aliases := range map[string]map[string]string{
		"alias resolves elsewhere": {"opus": "claude-opus-5"},
		"no CLI list":              nil,
	} {
		got := snapshot.ProjectOverrides("claude-opus-5-5", "", aliases, now)
		if len(got) != 1 || got[0].Configured != "opus[1m]" {
			t.Fatalf("%s: notices = %+v, want the project model notice", name, got)
		}
	}
}

// TestSettingsSnapshotProjectOverridesIgnoresWrongTypes mirrors the CLI's own
// `.catch(void 0)` on a malformed settings value: a wrong-typed key means the
// same thing to it as no key, so it must not surface as an override.
func TestSettingsSnapshotProjectOverridesIgnoresWrongTypes(t *testing.T) {
	snapshot, err := ParseSettingsSnapshot(json.RawMessage(
		`{"effective":{},"sources":[{"source":"localSettings","settings":{"model":42,"effortLevel":"  "}}]}`))
	if err != nil {
		t.Fatalf("ParseSettingsSnapshot: %v", err)
	}
	if got := snapshot.ProjectOverrides("claude-fable-5", "high", nil, time.Now()); len(got) != 0 {
		t.Fatalf("notices = %+v, want none for a non-string model and a blank effortLevel", got)
	}
}

// getSettingsTestSession stands up a session whose fake CLI answers
// get_settings with responsePayload, or — when errMsg is non-empty — with a
// control_response error carrying it.
func getSettingsTestSession(t *testing.T, cfg Config, responsePayload, errMsg string) (*Session, string) {
	t.Helper()
	scriptPath := filepath.Join(t.TempDir(), "fake-claude")
	capturePath := filepath.Join(t.TempDir(), "stdin.ndjson")
	readyPath := filepath.Join(t.TempDir(), "ready")

	answer := `printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":` + responsePayload + `}}\n' "$reqid"`
	if errMsg != "" {
		answer = `printf '{"type":"control_response","response":{"subtype":"error","request_id":"%s","error":"` + errMsg + `"}}\n' "$reqid"`
	}
	// Shell builtins only, so no process starts between a request and its
	// answer; the ready file marks the script running.
	script := `#!/bin/sh
set -eu
capture="${CAPTURE_FILE:?}"
: > '` + readyPath + `'
while IFS= read -r line; do
    printf '%s\n' "$line" >> "$capture"
    reqid=${line#*'"request_id":"'}
    reqid=${reqid%%'"'*}
    case "$line" in
        *'"subtype":"get_settings"'*)
            ` + answer + `
            ;;
        *'"type":"control_request"'*)
            printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{}}}\n' "$reqid"
            ;;
    esac
done
`
	mockexec.Write(t, scriptPath, script)
	cfg.Binary = scriptPath
	if cfg.Env == nil {
		cfg.Env = map[string]string{}
	}
	cfg.Env["CAPTURE_FILE"] = capturePath

	s, err := NewSession(context.Background(), testThread, cfg, func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	s.controlRequestTimeout = 2 * time.Second
	// macOS assesses a freshly written executable on its first exec, which
	// can take over a second; the 2s window is for the answer, not that.
	waitForFakeCLI(t, readyPath)
	return s, capturePath
}

// TestGetSettingsRoundTrip covers the structured path end to end: the
// request reaches stdin, the response decodes, and the applied values plus
// the project-override notices land on the session's live state.
func TestGetSettingsRoundTrip(t *testing.T) {
	// Single-line payload — the fake CLI echoes it through printf.
	payload := `{"effective":{},"sources":[{"source":"projectSettings","settings":{"effortLevel":"low"}}],` +
		`"applied":{"model":"claude-fable-5","effort":"high","advisor":null}}`
	s, capturePath := getSettingsTestSession(t, Config{
		Model:           "claude-fable-5",
		ReasoningEffort: "high",
	}, payload, "")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	snapshot, err := s.GetSettings(ctx)
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if snapshot.Applied == nil || snapshot.Applied.Effort != "high" {
		t.Fatalf("applied = %+v, want effort high", snapshot.Applied)
	}

	lines := waitCapturedLines(t, capturePath, 1)
	var captured struct {
		Request struct {
			Subtype string `json:"subtype"`
		} `json:"request"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &captured); err != nil {
		t.Fatalf("unmarshal captured line: %v", err)
	}
	if captured.Request.Subtype != "get_settings" {
		t.Fatalf("captured subtype = %q, want get_settings", captured.Request.Subtype)
	}

	if applied := s.AppliedSettingsSnapshot(); applied == nil || applied.Model != "claude-fable-5" {
		t.Fatalf("AppliedSettingsSnapshot = %+v, want the recorded applied object", applied)
	}
	overrides := s.SettingsOverrides()
	if len(overrides) != 1 || overrides[0].Field != settingsKeyEffort || overrides[0].Configured != "low" {
		t.Fatalf("SettingsOverrides = %+v, want the project effortLevel=low notice", overrides)
	}
	if overrides[0].Requested != "high" {
		t.Fatalf("notice requested = %q, want the tier AO asked for", overrides[0].Requested)
	}
}

// TestGetSettingsReadsProjectAliasThroughSessionAliases covers the spawn
// plumbing: Config.ModelAliases is what the live read-back resolves a
// project alias through.
func TestGetSettingsReadsProjectAliasThroughSessionAliases(t *testing.T) {
	payload := `{"effective":{},"sources":[{"source":"projectSettings","settings":{"model":"opus"}}],` +
		`"applied":{"model":"claude-opus-5-5","effort":"high","advisor":null}}`
	for _, tc := range []struct {
		name    string
		aliases map[string]string
		notices int
	}{
		{"alias resolves to the requested model", map[string]string{"opus": "claude-opus-5-5"}, 0},
		{"alias resolves elsewhere", map[string]string{"opus": "claude-opus-5"}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := getSettingsTestSession(t, Config{
				Model:        "claude-opus-5-5",
				ModelAliases: tc.aliases,
			}, payload, "")
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if _, err := s.GetSettings(ctx); err != nil {
				t.Fatalf("GetSettings: %v", err)
			}
			if got := s.SettingsOverrides(); len(got) != tc.notices {
				t.Fatalf("SettingsOverrides = %+v, want %d notice(s)", got, tc.notices)
			}
		})
	}
}

// TestGetSettingsUnsupportedIsAskedOnce pins the fallback condition. A CLI
// without the subtype answers with an error, and re-asking on every
// confirmation would put a request known to fail on stdin before each one.
func TestGetSettingsUnsupportedIsAskedOnce(t *testing.T) {
	s, capturePath := getSettingsTestSession(t, Config{Model: "claude-fable-5"}, "",
		"Unsupported control request subtype: get_settings")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := s.GetSettings(ctx); !errors.Is(err, ErrGetSettingsUnsupported) {
		t.Fatalf("GetSettings error = %v, want ErrGetSettingsUnsupported", err)
	}
	if !s.GetSettingsUnsupported() {
		t.Fatal("session did not remember that get_settings is unsupported")
	}
	if _, err := s.GetSettings(ctx); !errors.Is(err, ErrGetSettingsUnsupported) {
		t.Fatalf("second GetSettings error = %v, want ErrGetSettingsUnsupported", err)
	}

	lines := capturedLinesBeforeSentinel(t, s, capturePath)
	if len(lines) != 1 {
		t.Fatalf("captured %d control_requests, want exactly one — the second call must not reach the wire: %v", len(lines), lines)
	}
}

// TestGetSettingsRealErrorIsNotUnsupported keeps a genuine failure from being
// mistaken for "this CLI is too old", which would permanently disable the
// structured path for the session on one transient error.
func TestGetSettingsRealErrorIsNotUnsupported(t *testing.T) {
	s, _ := getSettingsTestSession(t, Config{Model: "claude-fable-5"}, "", "settings file is unreadable")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := s.GetSettings(ctx)
	if err == nil {
		t.Fatal("GetSettings accepted an error response")
	}
	if errors.Is(err, ErrGetSettingsUnsupported) {
		t.Fatalf("error = %v, want a plain failure rather than the unsupported signal", err)
	}
	if s.GetSettingsUnsupported() {
		t.Fatal("a transient error must not disable the structured path for the session")
	}
}
