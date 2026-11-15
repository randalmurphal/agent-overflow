package git

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

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

	// The config the diff is keyed by is still read beside status.
	hooks.set("before-status", `n=0; while [ $n -lt 100 ] && ! grep -q '^config --list' "$AO_GIT_LOG"; do sleep 0.05; n=$((n+1)); done
`)
	r.write("b.txt", "two\n")
	if got, want := r.scan(core, r.dir), (trackedChurnTotals{insertions: 2, deletions: 2, files: 2}); got != want {
		t.Fatalf("second scan = %+v, want %+v", got, want)
	}
	log := readFile(t, hooks.logPath)
	argvs := strings.Split(strings.TrimSpace(log), "\n")
	if config, done := strings.Index(log, "config --list"), strings.Index(log, "status-done"); config < 0 || config > done {
		t.Fatalf("second scan ran %q, want the config read before status ended", argvs)
	}
	if got := order(); got != "status-done diff" || !strings.HasSuffix(argvs[len(argvs)-1], "-- "+churnPathspecMagic+"b.txt") {
		t.Fatalf("second scan ran %q, want one diff of b.txt after status; last command %q", got, argvs[len(argvs)-1])
	}
}

// A scan asks git for the working tree's root and index in one command,
// and a later scan of the same .git entry asks again for neither them nor
// the attribute files outside the repository.
func TestTrackedChurnAsksForTheRootOnce(t *testing.T) {
	hooks := installHookedGit(t)
	r := newChurnRepo(t)
	r.write("a.txt", "one\n")
	r.git("add", "-A")
	r.git("commit", "-q", "-m", "base")
	r.write("a.txt", "two\n")
	core := r.core()
	if err := os.Truncate(hooks.logPath, 0); err != nil {
		t.Fatal(err)
	}
	asked := func(scan string, want int) {
		t.Helper()
		log := readFile(t, hooks.logPath)
		if err := os.Truncate(hooks.logPath, 0); err != nil {
			t.Fatal(err)
		}
		toplevel := 0
		for line := range strings.Lines(log) {
			if slices.Contains(strings.Fields(line), "--show-toplevel") {
				toplevel++
			}
		}
		if toplevel != want {
			t.Fatalf("%s scan asked for the root %d times, want %d:\n%s", scan, toplevel, want, log)
		}
		if got := strings.Count(log, "var GIT_ATTR_"); got != 2*want {
			t.Fatalf("%s scan asked git var %d times, want %d:\n%s", scan, got, 2*want, log)
		}
	}
	r.scan(core, r.dir)
	asked("first", 1)
	if memo := r.memo(core); !memo.root.isRoot || memo.root.indexPath != filepath.Join(r.dir, ".git", "index") {
		t.Fatalf("root proof = %+v, want the root with its index", memo.root)
	}
	r.write("a.txt", "three\n")
	if got, want := r.scan(core, r.dir), (trackedChurnTotals{insertions: 1, deletions: 1, files: 1}); got != want {
		t.Fatalf("second scan = %+v, want %+v", got, want)
	}
	asked("second", 0)
}

// The diff begun beside status is pinned to the commit HEAD named then,
// whether or not the scan can key its results. When status reads another
// commit, the churn is against that one.
func TestTrackedChurnDiffsAgainWhenStatusReadAnotherCommit(t *testing.T) {
	for _, unnamed := range []bool{false, true} {
		t.Run(fmt.Sprintf("attributeFilesUnnamed=%v", unnamed), func(t *testing.T) {
			hooks := installHookedGit(t)
			r := newChurnRepo(t)
			r.write("a.txt", "1\n")
			r.git("add", "-A")
			r.git("commit", "-q", "-m", "base")
			r.write("a.txt", "1\n2\n3\n")
			r.git("commit", "-q", "-am", "second")
			r.write("a.txt", "1\n2\n3\n4\n")
			core := r.core()
			core.attributeFilesUnnamed.Store(unnamed)
			r.clock.settle()

			// HEAD moves once the early diff has run, and status reads
			// HEAD after that.
			moved := filepath.Join(t.TempDir(), "moved")
			hooks.set("after-diff", fmt.Sprintf("if [ ! -f %[1]q ]; then \"$AO_GIT_REAL\" -C %[2]q reset -q --soft HEAD~1; : > %[1]q; fi\n", moved, r.dir))
			hooks.set("before-status", fmt.Sprintf("n=0; while [ $n -lt 100 ] && [ ! -f %q ]; do sleep 0.05; n=$((n+1)); done\n", moved))
			diffArgvs(t, hooks)
			got := r.scanNow(core, r.dir)
			hooks.set("after-diff", "")
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
		})
	}
}

// The context of a diff is the config read before that diff: a config
// written while it ran keys the next scan differently. A scan whose
// config read failed keys nothing, and reads the config no second time.
func TestTrackedChurnKeysTheConfigReadBeforeTheDiff(t *testing.T) {
	for _, tc := range []struct {
		name string
		warm bool
		// failRead fails the scan's config read.
		failRead bool
	}{
		{name: "the diff beside status"},
		{name: "the diff after status", warm: true},
		{name: "the diff beside status with a failed config read", failRead: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hooks := installHookedGit(t)
			r := newChurnRepo(t)
			r.write("a.txt", "one\ntwo\nthree\n")
			r.write("b.txt", "one\n")
			r.git("add", "-A")
			r.git("commit", "-q", "-m", "base")
			r.git("config", "core.autocrlf", "false")
			core := r.core()
			if tc.warm {
				r.write("b.txt", "two\n")
				r.scan(core, r.dir)
			}
			r.write("a.txt", "one\r\nTWO\r\nthree\r\n")
			r.clock.settle()

			if tc.failRead {
				// Only the first read fails, so a second one would succeed
				// under the config written while the diff ran.
				failed := filepath.Join(t.TempDir(), "failed")
				hooks.set("before-config", fmt.Sprintf("if [ ! -f %[1]q ]; then : > %[1]q; exit 1; fi\n", failed))
			}
			hooks.set("after-diff", fmt.Sprintf("\"$AO_GIT_REAL\" -C %q config core.autocrlf true\n", r.dir))
			if err := os.Truncate(hooks.logPath, 0); err != nil {
				t.Fatal(err)
			}
			first := r.scanNow(core, r.dir)
			hooks.set("after-diff", "")
			if tc.failRead {
				hooks.set("before-config", "")
			}
			reads := strings.Count(readFile(t, hooks.logPath), "config --list")
			if first.insertions != first.deletions || first.insertions < 3 {
				t.Fatalf("first scan = %+v: the diff must run under the first config for this test to mean anything", first)
			}
			want := r.whole(r.dir)
			if want.insertions >= 3 {
				t.Fatalf("whole-tree diff = %+v: the second config must change the diff for this test to mean anything", want)
			}
			if got := r.scan(core, r.dir); got != want {
				t.Fatalf("scan under the second config = %+v, whole-tree diff = %+v", got, want)
			}
			if reads != 1 {
				t.Fatalf("the first scan read the config %d times, want 1", reads)
			}
		})
	}
}
