package sessionfork

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// placeSession writes <id>.jsonl under the project slug for workspace and
// returns the file path. workspace must already exist (its canonical path is
// what Claude — and our slug encoder — key on).
func placeSession(t *testing.T, home, workspace, id, content string) string {
	t.Helper()
	canonical, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatalf("evalsymlinks %s: %v", workspace, err)
	}
	abs, err := filepath.Abs(canonical)
	if err != nil {
		t.Fatalf("abs %s: %v", canonical, err)
	}
	dir := filepath.Join(home, ".claude", "projects", claudeProjectDirName(abs))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	p := filepath.Join(dir, id+".jsonl")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

func TestRelocateSessionCopiesTranscriptAndSubdir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Use a source workspace with '.' and '_' so the slug encoding is
	// genuinely exercised (these are the chars the naive encoder missed).
	srcWS := filepath.Join(home, "wt.feature_x")
	dstWS := filepath.Join(home, "project-root")
	for _, d := range []string{srcWS, dstWS} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	const id = "sess-relocate-1"
	srcFile := placeSession(t, home, srcWS, id, "line1\nline2\n")
	// A subagent transcript in the sibling <id>/ subdir must survive the move.
	srcSubAgent := filepath.Join(filepath.Dir(srcFile), id, "subagents", "agent-1.jsonl")
	if err := os.MkdirAll(filepath.Dir(srcSubAgent), 0o700); err != nil {
		t.Fatalf("mkdir subagent: %v", err)
	}
	if err := os.WriteFile(srcSubAgent, []byte("sub\n"), 0o600); err != nil {
		t.Fatalf("write subagent: %v", err)
	}
	// Force a group/other-readable source mode so the clamp is observable
	// regardless of the test runner's umask: the relocated copy must come back
	// 0o600, never wider than the transcript itself.
	if err := os.Chmod(srcSubAgent, 0o644); err != nil {
		t.Fatalf("chmod subagent: %v", err)
	}

	_, destFile, err := RelocateSession(ProjectsDirForHome(home), id, srcWS, dstWS)
	if err != nil {
		t.Fatalf("RelocateSession: %v", err)
	}

	// The destination must be where a resume run with cwd == dstWS looks.
	located, err := LocateSessionFile(ProjectsDirForHome(home), id, dstWS)
	if err != nil {
		t.Fatalf("LocateSessionFile from dest: %v", err)
	}
	if located != destFile {
		t.Errorf("LocateSessionFile = %q, want relocated %q", located, destFile)
	}
	if got, _ := os.ReadFile(destFile); string(got) != "line1\nline2\n" {
		t.Errorf("relocated content = %q, want transcript copied verbatim", got)
	}
	// Subagent subdir copied.
	destSub := filepath.Join(filepath.Dir(destFile), id, "subagents", "agent-1.jsonl")
	if got, err := os.ReadFile(destSub); err != nil || string(got) != "sub\n" {
		t.Errorf("subagent transcript not relocated: content=%q err=%v", got, err)
	}
	// Clamped to owner-rw even though the source was 0o644.
	if st, err := os.Stat(destSub); err != nil {
		t.Errorf("stat relocated subagent: %v", err)
	} else if perm := st.Mode().Perm(); perm != 0o600 {
		t.Errorf("relocated subagent perm = %#o, want 0o600 (never wider than the transcript)", perm)
	}
	// RelocateSession copies; deleting the source is the caller's separate
	// post-commit step (RemoveSessionTranscript), so the original stays put here.
	if _, err := os.Stat(srcFile); err != nil {
		t.Errorf("source transcript should remain after RelocateSession copy: %v", err)
	}
}

func TestRelocateSessionIdempotent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	srcWS := filepath.Join(home, "src")
	dstWS := filepath.Join(home, "dst")
	for _, d := range []string{srcWS, dstWS} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	const id = "sess-idem"
	placeSession(t, home, srcWS, id, "payload\n")

	_, first, err := RelocateSession(ProjectsDirForHome(home), id, srcWS, dstWS)
	if err != nil {
		t.Fatalf("first relocate: %v", err)
	}
	_, second, err := RelocateSession(ProjectsDirForHome(home), id, srcWS, dstWS)
	if err != nil {
		t.Fatalf("second relocate (overwrites, same result): %v", err)
	}
	if first != second {
		t.Errorf("idempotent relocate paths differ: %q vs %q", first, second)
	}
}

func TestRelocateSessionAlreadyAtDestNoOp(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dstWS := filepath.Join(home, "dst-only")
	if err := os.MkdirAll(dstWS, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	const id = "sess-at-dest"
	const body = "the-live-transcript\n"
	atDest := placeSession(t, home, dstWS, id, body)

	src, got, err := RelocateSession(ProjectsDirForHome(home), id, dstWS, dstWS)
	if err != nil {
		t.Fatalf("RelocateSession: %v", err)
	}
	if src != got {
		t.Errorf("already-at-dest must report no move: src %q != dest %q", src, got)
	}
	if got != atDest {
		t.Errorf("got %q, want existing %q", got, atDest)
	}
	// The src == dest short-circuit must leave the live transcript untouched —
	// it must NOT self-copy it through copyFile (open + temp + rename over
	// itself), which would needlessly churn a multi-MB file and risk truncation
	// on a future copyFile bug. Re-read and assert the content is intact.
	if content, rerr := os.ReadFile(atDest); rerr != nil || string(content) != body {
		t.Errorf("live transcript content = %q (err %v), want intact %q", content, rerr, body)
	}
}

func TestRelocateSessionMissingSourceReturnsNotFound(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dstWS := filepath.Join(home, "dst")
	if err := os.MkdirAll(filepath.Join(home, ".claude", "projects"), 0o700); err != nil {
		t.Fatalf("mkdir projects: %v", err)
	}
	if err := os.MkdirAll(dstWS, 0o700); err != nil {
		t.Fatalf("mkdir dst: %v", err)
	}
	if _, _, err := RelocateSession(ProjectsDirForHome(home), "ghost-session", filepath.Join(home, "src"), dstWS); !errors.Is(err, ErrSessionFileNotFound) {
		t.Fatalf("err = %v, want ErrSessionFileNotFound (no silent fresh session)", err)
	}
}

// TestRelocateSessionOverLengthDestResolves: a destination whose sanitized slug
// exceeds MaxSanitizedSlugLen used to be a hard refusal ("Bun.hash we can't
// reproduce"). Since the encoder ports the CLI's truncate-and-hash exactly, the
// relocation lands in the very directory `claude --resume` will read from there.
func TestRelocateSessionOverLengthDestResolves(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	srcWS := filepath.Join(home, "src")
	if err := os.MkdirAll(srcWS, 0o700); err != nil {
		t.Fatalf("mkdir src: %v", err)
	}
	const id = "sess-overlong"
	srcPlaced := placeSession(t, home, srcWS, id, "p\n")

	// Build a destination whose absolute path sanitizes beyond 200 chars.
	deep := filepath.Join(home, "deep")
	for i := 0; i < 24; i++ {
		deep = filepath.Join(deep, "segmentxx")
	}
	if err := os.MkdirAll(deep, 0o700); err != nil {
		t.Fatalf("mkdir deep: %v", err)
	}

	src, dest, err := RelocateSession(ProjectsDirForHome(home), id, srcWS, deep)
	if err != nil {
		t.Fatalf("RelocateSession: %v", err)
	}
	if src != srcPlaced {
		t.Errorf("srcFile = %q, want %q", src, srcPlaced)
	}
	slug, err := exactWorkspaceSlug(deep)
	if err != nil {
		t.Fatalf("exactWorkspaceSlug(%q): %v", deep, err)
	}
	if len(slug) <= MaxSanitizedSlugLen {
		t.Fatalf("destination slug is not over-length (%d chars) — fixture no longer covers the case", len(slug))
	}
	want := filepath.Join(home, ".claude", "projects", slug, id+".jsonl")
	if dest != want {
		t.Errorf("destFile = %q, want %q", dest, want)
	}
	if got, rerr := os.ReadFile(dest); rerr != nil || string(got) != "p\n" {
		t.Errorf("transcript not copied to the hashed slug: content=%q err=%v", got, rerr)
	}
}

// TestRelocateSessionSubagentCopyFailureReturnsSentinel: when the transcript
// relocates but its sibling <id>/ subagent subdir can't be copied, the call
// returns the working destination path AND an error that errors.Is
// ErrSubagentCopyIncomplete — so the caller keeps resume alive while surfacing
// the partial history loss, rather than treating it as a hard relocation
// failure.
func TestRelocateSessionSubagentCopyFailureReturnsSentinel(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	srcWS := filepath.Join(home, "src")
	dstWS := filepath.Join(home, "dst")
	for _, d := range []string{srcWS, dstWS} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	const id = "sess-subfail"
	srcFile := placeSession(t, home, srcWS, id, "transcript\n")
	// A subagent subdir on the source so RelocateSession attempts the tree copy.
	srcSub := filepath.Join(filepath.Dir(srcFile), id, "agent.jsonl")
	if err := os.MkdirAll(filepath.Dir(srcSub), 0o700); err != nil {
		t.Fatalf("mkdir src subagent: %v", err)
	}
	if err := os.WriteFile(srcSub, []byte("sub\n"), 0o600); err != nil {
		t.Fatalf("write src subagent: %v", err)
	}

	// Block ONLY the subagent copy: pre-create the destination <id>/ path as a
	// regular file so copyTree's MkdirAll fails, while <id>.jsonl still copies.
	slug, err := exactWorkspaceSlug(dstWS)
	if err != nil {
		t.Fatalf("exactWorkspaceSlug(%q): %v", dstWS, err)
	}
	destDir := filepath.Join(home, ".claude", "projects", slug)
	if err := os.MkdirAll(destDir, 0o700); err != nil {
		t.Fatalf("mkdir destDir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(destDir, id), []byte("blocker\n"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}

	_, destFile, err := RelocateSession(ProjectsDirForHome(home), id, srcWS, dstWS)
	if !errors.Is(err, ErrSubagentCopyIncomplete) {
		t.Fatalf("err = %v, want ErrSubagentCopyIncomplete", err)
	}
	// Resume must still work: the transcript relocated despite the subagent
	// failure, and the returned path points at the relocated transcript.
	wantDest := filepath.Join(destDir, id+".jsonl")
	if destFile != wantDest {
		t.Errorf("destFile = %q, want %q", destFile, wantDest)
	}
	if got, rerr := os.ReadFile(destFile); rerr != nil || string(got) != "transcript\n" {
		t.Errorf("transcript not relocated despite subagent failure: content=%q err=%v", got, rerr)
	}
}

// TestRelocateSessionOverwritesStaleDestination is the switch-back regression: a
// thread that previously visited a workspace left a now-stale transcript under
// that slug. Relocation must OVERWRITE it with the authoritative source, never
// no-op on the existing file — a no-op would resume the stale history and
// silently drop every turn made since (the data-loss trap that made move
// semantics necessary).
func TestRelocateSessionOverwritesStaleDestination(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	srcWS := filepath.Join(home, "current")
	dstWS := filepath.Join(home, "return-trip")
	for _, d := range []string{srcWS, dstWS} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	const id = "sess-stale"
	// A stale copy already at the destination (older, fewer turns)...
	placeSession(t, home, dstWS, id, "turn1\n")
	// ...and the authoritative source carrying more turns.
	placeSession(t, home, srcWS, id, "turn1\nturn2\nturn3\n")

	_, dest, err := RelocateSession(ProjectsDirForHome(home), id, srcWS, dstWS)
	if err != nil {
		t.Fatalf("RelocateSession: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read relocated: %v", err)
	}
	if string(got) != "turn1\nturn2\nturn3\n" {
		t.Errorf("destination = %q, want the authoritative source content (stale copy must be overwritten, not preserved)", got)
	}
}

// TestRemoveSessionTranscript covers the DELETE half of a move: the transcript
// and its sibling <id>/ subagent subdir are both removed, a missing file is not
// an error (idempotent), and anything that isn't an absolute "<id>.jsonl" path is
// refused so RemoveAll can never be steered at a slug directory.
func TestRemoveSessionTranscript(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	ws := filepath.Join(home, "ws")
	if err := os.MkdirAll(ws, 0o700); err != nil {
		t.Fatalf("mkdir ws: %v", err)
	}
	const id = "sess-remove"
	jsonl := placeSession(t, home, ws, id, "x\n")
	subdir := filepath.Join(filepath.Dir(jsonl), id)
	if err := os.MkdirAll(subdir, 0o700); err != nil {
		t.Fatalf("mkdir subdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(subdir, "agent.jsonl"), []byte("s\n"), 0o600); err != nil {
		t.Fatalf("write sub: %v", err)
	}

	if err := RemoveSessionTranscript(jsonl); err != nil {
		t.Fatalf("RemoveSessionTranscript: %v", err)
	}
	if _, err := os.Stat(jsonl); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("transcript still present: %v", err)
	}
	if _, err := os.Stat(subdir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("subagent subdir still present: %v", err)
	}
	// Idempotent: removing an already-gone transcript is not an error.
	if err := RemoveSessionTranscript(jsonl); err != nil {
		t.Errorf("second remove should be a no-op, got %v", err)
	}
	// Guard: a path that isn't a session JSONL (here the slug dir itself) is
	// refused, so a derived RemoveAll can't wipe the directory.
	if err := RemoveSessionTranscript(filepath.Dir(jsonl)); err == nil {
		t.Error("expected refusal for a non-JSONL path")
	}
	// Guard: a relative path is refused.
	if err := RemoveSessionTranscript("relative/" + id + ".jsonl"); err == nil {
		t.Error("expected refusal for a relative path")
	}
	// Guard: traversal-token basenames must be refused, NOT silently followed.
	// "...jsonl" trims to id="..", whose sibling subdir Cleans to the whole
	// projects dir; "..jsonl" trims to id=".", the slug dir itself. Either would
	// turn the os.RemoveAll into a directory wipe. Plant real dirs at both blast
	// targets and assert they survive the refusal.
	projects := filepath.Join(home, ".claude", "projects")
	slugDir := filepath.Dir(jsonl)
	for _, bad := range []string{
		filepath.Join(slugDir, "...jsonl"), // id ".." -> grandparent (projects dir)
		filepath.Join(slugDir, "..jsonl"),  // id "."  -> slug dir
	} {
		if err := RemoveSessionTranscript(bad); err == nil {
			t.Errorf("expected refusal for traversal-token path %q", bad)
		}
	}
	if _, err := os.Stat(projects); err != nil {
		t.Errorf("projects dir must survive a traversal-token refusal: %v", err)
	}
	if _, err := os.Stat(slugDir); err != nil {
		t.Errorf("slug dir must survive a traversal-token refusal: %v", err)
	}
}
