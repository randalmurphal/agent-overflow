package sessionfork

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestClaudeProjectDirNameMatchesObservedDirs pins the encoding to real
// directory names observed on disk under ~/.claude/projects, and to the spike
// that confirmed the CLI's sanitizePath replaces EVERY non-alphanumeric (not
// just separators): note the '.' in `.config` becoming a second dash, the
// underscores in a temp path collapsing to dashes, and the Windows-reserved
// colon. These are the <=200-char branch, where no hash is appended.
func TestClaudeProjectDirNameMatchesObservedDirs(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/Users/randy/repos/agent-overflow", "-Users-randy-repos-agent-overflow"},
		{"/home/rmurphy/repos/m32rimm", "-home-rmurphy-repos-m32rimm"},
		{
			"/home/rmurphy/.config/agent-overflow/worktrees/m32rimm/tenable-test-harness",
			"-home-rmurphy--config-agent-overflow-worktrees-m32rimm-tenable-test-harness",
		},
		{"/tmp/ao_spike_src", "-tmp-ao-spike-src"},
		{"/a/b:c", "-a-b-c"},
		// macOS canonicalizes /tmp to /private/tmp before encoding.
		{"/private/tmp/foo", "-private-tmp-foo"},
		{"/", "-"},
	}
	for _, c := range cases {
		if got := claudeProjectDirName(c.in); got != c.want {
			t.Errorf("claudeProjectDirName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// writeScanDecoy plants a same-named transcript under a DIFFERENT project
// dir, so a primary-lookup test actually proves the primary lookup.
//
// LocateSessionFile falls back to scanning every project dir, and
// os.ReadDir returns entries sorted by name — so a decoy whose slug sorts
// ahead of the real one is what the scan would answer with. Without it, a
// test asserting "the hashed dir was used" passes just as happily when the
// primary candidate misses and the scan finds the real file anyway.
//
// Every real slug starts with '-' followed by the first path component
// ("home", "tmp", "private", …); "-0" sorts ahead of all of them.
func writeScanDecoy(t *testing.T, home, sessionID string) string {
	t.Helper()
	decoyDir := filepath.Join(home, ".claude", "projects", "-0000-decoy-project")
	if err := os.MkdirAll(decoyDir, 0o700); err != nil {
		t.Fatalf("mkdir decoy project dir: %v", err)
	}
	decoy := filepath.Join(decoyDir, sessionID+".jsonl")
	if err := os.WriteFile(decoy, []byte("decoy\n"), 0o600); err != nil {
		t.Fatalf("write decoy session file: %v", err)
	}
	return decoy
}

func TestLocateSessionFile_PrimaryHit(t *testing.T) {
	// Build a fake ~/.claude/projects layout under TempDir, then point
	// HOME at it so projectsDir() resolves correctly.
	home := t.TempDir()
	t.Setenv("HOME", home)

	workspace := filepath.Join(home, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}

	canonical, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatalf("eval symlinks: %v", err)
	}
	abs, _ := filepath.Abs(canonical)
	slug := claudeProjectDirName(abs)

	projectDir := filepath.Join(home, ".claude", "projects", slug)
	if err := os.MkdirAll(projectDir, 0o700); err != nil {
		t.Fatalf("mkdir project dir: %v", err)
	}
	want := filepath.Join(projectDir, "abc123.jsonl")
	if err := os.WriteFile(want, []byte("hello\n"), 0o600); err != nil {
		t.Fatalf("write session file: %v", err)
	}
	decoy := writeScanDecoy(t, home, "abc123")

	got, err := LocateSessionFile("abc123", workspace)
	if err != nil {
		t.Fatalf("LocateSessionFile: %v", err)
	}
	if got == decoy {
		t.Fatalf("got the fallback-scan decoy %q — the workspace slug never resolved", got)
	}
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestLocateSessionFile_FallbackScan(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Create a JSONL under a project dir that does NOT correspond to
	// the workspace path we'll pass — exercises the fallback scan.
	otherDir := filepath.Join(home, ".claude", "projects", "-some-other-project")
	if err := os.MkdirAll(otherDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	want := filepath.Join(otherDir, "stray-uuid.jsonl")
	if err := os.WriteFile(want, []byte("hi\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Pass a workspace dir that doesn't have a matching project slug —
	// should fall back and find the stray file.
	workspace := filepath.Join(home, "elsewhere")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	got, err := LocateSessionFile("stray-uuid", workspace)
	if err != nil {
		t.Fatalf("LocateSessionFile: %v", err)
	}
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestLocateSessionFile_NotFound(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// Create the projects dir so ReadDir doesn't fail.
	if err := os.MkdirAll(filepath.Join(home, ".claude", "projects"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	_, err := LocateSessionFile("does-not-exist", "/any/workspace")
	if !errors.Is(err, ErrSessionFileNotFound) {
		t.Fatalf("err=%v, want ErrSessionFileNotFound", err)
	}
}

func TestLocateSessionFile_EmptySessionID(t *testing.T) {
	_, err := LocateSessionFile("", "/some/path")
	if err == nil {
		t.Fatal("expected error for empty sessionID")
	}
	if !strings.Contains(err.Error(), "sessionID") {
		t.Errorf("err=%v, want message about sessionID", err)
	}
}

// TestClaudeProjectDirName pins the Go port of Claude Code's project-dir
// encoder against ground truth produced by running the CLI's own JS. The
// minified 2.1.237 bundle (~/.local/share/claude/versions/2.1.237) has:
//
//	function Act(e){let t=0;for(let r=0;r<e.length;r++)t=(t<<5)-t+e.charCodeAt(r)|0;return t}
//	function y__(e){return Math.abs(Act(e)).toString(36)}
//	function z$o(e){return e.replace(/[^a-zA-Z0-9]/g,"-")}
//	function W9(e){let t=z$o(e);if(t.length<=kie)return t;return `${t.slice(0,kie)}-${y__(e)}`}   // kie=200
//
// The JS is the specification, so the expected hash suffixes below are what it
// prints, not a second Go derivation of the same arithmetic (which would share
// any bug with the code under test). Regenerate with:
//
//	node -e 'const A=e=>{let t=0;for(let r=0;r<e.length;r++)t=(t<<5)-t+e.charCodeAt(r)|0;return t},S=e=>e.replace(/[^a-zA-Z0-9]/g,"-"),W=e=>{const t=S(e);return t.length<=200?t:`${t.slice(0,200)}-${Math.abs(A(e)).toString(36)}`};for(const p of [...])console.log(W(p).slice(200))'
func TestClaudeProjectDirName(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{
			// Under the cap: the sanitized path verbatim, no suffix.
			"short",
			"/home/rmurphy/repos/agent-overflow",
			"-home-rmurphy-repos-agent-overflow",
		},
		{
			// Exactly at the cap: `<=` keeps it verbatim (off-by-one guard).
			"exactly-200",
			"/" + strings.Repeat("a", 199),
			"-" + strings.Repeat("a", 199),
		},
		{
			// One over: truncate the SANITIZED string to 200, then `-<hash>`.
			"201-truncates-and-hashes",
			"/" + strings.Repeat("a", 200),
			"-" + strings.Repeat("a", 199) + "-b6ymvl",
		},
		{
			// "é" is ONE UTF-16 code unit: five of them contribute five dashes
			// to the length and five charCodeAt values to the hash.
			"latin1-one-utf16-unit",
			"/" + strings.Repeat("\u00e9", 5) + "/" + strings.Repeat("a", 200),
			strings.Repeat("-", 7) + strings.Repeat("a", 193) + "-8e8lxx",
		},
		{
			// An astral rune is TWO UTF-16 code units, so it sanitizes to TWO
			// dashes and folds BOTH surrogates into the hash. A rune-wise port
			// gets this case wrong twice over: one dash short (which shifts the
			// 200-char cut) and one code unit short in the hash.
			"astral-two-utf16-units",
			"/\U0001F680/" + strings.Repeat("a", 200),
			strings.Repeat("-", 4) + strings.Repeat("a", 196) + "-zbh5cz",
		},
		{
			// Act lands on exactly int32 min here (path found by solving the
			// final code unit for that target). JS `Math.abs(-2147483648)` is
			// the DOUBLE 2147483648 — "zik0zk" — so a Go port that negates in
			// int32 would wrap back to the negative and render "-zik0zk",
			// producing a slug with a doubled separator.
			"hash-is-int32-min",
			"/" + strings.Repeat("a", 205) + "/004960700\ue4bb",
			"-" + strings.Repeat("a", 199) + "-zik0zk",
		},
	}
	for _, c := range cases {
		if got := claudeProjectDirName(c.in); got != c.want {
			t.Errorf("%s: claudeProjectDirName() =\n  %q\nwant\n  %q", c.name, got, c.want)
		}
	}
}

// TestClaudeProjectDirNameOverLengthShape asserts the SHAPE the truncate-and-
// hash branch hands back — `<200 sanitized chars>-<base36 hash>`, which means
// LocateSessionFile's primary candidate HITS for a long workspace instead of
// falling through to the scan. The hash VALUE is pinned against the CLI's own
// JS in TestClaudeProjectDirName.
func TestClaudeProjectDirNameOverLengthShape(t *testing.T) {
	long := "/" + strings.Repeat("ab/", 200) // sanitizes to ~600 chars
	got := claudeProjectDirName(long)
	if len(got) <= MaxSanitizedSlugLen {
		t.Fatalf("length = %d, want a hashed form longer than %d", len(got), MaxSanitizedSlugLen)
	}
	if !strings.HasPrefix(got, "-ab-ab-") {
		t.Errorf("unexpected truncated prefix %q", got)
	}
	if got[MaxSanitizedSlugLen] != '-' {
		t.Errorf("byte %d = %q, want the '-' joining prefix and hash", MaxSanitizedSlugLen, got[MaxSanitizedSlugLen])
	}
	if hash := got[MaxSanitizedSlugLen+1:]; hash == "" || strings.ContainsAny(hash, "-/") {
		t.Errorf("want `<200 sanitized chars>-<base36 hash>`, got %q", got)
	}
}

// TestClaudeProjectDirHashAbsIsUnsigned isolates the abs/base-36 rendering:
// every suffix is a bare base-36 magnitude, never a signed value. The int32-min
// input is the only one where int32 negation differs from int64 negation.
func TestClaudeProjectDirHashAbsIsUnsigned(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"empty-is-zero", "", "0"},
		{"negative-hash", "/" + strings.Repeat("a", 200), "b6ymvl"},
		{"int32-min", "/" + strings.Repeat("a", 205) + "/004960700\ue4bb", "zik0zk"},
	}
	for _, c := range cases {
		got := claudeProjectDirHash(c.in)
		if got != c.want {
			t.Errorf("%s: claudeProjectDirHash() = %q, want %q", c.name, got, c.want)
		}
		if strings.HasPrefix(got, "-") {
			t.Errorf("%s: hash suffix must be an unsigned magnitude, got %q", c.name, got)
		}
	}
}

// overLengthWorkspace makes a real directory under home whose canonical path
// sanitizes past MaxSanitizedSlugLen — the case that used to be reported as
// unresolvable.
func overLengthWorkspace(t *testing.T, home string) string {
	t.Helper()
	dir := home
	for i := 0; i < 24; i++ {
		dir = filepath.Join(dir, "segmentxx")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir over-length workspace: %v", err)
	}
	return dir
}

// TestExactWorkspaceSlugResolvesOverLengthPaths: an over-length workspace now
// resolves EXACTLY (ok=true) instead of being refused, and the answer carries
// the truncate-and-hash shape the CLI writes.
func TestExactWorkspaceSlugResolvesOverLengthPaths(t *testing.T) {
	home := t.TempDir()
	ws := overLengthWorkspace(t, home)

	slug, err := exactWorkspaceSlug(ws)
	if err != nil {
		t.Fatalf("exactWorkspaceSlug: %v", err)
	}
	canonical, err := filepath.EvalSymlinks(ws)
	if err != nil {
		t.Fatalf("evalsymlinks: %v", err)
	}
	abs, err := filepath.Abs(canonical)
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	if want := claudeProjectDirName(abs); slug != want {
		t.Fatalf("slug = %q, want %q", slug, want)
	}
	if len(slug) <= MaxSanitizedSlugLen {
		t.Fatalf("slug length = %d, want a truncate-and-hash form longer than %d", len(slug), MaxSanitizedSlugLen)
	}
	if slug[MaxSanitizedSlugLen] != '-' {
		t.Errorf("byte %d = %q, want the '-' joining prefix and hash", MaxSanitizedSlugLen, slug[MaxSanitizedSlugLen])
	}
	if hash := slug[MaxSanitizedSlugLen+1:]; hash == "" || strings.ContainsAny(hash, "-/") {
		t.Errorf("hash suffix = %q, want a non-empty base-36 magnitude", hash)
	}

	dir, err := WorkspaceProjectDir("/projects", ws)
	if err != nil {
		t.Fatalf("WorkspaceProjectDir: %v", err)
	}
	if want := filepath.Join("/projects", slug); dir != want {
		t.Errorf("WorkspaceProjectDir = %q, want %q", dir, want)
	}
}

// TestLocateSessionFile_PrimaryHitOverLengthWorkspace: the primary lookup now
// lands for an over-length workspace, so the whole-projects-dir scan is no
// longer the de-facto path there. The scan is deliberately made unable to
// answer (a decoy .jsonl under a different slug would be returned by it).
func TestLocateSessionFile_PrimaryHitOverLengthWorkspace(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	ws := overLengthWorkspace(t, home)

	slug, err := exactWorkspaceSlug(ws)
	if err != nil {
		t.Fatalf("exactWorkspaceSlug: %v", err)
	}
	projectDir := filepath.Join(home, ".claude", "projects", slug)
	if err := os.MkdirAll(projectDir, 0o700); err != nil {
		t.Fatalf("mkdir project dir: %v", err)
	}
	want := filepath.Join(projectDir, "overlong-session.jsonl")
	if err := os.WriteFile(want, []byte("hello\n"), 0o600); err != nil {
		t.Fatalf("write session file: %v", err)
	}
	decoy := writeScanDecoy(t, home, "overlong-session")

	got, err := LocateSessionFile("overlong-session", ws)
	if err != nil {
		t.Fatalf("LocateSessionFile: %v", err)
	}
	if got == decoy {
		t.Fatalf("got the fallback-scan decoy %q — the truncate-and-hash slug never resolved", got)
	}
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
