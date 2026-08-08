//go:build !providersmoke

package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// importcorpussmoke_fixture_test.go — committed coverage for the corpus
// runner itself.
//
// The gate in importcorpussmoke_test.go only runs when a developer points it
// at a real corpus, which means its own logic — the env contract, the
// live-home refusal, the two-shape Claude root, the per-session loop, the
// histograms — would otherwise ship untested. These tests run on every
// `make go-test` against the hand-written provider home the App-level import
// tests already use (`app_session_import_fixture_test.go`), so the runner is
// exercised end to end without a byte of real session data.
//
// The negative half matters more than the positive half: a refusal that
// silently stopped firing would turn the gate into the thing it exists to
// prevent.

// importCorpusSecondCodexThread gives the fixture corpus two Codex sessions,
// so the per-session loop is exercised as a loop rather than as a single call.
const importCorpusSecondCodexThread = "dddddddd-4444-4444-8444-dddddddddddd"

// TestImportCorpusRunnerReadsAFixtureCorpus drives both legs over a corpus
// with known contents: two Claude transcripts (one linear, one two-leaf) and
// two Codex rollouts, one of which carries a line the reader cannot know and
// a line it cannot decode.
func TestImportCorpusRunnerReadsAFixtureCorpus(t *testing.T) {
	home := newImportHome(t)
	home.claudeLinearSession(t, importFixtureClaudeSession)
	home.claudeBranchedSession(t, importFixtureClaudeBranchy)
	home.codexLinearSession(t, importFixtureCodexThread)
	home.codexLinearSession(t, importCorpusSecondCodexThread)
	home.writeCodexIndex(t, importFixtureCodexThread, importCorpusSecondCodexThread)
	// One unknown envelope type and one undecodable line, so the report's
	// drift tables are asserted against something rather than assumed.
	home.appendCodexRollout(t, importCorpusSecondCodexThread,
		codexFixtureLine(t, 200, "ao_smoke_unknown_type", map[string]any{"anything": true}),
		`{"timestamp":`,
	)

	t.Run("claude", func(t *testing.T) {
		// The nested shape: `cp -a ~/.claude <root>` leaves projects/ inside.
		projectsDir, err := claudeCorpusProjectsDir(filepath.Join(home.root, ".claude"))
		if err != nil {
			t.Fatalf("resolve projects dir: %v", err)
		}
		if want := filepath.Join(home.root, ".claude", "projects"); projectsDir != want {
			t.Fatalf("projects dir = %q, want %q", projectsDir, want)
		}
		report := scanClaudeCorpus(t, projectsDir)
		report.log(t)
		if report.failures != 0 {
			t.Fatalf("fixture corpus produced %d hard errors, want 0", report.failures)
		}
		if report.listed != 2 || report.ok != 2 {
			t.Fatalf("listed=%d ok=%d, want 2/2", report.listed, report.ok)
		}
		// One leaf in the linear session, two in the branched one. Converting
		// per branch is the memory contract; a runner that only converted the
		// active branch would report 2 here.
		if report.branches != 3 {
			t.Fatalf("branches = %d, want 3", report.branches)
		}
		if report.events == 0 || report.rows == 0 {
			t.Fatalf("events=%d rows=%d, want both non-zero — Build must not be skipped",
				report.events, report.rows)
		}
	})

	t.Run("codex", func(t *testing.T) {
		if err := requireCodexCorpusShape(home.codexHome()); err != nil {
			t.Fatalf("corpus shape: %v", err)
		}
		report := scanCodexCorpus(t, home.codexHome())
		report.log(t)
		if report.failures != 0 {
			t.Fatalf("fixture corpus produced %d hard errors, want 0", report.failures)
		}
		if report.listed != 2 || report.ok != 2 {
			t.Fatalf("listed=%d ok=%d, want 2/2", report.listed, report.ok)
		}
		if report.events == 0 || report.rows == 0 {
			t.Fatalf("events=%d rows=%d, want both non-zero — Build must not be skipped",
				report.events, report.rows)
		}
		// The drift signals are REPORTED, never fatal: this is the assertion
		// that an unknown type stays a table row instead of becoming a
		// failure the next time Codex ships a new envelope.
		if report.corruptLines != 1 {
			t.Errorf("corrupt lines = %d, want 1", report.corruptLines)
		}
		if !histogramMentions(report.unknownTypes, "ao_smoke_unknown_type") {
			t.Errorf("unknown types = %v, want an entry naming ao_smoke_unknown_type", report.unknownTypes)
		}
	})
}

// TestImportCorpusRootsComeOnlyFromTheEnvVars is the isolation assertion: an
// unset variable resolves to NOTHING, never to a provider home.
//
// HOME is pointed at a fixture home that really does contain `.claude` and
// `.codex`, so a fallback would have something to find. It must still find
// nothing — the two env vars are the only inputs, which is what keeps
// `make go-test` from ever walking a developer's sessions.
func TestImportCorpusRootsComeOnlyFromTheEnvVars(t *testing.T) {
	home := newImportHome(t)
	t.Setenv("HOME", home.root)
	t.Setenv("USERPROFILE", home.root)

	for _, env := range []string{claudeCorpusEnv, codexCorpusEnv} {
		t.Setenv(env, "")
		root, ok, err := corpusRoot(env)
		if ok || root != "" || err != nil {
			t.Fatalf("corpusRoot(%s) with the var unset = (%q, %v, %v), want (\"\", false, nil)",
				env, root, ok, err)
		}
	}
}

// TestImportCorpusRefusesTheLiveProviderHomes pins the refusal in both the
// pure predicate and the wiring that supplies the real homes to it.
func TestImportCorpusRefusesTheLiveProviderHomes(t *testing.T) {
	live := []string{"/home/dev/.claude", "/home/dev/.codex"}
	cases := []struct {
		name   string
		root   string
		refuse bool
	}{
		{name: "is the live claude home", root: "/home/dev/.claude", refuse: true},
		{name: "inside the live claude home", root: "/home/dev/.claude/projects", refuse: true},
		{name: "is the live codex home", root: "/home/dev/.codex", refuse: true},
		{name: "contains both live homes", root: "/home/dev", refuse: true},
		{name: "contains everything", root: "/", refuse: true},
		{name: "a sibling copy", root: "/home/dev/.claude-corpus", refuse: false},
		{name: "a temp copy", root: "/tmp/claude-corpus", refuse: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := refuseLiveHome(tc.root, live)
			if tc.refuse && err == nil {
				t.Fatalf("refuseLiveHome(%q) = nil, want a refusal", tc.root)
			}
			if !tc.refuse && err != nil {
				t.Fatalf("refuseLiveHome(%q) = %v, want nil", tc.root, err)
			}
		})
	}
}

// TestImportCorpusRefusalReadsTheRealHomes closes the loop the table above
// cannot: that `liveProviderHomes` actually names the homes the guard has to
// refuse, rather than returning a list the predicate can never match.
func TestImportCorpusRefusalReadsTheRealHomes(t *testing.T) {
	home := newImportHome(t)
	t.Setenv("HOME", home.root)
	t.Setenv("USERPROFILE", home.root)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CODEX_HOME", "")

	for _, root := range []string{
		filepath.Join(home.root, ".claude"),
		filepath.Join(home.root, ".claude", "projects"),
		home.codexHome(),
		home.root,
	} {
		if err := refuseLiveHome(root, liveProviderHomes()); err == nil {
			t.Errorf("refuseLiveHome(%q) = nil, want a refusal — this path IS (or holds) a live provider home", root)
		}
	}
	// A copy beside the home is exactly what the gate is for.
	if err := refuseLiveHome(t.TempDir(), liveProviderHomes()); err != nil {
		t.Errorf("refuseLiveHome(<temp copy>) = %v, want nil", err)
	}
}

// TestClaudeCorpusProjectsDirAcceptsBothCopyShapes pins the one piece of
// convenience the gate offers: whichever half of the Claude home a developer
// copied, the runner finds the transcripts.
func TestClaudeCorpusProjectsDirAcceptsBothCopyShapes(t *testing.T) {
	home := newImportHome(t)
	home.claudeLinearSession(t, importFixtureClaudeSession)

	claudeRoot := filepath.Join(home.root, ".claude")
	projectsRoot := filepath.Join(claudeRoot, "projects")

	if got, err := claudeCorpusProjectsDir(claudeRoot); err != nil || got != projectsRoot {
		t.Fatalf("claudeCorpusProjectsDir(<home copy>) = (%q, %v), want (%q, nil)", got, err, projectsRoot)
	}
	if got, err := claudeCorpusProjectsDir(projectsRoot); err != nil || got != projectsRoot {
		t.Fatalf("claudeCorpusProjectsDir(<projects copy>) = (%q, %v), want (%q, nil)", got, err, projectsRoot)
	}
	// A root holding neither shape is a corpus that was copied wrong, and
	// saying so beats listing zero sessions out of a directory of files.
	if _, err := claudeCorpusProjectsDir(t.TempDir()); err == nil {
		t.Fatal("claudeCorpusProjectsDir(<empty dir>) = nil error, want a refusal")
	}
}

// histogramMentions matches on a substring because the qualified type a
// rollout reader mints for an unknown envelope is that package's own format
// (`event_msg/item_completed/<type>` and friends). Pinning the exact spelling
// here would make this test fail for a rename that has nothing to do with the
// runner it covers.
func histogramMentions(counts map[string]int, needle string) bool {
	for key, count := range counts {
		if count > 0 && strings.Contains(key, needle) {
			return true
		}
	}
	return false
}
