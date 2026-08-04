package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"agent-overflow/internal/codexskills"
	"agent-overflow/internal/provider"
)

func TestSkillsEntryForCwdPicksTheRequestedDirectory(t *testing.T) {
	entries := []codexskills.CwdSkills{
		{Cwd: "/other", Skills: []codexskills.Skill{{Name: "wrong", Path: "/p"}}},
		{Cwd: "/repo", Skills: []codexskills.Skill{{Name: "right", Path: "/p"}}},
	}
	got, err := skillsEntryForCwd(entries, "/repo")
	if err != nil {
		t.Fatalf("skillsEntryForCwd: %v", err)
	}
	if len(got.Skills) != 1 || got.Skills[0].Name != "right" {
		t.Fatalf("got %+v, want the /repo entry", got)
	}
}

func TestSkillsEntryForCwdToleratesOneNormalisedEntry(t *testing.T) {
	// A single-cwd request has exactly one answer, so a server that
	// normalised the path (trailing separator, symlinked root) is
	// unambiguous.
	entries := []codexskills.CwdSkills{
		{Cwd: "/repo/", Skills: []codexskills.Skill{{Name: "s", Path: "/p"}}},
	}
	got, err := skillsEntryForCwd(entries, "/repo")
	if err != nil {
		t.Fatalf("skillsEntryForCwd: %v", err)
	}
	if len(got.Skills) != 1 {
		t.Fatalf("got %+v, want the single entry", got)
	}
}

func TestSkillsEntryForCwdRefusesAnAmbiguousResponse(t *testing.T) {
	// Returning an empty list here would present a server disagreement as
	// "this workspace has no skills".
	entries := []codexskills.CwdSkills{{Cwd: "/a"}, {Cwd: "/b"}}
	if _, err := skillsEntryForCwd(entries, "/repo"); err == nil {
		t.Fatal("a response with no matching entry must be an error")
	}
	if _, err := skillsEntryForCwd(nil, "/repo"); err == nil {
		t.Fatal("an empty response must be an error")
	}
}

func TestCodexSkillsForWorkspaceRejectsUnusableWorkspacePaths(t *testing.T) {
	app := &App{}
	if _, err := app.codexSkillsForWorkspace(context.Background(), "  ", false); err == nil {
		t.Fatal("blank workspace path must be rejected")
	}
	// Relative paths resolve against whichever process answers, so they
	// must never reach the wire.
	if _, err := app.codexSkillsForWorkspace(context.Background(), "relative/path", false); err == nil {
		t.Fatal("relative workspace path must be rejected")
	}
}

func TestReadCodexSkillsFallsBackAndRefusesAnUnconfiguredBinary(t *testing.T) {
	// No live Codex session and no configured binary: the fallback path is
	// reached and reports a specific, actionable failure rather than
	// attempting a spawn.
	app := &App{}
	_, err := app.readCodexSkills(context.Background(), "", "/repo", false)
	if err == nil || !strings.Contains(err.Error(), "binary not configured") {
		t.Fatalf("readCodexSkills = %v, want a binary-not-configured error", err)
	}
}

func TestNormalizeCodexBinaryFoldsTheUnsetSetting(t *testing.T) {
	// codex.NewSession defaults an empty Config.Binary to "codex", so the
	// setting's empty value and a session's recorded "codex" must not look
	// like two different builds.
	if normalizeCodexBinary("") != "codex" || normalizeCodexBinary("  ") != "codex" {
		t.Fatal("empty binary must fold onto the NewSession default")
	}
	if normalizeCodexBinary(" /usr/bin/codex ") != "/usr/bin/codex" {
		t.Fatal("a configured binary must be trimmed, not rewritten")
	}
}

// Transition coverage for the app-level invalidation edge: a
// skills/changed that arrives with nothing cached is a no-op, and one that
// arrives after a populated read forces the next read to refetch.
func TestHandleCodexSkillsChangedInvalidatesAndSurvivesAnEmptyCache(t *testing.T) {
	app := &App{}
	app.handleCodexSkillsChanged() // nothing cached yet

	key := codexskills.Key("codex", "/repo")
	calls := 0
	fetch := func(context.Context) (codexskills.CwdSkills, error) {
		calls++
		return codexskills.CwdSkills{Cwd: "/repo"}, nil
	}
	if _, err := app.codexSkills().Get(context.Background(), key, fetch); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, err := app.codexSkills().Get(context.Background(), key, fetch); err != nil {
		t.Fatalf("second Get: %v", err)
	}
	if calls != 1 {
		t.Fatalf("fetch calls = %d, want 1 before invalidation", calls)
	}

	app.handleCodexSkillsChanged()
	if _, err := app.codexSkills().Get(context.Background(), key, fetch); err != nil {
		t.Fatalf("Get after skills/changed: %v", err)
	}
	if calls != 2 {
		t.Fatalf("fetch calls = %d, want 2 after skills/changed", calls)
	}
}

func TestCodexSkillsCacheDoesNotConvertAFailureIntoAnEmptyList(t *testing.T) {
	app := &App{}
	wantErr := errors.New("app-server exited")
	_, err := app.codexSkills().Get(context.Background(), codexskills.Key("codex", "/repo"),
		func(context.Context) (codexskills.CwdSkills, error) {
			return codexskills.CwdSkills{}, wantErr
		})
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want the fetch error", err)
	}
}

// TestGetCodexSkillsRejectsUnusableWorkspacePathsAtTheBinding — the wire entry
// point must refuse the same paths the helper does. A relative path resolves
// against whichever process answers, so it can never reach the wire.
func TestGetCodexSkillsRejectsUnusableWorkspacePathsAtTheBinding(t *testing.T) {
	app := &App{}
	if _, err := app.GetCodexSkills(context.Background(), "  ", false); err == nil {
		t.Fatal("blank workspace path must be rejected")
	}
	if _, err := app.GetCodexSkills(context.Background(), "relative/path", false); err == nil {
		t.Fatal("relative workspace path must be rejected")
	}
}

// TestGetCodexSkillsReturnsAllocatedSlices — an empty skill list is a real
// answer here (the error return is what says a read failed), so it must reach
// a non-nullable frontend field as [] rather than null.
func TestGetCodexSkillsReturnsAllocatedSlices(t *testing.T) {
	app := newTestAppWithStore(t)
	cwd := t.TempDir()

	// Prime the cache under the key the binding will compute, so the read is
	// served without any spawn. A miss here would try to start a process and
	// fail — which is exactly what makes this a real assertion about the
	// key the binding uses.
	key := codexskills.Key(app.providerBinaryPath(string(provider.Codex)), cwd)
	if _, err := app.codexSkills().Get(context.Background(), key, func(context.Context) (codexskills.CwdSkills, error) {
		return codexskills.CwdSkills{Cwd: cwd}, nil
	}); err != nil {
		t.Fatalf("prime cache: %v", err)
	}

	got, err := app.GetCodexSkills(context.Background(), cwd, false)
	if err != nil {
		t.Fatalf("GetCodexSkills: %v", err)
	}
	if got.Skills == nil || got.Errors == nil {
		t.Fatalf("GetCodexSkills = %+v, want allocated (empty) slices", got)
	}
	if got.Cwd != cwd {
		t.Fatalf("Cwd = %q, want %q", got.Cwd, cwd)
	}
}
