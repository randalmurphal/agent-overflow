package gitdiff

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"agent-overflow/internal/testutil"
)

// The canonical flags every patch-producing invocation carries. Spelled
// out here rather than referenced from options.go so silently dropping
// one (--no-ext-diff, say — an execution surface) fails this test.
var canonicalPatchFlags = []string{"--patch", "--minimal", "--no-color", "--no-ext-diff", "--no-textconv"}

func TestOptionsGitArgs(t *testing.T) {
	tests := []struct {
		name string
		opts Options
		sub  string
		rest []string
		want []string
	}{
		{
			name: "zero value carries the canonical flags and no -w",
			sub:  "diff",
			rest: []string{"HEAD", "abc123", "--"},
			want: append(append([]string{"diff"}, canonicalPatchFlags...), "HEAD", "abc123", "--"),
		},
		{
			name: "ignore whitespace appends -w after the canonical flags",
			opts: Options{IgnoreWhitespace: true},
			sub:  "diff",
			rest: []string{"HEAD", "abc123", "--"},
			want: append(append([]string{"diff"}, canonicalPatchFlags...), "-w", "HEAD", "abc123", "--"),
		},
		{
			name: "diff-tree keeps its own flags after -w",
			opts: Options{IgnoreWhitespace: true},
			sub:  "diff-tree",
			rest: []string{"--root", "--find-renames", "abc123", "--"},
			want: append(append([]string{"diff-tree"}, canonicalPatchFlags...),
				"-w", "--root", "--find-renames", "abc123", "--"),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.opts.gitArgs(tc.sub, tc.rest...)
			if strings.Join(got, " ") != strings.Join(tc.want, " ") {
				t.Fatalf("gitArgs() =\n  %v\nwant\n  %v", got, tc.want)
			}
			// -w and only -w: never --ignore-blank-lines, which would
			// change which lines exist rather than how they compare.
			for _, arg := range got {
				if strings.HasPrefix(arg, "--ignore-blank") || arg == "-b" {
					t.Fatalf("gitArgs() emitted %q; only -w is allowed", arg)
				}
			}
		})
	}
}

// traceGitArgv runs fn with GIT_TRACE pointed at a temp file and returns
// every git argv it recorded. This is the direct evidence that an Options
// value reaches the real subprocess: the flags asserted in
// TestOptionsGitArgs are only worth anything if the diff functions
// actually build their argv through them.
func traceGitArgv(t *testing.T, fn func()) []string {
	t.Helper()
	tracePath := filepath.Join(t.TempDir(), "git-trace.log")
	t.Setenv("GIT_TRACE", tracePath)
	fn()
	file, err := os.Open(tracePath)
	if err != nil {
		t.Fatalf("open GIT_TRACE log: %v", err)
	}
	defer func() { _ = file.Close() }()

	var argv []string
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		// git writes `<timestamp> git.c:NNN  trace: built-in: git <argv>`
		// for every command it dispatches.
		if _, rest, ok := strings.Cut(line, "trace: built-in: git "); ok {
			argv = append(argv, rest)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read GIT_TRACE log: %v", err)
	}
	if len(argv) == 0 {
		t.Fatal("GIT_TRACE recorded no git invocations; the capture is not working")
	}
	return argv
}

// patchInvocation returns the single traced argv that produced a patch
// (the only one carrying --patch), failing if there isn't exactly one.
func patchInvocation(t *testing.T, argv []string) string {
	t.Helper()
	var found []string
	for _, line := range argv {
		if strings.Contains(line, "--patch") {
			found = append(found, line)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly one patch-producing invocation, got %d:\n%s",
			len(found), strings.Join(argv, "\n"))
	}
	return found[0]
}

func hasDashW(argv string) bool {
	for _, field := range strings.Fields(argv) {
		if field == "-w" {
			return true
		}
	}
	return false
}

func TestIgnoreWhitespacePassesDashWToGit(t *testing.T) {
	ctx := context.Background()
	// Each producer gets its own repo: the traced run and the fixture must
	// not share state across subtests.
	producers := []struct {
		name string
		run  func(t *testing.T, repo string, opts Options)
	}{
		{
			name: "DiffWorkspaceVsHead",
			run: func(t *testing.T, repo string, opts Options) {
				if _, err := DiffWorkspaceVsHead(ctx, repo, opts); err != nil {
					t.Fatalf("DiffWorkspaceVsHead: %v", err)
				}
			},
		},
		{
			name: "DiffBranchBaseToWorktree",
			run: func(t *testing.T, repo string, opts Options) {
				if _, err := DiffBranchBaseToWorktree(ctx, repo, "main", opts); err != nil {
					t.Fatalf("DiffBranchBaseToWorktree: %v", err)
				}
			},
		},
		{
			name: "CommitDiff",
			run: func(t *testing.T, repo string, opts Options) {
				if _, err := CommitDiff(ctx, repo, headSHA(t, repo), opts); err != nil {
					t.Fatalf("CommitDiff: %v", err)
				}
			},
		},
	}
	for _, producer := range producers {
		t.Run(producer.name, func(t *testing.T) {
			for _, ignore := range []bool{false, true} {
				name := "canonical"
				if ignore {
					name = "ignore-whitespace"
				}
				t.Run(name, func(t *testing.T) {
					repo := testutil.InitGitRepo(t)
					commitFile(t, repo, "code.txt", "alpha\nbeta\n", "add code")
					writeFile(t, repo, "code.txt", "alpha\n  beta\ngamma\n")
					argv := traceGitArgv(t, func() {
						producer.run(t, repo, Options{IgnoreWhitespace: ignore})
					})
					invocation := patchInvocation(t, argv)
					if got := hasDashW(invocation); got != ignore {
						t.Fatalf("IgnoreWhitespace=%v produced argv %q; -w present = %v, want %v",
							ignore, invocation, got, ignore)
					}
				})
			}
		})
	}
}

func TestIgnoreWhitespaceHidesIndentationOnlyChange(t *testing.T) {
	ctx := context.Background()
	repo := testutil.InitGitRepo(t)
	commitFile(t, repo, "code.txt", "func main() {\nsetup()\nrun()\n}\n", "add code")
	// The shape this feature exists for: a block wrapped in an `if`, so
	// every body line is re-indented and nothing else changed.
	writeFile(t, repo, "code.txt", "func main() {\n    setup()\n    run()\n}\n")

	canonical, err := DiffWorkspaceVsHead(ctx, repo, Options{})
	if err != nil {
		t.Fatalf("DiffWorkspaceVsHead(canonical): %v", err)
	}
	if !strings.Contains(string(canonical), "+    setup()") {
		t.Fatalf("canonical patch should carry the re-indent, got:\n%s", canonical)
	}

	ignored, err := DiffWorkspaceVsHead(ctx, repo, Options{IgnoreWhitespace: true})
	if err != nil {
		t.Fatalf("DiffWorkspaceVsHead(ignore whitespace): %v", err)
	}
	if len(ignored) != 0 {
		t.Fatalf("a whitespace-only change should produce an empty patch, got:\n%s", ignored)
	}
}

func TestIgnoreWhitespaceKeepsRealEditsFromReindentedBlock(t *testing.T) {
	ctx := context.Background()
	repo := testutil.InitGitRepo(t)
	commitFile(t, repo, "code.txt", "alpha\nbeta\ngamma\ndelta\nepsilon\n", "add code")
	// Re-indent three lines and make one real edit.
	writeFile(t, repo, "code.txt", "alpha\n    beta\n    gamma\n    delta\nEPSILON\n")

	ignored, err := DiffWorkspaceVsHead(ctx, repo, Options{IgnoreWhitespace: true})
	if err != nil {
		t.Fatalf("DiffWorkspaceVsHead: %v", err)
	}
	text := string(ignored)
	if !strings.Contains(text, "+EPSILON") || !strings.Contains(text, "-epsilon") {
		t.Fatalf("the real edit must survive -w, got:\n%s", text)
	}
	for _, line := range strings.Split(text, "\n") {
		// The re-indented lines must not appear as +/- rows: that is the
		// whole point of the toggle.
		if strings.HasPrefix(line, "+ ") || strings.HasPrefix(line, "-b") ||
			strings.HasPrefix(line, "-g") || strings.HasPrefix(line, "-d") {
			t.Fatalf("re-indented line leaked into the -w patch as %q:\n%s", line, text)
		}
	}
}

func TestIgnoreWhitespaceAppliesToBranchBaseAndCommitDiffs(t *testing.T) {
	ctx := context.Background()
	repo := testutil.InitGitRepo(t)
	commitFile(t, repo, "code.txt", "alpha\nbeta\n", "add code")
	testutil.RunGit(t, repo, "checkout", "-b", "feature")
	commitFile(t, repo, "code.txt", "alpha\n    beta\n", "re-indent only")

	branchPatch, err := DiffBranchBaseToWorktree(ctx, repo, "main", Options{IgnoreWhitespace: true})
	if err != nil {
		t.Fatalf("DiffBranchBaseToWorktree: %v", err)
	}
	if len(branchPatch) != 0 {
		t.Fatalf("branch-base -w patch should be empty for a whitespace-only branch, got:\n%s", branchPatch)
	}

	commitPatch, err := CommitDiff(ctx, repo, headSHA(t, repo), Options{IgnoreWhitespace: true})
	if err != nil {
		t.Fatalf("CommitDiff: %v", err)
	}
	if len(commitPatch) != 0 {
		t.Fatalf("commit -w patch should be empty for a whitespace-only commit, got:\n%s", commitPatch)
	}
}

// A root commit takes the `diff-tree --root` path rather than the
// two-revision `diff` path, so `-w` has to be accepted by a different
// git subcommand. Nothing is whitespace-only in a file's creation, so
// this asserts the invocation succeeds and still carries the content.
func TestIgnoreWhitespaceOnRootCommit(t *testing.T) {
	ctx := context.Background()
	repo := testutil.InitGitRepo(t)
	rootSHA, _, _, err := runGit(ctx, repo, nil, false, "rev-list", "--max-parents=0", "HEAD")
	if err != nil {
		t.Fatalf("rev-list root: %v", err)
	}
	patch, err := CommitDiff(ctx, repo, strings.TrimSpace(rootSHA), Options{IgnoreWhitespace: true})
	if err != nil {
		t.Fatalf("CommitDiff(root, ignore whitespace): %v", err)
	}
	if !strings.Contains(string(patch), "+hello") {
		t.Fatalf("root commit patch lost its content under -w:\n%s", patch)
	}
}

// hunkHeaderPattern matches `@@ -oldStart[,oldCount] +newStart[,newCount] @@`.
var hunkHeaderPattern = regexp.MustCompile(`^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

type anchoredLine struct {
	oldLine int
	newLine int
}

// anchorsByContent walks a unified patch exactly the way the review
// pane's row model does — advancing the old and new counters per row —
// and returns each changed/context line's (old, new) anchor keyed by the
// line's text. This is the mapping the diff-review comment flow turns
// into the `path:line` it hands the provider.
func anchorsByContent(t *testing.T, patch string) map[string]anchoredLine {
	t.Helper()
	anchors := map[string]anchoredLine{}
	oldLine, newLine := 0, 0
	inHunk := false
	for _, row := range strings.Split(patch, "\n") {
		if match := hunkHeaderPattern.FindStringSubmatch(row); match != nil {
			oldStart, err := strconv.Atoi(match[1])
			if err != nil {
				t.Fatalf("parse hunk old start %q: %v", match[1], err)
			}
			newStart, err := strconv.Atoi(match[2])
			if err != nil {
				t.Fatalf("parse hunk new start %q: %v", match[2], err)
			}
			oldLine, newLine = oldStart, newStart
			inHunk = true
			continue
		}
		if !inHunk || row == "" {
			continue
		}
		switch row[0] {
		case '+':
			anchors[row[1:]] = anchoredLine{newLine: newLine}
			newLine++
		case '-':
			anchors[row[1:]] = anchoredLine{oldLine: oldLine}
			oldLine++
		case ' ':
			anchors[row[1:]] = anchoredLine{oldLine: oldLine, newLine: newLine}
			oldLine++
			newLine++
		default:
			// `\ No newline at end of file` and the next file's header.
			inHunk = strings.HasPrefix(row, "\\")
		}
	}
	return anchors
}

// TestIgnoreWhitespaceKeepsCanonicalLineNumbers is the load-bearing test
// behind the decision to leave diff-review comment creation ENABLED while
// the toggle is on.
//
// `-w` narrows and drops hunks, but the `@@` ranges it emits are still
// true file line numbers, so a line's (old, new) anchor is identical in
// both patches. A comment anchored on the -w patch therefore names the
// same physical line as the same comment anchored on the canonical patch
// — the `path:line` handed to the provider cannot silently drift.
//
// If a future git ever changed that, this test fails and the frontend's
// "comments work under -w" assumption has to be revisited.
func TestIgnoreWhitespaceKeepsCanonicalLineNumbers(t *testing.T) {
	ctx := context.Background()
	repo := testutil.InitGitRepo(t)
	commitFile(t, repo,
		"code.txt",
		"package main\n\nfunc main() {\nsetup()\nrun()\nteardown()\n}\n\nfunc other() {}\n",
		"add code")
	// A wrapped-in-an-if re-indent plus real edits on both sides of it.
	writeFile(t, repo,
		"code.txt",
		"package main\n\nfunc main() {\n    setup()\n    launch()\n    teardown()\n}\n\nfunc helper() {}\n")

	canonical, err := DiffWorkspaceVsHead(ctx, repo, Options{})
	if err != nil {
		t.Fatalf("DiffWorkspaceVsHead(canonical): %v", err)
	}
	ignored, err := DiffWorkspaceVsHead(ctx, repo, Options{IgnoreWhitespace: true})
	if err != nil {
		t.Fatalf("DiffWorkspaceVsHead(ignore whitespace): %v", err)
	}

	// Guard against a vacuous pass: if -w stopped reaching git the two
	// patches would be identical and every comparison below trivial.
	if string(canonical) == string(ignored) {
		t.Fatalf("-w produced the canonical patch; the fixture no longer exercises the option:\n%s", ignored)
	}

	canonicalAnchors := anchorsByContent(t, string(canonical))
	ignoredAnchors := anchorsByContent(t, string(ignored))
	if len(ignoredAnchors) == 0 {
		t.Fatalf("the -w patch produced no anchored rows:\n%s", ignored)
	}

	// Every line the -w patch shows must anchor exactly where the
	// canonical patch anchors it. Whitespace-changed lines appear as
	// context rows under -w and as +/- rows in the canonical patch, so
	// compare only the sides the -w row actually claims.
	for content, ignoredAnchor := range ignoredAnchors {
		canonicalAnchor, ok := canonicalAnchors[content]
		if !ok {
			// A line that -w renders as context but the canonical patch
			// never shows verbatim (the pre-image side of a whitespace
			// change). Its number still has to be right; assert it
			// against the file itself below.
			continue
		}
		if ignoredAnchor.newLine != 0 && canonicalAnchor.newLine != 0 &&
			ignoredAnchor.newLine != canonicalAnchor.newLine {
			t.Fatalf("line %q anchors at new line %d under -w but %d canonically",
				content, ignoredAnchor.newLine, canonicalAnchor.newLine)
		}
		if ignoredAnchor.oldLine != 0 && canonicalAnchor.oldLine != 0 &&
			ignoredAnchor.oldLine != canonicalAnchor.oldLine {
			t.Fatalf("line %q anchors at old line %d under -w but %d canonically",
				content, ignoredAnchor.oldLine, canonicalAnchor.oldLine)
		}
	}

	// Ground the new-side numbers against the real file: an anchor is
	// only useful if `path:line` points at that line on disk.
	fileLines := readWorkspaceLines(t, repo, "code.txt")
	for content, anchor := range ignoredAnchors {
		if anchor.newLine == 0 {
			continue
		}
		if anchor.newLine > len(fileLines) {
			t.Fatalf("line %q anchors at new line %d, past the file's %d lines",
				content, anchor.newLine, len(fileLines))
		}
		if got := fileLines[anchor.newLine-1]; got != content {
			t.Fatalf("new line %d is %q on disk but the -w patch anchors %q there",
				anchor.newLine, got, content)
		}
	}

	// And the real edits must be present with the anchors the provider
	// would receive.
	for _, want := range []struct {
		content string
		newLine int
	}{
		{content: "    launch()", newLine: 5},
		{content: "func helper() {}", newLine: 9},
	} {
		anchor, ok := ignoredAnchors[want.content]
		if !ok {
			t.Fatalf("the -w patch dropped the real edit %q:\n%s", want.content, ignored)
		}
		if anchor.newLine != want.newLine {
			t.Fatalf("%q anchors at new line %d, want %d", want.content, anchor.newLine, want.newLine)
		}
	}
}

func readWorkspaceLines(t *testing.T, repo, name string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repo, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}
