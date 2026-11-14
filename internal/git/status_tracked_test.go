package git

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unsafe"

	"agent-overflow/internal/testutil"
)

// churnRepo is a repository scanned under a scanClock.
type churnRepo struct {
	t     *testing.T
	dir   string
	clock *scanClock
}

func newChurnRepo(t *testing.T) *churnRepo {
	t.Helper()
	return &churnRepo{t: t, dir: seedRepo(t), clock: newScanClock(t)}
}

// core returns a Core with no memo, scanning by the repo's clock.
func (r *churnRepo) core() *Core {
	return r.clock.drive(NewCore())
}

func (r *churnRepo) git(args ...string) {
	r.t.Helper()
	testutil.RunGit(r.t, r.dir, args...)
}

func (r *churnRepo) write(rel, content string) {
	r.t.Helper()
	writeRepoFile(r.t, r.dir, rel, content)
}

// scan settles everything written so far, then runs the status read and
// the churn scan the way baseStatus does.
func (r *churnRepo) scan(core *Core, cwd string) trackedChurnTotals {
	r.t.Helper()
	r.clock.settle()
	return r.scanNow(core, cwd)
}

// scanNow scans without settling: files written since the last settle
// are inside the racy window.
func (r *churnRepo) scanNow(core *Core, cwd string) trackedChurnTotals {
	r.t.Helper()
	totals, err := r.tryScan(core, cwd)
	if err != nil {
		r.t.Fatalf("trackedChurn: %v", err)
	}
	return totals
}

func (r *churnRepo) tryScan(core *Core, cwd string) (trackedChurnTotals, error) {
	r.t.Helper()
	scan := core.scanStatus(cwd)
	if scan.err != nil || scan.result.exitCode != 0 {
		r.t.Fatalf("status: %v exit %d: %s", scan.err, scan.result.exitCode, scan.result.stderr)
	}
	return scan.churn, scan.churnErr
}

// whole is the churn of the whole working tree as git reports it in one
// diff: the totals every scan must equal.
func (r *churnRepo) whole(cwd string) trackedChurnTotals {
	r.t.Helper()
	status := parsePorcelainStatus(gitStdout(r.t, cwd, "status", "--porcelain=v2", "--branch", "-z"))
	paths := make(map[string]struct{})
	for _, entry := range status.entries {
		paths[entry.path] = struct{}{}
	}
	var totals trackedChurnTotals
	// The panel's flags (gitdiff patchFlags), counted rather than printed.
	for record := range numstatRecords(gitStdout(r.t, cwd, "diff", "--numstat", "-z", "--minimal", "--no-ext-diff", "--no-textconv", "--find-renames", "--no-relative", "HEAD", "--")) {
		totals.insertions += record.insertions
		totals.deletions += record.deletions
		paths[record.path] = struct{}{}
	}
	totals.files = len(paths)
	return totals
}

func (r *churnRepo) memo(core *Core) trackedChurnMemo {
	memo, _ := core.trackedChurnMemos.snapshot(r.dir)
	return memo
}

// plant replaces the insertions the memo holds for rel with a count no
// diff reports, so a scan that replays the entry shows it in its totals.
func (r *churnRepo) plant(core *Core, rel string, insertions int) {
	r.t.Helper()
	memo := r.memo(core)
	entry, ok := memo.files[rel]
	if !ok {
		r.t.Fatalf("the memo holds no entry for %s: %+v", rel, memo.files)
	}
	entry.insertions = insertions
	memo.files = maps.Clone(memo.files)
	memo.files[rel] = entry
	core.trackedChurnMemos.store(r.dir, memo, core.nowFn())
}

// plantGroup is plant for the rename group.
func (r *churnRepo) plantGroup(core *Core, insertions int) {
	r.t.Helper()
	memo := r.memo(core)
	if memo.group == nil {
		r.t.Fatal("the memo holds no group")
	}
	group := *memo.group
	group.insertions = insertions
	memo.group = &group
	core.trackedChurnMemos.store(r.dir, memo, core.nowFn())
}

func lines(prefix string, n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "%s line %03d\n", prefix, i)
	}
	return b.String()
}

// A scan with a warm memo, and one with none, equal git's whole-tree diff
// after every kind of change a working tree goes through.
func TestTrackedChurnMatchesTheWholeTreeDiffAcrossChanges(t *testing.T) {
	t.Parallel()
	r := newChurnRepo(t)
	awkward := []string{"sp ace/it's \"quoted\".txt", "glob[1]*?.txt", "-dash.txt", "naïve.txt", "new\nline.txt", ":(top)magic.txt"}
	for _, name := range append([]string{"a.txt", "b.txt", "dir/c.txt", "gone.txt", "moved-from.txt", "exec.sh"}, awkward...) {
		r.write(name, lines(name, 12))
	}
	r.write("bin.dat", "bin\x00ary\n")
	if err := os.Symlink("a.txt", filepath.Join(r.dir, "link")); err != nil {
		t.Fatal(err)
	}
	r.git("add", "-A")
	r.git("commit", "-q", "-m", "base")
	r.git("branch", "other")

	steps := []struct {
		name string
		run  func()
	}{
		{"clean", func() {}},
		{"modify one file", func() { r.write("a.txt", lines("a.txt", 12)+"extra\n") }},
		{"modify awkward paths", func() {
			for _, name := range awkward {
				r.write(name, lines(name, 9)+"changed\n")
			}
		}},
		{"stage a modification", func() { r.git("add", "a.txt") }},
		{"modify a staged file again", func() { r.write("a.txt", lines("a.txt", 6)) }},
		{"replace a file keeping its size and mtime", func() {
			replaceKeepingStat(t, filepath.Join(r.dir, "a.txt"), strings.ToUpper(lines("a.txt", 6)))
		}},
		{"delete in the working tree", func() {
			if err := os.Remove(filepath.Join(r.dir, "gone.txt")); err != nil {
				t.Fatal(err)
			}
		}},
		{"remove from the index", func() { r.git("rm", "-q", "b.txt") }},
		{"add a file equal to a deleted one", func() {
			r.write("arrived.txt", lines("gone.txt", 12))
			r.git("add", "arrived.txt")
		}},
		{"rename in the index", func() { r.git("mv", "moved-from.txt", "moved-to.txt") }},
		{"rewrite the renamed file", func() { r.write("moved-to.txt", lines("unrelated content entirely", 30)) }},
		{"change a mode", func() {
			if err := os.Chmod(filepath.Join(r.dir, "dir/c.txt"), 0o755); err != nil {
				t.Fatal(err)
			}
		}},
		{"replace a file with a symlink", func() {
			if err := os.Remove(filepath.Join(r.dir, "exec.sh")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("a.txt", filepath.Join(r.dir, "exec.sh")); err != nil {
				t.Fatal(err)
			}
		}},
		{"retarget a symlink", func() {
			if err := os.Remove(filepath.Join(r.dir, "link")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("dir/c.txt", filepath.Join(r.dir, "link")); err != nil {
				t.Fatal(err)
			}
		}},
		{"replace a directory with a file", func() {
			if err := os.RemoveAll(filepath.Join(r.dir, "sp ace")); err != nil {
				t.Fatal(err)
			}
			r.write("sp ace", "a file where a directory was\n")
		}},
		{"change a binary file", func() { r.write("bin.dat", "bin\x00ary changed\n") }},
		{"intend to add a file", func() {
			r.write("intent.txt", lines("intent", 4))
			r.git("add", "-N", "intent.txt")
		}},
		{"add an untracked file", func() { r.write("untracked.txt", lines("untracked", 3)) }},
		{"commit everything", func() {
			r.git("add", "-A")
			r.git("commit", "-q", "-m", "second")
		}},
		{"modify after the commit", func() { r.write("a.txt", lines("a.txt", 7)+"after\n") }},
		{"stash", func() { r.git("stash", "-q") }},
		{"pop the stash", func() { r.git("stash", "pop", "-q") }},
		{"restore a file to HEAD", func() { r.git("checkout", "-q", "--", "a.txt") }},
		{"conflict in a merge", func() {
			r.write("dir/c.txt", lines("ours", 12))
			r.git("commit", "-q", "-am", "ours")
			r.git("checkout", "-q", "other")
			r.write("dir/c.txt", lines("theirs", 12))
			r.write("a.txt", lines("a.txt", 12)+"theirs\n")
			r.git("commit", "-q", "-am", "theirs")
			r.git("checkout", "-q", "main")
			if err := testutil.RunGitAllowError(r.dir, "merge", "-q", "other"); err == nil {
				t.Fatal("merge did not conflict")
			}
		}},
		{"abort the merge", func() { r.git("merge", "--abort") }},
		{"carry a change to another branch", func() {
			r.write("naïve.txt", lines("carried", 5))
			r.git("checkout", "-q", "-b", "third")
		}},
		{"move HEAD under an unchanged working tree", func() { r.git("reset", "-q", "--soft", "HEAD~1") }},
		{"an attribute file turns a changed file binary", func() { r.write(".gitattributes", "naïve.txt -diff\n") }},
		{"the attribute file changes again", func() { r.write(".gitattributes", "naïve.txt diff\n") }},
	}

	warm := r.core()
	for _, step := range steps {
		step.run()
		want := r.whole(r.dir)
		if got := r.scan(warm, r.dir); got != want {
			t.Fatalf("after %q: warm scan = %+v, whole-tree diff = %+v", step.name, got, want)
		}
		// The same scan again replays what the first one stored.
		if got := r.scan(warm, r.dir); got != want {
			t.Fatalf("after %q: repeated scan = %+v, whole-tree diff = %+v", step.name, got, want)
		}
		if got := r.scan(r.core(), r.dir); got != want {
			t.Fatalf("after %q: cold scan = %+v, whole-tree diff = %+v", step.name, got, want)
		}
	}
}

// A file whose key did not change is not diffed again, and a file whose
// key changed is the only one that is.
func TestTrackedChurnReplaysAFileWhoseKeyDidNotChange(t *testing.T) {
	t.Parallel()
	r := newChurnRepo(t)
	r.write("a.txt", "one\ntwo\nthree\n")
	r.write("b.txt", "one\ntwo\nthree\n")
	r.write("staged.txt", "one\ntwo\nthree\n")
	r.git("add", "-A")
	r.git("commit", "-q", "-m", "base")
	core := r.core()

	r.write("a.txt", "one\nTWO\nthree\n")
	r.write("staged.txt", "one\n")
	r.git("add", "staged.txt")
	if got, want := r.scan(core, r.dir), (trackedChurnTotals{insertions: 1, deletions: 3, files: 2}); got != want {
		t.Fatalf("first scan = %+v, want %+v", got, want)
	}
	r.plant(core, "a.txt", 100)
	r.plant(core, "staged.txt", 1000)
	if got, want := r.scan(core, r.dir), (trackedChurnTotals{insertions: 1100, deletions: 3, files: 2}); got != want {
		t.Fatalf("scan with nothing changed = %+v, want the replayed %+v", got, want)
	}

	// Another file changing diffs that file only.
	r.write("b.txt", "one\ntwo\n")
	if got, want := r.scan(core, r.dir), (trackedChurnTotals{insertions: 1100, deletions: 4, files: 3}); got != want {
		t.Fatalf("scan after b.txt changed = %+v, want %+v", got, want)
	}

	// A file that changed is diffed again, whichever side its key names.
	r.write("a.txt", "ONE\nTWO\nTHREE\n")
	r.write("staged.txt", "one\nagain\n")
	if got, want := r.scan(core, r.dir), r.whole(r.dir); got != want {
		t.Fatalf("scan after both changed = %+v, whole-tree diff = %+v", got, want)
	}

	// So is a file staged again with other content: its key names the
	// index's blob.
	for _, content := range []string{"one\nagain\n", "one\nagain\nand again\n"} {
		r.write("staged.txt", content)
		r.git("add", "staged.txt")
		if got, want := r.scan(core, r.dir), r.whole(r.dir); got != want {
			t.Fatalf("scan after %q was staged = %+v, whole-tree diff = %+v", content, got, want)
		}
	}
}

// A commit of other paths leaves a file's stored counts valid.
func TestTrackedChurnKeepsItsMemoAcrossACommitOfOtherFiles(t *testing.T) {
	t.Parallel()
	r := newChurnRepo(t)
	r.write("a.txt", "one\n")
	r.write("b.txt", "one\n")
	r.git("add", "-A")
	r.git("commit", "-q", "-m", "base")
	core := r.core()

	r.write("a.txt", "two\n")
	r.write("b.txt", "two\n")
	r.scan(core, r.dir)
	r.plant(core, "a.txt", 100)
	r.git("commit", "-q", "-m", "b", "b.txt")
	if got, want := r.scan(core, r.dir), (trackedChurnTotals{insertions: 100, deletions: 1, files: 1}); got != want {
		t.Fatalf("scan after b.txt was committed = %+v, want %+v", got, want)
	}

	// A commit of the file itself changes the blob its key names.
	r.git("commit", "-q", "-m", "a", "a.txt")
	r.write("a.txt", "three\nfour\n")
	if got, want := r.scan(core, r.dir), (trackedChurnTotals{insertions: 2, deletions: 1, files: 1}); got != want {
		t.Fatalf("scan after a.txt was committed and changed = %+v, want %+v", got, want)
	}
}

// A file replaced by one of the same size and modification time is a
// different file.
func TestTrackedChurnSeesAReplacedFileOfTheSameSizeAndMtime(t *testing.T) {
	t.Parallel()
	r := newChurnRepo(t)
	r.write("a.txt", "one\ntwo\nthree\n")
	r.git("add", "-A")
	r.git("commit", "-q", "-m", "base")
	core := r.core()

	r.write("a.txt", "one\nTWO\nthree\n")
	if got, want := r.scan(core, r.dir), (trackedChurnTotals{insertions: 1, deletions: 1, files: 1}); got != want {
		t.Fatalf("first scan = %+v, want %+v", got, want)
	}
	replaceKeepingStat(t, filepath.Join(r.dir, "a.txt"), "ONE\nTWO\nTHREE\n")
	if got, want := r.scan(core, r.dir), (trackedChurnTotals{insertions: 3, deletions: 3, files: 1}); got != want {
		t.Fatalf("scan after the replacement = %+v, want %+v", got, want)
	}
}

// A file written inside the racy window is diffed on every scan.
func TestTrackedChurnDoesNotKeyAFreshlyWrittenFile(t *testing.T) {
	t.Parallel()
	r := newChurnRepo(t)
	r.write("a.txt", "one\ntwo\nthree\n")
	r.write("staged.txt", "one\ntwo\nthree\n")
	r.write("old.txt", lines("old", 20))
	r.git("add", "-A")
	r.git("commit", "-q", "-m", "base")
	core := r.core()

	r.write("staged.txt", "one\n")
	r.git("add", "staged.txt")
	r.git("mv", "old.txt", "new.txt")
	// The index is settled; each file is then written again.
	r.clock.settle()
	r.write("a.txt", "one\nTWO\nthree\n")
	r.write("staged.txt", "one\n")
	r.write("new.txt", lines("old", 20))
	want := trackedChurnTotals{insertions: 1, deletions: 3, files: 3}
	if got := r.scanNow(core, r.dir); got != want {
		t.Fatalf("first scan = %+v, want %+v", got, want)
	}
	if memo := r.memo(core); len(memo.files) != 0 || memo.group != nil {
		t.Fatalf("files written just now were keyed: %+v", memo)
	}

	// Left alone, they are keyed by the next scan.
	if got := r.scan(core, r.dir); got != want {
		t.Fatalf("settled scan = %+v, want %+v", got, want)
	}
	if memo := r.memo(core); len(memo.files) != 2 || memo.group == nil {
		t.Fatalf("settled files were not keyed: %+v", memo)
	}
}

// The paths a rename can involve are diffed together, and not at all when
// none of them changed.
func TestTrackedChurnReplaysTheRenameGroup(t *testing.T) {
	t.Parallel()
	r := newChurnRepo(t)
	r.write("a.txt", "one\ntwo\nthree\n")
	r.write("old.txt", lines("old", 20))
	r.git("add", "-A")
	r.git("commit", "-q", "-m", "base")
	core := r.core()

	// A staged rename whose working tree file then changed, and a staged
	// add: the rename pairs old.txt with new.txt.
	r.git("mv", "old.txt", "new.txt")
	r.write("new.txt", lines("old", 20)+"tail one\n")
	r.write("added.txt", "x\n")
	r.git("add", "added.txt")
	if got, want := r.scan(core, r.dir), (trackedChurnTotals{insertions: 2, deletions: 0, files: 2}); got != want {
		t.Fatalf("first scan = %+v, want %+v", got, want)
	}
	r.plantGroup(core, 500)
	if got, want := r.scan(core, r.dir), (trackedChurnTotals{insertions: 500, deletions: 0, files: 2}); got != want {
		t.Fatalf("scan with nothing changed = %+v, want the replayed %+v", got, want)
	}

	// An in-place file changing leaves the group alone.
	r.write("a.txt", "one\n")
	if got, want := r.scan(core, r.dir), (trackedChurnTotals{insertions: 500, deletions: 2, files: 3}); got != want {
		t.Fatalf("scan after a.txt changed = %+v, want %+v", got, want)
	}

	// A member changing diffs the group again.
	r.write("new.txt", lines("old", 20)+"TAIL\nTWO\n")
	if got, want := r.scan(core, r.dir), r.whole(r.dir); got != want {
		t.Fatalf("scan after new.txt changed = %+v, whole-tree diff = %+v", got, want)
	}

	// So does a member replaced under the same size and mtime.
	r.plantGroup(core, 500)
	replaceKeepingStat(t, filepath.Join(r.dir, "new.txt"), lines("old", 20)+"tail\ntwo\n")
	if got, want := r.scan(core, r.dir), r.whole(r.dir); got != want {
		t.Fatalf("scan after new.txt was replaced = %+v, whole-tree diff = %+v", got, want)
	}
}

// An unmerged path's record does not name the commit's blob, so the group
// is diffed again when the commit moves under it.
func TestTrackedChurnDiffsAnUnmergedPathAgainWhenTheCommitMoves(t *testing.T) {
	t.Parallel()
	r := newChurnRepo(t)
	r.write("c.txt", lines("base", 12))
	r.git("add", "-A")
	r.git("commit", "-q", "-m", "base")
	r.git("branch", "other")
	r.write("c.txt", lines("ours", 14))
	r.git("commit", "-q", "-am", "ours")
	r.git("checkout", "-q", "other")
	r.write("c.txt", lines("theirs", 12))
	r.git("commit", "-q", "-am", "theirs")
	r.git("checkout", "-q", "main")
	if err := testutil.RunGitAllowError(r.dir, "merge", "-q", "other"); err == nil {
		t.Fatal("merge did not conflict")
	}
	core := r.core()

	first, before := r.scan(core, r.dir), r.whole(r.dir)
	if first != before {
		t.Fatalf("scan in the conflict = %+v, whole-tree diff = %+v", first, before)
	}
	r.git("update-ref", "refs/heads/main", "HEAD~1")
	after := r.whole(r.dir)
	if after == before {
		t.Fatal("moving the commit must change the whole-tree diff for this test to mean anything")
	}
	if got := r.scan(core, r.dir); got != after {
		t.Fatalf("scan after the commit moved = %+v, whole-tree diff = %+v", got, after)
	}
}

// A rename status pairs counts as one file, and as two once the diff no
// longer pairs it, matching what the panel lists.
func TestStatusCountsARenameAsTheDiffDoes(t *testing.T) {
	t.Parallel()
	r := newChurnRepo(t)
	r.write("src/old.txt", lines("old", 20))
	r.git("add", "-A")
	r.git("commit", "-q", "-m", "base")
	r.git("mv", "src/old.txt", "src/new.txt")
	core := r.core()

	status, err := core.baseStatus(r.dir)
	if err != nil {
		t.Fatal(err)
	}
	if status.FileCount != 1 || status.Insertions != 0 || status.Deletions != 0 {
		t.Fatalf("staged rename = %d files +%d -%d, want 1 file +0 -0", status.FileCount, status.Insertions, status.Deletions)
	}

	r.write("src/new.txt", lines("nothing alike", 5))
	status, err = core.baseStatus(r.dir)
	if err != nil {
		t.Fatal(err)
	}
	if status.FileCount != 2 || status.Insertions != 5 || status.Deletions != 20 {
		t.Fatalf("unpaired rename = %d files +%d -%d, want 2 files +5 -20", status.FileCount, status.Insertions, status.Deletions)
	}
}

// diffArgvs returns the argument lists of the churn diffs hooks logged
// since the log was last cleared, and clears it.
func diffArgvs(t *testing.T, hooks *gitHooks) []string {
	t.Helper()
	var argvs []string
	for _, line := range strings.Split(readFile(t, hooks.logPath), "\n") {
		if strings.HasPrefix(line, "diff --numstat ") {
			argvs = append(argvs, line)
		}
	}
	if err := os.Truncate(hooks.logPath, 0); err != nil {
		t.Fatal(err)
	}
	return argvs
}

// More changed paths than one argument list holds are diffed over the
// whole tree, and a whole-tree diff counts a replayed file once and a
// path status did not list as one more.
func TestTrackedChurnDiffsTheWholeTreePastThePathspecLimit(t *testing.T) {
	hooks := installHookedGit(t)
	r := newChurnRepo(t)
	const files = 700
	name := func(i int) string {
		return fmt.Sprintf("%sfile-%04d.txt", strings.Repeat("deep-directory-name/", 5), i)
	}
	for i := 0; i < files; i++ {
		r.write(name(i), "one\ntwo\n")
	}
	r.write("old.txt", lines("old", 20))
	r.write("late.txt", "one\n")
	r.git("add", "-A")
	r.git("commit", "-q", "-m", "base")
	core := r.core()
	wholeTree := func(scan string) {
		t.Helper()
		argvs := diffArgvs(t, hooks)
		if len(argvs) != 1 || !strings.HasSuffix(argvs[0], " --") {
			t.Fatalf("%s scan ran %d diffs, want one of the whole tree; first 200 bytes: %.200q", scan, len(argvs), argvs)
		}
	}

	for i := 0; i < files; i++ {
		r.write(name(i), "one\ntwo\nthree\n")
	}
	r.git("mv", "old.txt", "new.txt")
	r.write("new.txt", lines("old", 20)+"tail\n")
	diffArgvs(t, hooks)
	if got, want := r.scan(core, r.dir), (trackedChurnTotals{insertions: files + 1, files: files + 1}); got != want {
		t.Fatalf("first scan = %+v, want %+v", got, want)
	}
	wholeTree("first")
	if memo := r.memo(core); len(memo.files) != files || memo.group == nil {
		t.Fatalf("memo holds %d files and group %v, want %d and a group", len(memo.files), memo.group, files)
	}

	// All but ten change again: past the limit, with ten files and the
	// group replayed.
	for i := 10; i < files; i++ {
		r.write(name(i), "one\ntwo\nthree\nfour\n")
	}
	r.plant(core, name(0), 100)
	r.plantGroup(core, 1000)
	r.clock.settle()
	// A tracked file changes after status listed the others.
	hooks.set("before-diff", fmt.Sprintf("printf 'one\\ntwo\\n' > %q\n", filepath.Join(r.dir, "late.txt")))
	diffArgvs(t, hooks)
	if got, want := r.scanNow(core, r.dir), (trackedChurnTotals{insertions: 1100 + 9 + 2*(files-10) + 1, files: files + 2}); got != want {
		t.Fatalf("second scan = %+v, want %+v", got, want)
	}
	hooks.set("before-diff", "")
	wholeTree("second")

	// One changes: a diff of that path and of the file status now lists.
	r.write(name(1), "one\n")
	diffArgvs(t, hooks)
	if got, want := r.scan(core, r.dir), (trackedChurnTotals{insertions: 1100 + 8 + 2*(files-10) + 1, deletions: 1, files: files + 2}); got != want {
		t.Fatalf("third scan = %+v, want %+v", got, want)
	}
	argvs := diffArgvs(t, hooks)
	if want := "-- " + churnPathspecMagic + name(1) + " " + churnPathspecMagic + "late.txt"; len(argvs) != 1 || !strings.HasSuffix(argvs[0], want) {
		t.Fatalf("third scan's diffs = %q, want one ending %q", argvs, want)
	}

	// Nothing changes: no diff.
	if got, want := r.scan(core, r.dir), (trackedChurnTotals{insertions: 1100 + 8 + 2*(files-10) + 1, deletions: 1, files: files + 2}); got != want {
		t.Fatalf("fourth scan = %+v, want %+v", got, want)
	}
	if argvs := diffArgvs(t, hooks); len(argvs) != 0 {
		t.Fatalf("a scan with nothing changed ran %d diffs", len(argvs))
	}
}

// The churn is against the commit status read, whatever HEAD names by
// the time the diff runs.
func TestTrackedChurnDiffsAgainstTheCommitStatusRead(t *testing.T) {
	hooks := installHookedGit(t)
	r := newChurnRepo(t)
	r.write("a.txt", "1\n")
	r.git("add", "-A")
	r.git("commit", "-q", "-m", "base")
	r.write("a.txt", "1\n2\n3\n")
	r.git("commit", "-q", "-am", "second")
	core := r.core()
	r.scan(core, r.dir)
	r.write("a.txt", "1\n2\n3\n4\n")
	r.clock.settle()

	hooks.set("before-diff", fmt.Sprintf("\"$AO_GIT_REAL\" -C %q reset -q --soft HEAD~1\n", r.dir))
	if got, want := r.scanNow(core, r.dir), (trackedChurnTotals{insertions: 1, files: 1}); got != want {
		t.Fatalf("scan while HEAD moved = %+v, want %+v against the commit status read", got, want)
	}
	hooks.set("before-diff", "")
	if got, want := r.scan(core, r.dir), (trackedChurnTotals{insertions: 3, files: 1}); got != want {
		t.Fatalf("scan after HEAD moved = %+v, want %+v", got, want)
	}
}

// Git reads the index's copy of an attribute file the working tree does
// not have, so that entry's state is part of the context.
func TestTrackedChurnKeysTheIndexCopyOfAnAttributeFile(t *testing.T) {
	t.Parallel()
	r := newChurnRepo(t)
	r.write("a.txt", "one\ntwo\nthree\n")
	r.write(".gitattributes", "a.txt -diff\n")
	r.git("add", "-A")
	r.git("commit", "-q", "-m", "base")
	core := r.core()

	r.write("a.txt", "one\nTWO\nthree\n")
	if err := os.Remove(filepath.Join(r.dir, ".gitattributes")); err != nil {
		t.Fatal(err)
	}
	steps := []struct {
		name string
		run  func()
		want trackedChurnTotals
	}{
		{"the working tree copy deleted", func() {}, trackedChurnTotals{deletions: 1, files: 2}},
		{"the index copy removed", func() { r.git("rm", "-q", "--cached", ".gitattributes") }, trackedChurnTotals{insertions: 1, deletions: 2, files: 2}},
	}
	for _, step := range steps {
		step.run()
		if whole := r.whole(r.dir); whole != step.want {
			t.Fatalf("whole-tree diff after %q = %+v, want %+v: the step must change the diff", step.name, whole, step.want)
		}
		if got := r.scan(core, r.dir); got != step.want {
			t.Fatalf("scan after %q = %+v, want %+v", step.name, got, step.want)
		}
	}
}

// A repository with no commit has no churn against HEAD; its staged
// files are still counted, and no diff runs.
func TestTrackedChurnWithoutAHead(t *testing.T) {
	hooks := installHookedGit(t)
	dir := t.TempDir()
	testutil.RunGit(t, dir, "init", "-q", "-b", "main")
	r := &churnRepo{t: t, dir: dir, clock: newScanClock(t)}
	r.write("a.txt", "one\n")
	r.write("b.txt", "two\n")
	r.git("add", "-A")
	core := r.core()
	for round := 0; round < 2; round++ {
		if got, want := r.scan(core, r.dir), (trackedChurnTotals{files: 2}); got != want {
			t.Fatalf("scan %d = %+v, want %+v", round, got, want)
		}
	}
	if log := readFile(t, hooks.logPath); strings.Contains(log, "diff --numstat") {
		t.Fatalf("a diff ran with no commit to diff against:\n%s", log)
	}
}

// A diff that fails fails the scan.
func TestTrackedChurnReportsAFailedDiff(t *testing.T) {
	hooks := installHookedGit(t)
	r := newChurnRepo(t)
	r.write("a.txt", "one\n")
	r.git("add", "-A")
	r.git("commit", "-q", "-m", "base")
	r.write("a.txt", "two\n")
	r.clock.settle()

	// The diff of a first scan, begun beside status, and of a later one.
	hooks.set("before-diff", "echo 'fatal: broken' >&2; exit 128\n")
	if _, err := r.tryScan(r.core(), r.dir); err == nil || !strings.Contains(err.Error(), "fatal: broken") {
		t.Fatalf("first scan error = %v, want the diff's failure", err)
	}
	hooks.set("before-diff", "")
	core := r.core()
	r.git("stash", "-q")
	r.scan(core, r.dir)
	r.git("stash", "pop", "-q")
	r.clock.settle()
	hooks.set("before-diff", "echo 'fatal: broken' >&2; exit 128\n")
	if _, err := r.tryScan(core, r.dir); err == nil || !strings.Contains(err.Error(), "fatal: broken") {
		t.Fatalf("scan error = %v, want the diff's failure", err)
	}
	hooks.set("before-diff", "")
	if got, want := r.scan(core, r.dir), (trackedChurnTotals{insertions: 1, deletions: 1, files: 1}); got != want {
		t.Fatalf("scan after the failure = %+v, want %+v", got, want)
	}
}

// A file written while the diff runs may be the content the diff read, so
// its counts are not stored under the key taken before the diff.
func TestTrackedChurnDoesNotStoreAFileWrittenDuringTheDiff(t *testing.T) {
	hooks := installHookedGit(t)
	r := newChurnRepo(t)
	r.write("a.txt", "one\ntwo\nthree\n")
	r.write("staged.txt", "one\ntwo\nthree\n")
	r.write("old.txt", lines("old", 20))
	r.git("add", "-A")
	r.git("commit", "-q", "-m", "base")
	core := r.core()
	r.scan(core, r.dir)

	r.write("a.txt", "one\nTWO\nthree\n")
	r.write("staged.txt", "one\nTWO\nthree\n")
	r.git("add", "staged.txt")
	r.git("mv", "old.txt", "new.txt")
	r.clock.settle()
	// Status has read all three; the diff reads what this leaves.
	hooks.set("before-diff", fmt.Sprintf("printf 'ONE\\nTWO\\nTHREE\\n' > %q\nprintf 'ONE\\nTWO\\nTHREE\\n' > %q\nprintf 'rewritten\\n' > %q\n",
		filepath.Join(r.dir, "a.txt"), filepath.Join(r.dir, "staged.txt"), filepath.Join(r.dir, "new.txt")))
	r.scanNow(core, r.dir)
	hooks.set("before-diff", "")
	if memo := r.memo(core); len(memo.files) != 0 || memo.group != nil {
		t.Fatalf("results of files written during the diff were stored: %+v", memo)
	}

	// The staged file returns to what the index holds: the key it had
	// when the diff read something else.
	r.write("staged.txt", "one\nTWO\nthree\n")
	if got, want := r.scan(core, r.dir), r.whole(r.dir); got != want {
		t.Fatalf("scan after the file returned to its staged content = %+v, whole-tree diff = %+v", got, want)
	}
}

// A file replaced while the diff runs by one carrying its old modification
// time was still changed inside the racy window.
func TestTrackedChurnDoesNotStoreAFileReplacedDuringTheDiffUnderItsOldMtime(t *testing.T) {
	hooks := installHookedGit(t)
	r := newChurnRepo(t)
	r.write("staged.txt", "one\ntwo\nthree\n")
	r.git("add", "-A")
	r.git("commit", "-q", "-m", "base")
	core := r.core()
	r.scan(core, r.dir)
	r.write("staged.txt", "one\nTWO\nthree\n")
	r.git("add", "staged.txt")
	r.clock.settle()

	path := filepath.Join(r.dir, "staged.txt")
	hooks.set("before-diff", fmt.Sprintf("printf 'ONE\\nTWO\\nTHREE\\n' > %[1]q.new\ntouch -r %[1]q %[1]q.new\nmv %[1]q.new %[1]q\n", path))
	if got, want := r.scanNow(core, r.dir), (trackedChurnTotals{insertions: 3, deletions: 3, files: 1}); got != want {
		t.Fatalf("scan = %+v, want %+v: the diff must read the replacement for this test to mean anything", got, want)
	}
	hooks.set("before-diff", "")
	if memo := r.memo(core); len(memo.files) != 0 {
		t.Fatalf("the counts of a file replaced during the diff were stored: %+v", memo.files)
	}
}

// Status and the diff each read the index. When it changes between them,
// what the diff printed is not the churn of the entries status listed, so
// none of it is stored under their keys.
func TestTrackedChurnDoesNotStoreWhenTheIndexChangedDuringTheScan(t *testing.T) {
	hooks := installHookedGit(t)
	r := newChurnRepo(t)
	r.write("a.txt", "one\ntwo\nthree\n")
	r.git("add", "-A")
	r.git("commit", "-q", "-m", "base")
	core := r.core()
	r.scan(core, r.dir)

	// An in-place file and a staged add.
	r.write("a.txt", "one\nTWO\nthree\n")
	r.write("new.txt", lines("new", 5))
	r.git("add", "new.txt")
	r.clock.settle()
	// Status has listed both; the diff reads an index that has neither.
	hooks.set("after-status", fmt.Sprintf("\"$AO_GIT_REAL\" -C %[1]q reset -q -- new.txt\n\"$AO_GIT_REAL\" -C %[1]q rm -q --cached -- a.txt\n", r.dir))
	r.scanNow(core, r.dir)
	hooks.set("after-status", "")
	if memo := r.memo(core); len(memo.files) != 0 || memo.group != nil {
		t.Fatalf("results of a diff that read another index were stored: %+v", memo)
	}

	// The index returns to what status read: the same records and stats.
	r.git("reset", "-q", "--", "a.txt")
	r.git("add", "new.txt")
	want := trackedChurnTotals{insertions: 6, deletions: 1, files: 2}
	if whole := r.whole(r.dir); whole != want {
		t.Fatalf("whole-tree diff = %+v, want %+v", whole, want)
	}
	if got := r.scan(core, r.dir); got != want {
		t.Fatalf("scan after the index returned = %+v, want %+v", got, want)
	}
	if memo := r.memo(core); len(memo.files) != 1 || memo.group == nil {
		t.Fatalf("a scan under a steady index stored %+v, want one file and the group", memo)
	}
}

// An index written inside the racy window may be written again under the
// same stat, so a scan that begins there stores nothing.
func TestTrackedChurnDoesNotStoreUnderAFreshlyWrittenIndex(t *testing.T) {
	t.Parallel()
	r := newChurnRepo(t)
	r.write("a.txt", "one\ntwo\nthree\n")
	r.write("b.txt", "one\n")
	r.git("add", "-A")
	r.git("commit", "-q", "-m", "base")
	core := r.core()
	r.scan(core, r.dir)

	r.write("a.txt", "one\nTWO\nthree\n")
	r.write("b.txt", "two\n")
	r.clock.settle()
	r.git("add", "b.txt")
	want := trackedChurnTotals{insertions: 2, deletions: 2, files: 2}
	if got := r.scanNow(core, r.dir); got != want {
		t.Fatalf("scan = %+v, want %+v", got, want)
	}
	if memo := r.memo(core); len(memo.files) != 0 {
		t.Fatalf("results were stored under an index written just now: %+v", memo.files)
	}
	if got := r.scan(core, r.dir); got != want {
		t.Fatalf("settled scan = %+v, want %+v", got, want)
	}
	if memo := r.memo(core); len(memo.files) != 2 {
		t.Fatalf("settled scan stored %d files, want 2", len(memo.files))
	}
}

// A file written while status runs is inside the racy window of that
// scan, however long status took.
func TestStatusDoesNotKeyAFileWrittenWhileStatusRan(t *testing.T) {
	hooks := installHookedGit(t)
	r := newChurnRepo(t)
	r.write("staged.txt", "one\ntwo\nthree\n")
	r.git("add", "-A")
	r.git("commit", "-q", "-m", "base")
	r.write("staged.txt", "one\nTWO\nthree\n")
	r.git("add", "staged.txt")
	core := r.core()
	r.clock.tick()

	// Status has reported the file equal to the index; then it is written,
	// and status takes longer than the window is wide.
	hooks.set("after-status", fmt.Sprintf("printf 'ONE\\nTWO\\nTHREE\\n' > %q\nsleep %.2f\n", filepath.Join(r.dir, "staged.txt"), (2*freshMargin).Seconds()))
	if _, err := core.baseStatus(r.dir); err != nil {
		t.Fatal(err)
	}
	hooks.set("after-status", "")
	if memo := r.memo(core); len(memo.files) != 0 {
		t.Fatalf("a file written while status ran was keyed: %+v", memo.files)
	}
}

// Status paths are relative to the working tree's root. From a
// subdirectory, and from a subdirectory that only looks like a root, the
// totals are still the whole tree's and no file is keyed by a stat of the
// wrong path.
func TestTrackedChurnFromASubdirectory(t *testing.T) {
	t.Parallel()
	for _, lookalike := range []bool{false, true} {
		t.Run(fmt.Sprintf("lookalike=%v", lookalike), func(t *testing.T) {
			t.Parallel()
			r := newChurnRepo(t)
			r.write("sub/a.txt", "one\ntwo\n")
			r.write("sub/sub/a.txt", "one\ntwo\n")
			r.write("top.txt", "one\n")
			r.git("add", "-A")
			r.git("commit", "-q", "-m", "base")
			// A config under which a diff from a subdirectory lists only
			// that directory, unless told otherwise.
			r.git("config", "diff.relative", "true")
			sub := filepath.Join(r.dir, "sub")
			if lookalike {
				if err := os.Mkdir(filepath.Join(sub, ".git"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			core := r.core()

			// sub/sub/a.txt is where a stat relative to cwd would look
			// for sub/a.txt.
			r.write("sub/a.txt", "one\nTWO\n")
			r.write("top.txt", "ONE\n")
			for round := 0; round < 2; round++ {
				got := r.scan(core, sub)
				if want := r.whole(sub); got != want || got.files != 2 {
					t.Fatalf("scan %d = %+v, whole-tree diff = %+v, want 2 files", round, got, want)
				}
				// Nothing is keyed, so each scan diffs beside status.
				if memo, _ := core.trackedChurnMemos.snapshot(sub); len(memo.files) != 0 || memo.group != nil || memo.warm {
					t.Fatalf("scan %d keyed files from a cwd that is not the root: %+v", round, memo)
				}
			}
		})
	}
}

// What a diff reads besides the two contents is part of every key: when
// it changes, what was stored under the old one is not replayed.
func TestTrackedChurnForgetsItsMemoWhenTheDiffContextChanges(t *testing.T) {
	t.Parallel()
	r := newChurnRepo(t)
	r.write("dir/a.txt", "one\ntwo\nthree\n")
	r.write(".gitignore", "dir/.gitattributes\n")
	r.git("add", "-A")
	r.git("commit", "-q", "-m", "base")
	core := r.core()
	outside := t.TempDir()
	writeFile := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	r.write("dir/a.txt", "one\nTWO\nthree\n")
	text, binary := trackedChurnTotals{insertions: 1, deletions: 1, files: 1}, trackedChurnTotals{files: 1}
	if got := r.scan(core, r.dir); got != text {
		t.Fatalf("first scan = %+v, want %+v", got, text)
	}
	if memo := r.memo(core); len(memo.files) != 1 {
		t.Fatalf("memo holds %d files, want 1", len(memo.files))
	}

	// Each step changes how dir/a.txt diffs and nothing about the file.
	infoAttributes := filepath.Join(r.dir, ".git", "info", "attributes")
	globalAttributes := filepath.Join(outside, "global-attributes")
	included := filepath.Join(r.dir, ".git", "included.cfg")
	steps := []struct {
		name string
		run  func()
		want trackedChurnTotals
	}{
		{"an untracked attribute file", func() { r.write(".gitattributes", "a.txt -diff\n") }, binary},
		{"the attribute file removed", func() {
			if err := os.Remove(filepath.Join(r.dir, ".gitattributes")); err != nil {
				t.Fatal(err)
			}
		}, text},
		{"an attribute file git ignores", func() { r.write("dir/.gitattributes", "a.txt -diff\n") }, binary},
		{"the ignored attribute file emptied", func() { r.write("dir/.gitattributes", "") }, text},
		{"the repository's info/attributes", func() { writeFile(infoAttributes, "a.txt -diff\n") }, binary},
		{"info/attributes emptied", func() { writeFile(infoAttributes, "") }, text},
		{"a config naming an attribute file", func() {
			writeFile(globalAttributes, "a.txt -diff\n")
			r.git("config", "core.attributesFile", globalAttributes)
		}, binary},
		{"that attribute file rewritten", func() { writeFile(globalAttributes, "a.txt diff\n") }, text},
		{"that attribute file rewritten again", func() { writeFile(globalAttributes, "a.txt -diff\n") }, binary},
		{"the config changed back", func() { r.git("config", "--unset", "core.attributesFile") }, text},
		{"a config file the config includes", func() {
			writeFile(included, "[core]\n\tattributesFile = "+globalAttributes+"\n")
			r.git("config", "include.path", "included.cfg")
		}, binary},
		{"the included config file rewritten", func() { writeFile(included, "[core]\n\tattributesFile = /nonexistent\n") }, text},
	}
	for _, step := range steps {
		step.run()
		if whole := r.whole(r.dir); whole != step.want {
			t.Fatalf("whole-tree diff after %q = %+v, want %+v: the step must change the diff", step.name, whole, step.want)
		}
		if got := r.scan(core, r.dir); got != step.want {
			t.Fatalf("scan after %q = %+v, want %+v", step.name, got, step.want)
		}
	}
}

// The effective config is part of the context: one that changes how a
// file's content is read changes its counts.
func TestTrackedChurnForgetsItsMemoWhenTheConfigChanges(t *testing.T) {
	t.Parallel()
	r := newChurnRepo(t)
	r.write("a.txt", "one\ntwo\nthree\n")
	r.git("add", "-A")
	r.git("commit", "-q", "-m", "base")
	core := r.core()

	r.write("a.txt", "one\r\nTWO\r\nthree\r\n")
	steps := []struct {
		name string
		run  func()
		want trackedChurnTotals
	}{
		{"line endings read as written", func() { r.git("config", "core.autocrlf", "false") }, trackedChurnTotals{insertions: 3, deletions: 3, files: 1}},
		{"line endings converted", func() { r.git("config", "core.autocrlf", "true") }, trackedChurnTotals{insertions: 1, deletions: 1, files: 1}},
		{"line endings read as written again", func() { r.git("config", "core.autocrlf", "false") }, trackedChurnTotals{insertions: 3, deletions: 3, files: 1}},
	}
	for _, step := range steps {
		step.run()
		if whole := r.whole(r.dir); whole != step.want {
			t.Fatalf("whole-tree diff under %q = %+v, want %+v: the step must change the diff", step.name, whole, step.want)
		}
		if got := r.scan(core, r.dir); got != step.want {
			t.Fatalf("scan under %q = %+v, want %+v", step.name, got, step.want)
		}
	}
}

// A changed file in a directory that has no attribute file leaves the
// context, and so every stored result, as it was.
func TestTrackedChurnKeepsItsMemoWhenAnotherDirectoryChanges(t *testing.T) {
	t.Parallel()
	r := newChurnRepo(t)
	r.write("a.txt", "one\n")
	r.write("deep/er/b.txt", "one\n")
	r.write("attributed/.gitattributes", "*.txt text\n")
	r.write("attributed/c.txt", "one\n")
	r.write("attributed/zz/.gitattributes", "*.txt text\n")
	r.write("attributed/zz/d.txt", "one\n")
	r.git("add", "-A")
	r.git("commit", "-q", "-m", "base")
	core := r.core()

	r.write("a.txt", "two\n")
	r.write("attributed/c.txt", "two\n")
	r.write("attributed/zz/d.txt", "two\n")
	r.scan(core, r.dir)
	r.plant(core, "a.txt", 100)
	r.write("deep/er/b.txt", "two\n")
	if got, want := r.scan(core, r.dir), (trackedChurnTotals{insertions: 103, deletions: 4, files: 4}); got != want {
		t.Fatalf("scan after a file in another directory changed = %+v, want %+v", got, want)
	}

	// The same attribute files, met in another order.
	r.git("checkout", "-q", "--", "attributed/c.txt")
	if got, want := r.scan(core, r.dir), (trackedChurnTotals{insertions: 102, deletions: 3, files: 3}); got != want {
		t.Fatalf("scan after a file was restored = %+v, want %+v", got, want)
	}
}

// The attribute file of the git installation is part of the context.
func TestTrackedChurnKeysTheSystemAttributeFile(t *testing.T) {
	hooks := installHookedGit(t)
	r := newChurnRepo(t)
	r.write("a.txt", "one\n")
	r.git("add", "-A")
	r.git("commit", "-q", "-m", "base")
	r.write("a.txt", "two\n")
	core := r.core()
	system := filepath.Join(t.TempDir(), "gitattributes")
	hooks.set("before-var", fmt.Sprintf("if [ \"$2\" = GIT_ATTR_SYSTEM ]; then echo %q; exit 0; fi\n", system))

	want := trackedChurnTotals{insertions: 1, deletions: 1, files: 1}
	if got := r.scan(core, r.dir); got != want {
		t.Fatalf("first scan = %+v, want %+v", got, want)
	}
	r.plant(core, "a.txt", 100)
	if got, replayed := r.scan(core, r.dir), (trackedChurnTotals{insertions: 100, deletions: 1, files: 1}); got != replayed {
		t.Fatalf("scan with nothing changed = %+v, want the replayed %+v", got, replayed)
	}
	if err := os.WriteFile(system, []byte("* text\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := r.scan(core, r.dir); got != want {
		t.Fatalf("scan after the system attribute file appeared = %+v, want %+v", got, want)
	}
}

// A git that cannot name its attribute files keys nothing and is asked
// once. Each later scan runs what a scan without a memo runs: status and
// one whole-tree diff.
func TestTrackedChurnKeysNothingWhenGitCannotNameItsAttributeFiles(t *testing.T) {
	hooks := installHookedGit(t)
	r := newChurnRepo(t)
	r.write("a.txt", "one\n")
	r.git("add", "-A")
	r.git("commit", "-q", "-m", "base")
	r.write("a.txt", "two\n")
	core := r.core()
	r.clock.settle()

	hooks.set("before-var", "echo 'usage: git var (-l | <variable>)' >&2; exit 129\n")
	want := trackedChurnTotals{insertions: 1, deletions: 1, files: 1}
	if got := r.scanNow(core, r.dir); got != want {
		t.Fatalf("first scan = %+v, want %+v", got, want)
	}
	if asked := strings.Count(readFile(t, hooks.logPath), "var GIT_ATTR_"); asked != 1 {
		t.Fatalf("git var ran %d times, want 1", asked)
	}
	if memo := r.memo(core); len(memo.files) != 0 {
		t.Fatalf("the first scan keyed a file under an unknown context: %+v", memo.files)
	}
	for round := 0; round < 2; round++ {
		if err := os.Truncate(hooks.logPath, 0); err != nil {
			t.Fatal(err)
		}
		if got := r.scanNow(core, r.dir); got != want {
			t.Fatalf("later scan %d = %+v, want %+v", round, got, want)
		}
		if memo := r.memo(core); len(memo.files) != 0 {
			t.Fatalf("later scan %d keyed a file under an unknown context: %+v", round, memo.files)
		}
		ran := strings.Split(strings.TrimSpace(readFile(t, hooks.logPath)), "\n")
		if len(ran) != 2 {
			t.Fatalf("later scan %d ran %q, want status and one diff", round, ran)
		}
		if argvs := diffArgvs(t, hooks); len(argvs) != 1 || !strings.HasSuffix(argvs[0], " HEAD --") {
			t.Fatalf("later scan %d ran diffs %q, want one of the whole tree against HEAD", round, argvs)
		}
	}
}

// A scan with nothing stored diffs the whole tree while status runs, and
// a scan with a memo waits for status to name the paths.
func TestTrackedChurnDiffsBesideStatusWithoutAMemo(t *testing.T) {
	hooks := installHookedGit(t)
	r := newChurnRepo(t)
	r.write("a.txt", "one\n")
	r.write("b.txt", "one\n")
	r.git("add", "-A")
	r.git("commit", "-q", "-m", "base")
	r.write("a.txt", "two\n")
	core := r.core()
	r.clock.settle()

	// Status holds until a diff has started, for as long as a serial scan
	// would need to show it never does.
	hooks.set("before-status", `n=0; while [ $n -lt 100 ] && ! grep -q '^diff --numstat ' "$AO_GIT_LOG"; do sleep 0.05; n=$((n+1)); done
`)
	hooks.set("after-status", "printf 'status-done\\n' >> \"$AO_GIT_LOG\"\n")
	order := func() string {
		t.Helper()
		var seen []string
		for _, line := range strings.Split(readFile(t, hooks.logPath), "\n") {
			switch {
			case strings.HasPrefix(line, "diff --numstat "):
				seen = append(seen, "diff")
			case line == "status-done":
				seen = append(seen, "status-done")
			}
		}
		if err := os.Truncate(hooks.logPath, 0); err != nil {
			t.Fatal(err)
		}
		return strings.Join(seen, " ")
	}
	order()
	want := trackedChurnTotals{insertions: 1, deletions: 1, files: 1}
	if got := r.scanNow(core, r.dir); got != want {
		t.Fatalf("first scan = %+v, want %+v", got, want)
	}
	if got := order(); got != "diff status-done" {
		t.Fatalf("first scan ran %q, want the one diff started before status ended", got)
	}
	if memo := r.memo(core); len(memo.files) != 1 {
		t.Fatalf("the first scan stored %d files, want 1", len(memo.files))
	}

	hooks.set("before-status", "")
	r.write("b.txt", "two\n")
	if got, want := r.scan(core, r.dir), (trackedChurnTotals{insertions: 2, deletions: 2, files: 2}); got != want {
		t.Fatalf("second scan = %+v, want %+v", got, want)
	}
	argvs := strings.Split(strings.TrimSpace(readFile(t, hooks.logPath)), "\n")
	if got := order(); got != "status-done diff" || !strings.HasSuffix(argvs[len(argvs)-1], "-- "+churnPathspecMagic+"b.txt") {
		t.Fatalf("second scan ran %q, want one diff of b.txt after status; last command %q", got, argvs[len(argvs)-1])
	}
}

// The diff begun beside status is pinned to the commit HEAD named then.
// When status reads another commit, the churn is against that one.
func TestTrackedChurnDiffsAgainWhenStatusReadAnotherCommit(t *testing.T) {
	hooks := installHookedGit(t)
	r := newChurnRepo(t)
	r.write("a.txt", "1\n")
	r.git("add", "-A")
	r.git("commit", "-q", "-m", "base")
	r.write("a.txt", "1\n2\n3\n")
	r.git("commit", "-q", "-am", "second")
	r.write("a.txt", "1\n2\n3\n4\n")
	core := r.core()
	r.clock.settle()

	// HEAD moves once the early diff has its commit, and status reads
	// HEAD after that.
	moved := filepath.Join(t.TempDir(), "moved")
	hooks.set("after-rev-parse", fmt.Sprintf("if [ \"$2\" = --verify ] && [ ! -f %[1]q ]; then \"$AO_GIT_REAL\" -C %[2]q reset -q --soft HEAD~1; : > %[1]q; fi\n", moved, r.dir))
	hooks.set("before-status", fmt.Sprintf("n=0; while [ $n -lt 100 ] && [ ! -f %q ]; do sleep 0.05; n=$((n+1)); done\n", moved))
	diffArgvs(t, hooks)
	got := r.scanNow(core, r.dir)
	hooks.set("after-rev-parse", "")
	hooks.set("before-status", "")
	if _, err := os.Stat(moved); err != nil {
		t.Fatalf("HEAD never moved: %v", err)
	}
	if want := (trackedChurnTotals{insertions: 3, files: 1}); got != want {
		t.Fatalf("scan while HEAD moved = %+v, want %+v against the commit status read", got, want)
	}
	if argvs := diffArgvs(t, hooks); len(argvs) != 2 {
		t.Fatalf("the scan ran %d diffs, want the early one and one against the commit status read: %q", len(argvs), argvs)
	}
	if got, want := r.scan(core, r.dir), (trackedChurnTotals{insertions: 3, files: 1}); got != want {
		t.Fatalf("next scan = %+v, want %+v", got, want)
	}
}

// The context of a diff begun beside status is the config read before
// that diff: a config written while it ran keys the next scan
// differently.
func TestTrackedChurnKeysTheConfigReadBeforeTheEarlyDiff(t *testing.T) {
	hooks := installHookedGit(t)
	r := newChurnRepo(t)
	r.write("a.txt", "one\ntwo\nthree\n")
	r.git("add", "-A")
	r.git("commit", "-q", "-m", "base")
	r.git("config", "core.autocrlf", "false")
	r.write("a.txt", "one\r\nTWO\r\nthree\r\n")
	core := r.core()
	r.clock.settle()

	hooks.set("after-diff", fmt.Sprintf("\"$AO_GIT_REAL\" -C %q config core.autocrlf true\n", r.dir))
	if got, want := r.scanNow(core, r.dir), (trackedChurnTotals{insertions: 3, deletions: 3, files: 1}); got != want {
		t.Fatalf("first scan = %+v, want %+v: the diff must run under the first config for this test to mean anything", got, want)
	}
	hooks.set("after-diff", "")
	want := trackedChurnTotals{insertions: 1, deletions: 1, files: 1}
	if whole := r.whole(r.dir); whole != want {
		t.Fatalf("whole-tree diff = %+v, want %+v under the second config", whole, want)
	}
	if got := r.scan(core, r.dir); got != want {
		t.Fatalf("scan under the second config = %+v, want %+v", got, want)
	}
}

// sharesMemory reports whether part's bytes lie inside whole's.
func sharesMemory(part, whole string) bool {
	if part == "" || whole == "" {
		return false
	}
	start, at := uintptr(unsafe.Pointer(unsafe.StringData(whole))), uintptr(unsafe.Pointer(unsafe.StringData(part)))
	return at >= start && at < start+uintptr(len(whole))
}

// A stored entry holds its own copy of its path, whichever git output the
// path was read from.
func TestTrackedFileChurnOwnsItsPath(t *testing.T) {
	t.Parallel()
	output := strings.Repeat("1\t1\tdir/file.txt\x00", 3)
	path := output[4:16]
	stored := newTrackedFileChurn(path, trackedFileKey{}, 1, 1)
	if stored.path != "dir/file.txt" {
		t.Fatalf("stored path = %q", stored.path)
	}
	if sharesMemory(stored.path, output) {
		t.Fatal("the stored path is part of the output it was read from")
	}
}

// A memo outlives the status output of the scan that wrote it, so nothing
// in it is a part of one.
func TestTrackedChurnMemoHoldsNoPartOfAStatusOutput(t *testing.T) {
	t.Parallel()
	r := newChurnRepo(t)
	r.write("a.txt", "one\n")
	r.write("staged.txt", "one\n")
	r.git("add", "-A")
	r.git("commit", "-q", "-m", "base")
	r.write("a.txt", "two\n")
	r.write("staged.txt", "two\n")
	r.git("add", "staged.txt")
	core := r.core()
	r.clock.settle()

	// The first scan stores its results; the second replays them.
	for round := 0; round < 2; round++ {
		scan := core.scanStatus(r.dir)
		if scan.err != nil || scan.churnErr != nil {
			t.Fatalf("scan %d: %v, %v", round, scan.err, scan.churnErr)
		}
		memo := r.memo(core)
		if len(memo.files) != 2 {
			t.Fatalf("scan %d stored %d files, want 2", round, len(memo.files))
		}
		for key, file := range memo.files {
			if key != file.path {
				t.Fatalf("scan %d stored %q under %q", round, file.path, key)
			}
			if sharesMemory(key, scan.result.stdout) || sharesMemory(file.path, scan.result.stdout) {
				t.Fatalf("scan %d: the memo's %q is part of the status output", round, key)
			}
		}
	}
}

// Git reads the attribute files outside the working tree through a link,
// so the file a link names is what the context keys.
func TestTrackedChurnKeysALinkedAttributeFileByItsTarget(t *testing.T) {
	t.Parallel()
	for _, source := range []string{"info/attributes", "core.attributesFile"} {
		t.Run(source, func(t *testing.T) {
			t.Parallel()
			r := newChurnRepo(t)
			r.write("a.txt", "one\ntwo\nthree\n")
			r.git("add", "-A")
			r.git("commit", "-q", "-m", "base")
			r.write("a.txt", "one\nTWO\nthree\n")
			target := filepath.Join(t.TempDir(), "attributes")
			if err := os.WriteFile(target, []byte("a.txt -diff\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(r.dir, ".git", "info", "attributes")
			if source == "core.attributesFile" {
				link = filepath.Join(t.TempDir(), "link")
				r.git("config", "core.attributesFile", link)
			}
			if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			core := r.core()

			steps := []struct {
				content string
				want    trackedChurnTotals
			}{
				{"a.txt -diff\n", trackedChurnTotals{files: 1}},
				{"a.txt diff\n", trackedChurnTotals{insertions: 1, deletions: 1, files: 1}},
				{"a.txt -diff\n", trackedChurnTotals{files: 1}},
			}
			for _, step := range steps {
				if err := os.WriteFile(target, []byte(step.content), 0o644); err != nil {
					t.Fatal(err)
				}
				if whole := r.whole(r.dir); whole != step.want {
					t.Fatalf("whole-tree diff with %q = %+v, want %+v: the link must be followed", step.content, whole, step.want)
				}
				if got := r.scan(core, r.dir); got != step.want {
					t.Fatalf("scan with %q = %+v, want %+v", step.content, got, step.want)
				}
			}
		})
	}
}

// Attributes read from a tree change when the ref naming it moves, which
// no file or config this scan keys shows: nothing is keyed under them.
func TestTrackedChurnKeysNothingUnderAttributesFromATree(t *testing.T) {
	for _, source := range []string{"attr.tree", "GIT_ATTR_SOURCE"} {
		t.Run(source, func(t *testing.T) {
			r := newChurnRepo(t)
			r.write("a.txt", "one\ntwo\nthree\n")
			r.git("add", "-A")
			r.git("commit", "-q", "-m", "base")
			r.git("checkout", "-q", "-b", "attrs")
			r.write(".gitattributes", "a.txt -diff\n")
			r.git("add", "-A")
			r.git("commit", "-q", "-m", "binary")
			r.write(".gitattributes", "a.txt diff\n")
			r.git("commit", "-q", "-am", "text")
			r.git("checkout", "-q", "main")
			r.git("branch", "source", "attrs~1")
			if source == "attr.tree" {
				r.git("config", "attr.tree", "refs/heads/source")
			} else {
				t.Setenv("GIT_ATTR_SOURCE", "refs/heads/source")
			}
			r.write("a.txt", "one\nTWO\nthree\n")
			core := r.core()

			steps := []struct {
				ref  string
				want trackedChurnTotals
			}{
				{"attrs~1", trackedChurnTotals{files: 1}},
				{"attrs", trackedChurnTotals{insertions: 1, deletions: 1, files: 1}},
			}
			for _, step := range steps {
				r.git("update-ref", "refs/heads/source", step.ref)
				if whole := r.whole(r.dir); whole != step.want {
					t.Fatalf("whole-tree diff with the tree of %s = %+v, want %+v: the tree must supply the attributes", step.ref, whole, step.want)
				}
				for round := 0; round < 2; round++ {
					if got := r.scan(core, r.dir); got != step.want {
						t.Fatalf("scan %d with the tree of %s = %+v, want %+v", round, step.ref, got, step.want)
					}
					if memo := r.memo(core); len(memo.files) != 0 || memo.warm {
						t.Fatalf("scan %d keyed results under attributes from a tree: %+v", round, memo)
					}
				}
			}
		})
	}
}

// The memo of a workspace nobody scans is dropped.
func TestTrackedChurnMemoIsSweptWithItsWorkspace(t *testing.T) {
	t.Parallel()
	var repos []*churnRepo
	for i := 0; i < 2; i++ {
		r := newChurnRepo(t)
		r.write("a.txt", "one\n")
		r.git("add", "-A")
		r.git("commit", "-q", "-m", "base")
		r.write("a.txt", "two\n")
		repos = append(repos, r)
	}
	now := time.Now().Add(time.Hour)
	core := NewCore()
	core.nowFn = func() time.Time { return now }
	for _, r := range repos {
		r.scanNow(core, r.dir)
		if memo := r.memo(core); len(memo.files) != 1 {
			t.Fatalf("memo holds %d files, want 1", len(memo.files))
		}
	}
	now = now.Add(workspaceMemoTTL + time.Minute)
	repos[1].scanNow(core, repos[1].dir)
	if _, kept := core.trackedChurnMemos.snapshot(repos[0].dir); kept {
		t.Fatal("the idle workspace's memo survived the sweep")
	}
	if _, kept := core.trackedChurnMemos.snapshot(repos[1].dir); !kept {
		t.Fatal("the scanned workspace's memo was swept")
	}
}
