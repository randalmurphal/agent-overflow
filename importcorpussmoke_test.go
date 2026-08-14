//go:build !providersmoke

// The import-corpus smoke gate (`make import-corpus-smoke`).
//
// Every committed test of the session importer runs against synthetic
// fixtures: hand-written transcripts and rollouts under `t.TempDir()`. That
// is the right default — it is deterministic, it is fast, and it can never
// touch a provider home — but a fixture only knows the shapes whoever wrote
// it knew about. Real provider homes carry years of format drift: envelope
// types a Codex release added last month, transcript rows written by a CLI
// three versions back, tool results that were garbage-collected out from
// under their launch. This gate is the check that closes that gap. It runs
// the two provider readers and the store writer over a COPY of a developer's
// real `~/.claude` / `~/.codex` and reports what the corpus contains.
//
// It is manual and env-gated, not tagged, because it has to skip cleanly:
// `make go-test` compiles it and both legs skip in microseconds when the
// vars are unset. It spends no tokens and spawns nothing — the whole gate is
// a file read plus writes to a throwaway store under the test temp directory.
// A multi-gigabyte corpus can still take minutes, so the Make target keeps a
// generous twenty-minute safety ceiling. This is a pre-release/manual gate,
// not part of the default suite; the app's Import All path additionally uses
// bounded concurrency, while this diagnostic gate stays sequential so its
// per-provider timing and peak-memory reports remain reproducible.
//
// # Running it
//
//	# Claude: copy the home (or just its projects/ dir) somewhere else.
//	cp -a ~/.claude /tmp/claude-corpus
//	AO_IMPORT_CORPUS_CLAUDE=/tmp/claude-corpus make import-corpus-smoke
//
//	# Codex: copy the home, then repoint the thread index at the copy.
//	cp -a ~/.codex /tmp/codex-corpus
//	sqlite3 /tmp/codex-corpus/state_5.sqlite \
//	  "UPDATE threads SET rollout_path = replace(rollout_path, '$HOME/.codex', '/tmp/codex-corpus');"
//	AO_IMPORT_CORPUS_CODEX=/tmp/codex-corpus make import-corpus-smoke
//
// The Codex rewrite is not optional and not a workaround: `rollout_path` is
// an ABSOLUTE path written by Codex, and `rollout.PathInHome` refuses any row
// naming a file outside the home it was listed under. Without the rewrite
// every row in the copy points back at the live home, so the reader — quite
// correctly — skips all of them. The gate detects that state and prints this
// command rather than silently reporting an empty corpus.
//
// # Never the live files
//
// The corpus roots come from the two env vars and from nothing else: there is
// no `~/.claude` fallback anywhere in this file, and `corpusRoot` returns
// "unset" rather than guessing (`TestImportCorpusRootsComeOnlyFromTheEnvVars`).
// On top of that, `refuseLiveHome` FAILS the gate when a supplied root
// overlaps the real provider homes in either direction — is one of them, sits
// inside one, or contains one. That is a structural refusal, not advice in a
// comment: a run against the live homes would read gigabytes of private
// transcripts, and a reader bug that ever grew a write would do it to the
// files a live login depends on (root AGENTS.md §Permanent invariants).
// `liveProviderHomes` is the ONLY place here that consults the user's home
// directory, and it exists solely to have something to refuse.
//
// # What fails and what is merely reported
//
//   - HARD ERROR — a session that fails to list, load, convert, Build, or apply.
//     These are contract violations: the reader handed the writer something
//     it refuses, or the file is shaped in a way the reader cannot walk. The
//     gate fails, naming the file and the error.
//   - REPORTED — warnings, unknown envelope types, corrupt/oversized lines,
//     and the documented >1 GB transcript refusal. None of these fail: they
//     are what a real corpus legitimately contains. Drift shows up as new
//     codes and new unknown types in the summary tables, which is exactly the
//     signal this gate exists to produce.
//
// Sessions are processed strictly one at a time and released between — the
// two-pass Claude loader is used as designed, with Close — so the gate's
// memory ceiling is one session, not one corpus.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/provider/codex/rollout"
)

const (
	// claudeCorpusEnv / codexCorpusEnv are the ONLY inputs that can point
	// this gate at a corpus. Unset means skip; there is deliberately no
	// fallback to a resolved provider home.
	claudeCorpusEnv = "AO_IMPORT_CORPUS_CLAUDE"
	codexCorpusEnv  = "AO_IMPORT_CORPUS_CODEX"

	// corpusFailureLimit bounds how many individual hard errors are printed.
	// One systemic reader bug fails every session in a thousand-session
	// corpus, and a thousand identical stack-shaped messages hide the count
	// that actually matters.
	corpusFailureLimit = 25
)

func TestImportCorpusSmokeClaude(t *testing.T) {
	root, ok := requireCorpusRoot(t, claudeCorpusEnv)
	if !ok {
		return
	}
	projectsDir, err := claudeCorpusProjectsDir(root)
	if err != nil {
		t.Fatalf("import corpus (claude): %v", err)
	}
	if err := refuseLiveHome(projectsDir, liveProviderHomes()); err != nil {
		t.Fatalf("import corpus (claude): %v", err)
	}
	report := scanClaudeCorpus(t, projectsDir)
	report.log(t)
	report.settle(t)
}

func TestImportCorpusSmokeCodex(t *testing.T) {
	root, ok := requireCorpusRoot(t, codexCorpusEnv)
	if !ok {
		return
	}
	if err := requireCodexCorpusShape(root); err != nil {
		t.Fatalf("import corpus (codex): %v", err)
	}
	report := scanCodexCorpus(t, root)
	report.log(t)
	report.settle(t)
}

// --- corpus root resolution + the live-home refusal ---

// requireCorpusRoot resolves one env var into a usable corpus root, skipping
// the test when it is unset and failing when it is set to something this gate
// must not read.
func requireCorpusRoot(t *testing.T, env string) (string, bool) {
	t.Helper()
	root, ok, err := corpusRoot(env)
	if !ok {
		t.Skipf("%s is unset — this is the manual import-corpus gate; see the header of importcorpussmoke_test.go for how to build a corpus", env)
		return "", false
	}
	if err != nil {
		t.Fatalf("import corpus: %v", err)
	}
	if err := refuseLiveHome(root, liveProviderHomes()); err != nil {
		t.Fatalf("import corpus: %v", err)
	}
	return root, true
}

// corpusRoot reads one env var and validates that it names a directory.
//
// ok=false means the var is unset or blank, which is the skip case and NOT an
// error. There is no fallback: a gate that guessed a provider home when its
// var was missing would read the live files by default, which is the one
// thing this file may never do.
func corpusRoot(env string) (string, bool, error) {
	raw := strings.TrimSpace(os.Getenv(env))
	if raw == "" {
		return "", false, nil
	}
	abs, err := filepath.Abs(raw)
	if err != nil {
		return "", true, fmt.Errorf("%s=%q is not a usable path: %w", env, raw, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", true, fmt.Errorf("%s=%q: %w", env, raw, err)
	}
	if !info.IsDir() {
		return "", true, fmt.Errorf("%s=%q is not a directory", env, raw)
	}
	return abs, true, nil
}

// liveProviderHomes lists the provider homes this gate must refuse to read.
//
// This is the ONE function in this file that consults the user's home
// directory or provider-home env vars, and it does so only to have something
// to compare a supplied corpus root against — nothing here ever becomes a
// path the gate reads. The CLI-level relocations (`CLAUDE_CONFIG_DIR`,
// `CODEX_HOME`) are included because a developer who moved a home would
// otherwise get no protection at all for the home they actually use.
//
// A home that does not exist still contributes its path: the comparison is
// lexical after symlink resolution, and refusing a not-yet-created home costs
// nothing while missing one could cost a login.
func liveProviderHomes() []string {
	var homes []string
	if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		homes = append(homes, filepath.Join(home, ".claude"), filepath.Join(home, ".codex"))
	}
	for _, env := range []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME"} {
		if value := strings.TrimSpace(os.Getenv(env)); value != "" {
			homes = append(homes, value)
		}
	}
	return homes
}

// refuseLiveHome fails when root overlaps a live provider home in EITHER
// direction.
//
// All three overlap shapes are refusals, and only the first is obvious:
//
//   - root IS a live home (`~/.claude`) — reads live files;
//   - root is INSIDE a live home (`~/.claude/projects`) — reads live files,
//     and is the shape a developer is most likely to type by accident;
//   - root CONTAINS a live home (`~`, `/`) — the walk reaches them anyway.
//
// Comparison is on canonical paths so a symlinked corpus (or a symlinked
// home, which is how several dotfile managers lay one out) cannot slip past
// by spelling.
func refuseLiveHome(root string, liveHomes []string) error {
	canonicalRoot := gitops.CanonicalPath(root)
	for _, home := range liveHomes {
		canonicalHome := gitops.CanonicalPath(home)
		if !pathsOverlap(canonicalRoot, canonicalHome) {
			continue
		}
		return fmt.Errorf(
			"corpus root %q resolves to %q, which overlaps the live provider home %q — "+
				"this gate must never read live provider files; point it at a COPY "+
				"(see the header of importcorpussmoke_test.go)",
			root, canonicalRoot, canonicalHome)
	}
	return nil
}

// pathsOverlap reports whether two cleaned paths name the same location or
// one contains the other.
//
// Both sides get a trailing separator first, which is what makes the
// comparison a PATH containment test rather than a string one: without it
// `/home/dev/.claude-corpus` reads as living inside `/home/dev/.claude`, and
// the filesystem root (already separator-terminated) reads as containing
// nothing at all.
func pathsOverlap(a, b string) bool {
	a, b = withTrailingSeparator(a), withTrailingSeparator(b)
	return strings.HasPrefix(a, b) || strings.HasPrefix(b, a)
}

func withTrailingSeparator(path string) string {
	sep := string(filepath.Separator)
	if strings.HasSuffix(path, sep) {
		return path
	}
	return path + sep
}

// claudeCorpusProjectsDir accepts either shape a natural copy takes.
//
// `cp -a ~/.claude <root>` leaves the transcripts at `<root>/projects`, while
// someone copying only the part that matters lands them at `<root>` itself.
// Both are the same corpus, so both are accepted rather than making the
// developer remember which one this gate wanted.
func claudeCorpusProjectsDir(root string) (string, error) {
	nested := filepath.Join(root, "projects")
	if info, err := os.Stat(nested); err == nil && info.IsDir() {
		return nested, nil
	}
	// No projects/ subdirectory: the root must already be one. A Claude
	// projects directory holds one directory per project slug, so an empty
	// or file-only root is a corpus that was copied wrong.
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", fmt.Errorf("read corpus root %s: %w", root, err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return root, nil
		}
	}
	return "", fmt.Errorf(
		"%s holds neither a projects/ directory nor any project directories of its own — "+
			"copy ~/.claude (or ~/.claude/projects) there first", root)
}

// requireCodexCorpusShape confirms the Codex corpus carries the thread index.
// The rollout files are found THROUGH that index, so its absence is the whole
// corpus missing rather than a degraded one.
func requireCodexCorpusShape(root string) error {
	dbPath := filepath.Join(root, rollout.StateDBName)
	if _, err := os.Stat(dbPath); err != nil {
		return fmt.Errorf(
			"%s has no %s — copy ~/.codex there (thread index AND sessions/) first: %w",
			root, rollout.StateDBName, err)
	}
	return nil
}
