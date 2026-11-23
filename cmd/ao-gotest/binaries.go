package main

import (
	"bufio"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"agent-overflow/internal/goenvfile"
	"agent-overflow/internal/ownedtemp"
)

// buckets is how many buckets a split package's tests are hashed into. It
// bounds how many of one package's processes can run at once.
const buckets = 8

// runDirPrefix names the directory each run builds its binaries in.
const runDirPrefix = "ao-gotest-"

// runSplit runs inv with declared split: the unsplit `go test` of whole at
// once, and the split packages under a reservation, in a directory of this
// run's own that it removes once every process of the run has exited.
func (r *runner) runSplit(inv invocation, declared []split, whole []string) {
	parent, err := runDirParent()
	if err != nil {
		r.fail("find the directory for the test binaries: %v", err)
		return
	}
	// A run that was killed left its directory; a live one keeps its own.
	if err := ownedtemp.Sweep(parent, runDirPrefix); err != nil {
		r.note("remove a killed run's test binaries: %v", err)
	}
	dir, err := ownedtemp.MakeShared(parent, runDirPrefix)
	if err != nil {
		r.fail("make the directory for the test binaries: %v", err)
		return
	}
	defer func() {
		if err := dir.Release(); err != nil {
			r.note("remove the test binaries: %v", err)
		}
	}()
	// Every process this one starts keeps the directory's lock, so no
	// later run's sweep removes it while one of them runs.
	if _, err := unix.FcntlInt(dir.Lock.Fd(), unix.F_SETFD, 0); err != nil {
		r.fail("pass the lock of %s to the test processes: %v", dir.Dir, err)
		return
	}
	admit, err := r.admission(dir.Dir)
	if err != nil {
		r.fail("%v", err)
		return
	}
	// The unsplit `go test` runs outside the reservation.
	r.run(plan(inv, declared, whole, dir.Dir), admit)
}

// runDirParent is where a run makes its directory: the go command's
// GOTMPDIR, which the test namespace puts on disk (internal/netisolate),
// else the temp directory. A package's binary and its links take some
// 150 MB, too much for the namespace's RAM temp directory.
func runDirParent() (string, error) {
	return goenvfile.Getenv("GOTMPDIR")
}

// job is one `go test`: its arguments after the go command.
type job struct {
	args  []string
	label string
}

// splitPackage is a package run as buckets.
type splitPackage struct {
	split
	// list builds binary, lists the tests and leaves binary in place.
	list   job
	binary string
	// test is the go test arguments every bucket starts with.
	test []string
}

// bucket is one bucket's `go test` and the link it runs the package's
// binary through, which also names its output files.
type bucket struct {
	job
	pkg  *splitPackage
	link string
}

type runPlan struct {
	// whole runs the packages that are not split, if any.
	whole *job
	split []*splitPackage
}

func plan(inv invocation, declared []split, whole []string, dir string) runPlan {
	var p runPlan
	build := []string{"test"}
	test := []string{"test"}
	for _, f := range inv.flags {
		if slices.Contains(listFlags, f.name) {
			build = append(build, f.args...)
		}
		test = append(test, f.args...)
	}
	if len(whole) > 0 {
		p.whole = &job{args: append(slices.Clone(test), whole...)}
	}
	for i, s := range declared {
		binary := filepath.Join(dir, strconv.Itoa(i)+"-"+path.Base(s.pkg)+".test")
		p.split = append(p.split, &splitPackage{
			split:  s,
			list:   job{args: append(slices.Clone(build), "-o", binary, "-list", ".", s.pkg), label: s.pkg + " list"},
			binary: binary,
			test:   test,
		})
	}
	return p
}

// bucketOf is the bucket a test name runs in.
func bucketOf(name string) int {
	h := fnv.New64a()
	h.Write([]byte(name))
	return int(h.Sum64() % buckets)
}

// bucketRun is the -run pattern that selects exactly names.
func bucketRun(names []string) string {
	quoted := make([]string, len(names))
	for i, name := range names {
		quoted[i] = regexp.QuoteMeta(name)
	}
	return "^(?:" + strings.Join(quoted, "|") + ")$"
}

// testName is a line `go test -list` prints for a test, benchmark, fuzz
// test or example: go finds only functions named so.
var testName = regexp.MustCompile(`^(?:Test|Benchmark|Fuzz|Example)[\p{L}\p{N}_]*$`)

// parseList reads the names `go test -list .` printed for pkg, one per
// line, before its `ok` line. go test prints what the binary writes to
// stderr there too, such as a TestMain's log, so the other lines are left.
func parseList(out io.Reader, pkg string) ([]string, error) {
	var names []string
	var last string
	scanner := bufio.NewScanner(out)
	for scanner.Scan() {
		last = scanner.Text()
		if testName.MatchString(last) {
			names = append(names, last)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read the list: %w", err)
	}
	if fields := strings.Fields(last); len(fields) < 2 || fields[0] != "ok" || fields[1] != pkg {
		return nil, fmt.Errorf("the list ends with %q, not the ok line of %s", last, pkg)
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("%s has no tests to split", pkg)
	}
	return names, nil
}

// list runs sp's list step and returns its buckets, none when it failed,
// whose output it prints. started is false once a signal has arrived.
func (r *runner) list(sp *splitPackage) (bs []bucket, started bool) {
	out, err := createOutput(sp.binary + ".list")
	if err != nil {
		r.fail("list the tests of %s: %v", sp.pkg, err)
		return nil, true
	}
	defer func() {
		if err := out.close(); err != nil {
			r.fail("list the tests of %s: %v", sp.pkg, err)
		}
	}()
	var peak uint64
	status, started := r.start(sp.list, out.stdout, out.stderr, &peak)
	if !started {
		return nil, false
	}
	var names []string
	err = fmt.Errorf("exit status %d", status)
	if status == 0 {
		r.mu.Lock()
		r.listPeaks[sp] = peak
		r.mu.Unlock()
		if _, err = out.stdout.Seek(0, io.SeekStart); err == nil {
			names, err = parseList(out.stdout, sp.pkg)
		}
	}
	if err == nil {
		// The buckets run the binary through links. Read-only, it cannot
		// be truncated through one: a bucket that builds its own replaces
		// its link instead.
		err = os.Chmod(sp.binary, 0o555)
	}
	if err != nil {
		r.record(max(status, 1))
		r.out.Lock()
		fmt.Fprintf(r.stderr, "ao-gotest: list the tests of %s: %v\n", sp.pkg, err)
		printErr := out.printTo(r.stderr, r.stderr)
		r.out.Unlock()
		if printErr != nil {
			r.fail("print the list step of %s: %v", sp.pkg, printErr)
		}
		return nil, true
	}
	if info, err := out.stderr.Stat(); err != nil || info.Size() > 0 {
		r.out.Lock()
		fmt.Fprintf(r.stderr, "ao-gotest: %s:\n", sp.list.label)
		printErr := errors.Join(err, copyFile(r.stderr, out.stderr))
		r.out.Unlock()
		if printErr != nil {
			r.fail("print the list step of %s: %v", sp.pkg, printErr)
		}
	}
	byBucket := make([][]string, buckets)
	for _, name := range names {
		i := bucketOf(name)
		byBucket[i] = append(byBucket[i], name)
	}
	for i, names := range byBucket {
		if len(names) == 0 {
			continue
		}
		link := strings.TrimSuffix(sp.binary, ".test") + "-" + strconv.Itoa(i) + ".test"
		if err := os.Link(sp.binary, link); err != nil {
			r.fail("link the test binary of %s for bucket %d: %v", sp.pkg, i, err)
			continue
		}
		args := append(slices.Clone(sp.test), "-o", link, "-parallel", "1", "-run", bucketRun(names), sp.pkg)
		bs = append(bs, bucket{job: job{args: args, label: fmt.Sprintf("%s bucket %d of %d", sp.pkg, i, buckets)}, pkg: sp, link: link})
	}
	return bs, true
}

// replacedLink reports whether the go command replaced link, a link to
// binary, with a binary of its own, as it does when binary is stale.
func replacedLink(binary, link string) (bool, error) {
	shared, err := os.Stat(binary)
	if err != nil {
		return false, err
	}
	linked, err := os.Stat(link)
	if err != nil {
		return false, err
	}
	return !os.SameFile(shared, linked), nil
}
