// Command ao-gotest runs the `go test` command line it is given, for `make
// go-test`, inside the test namespace (bin/ao-netns). A package named by a
// -split flag runs as buckets: `go test -list` prints its top-level tests,
// each goes to one of a fixed number of buckets by a hash of its name, and
// each bucket that has tests runs as one `go test -parallel 1 -run
// '^(?:names)$'`. The other packages run in one `go test` beside them.
//
//	ao-gotest [-split importpath=MiB,MiB]... <go command> test [go test flag or package]...
//
// A package is split when its tests open so many SQLite stores that one
// process cannot use more cores: modernc's allocator lock serializes them.
// The bucket count is fixed, not the number of cores, so a bucket's command
// line, and with it its entry in the test cache, is the same whatever ran
// beside it: a result cached at one worker count is cached at any other,
// and a failed bucket reruns without hiding behind another's pass.
//
// Nothing is split, and this process becomes exactly the command line it
// was given, when no package it names is declared, or when a flag on the
// command line or in GOFLAGS means something else in a bucket than in one
// `go test` of the package. The command line may hold only bucketFlags, and
// GOFLAGS also goflagsBuildFlags; anything else, such as -run, -list, -json,
// -failfast, -p, -cpu, -race, a profile or coverage, runs as one `go test`.
// Packages are matched by import path, as `make go-test` passes them.
//
// A split run reserves memory in the host's memory governor
// (internal/harness/governor) before it builds anything, and runs W
// buckets at once, W being the units the governor grants
// (reservationUnits). With none to grant it says so once and asks again
// until admitWait has passed, then fails: starting a bucket the host cannot
// hold is what takes it down. The unsplit `go test` runs outside the
// reservation from the start, as it did before this command existed.
//
// Under the reservation the list steps run first, one at a time: each
// builds its package's test binary once per run, by the `go test -o P
// -list` that reads its names, into a directory of this run's own on the
// go command's work area (runDirParent). Then the buckets run, W at a time,
// packages in declaration order, so the longest starts first. Each bucket
// runs the binary through a hard link of its own (-o), so no bucket links.
// A bucket whose package or a dependency changed after the list step builds
// its own binary over its own link, never over the one the other buckets
// run; the run fails that bucket and says why, since its tests ran
// different code. Each bucket's and list step's largest process is read
// from rusage; one over its package's declaration is named at the end of
// the run, so the declaration can be raised.
//
// Every `go test` runs under a leader of its own (lead), which ends the
// processes it started, however its `go test` ended and however this
// process ends; this process waits for every leader before it releases
// the reservation and removes the directory. A signal this process
// receives goes to every leader, which passes it to its tree, and no new
// `go test` starts.
//
// The unsplit `go test` streams its output line by line. A bucket writes
// its output to files in the run's directory, printed whole when it ends
// after a header line, so lines of different processes never interleave
// and no bucket's output is held in memory. The exit status is the worst of
// the `go test` processes'. Each has the -timeout the flags give.
package main

import (
	"bufio"
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
	"time"

	"golang.org/x/sys/unix"

	"agent-overflow/internal/goenvfile"
	"agent-overflow/internal/harness/governor"
	"agent-overflow/internal/ownedtemp"
)

// buckets is how many buckets a split package's tests are hashed into. It
// bounds how many of one package's processes can run at once.
const buckets = 8

// runDirPrefix names the directory each run builds its binaries in.
const runDirPrefix = "ao-gotest-"

// admitWait is how long a split run asks the governor for memory before it
// fails, and admitPoll how often it asks.
const (
	admitWait = 10 * time.Minute
	admitPoll = 5 * time.Second
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == leaderArg {
		os.Exit(lead(os.Args[2:], os.Stdin, os.Stderr))
	}
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// bucketFlags are the go test flags a split run keeps, each with whether it
// takes a value: each means the same in every bucket as in one `go test` of
// the package. -tags and -vet select what builds and is checked, the same
// in every process; the rest filter, repeat, time or report each test.
var bucketFlags = map[string]bool{
	"count":   true,
	"short":   false,
	"shuffle": true,
	"skip":    true,
	"tags":    true,
	"timeout": true,
	"v":       false,
	"vet":     true,
}

// listFlags are the bucketFlags the list step, which builds and vets, takes.
var listFlags = []string{"tags", "vet"}

// goflagsBuildFlags are the build flags GOFLAGS may also set in a split
// run. Every process of the run reads the same GOFLAGS, so they build the
// list step's binary as they build each bucket's, and change nothing else.
// make sets -trimpath there.
var goflagsBuildFlags = []string{"buildvcs", "mod", "modcacherw", "trimpath"}

// split is a -split flag: a package to run as buckets and the bytes one of
// its buckets, and its list step, is declared to hold.
type split struct {
	pkg                  string
	bucketUnit, listUnit uint64
}

type splitFlags []split

func (s *splitFlags) String() string { return fmt.Sprint(*s) }

func (s *splitFlags) Set(value string) error {
	pkg, mibs, ok := strings.Cut(value, "=")
	bucket, list, two := strings.Cut(mibs, ",")
	b, bucketErr := strconv.ParseUint(bucket, 10, 32)
	l, listErr := strconv.ParseUint(list, 10, 32)
	if !ok || !two || pkg == "" || bucketErr != nil || listErr != nil || b == 0 || l == 0 {
		return fmt.Errorf("want importpath=MiB,MiB, what one bucket and the list step each hold in positive whole MiB, got %q", value)
	}
	*s = append(*s, split{pkg: pkg, bucketUnit: b << 20, listUnit: l << 20})
	return nil
}

// parseSplits reads this command's own flags, up to the go command.
func parseSplits(args []string) ([]split, []string, error) {
	var splits splitFlags
	flags := flag.NewFlagSet("ao-gotest", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.Var(&splits, "split", "run the package as buckets, each and its list step declared to hold MiB,MiB")
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
	// shardable is false when an argument is a flag outside bucketFlags,
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
		return invocation{}, fmt.Errorf("usage: ao-gotest [-split importpath=MiB,MiB]... <go command> test [go test flag or package]...; got %q", argv)
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
		takesValue, known := bucketFlags[name]
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

// goflagsSplit reports whether value, GOFLAGS, leaves a run splittable.
// The go command reads it as fields of -flag or -flag=value, a field
// quoted when it holds a space, and applies each to every go command that
// knows the flag, the list step and every bucket alike; test flags may
// carry a "test." prefix. A field this command cannot read is the go
// command's to report, so the run is not split.
func goflagsSplit(value string) bool {
	fields, ok := splitQuoted(value)
	if !ok {
		return false
	}
	for _, field := range fields {
		name, _, _ := strings.Cut(field, "=")
		if !strings.HasPrefix(name, "-") {
			return false
		}
		name = strings.TrimPrefix(strings.TrimPrefix(strings.TrimPrefix(name, "-"), "-"), "test.")
		if _, known := bucketFlags[name]; !known && !slices.Contains(goflagsBuildFlags, name) {
			return false
		}
	}
	return true
}

// splitQuoted splits s as the go command splits GOFLAGS (cmd/internal/quoted):
// at spaces, tabs, carriage returns and newlines, a field that starts with
// a quote running to the next of that quote, quotes left out. ok is false
// for a quote left open.
func splitQuoted(s string) (fields []string, ok bool) {
	const space = " \t\r\n"
	for {
		s = strings.TrimLeft(s, space)
		if s == "" {
			return fields, true
		}
		if quote := s[0]; quote == '"' || quote == '\'' {
			end := strings.IndexByte(s[1:], quote)
			if end < 0 {
				return nil, false
			}
			fields = append(fields, s[1:1+end])
			s = s[2+end:]
			continue
		}
		end := strings.IndexAny(s, space)
		if end < 0 {
			end = len(s)
		}
		fields = append(fields, s[:end])
		s = s[end:]
	}
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
	goflags, err := goenvfile.Getenv("GOFLAGS")
	if err != nil {
		fmt.Fprintln(stderr, "ao-gotest: read GOFLAGS as the go command does:", err)
		return 1
	}
	// Declaration order is the order the packages are listed and their
	// buckets start in, so the longest goes first.
	var declared []split
	var names []string
	for _, s := range splits {
		if slices.Contains(inv.packages, s.pkg) {
			declared = append(declared, s)
			names = append(names, s.pkg)
		}
	}
	if !inv.shardable || !goflagsSplit(goflags) || len(declared) == 0 {
		return passThrough(inv.argv, stderr)
	}
	whole := slices.DeleteFunc(slices.Clone(inv.packages), func(pkg string) bool { return slices.Contains(names, pkg) })
	r, err := newRunner(inv.argv[0], stdout, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "ao-gotest:", err)
		return 1
	}
	defer r.close()
	stopSignals := r.forwardSignals()
	r.runSplit(inv, declared, whole, names)
	stopSignals()
	r.printNotes()
	return r.status()
}

// runSplit runs inv with declared split: the unsplit `go test` of whole at
// once, and the split packages under a reservation, in a directory of this
// run's own that it removes once every process of the run has exited.
func (r *runner) runSplit(inv invocation, declared []split, whole, names []string) {
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
	p := plan(inv, declared, whole, dir.Dir)
	waitWhole := r.startWhole(p.whole)
	defer waitWhole()
	// W is what the governor grants; the unsplit `go test` started above is
	// outside the reservation.
	units := reservationUnits(declared, runtime.GOMAXPROCS(0))
	w, release, err := r.admit(dir.Dir, units)
	if err != nil {
		if !errors.Is(err, errStopped) {
			r.fail("%v", err)
		}
		return
	}
	r.out.Lock()
	fmt.Fprintf(r.stderr, "ao-gotest: %s: %d buckets each, %d at a time (%d MiB reserved)\n", strings.Join(names, ", "), buckets, w, sum(units[:w])>>20)
	r.out.Unlock()
	r.runPackages(p.split, w)
	if err := release(); err != nil {
		r.note("release the memory reservation: %v", err)
	}
}

// runDirParent is where a run makes its directory: the go command's
// GOTMPDIR, which the test namespace puts on disk (internal/netisolate),
// else the temp directory. A package's binary and its links take some
// 150 MB, too much for the namespace's RAM temp directory.
func runDirParent() (string, error) {
	return goenvfile.Getenv("GOTMPDIR")
}

// reservationUnits is what a run of declared reserves to run buckets at
// once on cores: the bucket units of the largest buckets, at most one per
// core and at most one package's bucket count, since the buckets start in
// declaration order and no more of the largest package's run at once. The
// governor grants a prefix of them, and the list steps run first, alone,
// under that lease, so the first unit is raised to the largest list step's.
func reservationUnits(declared []split, cores int) []uint64 {
	var units []uint64
	var list uint64
	for _, s := range declared {
		list = max(list, s.listUnit)
		for range buckets {
			units = append(units, s.bucketUnit)
		}
	}
	slices.SortFunc(units, func(a, b uint64) int { return cmp.Compare(b, a) })
	units = units[:min(max(cores, 1), buckets, len(units))]
	units[0] = max(units[0], list)
	return units
}

func sum(units []uint64) uint64 {
	var total uint64
	for _, unit := range units {
		total += unit
	}
	return total
}

// errStopped is admit's error when a signal ended the wait.
var errStopped = errors.New("stopped by a signal")

// admit reserves units in the host's governor, for the run whose binaries
// are in dir.
func (r *runner) admit(dir string, units []uint64) (int, func() error, error) {
	manager, err := governor.New(governor.Options{})
	if err != nil {
		return 0, nil, fmt.Errorf("reserve memory for the buckets: %w", err)
	}
	worktree, err := os.Getwd()
	if err != nil {
		return 0, nil, fmt.Errorf("reserve memory for the buckets: %w", err)
	}
	return r.reserve(manager, governor.Request{
		RunID:    "go-test-" + strconv.Itoa(os.Getpid()),
		Worktree: worktree,
		DataRoot: dir,
		OwnerPID: os.Getpid(),
	}, units)
}

// reserve asks m for the longest prefix of units, W of them, and returns W
// and the lease's release. While m grants none it says once why and asks
// again every admitPoll; after admitWait it fails with the reason.
func (r *runner) reserve(m *governor.Manager, req governor.Request, units []uint64) (int, func() error, error) {
	deadline := time.Now().Add(r.admitWait)
	told := false
	for {
		lease, granted, err := m.ReserveUpTo(req, units)
		if err != nil {
			return 0, nil, fmt.Errorf("reserve memory for the buckets: %w", err)
		}
		if granted > 0 {
			return granted, func() error { return m.Release(lease) }, nil
		}
		why := refusal(m, units[0])
		if !time.Now().Before(deadline) {
			return 0, nil, fmt.Errorf("no memory for the buckets after %v: %s", r.admitWait, why)
		}
		if !told {
			told = true
			r.out.Lock()
			fmt.Fprintf(r.stderr, "ao-gotest: waiting for memory for the buckets: %s\n", why)
			r.out.Unlock()
		}
		select {
		case <-r.stopping:
			return 0, nil, errStopped
		case <-time.After(r.admitPoll):
		}
	}
}

// refusal says why m grants no unit of size need: what is free, and who
// holds the rest.
func refusal(m *governor.Manager, need uint64) string {
	snap, err := m.Snapshot()
	if err != nil {
		return fmt.Sprintf("one bucket needs %d MiB, which is not free, and the reservations could not be read: %v", need>>20, err)
	}
	var free uint64
	if kept := snap.AvailableFloorBytes + snap.ReservedBytes; snap.AvailableBytes > kept {
		free = snap.AvailableBytes - kept
	}
	holders := "none"
	if len(snap.Leases) > 0 {
		var held []string
		for _, lease := range snap.Leases {
			held = append(held, fmt.Sprintf("%s in %s holds %d MiB", lease.RunID, lease.Worktree, lease.CeilingBytes>>20))
		}
		holders = strings.Join(held, "; ")
	}
	return fmt.Sprintf("one bucket needs %d MiB and %d MiB is free: %d MiB available less the %d MiB floor the host keeps and the %d MiB other runs reserved (%s)",
		need>>20, free>>20, snap.AvailableBytes>>20, snap.AvailableFloorBytes>>20, snap.ReservedBytes>>20, holders)
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

// runner starts the jobs and serializes their output.
type runner struct {
	// goCommand is the go command the jobs run, the one the command line
	// names, and self this executable, each job's leader.
	goCommand, self string
	// lifeline is the read end of a pipe every leader reads until it ends,
	// and lifelineEnd its write end, which only this process holds: a
	// leader reads EOF once this process has exited, however it exited.
	lifeline, lifelineEnd *os.File
	stdout, stderr        io.Writer
	// out guards stdout and stderr: one line of the unsplit run, or one
	// whole process's output, at a time. It is never held while taking mu.
	out sync.Mutex
	// admitWait and admitPoll are the constants of the same names but in
	// tests.
	admitWait, admitPoll time.Duration

	mu      sync.Mutex
	running map[*exec.Cmd]bool
	stopped bool
	// stopping is closed when a signal arrives.
	stopping chan struct{}
	worst    int
	// notes are printed when the run ends.
	notes []string
	// peaks is each split package's largest bucket process, and listPeaks
	// its list step's.
	peaks, listPeaks map[*splitPackage]uint64
}

func newRunner(goCommand string, stdout, stderr io.Writer) (*runner, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("find this executable, the leader of each go test: %w", err)
	}
	lifeline, lifelineEnd, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("make the leaders' lifeline: %w", err)
	}
	return &runner{
		goCommand: goCommand, self: self, lifeline: lifeline, lifelineEnd: lifelineEnd,
		stdout: stdout, stderr: stderr, admitWait: admitWait, admitPoll: admitPoll,
		running: map[*exec.Cmd]bool{}, stopping: make(chan struct{}),
		peaks: map[*splitPackage]uint64{}, listPeaks: map[*splitPackage]uint64{},
	}, nil
}

// close closes the lifeline once every leader has exited.
func (r *runner) close() {
	_ = r.lifelineEnd.Close()
	_ = r.lifeline.Close()
}

// forwardSignals passes the signals this process receives to every leader
// until stop, which returns once no signal is being passed on.
func (r *runner) forwardSignals() (stop func()) {
	signals := make(chan os.Signal, len(forwarded))
	signal.Notify(signals, forwarded...)
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			select {
			case sig := <-signals:
				r.signal(sig)
			case <-done:
				return
			}
		}
	})
	return func() {
		signal.Stop(signals)
		close(done)
		wg.Wait()
	}
}

// run runs p's unsplit `go test` and its split packages, w buckets at a
// time.
func (r *runner) run(p runPlan, w int) int {
	waitWhole := r.startWhole(p.whole)
	r.runPackages(p.split, w)
	waitWhole()
	return r.status()
}

// startWhole starts the unsplit `go test`, if any, and returns the wait
// for it.
func (r *runner) startWhole(j *job) (wait func()) {
	if j == nil {
		return func() {}
	}
	var wg sync.WaitGroup
	wg.Go(func() { r.runWhole(*j) })
	return wg.Wait
}

// runPackages runs each package's list step, one after another, then their
// buckets, w at a time, in the packages' order.
func (r *runner) runPackages(packages []*splitPackage, w int) {
	var all []bucket
	for _, sp := range packages {
		bs, started := r.list(sp)
		if !started {
			return
		}
		all = append(all, bs...)
	}
	queue := make(chan bucket)
	var wg sync.WaitGroup
	for range w {
		wg.Go(func() {
			for b := range queue {
				r.runBucket(b)
			}
		})
	}
	for _, b := range all {
		queue <- b
	}
	close(queue)
	wg.Wait()
	for _, sp := range packages {
		if peak := r.listPeaks[sp]; peak > sp.listUnit {
			r.note("%s: its list step's largest process reached %d MiB, over the %d MiB its -split declares; raise it in GO_TEST_SPLIT in the Makefile", sp.pkg, peak>>20, sp.listUnit>>20)
		}
		if peak := r.peaks[sp]; peak > sp.bucketUnit {
			r.note("%s: a bucket's largest process reached %d MiB, over the %d MiB its -split declares; raise it in GO_TEST_SPLIT in the Makefile", sp.pkg, peak>>20, sp.bucketUnit>>20)
		}
	}
}

// output is the pair of files a job of a split package writes to, in the
// run's directory.
type output struct{ stdout, stderr *os.File }

func createOutput(base string) (output, error) {
	stdout, err := os.Create(base + ".out")
	if err != nil {
		return output{}, err
	}
	stderr, err := os.Create(base + ".err")
	if err != nil {
		return output{}, errors.Join(err, stdout.Close())
	}
	return output{stdout: stdout, stderr: stderr}, nil
}

// printTo copies what the job wrote to stdout and stderr, from the start.
func (o output) printTo(stdout, stderr io.Writer) error {
	return errors.Join(copyFile(stdout, o.stdout), copyFile(stderr, o.stderr))
}

// copyFile copies f, from its start, to w.
func copyFile(w io.Writer, f *os.File) error {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	_, err := io.Copy(w, f)
	return err
}

func (o output) close() error { return errors.Join(o.stdout.Close(), o.stderr.Close()) }

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

func (r *runner) runBucket(b bucket) {
	out, err := createOutput(b.link)
	if err != nil {
		r.fail("%s: %v", b.label, err)
		return
	}
	defer func() {
		if err := out.close(); err != nil {
			r.fail("%s: %v", b.label, err)
		}
	}()
	var peak uint64
	status, started := r.start(b.job, out.stdout, out.stderr, &peak)
	if !started {
		return
	}
	var why string
	stale, err := replacedLink(b.pkg.binary, b.link)
	switch {
	case err != nil:
		why = fmt.Sprintf("ao-gotest: %s: %v\n", b.label, err)
	case stale:
		why = fmt.Sprintf("ao-gotest: %s built its own test binary: %s or a package it imports changed after this run listed its tests, so this bucket tested other code than the rest; run again to test one version\n", b.label, b.pkg.pkg)
	case status == 0:
		// A bucket that built its own binary, or failed to, peaked in the
		// build; one that passed on the shared binary measures its tests.
		r.mu.Lock()
		r.peaks[b.pkg] = max(r.peaks[b.pkg], peak)
		r.mu.Unlock()
	}
	if why != "" {
		status = max(status, 1)
	}
	r.record(status)
	r.out.Lock()
	_, headErr := io.WriteString(r.stdout, "ao-gotest: "+b.label+"\n")
	printErr := out.printTo(r.stdout, r.stderr)
	_, whyErr := io.WriteString(r.stderr, why)
	r.out.Unlock()
	if err := errors.Join(headErr, printErr, whyErr); err != nil {
		r.fail("print %s: %v", b.label, err)
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

// start runs j under a leader of its own to completion and returns its
// status, and when peak is not nil sets it to the largest process's
// resident set. It starts nothing once a signal has arrived.
func (r *runner) start(j job, stdout, stderr io.Writer, peak *uint64) (status int, started bool) {
	command := exec.Command(r.self, append([]string{leaderArg, r.goCommand}, j.args...)...)
	command.Stdin = r.lifeline
	command.Stdout, command.Stderr = stdout, stderr
	// A group of its own, so a signal from the terminal reaches it only
	// through this process, once.
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
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
		r.fail("start %s: %v", strings.Join(command.Args, " "), err)
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

// status is the worst status recorded.
func (r *runner) status() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.worst
}

// fail records a failure and prints why at once.
func (r *runner) fail(format string, args ...any) {
	r.record(1)
	r.out.Lock()
	fmt.Fprintf(r.stderr, "ao-gotest: "+format+"\n", args...)
	r.out.Unlock()
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
	if !r.stopped {
		close(r.stopping)
	}
	r.stopped = true
	r.worst = max(r.worst, 1)
	for command := range r.running {
		if err := command.Process.Signal(sig); err != nil && !errors.Is(err, os.ErrProcessDone) {
			failed = append(failed, fmt.Sprintf("ao-gotest: forward %v to the leader of %s: %v\n", sig, strings.Join(command.Args[2:], " "), err))
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
