package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// fixture copies testdata/fixture, a module whose package split runs as
// buckets with the real go command, and runs the tests below in it.
func fixture(t *testing.T) (goCommand, dir string) {
	t.Helper()
	goCommand, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	dir = t.TempDir()
	if err := os.CopyFS(dir, os.DirFS("testdata/fixture")); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	t.Setenv("GOWORK", "off")
	t.Setenv("GOTOOLCHAIN", "local")
	return goCommand, dir
}

// fixturePlan is the plan for `go test -v` of the fixture's package,
// split, with its binaries in a directory of its own.
func fixturePlan(t *testing.T, goCommand string) runPlan {
	t.Helper()
	inv, err := parseArgs([]string{goCommand, "test", "-v", "example.com/fixture/split"})
	if err != nil {
		t.Fatal(err)
	}
	return plan(inv, []split{{"example.com/fixture/split", 1 << 40, 1 << 40}}, nil, t.TempDir())
}

// fixtureRun is one run's output, bucket by bucket.
type fixtureRun struct {
	status int
	// buckets holds each bucket's stdout and stderr by its number.
	buckets map[int]string
	out     string
}

func runFixture(t *testing.T, goCommand string, w int) fixtureRun {
	t.Helper()
	out := &lockedWriter{}
	r := must(newRunner(goCommand, out, out))
	r.cores = buckets
	t.Cleanup(r.close)
	status := r.run(fixturePlan(t, goCommand), grant(w))
	run := fixtureRun{status: status, buckets: map[int]string{}, out: out.String()}
	header := regexp.MustCompile(`(?m)^ao-gotest: example.com/fixture/split bucket (\d) of 8\n`)
	marks := header.FindAllStringSubmatchIndex(run.out, -1)
	for i, m := range marks {
		end := len(run.out)
		if i+1 < len(marks) {
			end = marks[i+1][0]
		}
		n, _ := strconv.Atoi(run.out[m[2]:m[3]])
		run.buckets[n] = run.out[m[1]:end]
	}
	if len(run.buckets) < 4 {
		t.Fatalf("%d buckets ran; want the fixture's names in at least four\n%s", len(run.buckets), run.out)
	}
	return run
}

var (
	cachedLine = regexp.MustCompile(`(?m)^ok  \texample.com/fixture/split\t\(cached\)$`)
	// The go command's debug line for a test ID: the binary's ID, then
	// the test ID, which adds the cacheable flags.
	testID = regexp.MustCompile(`(?m)^testcache: example.com/fixture/split: test ID [0-9a-f]+ => ([0-9a-f]+)$`)
	topRun = regexp.MustCompile(`(?m)^=== RUN   ([^/\s]+)$`)
)

// TestBucketsCacheApartAndAcrossWorkerCounts runs the fixture's package as
// buckets five times with the real go command, each run with a binary at a
// path of its own: TestFails fails twice, then passes, at one, two, four,
// one and two buckets at a time. Each bucket has a test ID, and so a cache
// entry, of its own, so the failing bucket reruns while the rest come from
// the cache, and the last two runs are cached whole: a bucket's entry does
// not depend on how many ran at once or where its binary was.
func TestBucketsCacheApartAndAcrossWorkerCounts(t *testing.T) {
	goCommand, _ := fixture(t)
	t.Setenv("GODEBUG", "gocachetest=1")
	failing := bucketOf("TestFails")
	t.Setenv("FIXTURE_FAIL", "1")
	first := runFixture(t, goCommand, 1)
	if first.status != 1 || !strings.Contains(first.buckets[failing], "--- FAIL: TestFails") {
		t.Fatalf("status %d; want 1 with TestFails failing\n%s", first.status, first.out)
	}
	// Every listed name ran once, in one bucket.
	ran := map[string]int{}
	// Each test ID belongs to one bucket.
	ids := map[string]int{}
	for n, out := range first.buckets {
		for _, m := range topRun.FindAllStringSubmatch(out, -1) {
			ran[m[1]]++
		}
		found := testID.FindAllStringSubmatch(out, -1)
		if found == nil {
			t.Fatalf("bucket %d printed no test ID\n%s", n, out)
		}
		for _, m := range found {
			if owner, ok := ids[m[1]]; ok && owner != n {
				t.Fatalf("buckets %d and %d share test ID %s, and so a cache entry\n%s", owner, n, m[1], first.out)
			}
			ids[m[1]] = n
		}
	}
	listed := []string{"ExampleVersion", "FuzzVersion", "TestA", "TestAB", "TestABC", "TestC", "TestD", "TestE", "TestFails"}
	for _, name := range listed {
		if ran[name] != 1 {
			t.Errorf("%s ran %d times; want once (ran %v)", name, ran[name], ran)
		}
	}
	if len(ran) != len(listed) {
		t.Fatalf("ran %v; want only the listed names", ran)
	}
	second := runFixture(t, goCommand, 2)
	for n, out := range second.buckets {
		if cached := cachedLine.MatchString(out); cached == (n == failing) {
			t.Fatalf("second run, bucket %d (cached %v); want only bucket %d, which failed, to run\n%s", n, cached, failing, second.out)
		}
	}
	if second.status != 1 || !strings.Contains(second.buckets[failing], "--- FAIL: TestFails") {
		t.Fatalf("second run: status %d; want TestFails to fail again\n%s", second.status, second.out)
	}
	// The failing bucket's pass may be cached from an earlier run of this
	// test; the rest are cached from this one.
	t.Setenv("FIXTURE_FAIL", "")
	third := runFixture(t, goCommand, 4)
	for n, out := range third.buckets {
		if third.status != 0 || n != failing && !cachedLine.MatchString(out) {
			t.Fatalf("third run: status %d, bucket %d not cached; want a pass with every bucket but %d cached\n%s", third.status, n, failing, third.out)
		}
	}
	for _, w := range []int{1, 2} {
		run := runFixture(t, goCommand, w)
		for n, out := range run.buckets {
			if run.status != 0 || !cachedLine.MatchString(out) {
				t.Fatalf("at %d at a time: status %d, bucket %d not cached\n%s", w, run.status, n, run.out)
			}
		}
	}
}

// TestABucketThatBuildsItsOwnBinaryIsFailedAndTheSharedOneIsKept changes
// the fixture's package after the list step, when the first bucket starts:
// that bucket builds its own binary over its link and fails with the
// reason, and every other either ran the shared binary, which no bucket
// wrote, or also failed with the reason.
func TestABucketThatBuildsItsOwnBinaryIsFailedAndTheSharedOneIsKept(t *testing.T) {
	goCommand, dir := fixture(t)
	source := filepath.Join(dir, "split", "split.go")
	before, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(proxyEnv, goCommand)
	t.Setenv(proxyEditEnv, source+"="+strings.Replace(string(before), "Version = 1", "Version = 2", 1))
	out := &lockedWriter{}
	r := must(newRunner(os.Args[0], out, out))
	r.cores = buckets
	t.Cleanup(r.close)
	p := fixturePlan(t, os.Args[0])
	binary := p.split[0].binary
	status := r.run(p, grant(2))
	shared, err := os.Stat(binary)
	if err != nil {
		t.Fatal(err)
	}
	if status != 1 || shared.Mode().Perm() != 0o555 {
		t.Fatalf("status %d, shared binary %v; want 1 and the read-only binary in place\n%s", status, shared.Mode(), out.String())
	}
	// The gate recorded the binary the list step left, before the change.
	sum := sha256.Sum256(must(os.ReadFile(binary)))
	if recorded := readFile(t, source+".sum"); recorded != hex.EncodeToString(sum[:]) {
		t.Fatalf("the shared binary is %x at the end, %s when the first bucket started; want it unchanged", sum, recorded)
	}
	reason := regexp.MustCompile(`(?m)^ao-gotest: example.com/fixture/split bucket (\d) of 8 built its own test binary: example.com/fixture/split or a package it imports changed after this run listed its tests`)
	stale := map[int]bool{}
	for _, m := range reason.FindAllStringSubmatch(out.String(), -1) {
		n, _ := strconv.Atoi(m[1])
		stale[n] = true
	}
	if len(stale) == 0 {
		t.Fatalf("no bucket named as having built its own binary\n%s", out.String())
	}
	links, err := filepath.Glob(strings.TrimSuffix(binary, ".test") + "-*.test")
	if err != nil || len(links) < 4 {
		t.Fatalf("links %v, %v", links, err)
	}
	for _, link := range links {
		n, _ := strconv.Atoi(strings.TrimSuffix(link[strings.LastIndexByte(link, '-')+1:], ".test"))
		info, err := os.Stat(link)
		if err != nil {
			t.Fatal(err)
		}
		if os.SameFile(shared, info) == stale[n] {
			t.Errorf("bucket %d: runs the shared binary %v, named as stale %v; want exactly one", n, os.SameFile(shared, info), stale[n])
		}
	}
}
