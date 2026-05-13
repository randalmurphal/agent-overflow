package pathlinks

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// seedWorkspace writes the given relative paths as empty files inside
// a fresh temp dir and returns the dir as the workspace root. Used by
// every test that needs a "real" file to validate against.
func seedWorkspace(t *testing.T, relPaths ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, p := range relPaths {
		abs := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(abs), err)
		}
		if err := os.WriteFile(abs, nil, 0o644); err != nil {
			t.Fatalf("write %s: %v", abs, err)
		}
	}
	return root
}

func TestExtractAndValidate(t *testing.T) {
	t.Run("empty text returns nil", func(t *testing.T) {
		got := ExtractAndValidate("", "")
		if got != nil {
			t.Fatalf("expected nil, got %#v", got)
		}
	})

	t.Run("matches a simple relative path with extension", func(t *testing.T) {
		ws := seedWorkspace(t, "src/lib/foo.ts")
		got := ExtractAndValidate(ws, "see src/lib/foo.ts for context")
		if len(got) != 1 || got[0].Path != "src/lib/foo.ts" {
			t.Fatalf("expected one ref for src/lib/foo.ts, got %#v", got)
		}
	})

	t.Run("captures line and col", func(t *testing.T) {
		ws := seedWorkspace(t, "src/lib/foo.ts")
		got := ExtractAndValidate(ws, "error in src/lib/foo.ts:42:7 today")
		if len(got) != 1 {
			t.Fatalf("expected one ref, got %#v", got)
		}
		if got[0].Line != 42 || got[0].Col != 7 {
			t.Fatalf("expected line=42 col=7, got %#v", got[0])
		}
	})

	t.Run("absolute path inside workspace validates", func(t *testing.T) {
		ws := seedWorkspace(t, "abs.ts")
		abs := filepath.Join(ws, "abs.ts")
		got := ExtractAndValidate(ws, "crashed at "+abs+":10")
		if len(got) != 1 || got[0].Path != abs || got[0].Line != 10 {
			t.Fatalf("expected one ref for absolute path with line=10, got %#v", got)
		}
	})

	t.Run("absolute path outside workspace is rejected", func(t *testing.T) {
		// Existence oracle defense: agent prose containing
		// `/etc/passwd` (which definitely exists on a Linux host)
		// must not validate, even though os.Stat would succeed.
		ws := seedWorkspace(t, "inside.ts")
		got := ExtractAndValidate(ws, "see /etc/passwd for the user list")
		if got != nil {
			t.Fatalf("expected nil for out-of-workspace absolute path, got %#v", got)
		}
	})

	t.Run("rejects nonexistent paths even when shape matches", func(t *testing.T) {
		ws := seedWorkspace(t, "src/lib/foo.ts")
		got := ExtractAndValidate(ws, "see foo/bar.nonsense or imaginary/x.ts")
		if got != nil {
			t.Fatalf("expected nil, got %#v", got)
		}
	})

	t.Run("rejects URLs", func(t *testing.T) {
		ws := seedWorkspace(t)
		got := ExtractAndValidate(ws, "see https://example.com/foo for docs")
		if got != nil {
			t.Fatalf("expected nil for URL, got %#v", got)
		}
	})

	t.Run("rejects scoped npm packages", func(t *testing.T) {
		ws := seedWorkspace(t)
		got := ExtractAndValidate(ws, "install @sveltejs/kit and use it")
		if got != nil {
			t.Fatalf("expected nil for scoped package, got %#v", got)
		}
	})

	t.Run("rejects emails", func(t *testing.T) {
		ws := seedWorkspace(t)
		got := ExtractAndValidate(ws, "contact user@example.com please")
		if got != nil {
			t.Fatalf("expected nil for email, got %#v", got)
		}
	})

	t.Run("rejects bare module names without slashes", func(t *testing.T) {
		ws := seedWorkspace(t)
		got := ExtractAndValidate(ws, "we use the marked package")
		if got != nil {
			t.Fatalf("expected nil for bare module, got %#v", got)
		}
	})

	t.Run("rejects directory-shaped paths without filename extension", func(t *testing.T) {
		ws := seedWorkspace(t, "path/to/dir/x.ts")
		got := ExtractAndValidate(ws, "look in path/to/dir please")
		if got != nil {
			t.Fatalf("expected nil for directory-only path, got %#v", got)
		}
	})

	t.Run("rejects trailing-dot tokens", func(t *testing.T) {
		// Regression: `something/else.` used to linkify because the
		// heuristic only required ANY dot in the final segment.
		ws := seedWorkspace(t)
		got := ExtractAndValidate(ws, "look in something/else. please")
		if got != nil {
			t.Fatalf("expected nil for trailing-dot token, got %#v", got)
		}
	})

	t.Run("rejects version strings", func(t *testing.T) {
		ws := seedWorkspace(t)
		got := ExtractAndValidate(ws, "upgrade lib/1.2.3")
		if got != nil {
			t.Fatalf("expected nil for version-string token, got %#v", got)
		}
	})

	t.Run("multi-dot filenames pass (config.json)", func(t *testing.T) {
		ws := seedWorkspace(t, "src/app/my.config.json")
		got := ExtractAndValidate(ws, "edit src/app/my.config.json next")
		if len(got) != 1 || got[0].Path != "src/app/my.config.json" {
			t.Fatalf("expected one ref for my.config.json, got %#v", got)
		}
	})

	t.Run("finds multiple distinct paths in one string", func(t *testing.T) {
		ws := seedWorkspace(t, "src/a.ts", "src/b.ts", "src/c.ts")
		got := ExtractAndValidate(ws, "edit src/a.ts:1 then src/b.ts:2:3 and finally src/c.ts")
		if len(got) != 3 {
			t.Fatalf("expected three refs, got %#v", got)
		}
		paths := []string{got[0].Path, got[1].Path, got[2].Path}
		expect := []string{"src/a.ts", "src/b.ts", "src/c.ts"}
		for i := range expect {
			if paths[i] != expect[i] {
				t.Fatalf("expected paths %v, got %v", expect, paths)
			}
		}
		if got[0].Line != 1 || got[1].Line != 2 || got[1].Col != 3 {
			t.Fatalf("line/col mismatch: %#v", got)
		}
	})

	t.Run("matches paths inside parentheses", func(t *testing.T) {
		ws := seedWorkspace(t, "src/lib/foo.ts")
		got := ExtractAndValidate(ws, "see (src/lib/foo.ts:5) for the bug")
		if len(got) != 1 || got[0].Line != 5 {
			t.Fatalf("expected one ref with line=5, got %#v", got)
		}
	})

	t.Run("matches paths inside backticks", func(t *testing.T) {
		ws := seedWorkspace(t, "src/lib/foo.ts")
		got := ExtractAndValidate(ws, "check `src/lib/foo.ts:42` for bug")
		if len(got) != 1 || got[0].Line != 42 {
			t.Fatalf("expected one ref with line=42, got %#v", got)
		}
	})

	t.Run("matches paths inside double quotes", func(t *testing.T) {
		ws := seedWorkspace(t, "src/lib/foo.ts")
		got := ExtractAndValidate(ws, `Edited "src/lib/foo.ts" successfully`)
		if len(got) != 1 {
			t.Fatalf("expected one ref, got %#v", got)
		}
	})

	t.Run("rejects path embedded inside URL", func(t *testing.T) {
		ws := seedWorkspace(t, "path/to/foo.bar")
		got := ExtractAndValidate(ws, "see https://x.com/path/to/foo.bar")
		if got != nil {
			t.Fatalf("expected nil for URL-embedded path, got %#v", got)
		}
	})

	t.Run("rejects path right after `@` of an email", func(t *testing.T) {
		// `name@host/path.ts` — even when host/path.ts exists, the
		// preceding context says this is an email, not a path.
		ws := seedWorkspace(t, "host/path.ts")
		got := ExtractAndValidate(ws, "user@host/path.ts contacted us")
		if got != nil {
			t.Fatalf("expected nil for email-tail path, got %#v", got)
		}
	})

	t.Run("accepts @-prefixed mention when boundary is safe", func(t *testing.T) {
		ws := seedWorkspace(t, "src/foo.ts")
		got := ExtractAndValidate(ws, "see @src/foo.ts:1 next")
		if len(got) != 1 || got[0].Path != "src/foo.ts" || got[0].Line != 1 {
			t.Fatalf("expected one ref for src/foo.ts with line=1, got %#v", got)
		}
	})

	t.Run("accepts leading `./` relative path", func(t *testing.T) {
		ws := seedWorkspace(t, "src/foo.ts")
		got := ExtractAndValidate(ws, "see ./src/foo.ts")
		if len(got) != 1 || got[0].Path != "./src/foo.ts" {
			t.Fatalf("expected one ref for ./src/foo.ts, got %#v", got)
		}
	})

	t.Run("rejects `../` traversal that escapes workspace", func(t *testing.T) {
		// Workspace-escape guard: a real file outside the workspace
		// must not validate, even when the regex captures the path
		// shape and the file exists on disk. Same rule click-time
		// `editor.ResolvePath` enforces, applied at validation time
		// so the link never gets emitted.
		root := t.TempDir()
		ws := filepath.Join(root, "ws")
		if err := os.MkdirAll(ws, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		sib := filepath.Join(root, "sibling")
		if err := os.MkdirAll(sib, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(sib, "x.ts"), nil, 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		got := ExtractAndValidate(ws, "relative ../sibling/x.ts:1")
		if got != nil {
			t.Fatalf("expected nil for escape-out path, got %#v", got)
		}
	})

	t.Run("rejects deep `../../etc/passwd`-style traversal", func(t *testing.T) {
		// Regression guard for the existence oracle that an
		// unbounded `..`-escape would otherwise create.
		ws := seedWorkspace(t)
		got := ExtractAndValidate(ws, "see ../../../etc/passwd for the list")
		if got != nil {
			t.Fatalf("expected nil for deep traversal, got %#v", got)
		}
	})

	t.Run("repeated mentions de-dup the stat call but emit per-occurrence refs", func(t *testing.T) {
		ws := seedWorkspace(t, "src/foo.ts")
		statCalls := map[string]int{}
		countingStat := func(p string) bool {
			statCalls[p]++
			_, err := os.Stat(p)
			return err == nil
		}
		text := "we edited src/foo.ts, but src/foo.ts still has the bug; src/foo.ts again"
		got := extractAndValidate(ws, text, countingStat)
		if len(got) != 3 {
			t.Fatalf("expected three per-occurrence refs, got %#v", got)
		}
		// Resolved path should be stat'd exactly once.
		resolved := filepath.Join(ws, "src/foo.ts")
		if statCalls[resolved] != 1 {
			t.Fatalf("expected exactly one stat call for %s, got %d (%#v)", resolved, statCalls[resolved], statCalls)
		}
	})

	t.Run("missing workspacePath drops everything", func(t *testing.T) {
		// Without a workspace root we can't run the boundary check,
		// so we can't tell `/etc/passwd` from a real in-project
		// file. Drop all candidates rather than fall back to
		// unbounded os.Stat.
		ws := seedWorkspace(t, "abs.ts")
		abs := filepath.Join(ws, "abs.ts")
		got := ExtractAndValidate("", "rel: src/foo.ts and abs: "+abs)
		if got != nil {
			t.Fatalf("expected nil with empty workspace, got %#v", got)
		}
	})

	t.Run("non-canonical workspacePath drops everything", func(t *testing.T) {
		// Defensive: a caller passing `/tmp/ws/..//ws/` would
		// otherwise have `filepath.Rel` behave unpredictably.
		ws := seedWorkspace(t, "src/foo.ts")
		got := ExtractAndValidate(ws+"/.", "see src/foo.ts please")
		if got != nil {
			t.Fatalf("expected nil for non-canonical workspace, got %#v", got)
		}
	})

	t.Run("caps candidate count under hostile fan-out", func(t *testing.T) {
		// DoS defense: a hostile prose blob crafted to produce
		// thousands of validly-shaped candidates must not stat each
		// one. Build a workspace with one real file, then a 1000-
		// path message of mostly-bogus shapes; assert stat is called
		// no more than maxCandidates times.
		ws := seedWorkspace(t, "src/real.ts")
		var b strings.Builder
		for i := 0; i < 1000; i++ {
			fmt.Fprintf(&b, "see src/fake%d.ts and ", i)
		}
		b.WriteString("the real one src/real.ts")
		statCalls := 0
		countingStat := func(p string) bool {
			statCalls++
			_, err := os.Stat(p)
			return err == nil
		}
		got := extractAndValidate(ws, b.String(), countingStat)
		if statCalls > maxCandidates {
			t.Fatalf("expected at most %d stat calls, got %d", maxCandidates, statCalls)
		}
		// Whether `src/real.ts` lands in the kept slice depends on
		// where it falls within the first maxCandidates matches —
		// the test only asserts the cap, not which paths survive.
		_ = got
	})

	t.Run("returned refs follow source-text order", func(t *testing.T) {
		ws := seedWorkspace(t, "src/a.ts", "src/b.ts")
		got := ExtractAndValidate(ws, "src/b.ts comes second after src/a.ts? no, first")
		if len(got) != 2 {
			t.Fatalf("expected two refs, got %#v", got)
		}
		if got[0].Path != "src/b.ts" || got[1].Path != "src/a.ts" {
			t.Fatalf("expected order [src/b.ts, src/a.ts], got %#v", got)
		}
	})
}
