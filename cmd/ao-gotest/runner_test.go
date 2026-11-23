package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func fakeRunner(t *testing.T, env ...string) (r *runner, stdout, stderr *lockedWriter, log string) {
	t.Helper()
	log = filepath.Join(t.TempDir(), "calls")
	t.Setenv(fakeEnv, log)
	t.Setenv(markEnv, "")
	for _, kv := range env {
		name, value, _ := strings.Cut(kv, "=")
		t.Setenv(name, value)
	}
	t.Setenv(standInsEnv, tieStandIns(t))
	stdout, stderr = &lockedWriter{}, &lockedWriter{}
	r = must(newRunner(os.Args[0], stdout, stderr))
	r.cores = buckets
	t.Cleanup(r.close)
	return r, stdout, stderr, log
}

// grant is an admitter that grants each phase up to w of its jobs at once
// and holds no memory.
func grant(w int) admitter {
	return func(_ phase, totals []uint64) (int, func() error, error) {
		return min(w, len(totals)), func() error { return nil }, nil
	}
}

// calls are the fake's logged calls, sorted, without the hold lines.
func calls(t *testing.T, log string) []string {
	t.Helper()
	var got []string
	for line := range strings.Lines(readFile(t, log)) {
		if !strings.HasPrefix(line, "held ") && !strings.HasPrefix(line, "ended ") && !strings.HasPrefix(line, "listed ") {
			got = append(got, strings.TrimSuffix(line, "\n"))
		}
	}
	slices.Sort(got)
	return got
}

func testPlan(t *testing.T, dir string, unit uint64) runPlan {
	t.Helper()
	inv, err := parseArgs([]string{"go", "test", "-timeout", "30m", "-count=1", "-tags", "x", "example/whole", "example/split"})
	if err != nil {
		t.Fatal(err)
	}
	return plan(inv, []split{{"example/split", unit, unit}}, []string{"example/whole"}, dir)
}

// wantCalls are the calls the plan of testPlan makes: the list step, one
// call per bucket with a link of its own and exactly its names, and the
// unsplit run. Every listed name is in one bucket.
func wantCalls(dir string) []string {
	binary := filepath.Join(dir, "0-split.test")
	want := []string{
		"go test -tags x -o " + binary + " -list . example/split|",
		"go test -timeout 30m -count=1 -tags x example/whole|",
	}
	byBucket := map[int][]string{}
	for _, name := range fakeNames {
		byBucket[bucketOf(name)] = append(byBucket[bucketOf(name)], name)
	}
	for i, names := range byBucket {
		link := filepath.Join(dir, "0-split-"+strconv.Itoa(i)+".test")
		want = append(want, "go test -timeout 30m -count=1 -tags x -o "+link+" -parallel 1 -run ^(?:"+strings.Join(names, "|")+")$ example/split|")
	}
	slices.Sort(want)
	return want
}

// TestASplitRunPrintsWholeLinesAndEachBucketWhole runs a split package's
// buckets and an unsplit run at once and checks the calls each made, that
// no line of one is cut by another's, and that each bucket's output is
// one block under its header.
func TestASplitRunPrintsWholeLinesAndEachBucketWhole(t *testing.T) {
	r, stdout, stderr, log := fakeRunner(t, fakeFailEnv+"=TestC")
	dir := t.TempDir()
	if status := r.run(testPlan(t, dir, 1<<40), grant(3)); status != 3 {
		t.Errorf("status %d; want 3, the failed bucket's", status)
	}
	if got, want := calls(t, log), wantCalls(dir); !slices.Equal(got, want) {
		t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	blocks := len(wantCalls(dir)) - 2
	checkBlocks(t, "stdout", stdout.String(), "line", blocks, true)
	// Once its buckets are granted, the run says how many run at once.
	granted := "ao-gotest: example/split: 8 buckets each, 3 at a time (3145728 MiB reserved)\n"
	if n := strings.Count(stderr.String(), granted); n != 1 {
		t.Fatalf("stderr says %d times %q; want once:\n%s", n, granted, stderr.String())
	}
	checkBlocks(t, "stderr", strings.Replace(stderr.String(), granted, "", 1), "err", blocks, false)
	if info, err := os.Stat(filepath.Join(dir, "0-split.test")); err != nil || info.Mode().Perm() != 0o555 {
		t.Fatalf("the shared binary: %v, %v; want it read-only", info, err)
	}
}

// TestEveryListStepRunsBeforeAnyBucket splits two packages: both list
// steps end, one after the other, before the first bucket starts, so no
// link runs beside a bucket the reservation counts.
func TestEveryListStepRunsBeforeAnyBucket(t *testing.T) {
	r, _, _, log := fakeRunner(t)
	inv, err := parseArgs([]string{"go", "test", "-count=1", "example/split", "example/other"})
	if err != nil {
		t.Fatal(err)
	}
	p := plan(inv, []split{{"example/split", 1 << 40, 1 << 40}, {"example/other", 1 << 40, 1 << 40}}, nil, t.TempDir())
	if status := r.run(p, grant(8)); status != 0 {
		t.Fatalf("status %d", status)
	}
	var order []string
	for line := range strings.Lines(readFile(t, log)) {
		switch {
		case strings.HasPrefix(line, "listed "):
			order = append(order, strings.TrimSpace(line))
		case strings.Contains(line, " -list . "):
			order = append(order, "list "+strings.Fields(line)[len(strings.Fields(line))-1])
		case strings.Contains(line, " -run "):
			if len(order) == 0 || order[len(order)-1] != "bucket" {
				order = append(order, "bucket")
			}
		}
	}
	want := []string{"list example/split|", "listed example/split", "list example/other|", "listed example/other", "bucket"}
	if !slices.Equal(order, want) {
		t.Fatalf("order %q; want %q\n%s", order, want, readFile(t, log))
	}
}

// checkBlocks checks that every line is one the fake wrote whole, that the
// unsplit run's lines came in order, and that each bucket's lines came as
// one block of consecutive lines, after its header when headed.
func checkBlocks(t *testing.T, stream, out, word string, blocks int, headed bool) {
	t.Helper()
	header := regexp.MustCompile(`^ao-gotest: example/split bucket \d of 8$`)
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	whole := 0
	seen := map[string]bool{}
	for i := 0; i < len(lines); i++ {
		if header.MatchString(lines[i]) != (headed && strings.HasPrefix(lines[i], "ao-gotest: ")) {
			t.Fatalf("%s: line %q; want a header only before a bucket's block in stdout", stream, lines[i])
		}
		if headed {
			if !header.MatchString(lines[i]) {
				if lines[i] != fmt.Sprintf("whole %s %d", word, whole) {
					t.Fatalf("%s: line %q; want a header or the unsplit run's next line", stream, lines[i])
				}
				whole++
				continue
			}
			i++
		}
		if i >= len(lines) {
			t.Fatalf("%s: a header ends the output", stream)
		}
		owner, _, ok := strings.Cut(lines[i], " "+word+" ")
		if !ok || !strings.HasPrefix(owner, "bucket ") {
			if headed || lines[i] != fmt.Sprintf("whole %s %d", word, whole) {
				t.Fatalf("%s: line %q; want a bucket's first line or the unsplit run's next", stream, lines[i])
			}
			whole++
			continue
		}
		for n := range fakeLines {
			want := fmt.Sprintf("%s %s %d", owner, word, n)
			if i >= len(lines) || lines[i] != want {
				t.Fatalf("%s: in the block of %s, line %d is %q; want %q", stream, owner, n, lines[min(i, len(lines)-1)], want)
			}
			i++
		}
		i--
		seen[owner] = true
	}
	if whole != fakeLines || len(seen) != blocks {
		t.Errorf("%s: %d lines of the unsplit run and the blocks of %v; want %d and %d blocks", stream, whole, seen, fakeLines, blocks)
	}
}

// TestAFailedListPrintsItsOutputOnceAndRunsNoBucket checks that a split
// package whose list step fails reports that once, with its status, and
// starts no bucket.
func TestAFailedListPrintsItsOutputOnceAndRunsNoBucket(t *testing.T) {
	r, _, stderr, log := fakeRunner(t, fakeListEnv+"=2")
	dir := t.TempDir()
	if status := r.run(testPlan(t, dir, 1<<40), grant(3)); status != 2 {
		t.Errorf("status %d; want 2, the list step's", status)
	}
	want := []string{
		"go test -tags x -o " + filepath.Join(dir, "0-split.test") + " -list . example/split|",
		"go test -timeout 30m -count=1 -tags x example/whole|",
	}
	if got := calls(t, log); !slices.Equal(got, want) {
		t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if out := stderr.String(); strings.Count(out, "list failed: a build error\n") != 1 || !strings.Contains(out, "ao-gotest: list the tests of example/split: exit status 2\n") {
		t.Fatalf("stderr:\n%s\nwant the list step's failure once, named", out)
	}
}

// TestABucketThatBuildsItsOwnBinaryFailsAndSaysWhy has one bucket replace
// its link, as the go command does when the package changed after the
// list step: that bucket fails with the reason, and the others pass.
func TestABucketThatBuildsItsOwnBinaryFailsAndSaysWhy(t *testing.T) {
	r, _, stderr, _ := fakeRunner(t, fakeStaleEnv+"=TestC")
	if status := r.run(testPlan(t, t.TempDir(), 1<<40), grant(3)); status != 1 {
		t.Errorf("status %d; want 1", status)
	}
	label := fmt.Sprintf("example/split bucket %d of 8", bucketOf("TestC"))
	want := "ao-gotest: " + label + " built its own test binary: example/split or a package it imports changed after this run listed its tests"
	if out := stderr.String(); strings.Count(out, " built its own test binary") != 1 || !strings.Contains(out, want) {
		t.Fatalf("stderr:\n%s\nwant one bucket named: %s", out, want)
	}
}

// TestABucketOverItsUnitIsNamedAtTheEnd declares a unit below what any
// process holds, and then one above it.
func TestABucketOverItsUnitIsNamedAtTheEnd(t *testing.T) {
	for _, tc := range []struct {
		unit  uint64
		named bool
	}{{1 << 20, true}, {1 << 40, false}} {
		r, _, stderr, _ := fakeRunner(t)
		if status := r.run(testPlan(t, t.TempDir(), tc.unit), grant(3)); status != 0 {
			t.Fatalf("status %d", status)
		}
		r.printNotes()
		lines := strings.Split(strings.TrimSuffix(stderr.String(), "\n"), "\n")
		over := fmt.Sprintf(" MiB, over the %d MiB its -split declares; raise it in GO_TEST_SPLIT in the Makefile", tc.unit>>20)
		bucket := len(lines) > 0 && strings.HasPrefix(lines[len(lines)-1], "ao-gotest: example/split: a bucket's largest process reached ") && strings.HasSuffix(lines[len(lines)-1], over)
		list := len(lines) > 1 && strings.HasPrefix(lines[len(lines)-2], "ao-gotest: example/split: its list step's largest process reached ") && strings.HasSuffix(lines[len(lines)-2], over)
		if bucket != tc.named || list != tc.named {
			t.Errorf("unit %d MiB: last lines %q; bucket and list step named %v and %v, want %v", tc.unit>>20, lines[max(0, len(lines)-2):], bucket, list, tc.named)
		}
	}
}

// TestASignalReachesEveryRunningProcessAndStartsNoMore holds every run
// open, each with children that outlive it unless they are signaled too,
// signals the runner, and checks that each running `go test` and every
// child got the signal and ended with it, that the run ended only after
// they had, and that no queued bucket started after it. While the
// buckets hold, what each wrote is in its file in the run's directory.
func TestASignalReachesEveryRunningProcessAndStartsNoMore(t *testing.T) {
	kids := t.TempDir()
	r, stdout, _, log := fakeRunner(t, fakeHoldEnv+"=1", fakeChildEnv+"="+kids)
	dir := t.TempDir()
	done := make(chan int)
	// Two slots for more buckets: the rest are queued when the signal
	// comes.
	go func() { done <- r.run(testPlan(t, dir, 1<<40), grant(2)) }()
	// The unsplit run and the buckets in the two slots hold, each once its
	// children and its signal handler are set.
	deadline := time.Now().Add(time.Minute)
	for strings.Count(readFile(t, log), "held ") < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("the runs never held:\n%s", readFile(t, log))
		}
		time.Sleep(5 * time.Millisecond)
	}
	outputs, err := filepath.Glob(filepath.Join(dir, "0-split-*.test.out"))
	if err != nil || len(outputs) != 2 {
		t.Fatalf("output files %v, %v; want the two running buckets'", outputs, err)
	}
	for _, path := range outputs {
		if got := readFile(t, path); !strings.HasPrefix(got, "before hold bucket ") {
			t.Fatalf("%s holds %q; want what its bucket wrote so far", path, got)
		}
	}
	r.signal(syscall.SIGTERM)
	var status int
	select {
	case status = <-done:
	case <-time.After(time.Minute):
		t.Fatal("the runner did not end after the signal")
	}
	checkChildrenGone(t, kids, 3*childrenPerRun(), true)
	if got := len(recorded(t, kids)); got != 3*childrenPerRun() {
		t.Errorf("%d children started; want those of the 3 runs that held", got)
	}
	if status != 128+int(syscall.SIGTERM) {
		t.Errorf("status %d; want %d", status, 128+int(syscall.SIGTERM))
	}
	if got := strings.Count(stdout.String(), "got terminated"); got != 3 {
		t.Errorf("%d runs got the signal; want 3\n%s", got, stdout.String())
	}
	if got := calls(t, log); len(got) != 4 {
		t.Errorf("calls:\n%s\nwant the list step, the unsplit run and two buckets", strings.Join(got, "\n"))
	}
}
