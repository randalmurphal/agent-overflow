package codex

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// skillsListWireSample is hand-written from the Rust types at
// codex-rs/app-server-protocol/src/protocol/v2/plugin.rs
// (rust-v0.146.0-alpha.4): SkillsListResponse{data: SkillsListEntry{cwd,
// skills, errors}}, SkillMetadata{name, description, shortDescription?,
// interface?, dependencies?, path, scope, enabled}, and
// SkillInterface{displayName?, shortDescription?, iconSmall?, iconLarge?,
// brandColor?, defaultPrompt?}. Scope is snake_case
// (`#[serde(rename_all = "snake_case")]` on SkillScope), everything else is
// camelCase.
const skillsListWireSample = `{"data":[{
  "cwd":"/repo",
  "skills":[
    {"name":"code-review",
     "description":"Reviews a diff",
     "shortDescription":"legacy short",
     "interface":{"displayName":"Code Review","shortDescription":"Review a diff",
                  "iconSmall":"/repo/.codex/skills/code-review/small.png",
                  "iconLarge":null,"brandColor":"#ff0000",
                  "defaultPrompt":"Review my working tree"},
     "dependencies":{"tools":[{"type":"mcp","value":"github"}]},
     "path":"/repo/.codex/skills/code-review/SKILL.md",
     "scope":"repo","enabled":true},
    {"name":"changelog",
     "description":"Writes a changelog",
     "path":"/home/u/.codex/skills/changelog/SKILL.md",
     "scope":"user","enabled":false}
  ],
  "errors":[{"path":"/repo/.codex/skills/broken","message":"missing SKILL.md"}]
}]}`

func TestParseSkillsListProjectsEveryConsumedField(t *testing.T) {
	entries, err := parseSkillsList(json.RawMessage(skillsListWireSample))
	if err != nil {
		t.Fatalf("parseSkillsList: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	entry := entries[0]
	if entry.Cwd != "/repo" {
		t.Errorf("Cwd = %q, want /repo", entry.Cwd)
	}
	if len(entry.Skills) != 2 {
		t.Fatalf("skills = %d, want 2", len(entry.Skills))
	}

	first := entry.Skills[0]
	if first.Name != "code-review" || first.Path != "/repo/.codex/skills/code-review/SKILL.md" {
		t.Errorf("first skill identity = %+v", first)
	}
	if first.Description != "Reviews a diff" {
		t.Errorf("Description = %q", first.Description)
	}
	// SKILL.json's interface wins over the legacy top-level field, per
	// upstream's own comment on SkillMetadata.short_description.
	if first.ShortDescription != "Review a diff" {
		t.Errorf("ShortDescription = %q, want the interface value to win over %q", first.ShortDescription, "legacy short")
	}
	if first.DisplayName != "Code Review" {
		t.Errorf("DisplayName = %q", first.DisplayName)
	}
	if first.DefaultPrompt != "Review my working tree" {
		t.Errorf("DefaultPrompt = %q", first.DefaultPrompt)
	}
	if first.Scope != "repo" || !first.Enabled {
		t.Errorf("scope/enabled = %q/%v, want repo/true", first.Scope, first.Enabled)
	}

	second := entry.Skills[1]
	// No interface block: the legacy short description is absent too, so
	// the fields simply stay empty rather than inventing a label.
	if second.DisplayName != "" || second.DefaultPrompt != "" || second.ShortDescription != "" {
		t.Errorf("interface-less skill invented fields: %+v", second)
	}
	// A disabled skill is still returned so a UI can show it as off.
	if second.Enabled {
		t.Errorf("second skill Enabled = true, want the wire's false")
	}
	if second.Scope != "user" {
		t.Errorf("second scope = %q, want user", second.Scope)
	}

	if len(entry.Errors) != 1 || entry.Errors[0].Message != "missing SKILL.md" {
		t.Fatalf("load errors = %+v, want the wire's one entry carried through", entry.Errors)
	}
}

func TestParseSkillsListUsesLegacyShortDescriptionWhenInterfaceHasNone(t *testing.T) {
	entries, err := parseSkillsList(json.RawMessage(
		`{"data":[{"cwd":"/repo","skills":[{"name":"s","description":"d",` +
			`"shortDescription":"legacy","interface":{"displayName":"S"},` +
			`"path":"/p/SKILL.md","scope":"user","enabled":true}],"errors":[]}]}`,
	))
	if err != nil {
		t.Fatalf("parseSkillsList: %v", err)
	}
	if got := entries[0].Skills[0].ShortDescription; got != "legacy" {
		t.Fatalf("ShortDescription = %q, want the legacy field when the interface omits it", got)
	}
}

func TestParseSkillsListDropsUninvocableSkills(t *testing.T) {
	// Both name and path are required to invoke a skill (the `$name` text
	// token and the structured input's path), so an entry missing either
	// must not reach a menu.
	entries, err := parseSkillsList(json.RawMessage(
		`{"data":[{"cwd":"/repo","skills":[` +
			`{"name":"","description":"d","path":"/p/SKILL.md","scope":"user","enabled":true},` +
			`{"name":"n","description":"d","path":"","scope":"user","enabled":true},` +
			`{"name":"ok","description":"d","path":"/p/SKILL.md","scope":"user","enabled":true}` +
			`],"errors":[]}]}`,
	))
	if err != nil {
		t.Fatalf("parseSkillsList: %v", err)
	}
	if len(entries[0].Skills) != 1 || entries[0].Skills[0].Name != "ok" {
		t.Fatalf("skills = %+v, want only the invocable one", entries[0].Skills)
	}
}

func TestParseSkillsListRejectsEmptyAndMalformedBodies(t *testing.T) {
	if _, err := parseSkillsList(nil); err == nil {
		t.Fatal("empty response must be an error, not an empty skill list")
	}
	if _, err := parseSkillsList(json.RawMessage(`{"data":`)); err == nil {
		t.Fatal("malformed response must be an error")
	}
}

func TestBuildSkillsListParamsRequiresAbsoluteCwds(t *testing.T) {
	if _, _, err := buildSkillsListParams(nil, false); err == nil {
		t.Fatal("empty cwds must be rejected: the wire's default is a property of whichever process answers")
	}
	if _, _, err := buildSkillsListParams([]string{"  "}, false); err == nil {
		t.Fatal("blank cwd must be rejected")
	}
	if _, _, err := buildSkillsListParams([]string{"relative/path"}, false); err == nil {
		t.Fatal("relative cwd must be rejected: upstream resolves it against the answering process's cwd")
	}

	params, cleaned, err := buildSkillsListParams([]string{" /repo ", "/other"}, false)
	if err != nil {
		t.Fatalf("buildSkillsListParams: %v", err)
	}
	if len(cleaned) != 2 || cleaned[0] != "/repo" || cleaned[1] != "/other" {
		t.Fatalf("cleaned = %#v, want trimmed absolute paths", cleaned)
	}
	// forceReload is omitted rather than sent false so an ordinary read
	// leaves the app-server's own skill cache alone.
	if _, present := params["forceReload"]; present {
		t.Fatalf("params = %#v, want no forceReload key on an ordinary read", params)
	}

	params, _, err = buildSkillsListParams([]string{"/repo"}, true)
	if err != nil {
		t.Fatalf("buildSkillsListParams forceReload: %v", err)
	}
	if params["forceReload"] != true {
		t.Fatalf("params = %#v, want forceReload true", params)
	}
}

func TestSkillsListParamsRoundTripToTheWireShape(t *testing.T) {
	params, _, err := buildSkillsListParams([]string{"/repo"}, true)
	if err != nil {
		t.Fatalf("buildSkillsListParams: %v", err)
	}
	encoded, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got, want := string(encoded), `{"cwds":["/repo"],"forceReload":true}`; got != want {
		t.Fatalf("params JSON = %s, want %s", got, want)
	}
}

func TestSessionListSkillsSendsTheWireFrameAndDecodesTheReply(t *testing.T) {
	s, capturePath := newCapturingSession(t, "codex-thread-skills")

	type result struct {
		entries []struct {
			cwd   string
			names []string
		}
		err error
	}
	done := make(chan result, 1)
	go func() {
		entries, err := s.ListSkills(context.Background(), []string{"/repo"}, false)
		var out result
		out.err = err
		for _, entry := range entries {
			names := make([]string, 0, len(entry.Skills))
			for _, skill := range entry.Skills {
				names = append(names, skill.Name)
			}
			out.entries = append(out.entries, struct {
				cwd   string
				names []string
			}{cwd: entry.Cwd, names: names})
		}
		done <- out
	}()

	newPendingAnswerer(s).answer(t, `{"data":[{"cwd":"/repo","skills":[
		{"name":"code-review","description":"d","path":"/p/SKILL.md","scope":"repo","enabled":true}
	],"errors":[]}]}`)

	got := <-done
	if got.err != nil {
		t.Fatalf("ListSkills: %v", got.err)
	}
	if len(got.entries) != 1 || got.entries[0].cwd != "/repo" ||
		len(got.entries[0].names) != 1 || got.entries[0].names[0] != "code-review" {
		t.Fatalf("ListSkills = %+v", got.entries)
	}

	frames := waitForCapturedRawFrames(t, capturePath, 1, backgroundTerminalTestTimeout)
	var frame struct {
		Method string `json:"method"`
		Params struct {
			Cwds        []string `json:"cwds"`
			ForceReload *bool    `json:"forceReload"`
			ThreadID    string   `json:"threadId"`
		} `json:"params"`
	}
	if err := json.Unmarshal(frames[0], &frame); err != nil {
		t.Fatalf("decode captured frame: %v", err)
	}
	if frame.Method != skillsListMethod {
		t.Errorf("method = %q, want %q", frame.Method, skillsListMethod)
	}
	if len(frame.Params.Cwds) != 1 || frame.Params.Cwds[0] != "/repo" {
		t.Errorf("cwds = %#v, want [/repo]", frame.Params.Cwds)
	}
	if frame.Params.ForceReload != nil {
		t.Errorf("forceReload = %v, want omitted", *frame.Params.ForceReload)
	}
	// skills/list is global; sending a threadId would be inventing a
	// parameter the method does not take.
	if frame.Params.ThreadID != "" {
		t.Errorf("threadId = %q, want none — skills/list is global", frame.Params.ThreadID)
	}
}

func TestSessionListSkillsRejectsBadCwdsBeforeWriting(t *testing.T) {
	s, capturePath := newCapturingSession(t, "codex-thread-skills-guard")
	if _, err := s.ListSkills(context.Background(), nil, false); err == nil {
		t.Fatal("ListSkills with no cwds must fail")
	}
	if _, err := s.ListSkills(context.Background(), []string{"rel"}, false); err == nil {
		t.Fatal("ListSkills with a relative cwd must fail")
	}
	if frames := readCapturedRawFrames(t, capturePath); len(frames) != 0 {
		t.Fatalf("rejected requests still wrote %d frames", len(frames))
	}
}

func TestSkillsFetcherRejectsMisconfigurationWithoutSpawning(t *testing.T) {
	// A missing binary must be a caller error, never a spawn attempt: the
	// repo forbids tests reaching a real provider binary, and production
	// wants the same specific message rather than an exec failure.
	fetcher := &SkillsFetcher{}
	if _, err := fetcher.Fetch(context.Background(), []string{"/repo"}, false); err == nil ||
		!strings.Contains(err.Error(), "binary path required") {
		t.Fatalf("Fetch without a binary = %v, want a binary-required error", err)
	}
	fetcher = &SkillsFetcher{Binary: "/definitely/not/a/real/codex"}
	if _, err := fetcher.Fetch(context.Background(), nil, false); err == nil ||
		!strings.Contains(err.Error(), "at least one cwd") {
		t.Fatalf("Fetch without cwds = %v, want a cwd-required error", err)
	}
}

func TestSkillsFetcherDrivesAFakeAppServer(t *testing.T) {
	binary := writeSkillsListFakeCodex(t)
	fetcher := &SkillsFetcher{Binary: binary, WorkDir: t.TempDir(), Timeout: 10 * time.Second}

	entries, err := fetcher.Fetch(context.Background(), []string{"/repo"}, true)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(entries) != 1 || len(entries[0].Skills) != 1 ||
		entries[0].Skills[0].Name != "code-review" {
		t.Fatalf("Fetch = %+v", entries)
	}
	if entries[0].Skills[0].DefaultPrompt != "Review my working tree" {
		t.Fatalf("DefaultPrompt = %q", entries[0].Skills[0].DefaultPrompt)
	}
}

func TestSkillsChangedIsConsumedAndNeverOptedOut(t *testing.T) {
	// The opt-out list is the complement of what this package consumes, so
	// claiming skills/changed as a side channel must be what keeps it
	// subscribed — with no second edit to remember.
	if !notificationMethodConsumed(skillsChangedMethod) {
		t.Fatal("skills/changed must count as consumed")
	}
	for _, method := range sessionOptOutNotificationMethods() {
		if method == skillsChangedMethod {
			t.Fatal("skills/changed must not appear in the initialize opt-out list")
		}
	}
}

func TestDispatchSkillsChangedFiresTheHandler(t *testing.T) {
	s := &Session{threadID: testThread}
	fired := 0
	s.SetSkillsChangedHandler(func() { fired++ })

	// Params are ignored by design (upstream types the notification as an
	// empty struct); both shapes must reach the handler.
	s.dispatchNotification(skillsChangedMethod, json.RawMessage(`{}`))
	s.dispatchNotification(skillsChangedMethod, nil)
	if fired != 2 {
		t.Fatalf("handler fired %d times, want 2", fired)
	}

	// Transition coverage: clearing the handler must stop delivery rather
	// than panic on the nil.
	s.SetSkillsChangedHandler(nil)
	s.dispatchNotification(skillsChangedMethod, json.RawMessage(`{}`))
	if fired != 2 {
		t.Fatalf("handler fired %d times after being cleared, want 2", fired)
	}
}

func writeSkillsListFakeCodex(t *testing.T) string {
	t.Helper()
	script := `#!/usr/bin/env bash
set -euo pipefail
while IFS= read -r line; do
  id="$(printf '%s\n' "$line" | sed -n 's/.*"id":[[:space:]]*\([0-9][0-9]*\).*/\1/p')"
  if [[ "$line" == *'"method":"initialize"'* ]]; then
    printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id"
  elif [[ "$line" == *'"method":"skills/list"'* ]]; then
    printf '{"jsonrpc":"2.0","id":%s,"result":{"data":[{"cwd":"/repo","skills":[{"name":"code-review","description":"Reviews a diff","interface":{"displayName":"Code Review","defaultPrompt":"Review my working tree"},"path":"/repo/.codex/skills/code-review/SKILL.md","scope":"repo","enabled":true}],"errors":[]}]}}\n' "$id"
  fi
done
`
	return writeExecutable(t, script)
}
