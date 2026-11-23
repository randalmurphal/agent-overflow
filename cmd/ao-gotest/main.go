// Command ao-gotest runs the `go test` command line it is given, for `make
// go-test`, inside the test namespace (bin/ao-netns). A package named by a
// -split flag runs as buckets: `go test -list` prints its top-level tests,
// each goes to one of a fixed number of buckets by a hash of its name, and
// each bucket that has tests runs as one `go test -parallel 1 -run
// '^(?:names)$'`. The other packages run in one `go test` beside them.
//
//	ao-gotest [-split importpath=MiB]... <go command> test [go test flag or package]...
//
// A package is split when its tests open so many SQLite stores that one
// process cannot use more cores: modernc's allocator lock serializes them.
// The bucket count is fixed, not the number of cores, so a bucket's command
// line, and with it its entry in the test cache, is the same whatever ran
// beside it: a result cached at one worker count is cached at any other,
// and a failed bucket reruns without hiding behind another's pass.
//
// W, the number of buckets running at once, is what the memory governor
// (internal/harness/governor) grants across every run on the host, at most
// one bucket per core: one lease holding a unit, the MiB a package's -split
// flag declares one of its buckets holds, for each of the W largest
// buckets, since no W buckets at once hold more. W is never below 1, so a
// host with nothing to spare runs the buckets one after another. Each
// bucket's largest process is read from rusage when it exits; a package
// whose bucket went over its unit is named at the end of the run, so its
// declaration can be raised.
//
// A split package's test binary is built once per run, by the `go test -o
// P -list` that reads its names, into a directory of this run's own, so a
// fully cached run still links it once. Each bucket runs it through a hard
// link of its own (-o), so no bucket links. A bucket whose package or a
// dependency changed after the list step builds its own binary over its own
// link, never over the one the other buckets run; the run fails that
// bucket and says why, since its tests ran different code.
//
// Nothing is split, and this process becomes exactly the command line it
// was given, when no package it names is declared, or when a flag is
// outside shardFlags: one that narrows the run (-run, -list, -bench, -fuzz),
// sets its concurrency (-parallel, -cpu), needs one binary or one process
// per package (-c, -o, -exec, profiles, coverage, -race) or that this
// command does not know. Packages are matched by import path, as `make
// go-test` passes them.
//
// The unsplit `go test` streams its output line by line; each bucket prints
// its output whole when it ends, after a header line unless -json is given,
// so lines of different processes never interleave. The exit status is the
// worst of the `go test` processes'. Each has the -timeout the flags give.
// A signal this command receives goes to every `go test` it started, no new
// one starts, and it exits once they have.
package main

import (
	"bytes"
	"cmp"
	"errors"
	"flag"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"agent-overflow/internal/harness/governor"
	"agent-overflow/internal/ownedtemp"
)

// buckets is how many buckets a split package's tests are hashed into. It
// bounds how many of one package's processes can run at once.
const buckets = 8

// runDirPrefix names the directory each run builds its binaries in.
const runDirPrefix = "ao-gotest-"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// shardFlags are the go test flags a split run keeps, each with whether it
// takes a value. They only filter, repeat or report the tests, or select the
// files that build (-tags, -vet), the same way in every process.
var shardFlags = map[string]bool{
	"count":    true,
	"failfast": false,
	"json":     false,
	"p":        true,
	"short":    false,
	"shuffle":  true,
	"skip":     true,
	"tags":     true,
	"timeout":  true,
	"v":        false,
	"vet":      true,
}

// buildFlags are the shardFlags the list step, which builds and vets, takes.
var buildFlags = []string{"p", "tags", "vet"}

// split is a -split flag: a package to run as buckets and the bytes one of
// its buckets is declared to hold.
type split struct {
	pkg  string
	unit uint64
}

type splitFlags []split

func (s *splitFlags) String() string { return fmt.Sprint(*s) }

func (s *splitFlags) Set(value string) error {
	pkg, mib, ok := strings.Cut(value, "=")
	n, err := strconv.ParseUint(mib, 10, 32)
	if !ok || pkg == "" || err != nil || n == 0 {
		return fmt.Errorf("want importpath=MiB with a positive whole MiB, got %q", value)
	}
	*s = append(*s, split{pkg: pkg, unit: n << 20})
	return nil
}

// parseSplits reads this command's own flags, up to the go command.
func parseSplits(args []string) ([]split, []string, error) {
	var splits splitFlags
	flags := flag.NewFlagSet("ao-gotest", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.Var(&splits, "split", "run the package as buckets, each declared to hold MiB")
	if err := flags.Parse(args); err != nil {
		return nil, nil, fmt.Errorf("-split: %w", err)
	}
	return splits, flags.Args(), nil
}

// invocation is a `go test` command line, argv, read as go test reads it.
type invocation struct {
	argv     []string
	flags    []parsedFlag
	packages []string
	// shardable is false when an argument is a flag outside shardFlags,
	// whose value this command cannot tell from a package.
	shardable bool
}

// parsedFlag is one go test flag: its name without dashes and its value,
// with the arguments it took.
type parsedFlag struct {
	name, value string
	args        []string
}

// parseArgs reads argv, `<go command> test` and its arguments. It reads
// flags as go test does, -name, --name, -name=value or -name value, and
// every other argument as a package.
func parseArgs(argv []string) (invocation, error) {
	if len(argv) < 2 || argv[1] != "test" {
		return invocation{}, fmt.Errorf("usage: ao-gotest [-split importpath=MiB]... <go command> test [go test flag or package]...; got %q", argv)
	}
	inv := invocation{argv: argv, shardable: true}
	args := argv[2:]
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") {
			inv.packages = append(inv.packages, arg)
			continue
		}
		name, value, hasValue := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(arg, "-"), "-"), "=")
		takesValue, known := shardFlags[name]
		if !known || (takesValue && !hasValue && i+1 == len(args)) {
			inv.shardable = false
			return inv, nil
		}
		taken := []string{arg}
		if takesValue && !hasValue {
			i++
			value = args[i]
			taken = append(taken, value)
		}
		inv.flags = append(inv.flags, parsedFlag{name: name, value: value, args: taken})
	}
	return inv, nil
}

func run(argv []string, stdout, stderr io.Writer) int {
	splits, rest, err := parseSplits(argv)
	if err == nil {
		var inv invocation
		inv, err = parseArgs(rest)
		if err == nil {
			return runInvocation(inv, splits, stdout, stderr)
		}
	}
	fmt.Fprintln(stderr, "ao-gotest:", err)
	return 2
}

func runInvocation(inv invocation, splits []split, stdout, stderr io.Writer) int {
	if !inv.shardable {
		return passThrough(inv.argv, stderr)
	}
	// Declaration order is the order the packages are listed and their
	// buckets start in, so the longest goes first.
	var declared []split
	var names []string
	var units []uint64
	for _, s := range splits {
		if slices.Contains(inv.packages, s.pkg) {
			declared = append(declared, s)
			names = append(names, s.pkg)
			for range buckets {
				units = append(units, s.unit)
			}
		}
	}
	if len(declared) == 0 {
		return passThrough(inv.argv, stderr)
	}
	whole := slices.DeleteFunc(slices.Clone(inv.packages), func(pkg string) bool { return slices.Contains(names, pkg) })
	r := newRunner(inv.argv[0], stdout, stderr)
	// A run that was killed left its directory; a live one keeps its own.
	if err := ownedtemp.Sweep("", runDirPrefix); err != nil {
		r.note("remove a killed run's test binaries: %v", err)
	}
	dir, removeDir, err := ownedtemp.Make("", runDirPrefix)
	if err != nil {
		fmt.Fprintln(stderr, "ao-gotest: make the directory for the test binaries:", err)
		return 1
	}
	slices.SortFunc(units, func(a, b uint64) int { return cmp.Compare(b, a) })
	w, release, granted := r.workers(dir, units[:min(runtime.GOMAXPROCS(0), len(units))])
	fmt.Fprintf(stderr, "ao-gotest: %s: %d buckets each, %d at a time (%s)\n", strings.Join(names, ", "), buckets, w, granted)
	status := r.run(plan(inv, declared, whole, dir), w)
	if err := release(); err != nil {
		r.note("release the memory reservation: %v", err)
	}
	if err := removeDir(); err != nil {
		r.note("remove the test binaries: %v", err)
	}
	r.printNotes()
	return status
}

// workers reserves memory in the host's governor for as many buckets at
// once as units has, largest first, and returns how many may run at once,
// the release of the reservation and what was reserved.
func (r *runner) workers(dir string, units []uint64) (int, func() error, string) {
	manager, err := governor.New(governor.Options{})
	var worktree string
	if err == nil {
		worktree, err = os.Getwd()
	}
	if err != nil {
		r.note("reserve memory for the buckets: %v; they ran one at a time", err)
		return 1, func() error { return nil }, "no reservation"
	}
	return reserveWorkers(r, manager, governor.Request{
		RunID:    "go-test-" + strconv.Itoa(os.Getpid()),
		Worktree: worktree,
		DataRoot: dir,
		OwnerPID: os.Getpid(),
	}, units)
}

// reserveWorkers is workers' reservation in m. W is what m granted, or 1
// when it granted none or failed: one bucket at a time holds no lease, as
// one unsplit `go test` holds none, so a run never waits for memory.
func reserveWorkers(r *runner, m *governor.Manager, req governor.Request, units []uint64) (int, func() error, string) {
	lease, granted, err := m.ReserveUpTo(req, units)
	if err != nil {
		r.note("reserve memory for the buckets: %v; they ran one at a time", err)
		return 1, func() error { return nil }, "no reservation"
	}
	if granted == 0 {
		return 1, func() error { return nil }, fmt.Sprintf("no %d MiB free above the host's floor and other runs' reservations", units[0]>>20)
	}
	return granted, func() error { return m.Release(lease) }, fmt.Sprintf("%d MiB reserved", lease.CeilingBytes>>20)
}

// passThrough replaces this process with argv, the one `go test` of every
// package, so a run that splits nothing is the command `make go-test` ran
// before this one existed: its arguments, environment and process tree.
func passThrough(argv []string, stderr io.Writer) int {
	path, err := exec.LookPath(argv[0])
	if err != nil {
		fmt.Fprintln(stderr, "ao-gotest:", err)
		return 1
	}
	err = syscall.Exec(path, argv, os.Environ())
	fmt.Fprintf(stderr, "ao-gotest: exec %s: %v\n", path, err)
	return 1
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
// binary through.
type bucket struct {
	job
	pkg  *splitPackage
	link string
}

type runPlan struct {
	// whole runs the packages that are not split, if any.
	whole *job
	split []*splitPackage
	json  bool
}

func plan(inv invocation, declared []split, whole []string, dir string) runPlan {
	var p runPlan
	build := []string{"test"}
	test := []string{"test"}
	for _, f := range inv.flags {
		if f.name == "json" && f.value != "false" {
			p.json = true
		}
		if slices.Contains(buildFlags, f.name) {
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
func parseList(out []byte, pkg string) ([]string, error) {
	lines := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	last := lines[len(lines)-1]
	if fields := strings.Fields(last); len(fields) < 2 || fields[0] != "ok" || fields[1] != pkg {
		return nil, fmt.Errorf("the list ends with %q, not the ok line of %s", last, pkg)
	}
	names := slices.DeleteFunc(lines[:len(lines)-1], func(line string) bool { return !testName.MatchString(line) })
	if len(names) == 0 {
		return nil, fmt.Errorf("%s has no tests to split", pkg)
	}
	return names, nil
}

// runner starts the jobs and serializes their output.
type runner struct {
	// goCommand is the go command the jobs run, the one the command line
	// names.
	goCommand      string
	stdout, stderr io.Writer
	// out guards stdout and stderr: one line of the unsplit run, or one
	// whole process's output, at a time. It is never held while taking mu.
	out sync.Mutex

	mu      sync.Mutex
	running map[*exec.Cmd]bool
	stopped bool
	worst   int
	// notes are printed when the run ends.
	notes []string
	// peaks is each split package's largest bucket process.
	peaks map[*splitPackage]uint64
}

func newRunner(goCommand string, stdout, stderr io.Writer) *runner {
	return &runner{goCommand: goCommand, stdout: stdout, stderr: stderr, running: map[*exec.Cmd]bool{}, peaks: map[*splitPackage]uint64{}}
}

// run runs p's unsplit `go test` beside the split packages: each package's
// list step, one after another, then its buckets, w at a time.
func (r *runner) run(p runPlan, w int) int {
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	defer signal.Stop(signals)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case sig := <-signals:
				r.signal(sig)
			case <-done:
				return
			}
		}
	}()
	defer close(done)

	var wg sync.WaitGroup
	if p.whole != nil {
		wg.Go(func() { r.runWhole(*p.whole) })
	}
	// Room for every bucket, so listing a package never waits for the
	// buckets of the one before it to start.
	queue := make(chan bucket, buckets*len(p.split))
	for range w {
		wg.Go(func() {
			for b := range queue {
				r.runBucket(b, p.json)
			}
		})
	}
	for _, sp := range p.split {
		bs, started := r.list(sp)
		if !started {
			break
		}
		for _, b := range bs {
			queue <- b
		}
	}
	close(queue)
	wg.Wait()
	for _, sp := range p.split {
		if peak := r.peaks[sp]; peak > sp.unit {
			r.note("%s: a bucket's largest process reached %d MiB, over the %d MiB its -split declares; raise it in GO_TEST_SPLIT in the Makefile", sp.pkg, peak>>20, sp.unit>>20)
		}
	}
	return r.worst
}

// list runs sp's list step and returns its buckets, none when it failed,
// whose output it prints. started is false once a signal has arrived.
func (r *runner) list(sp *splitPackage) (bs []bucket, started bool) {
	var stdout, stderr bytes.Buffer
	status, started := r.start(sp.list, &stdout, &stderr, nil)
	if !started {
		return nil, false
	}
	var names []string
	err := fmt.Errorf("exit status %d", status)
	if status == 0 {
		names, err = parseList(stdout.Bytes(), sp.pkg)
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
		_, _ = r.stderr.Write(stdout.Bytes())
		_, _ = r.stderr.Write(stderr.Bytes())
		r.out.Unlock()
		return nil, true
	}
	if stderr.Len() > 0 {
		r.out.Lock()
		fmt.Fprintf(r.stderr, "ao-gotest: %s:\n", sp.list.label)
		_, _ = r.stderr.Write(stderr.Bytes())
		r.out.Unlock()
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
			r.record(1)
			r.out.Lock()
			fmt.Fprintf(r.stderr, "ao-gotest: link the test binary of %s for bucket %d: %v\n", sp.pkg, i, err)
			r.out.Unlock()
			continue
		}
		args := append(slices.Clone(sp.test), "-o", link, "-parallel", "1", "-run", bucketRun(names), sp.pkg)
		bs = append(bs, bucket{job: job{args: args, label: fmt.Sprintf("%s bucket %d of %d", sp.pkg, i, buckets)}, pkg: sp, link: link})
	}
	return bs, true
}

func (r *runner) runWhole(j job) {
	lines := &lineWriter{out: &r.out}
	errLines := &lineWriter{out: &r.out}
	lines.w, errLines.w = r.stdout, r.stderr
	if status, started := r.start(j, lines, errLines, nil); started {
		r.record(status)
	}
	lines.flush()
	errLines.flush()
}

func (r *runner) runBucket(b bucket, json bool) {
	var stdout, stderr bytes.Buffer
	if !json {
		stdout.WriteString("ao-gotest: " + b.label + "\n")
	}
	var peak uint64
	status, started := r.start(b.job, &stdout, &stderr, &peak)
	if !started {
		return
	}
	stale, err := replacedLink(b.pkg.binary, b.link)
	switch {
	case err != nil:
		fmt.Fprintf(&stderr, "ao-gotest: %s: %v\n", b.label, err)
		status = max(status, 1)
	case stale:
		fmt.Fprintf(&stderr, "ao-gotest: %s built its own test binary: %s or a package it imports changed after this run listed its tests, so this bucket tested other code than the rest; run again to test one version\n", b.label, b.pkg.pkg)
		status = max(status, 1)
	case status == 0:
		// A bucket that built its own binary, or failed to, peaked in the
		// build; one that passed on the shared binary measures its tests.
		r.mu.Lock()
		r.peaks[b.pkg] = max(r.peaks[b.pkg], peak)
		r.mu.Unlock()
	}
	r.record(status)
	r.out.Lock()
	_, errOut := r.stdout.Write(stdout.Bytes())
	_, errErr := r.stderr.Write(stderr.Bytes())
	r.out.Unlock()
	if errOut != nil || errErr != nil {
		r.record(1)
	}
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

// start runs j to completion and returns its status, and when peak is not
// nil sets it to the largest process's resident set. It starts nothing
// once a signal has arrived.
func (r *runner) start(j job, stdout, stderr io.Writer, peak *uint64) (status int, started bool) {
	command := exec.Command(r.goCommand, j.args...)
	command.Stdout, command.Stderr = stdout, stderr
	// The parent-death signal fires when the thread that started the
	// child exits, so that thread lives until the child has.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	setParentDeathSignal(command)
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return 0, false
	}
	err := command.Start()
	if err == nil {
		r.running[command] = true
	}
	r.mu.Unlock()
	if err != nil {
		r.record(1)
		r.out.Lock()
		fmt.Fprintf(r.stderr, "ao-gotest: start %s: %v\n", strings.Join(command.Args, " "), err)
		r.out.Unlock()
		return 1, false
	}
	err = command.Wait()
	r.mu.Lock()
	delete(r.running, command)
	r.mu.Unlock()
	if peak != nil {
		if usage, ok := command.ProcessState.SysUsage().(*syscall.Rusage); ok {
			*peak = maxRSSBytes(usage)
		}
	}
	return exitStatus(command, err), true
}

func (r *runner) record(status int) {
	r.mu.Lock()
	r.worst = max(r.worst, status)
	r.mu.Unlock()
}

// note keeps a line for the end of the run, where it is not lost among the
// packages' output.
func (r *runner) note(format string, args ...any) {
	r.mu.Lock()
	r.notes = append(r.notes, fmt.Sprintf("ao-gotest: "+format, args...))
	r.mu.Unlock()
}

func (r *runner) printNotes() {
	r.mu.Lock()
	notes := r.notes
	r.mu.Unlock()
	r.out.Lock()
	defer r.out.Unlock()
	for _, line := range notes {
		fmt.Fprintln(r.stderr, line)
	}
}

func (r *runner) signal(sig os.Signal) {
	var failed []string
	r.mu.Lock()
	r.stopped = true
	r.worst = max(r.worst, 1)
	for command := range r.running {
		if err := command.Process.Signal(sig); err != nil && !errors.Is(err, os.ErrProcessDone) {
			failed = append(failed, fmt.Sprintf("ao-gotest: forward %v to %s: %v\n", sig, strings.Join(command.Args, " "), err))
		}
	}
	r.mu.Unlock()
	r.out.Lock()
	defer r.out.Unlock()
	for _, line := range failed {
		fmt.Fprint(r.stderr, line)
	}
}

func exitStatus(command *exec.Cmd, err error) int {
	if err == nil {
		return 0
	}
	if status, ok := command.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return 128 + int(status.Signal())
	}
	if code := command.ProcessState.ExitCode(); code > 0 {
		return code
	}
	return 1
}

// lineWriter passes whole lines to w under out, holding back a partial one.
type lineWriter struct {
	out     *sync.Mutex
	w       io.Writer
	partial []byte
}

func (l *lineWriter) Write(p []byte) (int, error) {
	l.partial = append(l.partial, p...)
	end := bytes.LastIndexByte(l.partial, '\n')
	if end < 0 {
		return len(p), nil
	}
	l.out.Lock()
	_, err := l.w.Write(l.partial[:end+1])
	l.out.Unlock()
	l.partial = append(l.partial[:0], l.partial[end+1:]...)
	return len(p), err
}

func (l *lineWriter) flush() {
	if len(l.partial) == 0 {
		return
	}
	l.out.Lock()
	_, _ = l.w.Write(l.partial)
	l.out.Unlock()
	l.partial = nil
}
