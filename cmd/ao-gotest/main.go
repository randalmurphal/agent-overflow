// Command ao-gotest runs the `go test` command line it is given, for `make
// go-test`, inside the test namespace (bin/ao-netns). Each package whose
// TestMain calls testshard.Select (internal/testutil/testshard) runs as one
// `go test` per core, each running the tests whose names hash to it with
// -parallel 1, and the other packages run in one `go test` beside them.
//
//	ao-gotest <go command> test [go test flag or package]...
//
// K, the number of processes a package is split into, is GOMAXPROCS: the
// cores this process may use, as Go counts them for -p and -parallel. So
// the run keeps the concurrency one `go test` has, one test per core, but
// no two of a package's tests share a process.
//
// Nothing is split, and this process becomes exactly the command line it
// was given, when K is 1, when no package calls Select, or when a flag is
// outside shardFlags: one that narrows the run (-run, -list, -bench,
// -fuzz), sets its concurrency (-parallel, -cpu), needs one binary or one
// process per package (-c, -o, -exec, profiles, coverage, -race) or that
// this command does not know.
//
// A split package's tests are compiled and vetted once (`go test -c -o
// /dev/null`) before its processes start, so they do not compile it K times
// over; a package that fails that build runs unsplit instead, which reports
// the failure once. The unsplit `go test` streams its output line by line;
// each process of a split package prints its output whole when it ends,
// after a header line unless -json is given, so lines of different
// processes never interleave. The exit status is the worst of the `go
// test` processes'. Each has the -timeout the flags give. A signal this
// command receives goes to every `go test` it started, no new one starts,
// and it exits once they have.
package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"

	"agent-overflow/internal/testutil/testshard/shardspec"
)

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

// buildFlags are the shardFlags that change what `go test -c` builds.
var buildFlags = []string{"tags", "vet"}

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
		return invocation{}, fmt.Errorf("usage: ao-gotest <go command> test [go test flag or package]...; got %q", argv)
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
	inv, err := parseArgs(argv)
	if err != nil {
		fmt.Fprintln(stderr, "ao-gotest:", err)
		return 2
	}
	k := runtime.GOMAXPROCS(0)
	if k == 1 || !inv.shardable {
		return passThrough(inv.argv, stderr)
	}
	split, whole, err := optedIn(inv, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "ao-gotest:", err)
		return 1
	}
	if len(split) == 0 {
		return passThrough(inv.argv, stderr)
	}
	r := newRunner(inv.argv[0], stdout, stderr)
	return r.run(plan(inv, split, whole, k))
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

// optedIn splits the packages, in order, into those whose test files
// import testshard, and so call Select, and the rest.
func optedIn(inv invocation, stderr io.Writer) (split, whole []string, err error) {
	args := []string{"list", "-f", "{{.ImportPath}}{{range .TestImports}} {{.}}{{end}}{{range .XTestImports}} {{.}}{{end}}"}
	for _, f := range inv.flags {
		if f.name == "tags" {
			args = append(args, f.args...)
		}
	}
	var out bytes.Buffer
	list := exec.Command(inv.argv[0], append(args, inv.packages...)...)
	list.Stdout, list.Stderr = &out, stderr
	if err := list.Run(); err != nil {
		return nil, nil, fmt.Errorf("go list the packages' test imports: %w", err)
	}
	for line := range strings.Lines(out.String()) {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if slices.Contains(fields[1:], shardspec.SelectPackage) {
			split = append(split, fields[0])
		} else {
			whole = append(whole, fields[0])
		}
	}
	return split, whole, nil
}

// job is one `go test`: its arguments after the go command, and for a
// process of a split package its shard, "i/K".
type job struct {
	args  []string
	shard string
	label string
}

// splitPackage is a package split into K processes, and how it runs when
// its build fails.
type splitPackage struct {
	prebuild job
	shards   []job
	whole    job
}

type runPlan struct {
	// whole runs the packages that are not split, if any.
	whole *job
	split []splitPackage
	k     int
	json  bool
}

func plan(inv invocation, split, whole []string, k int) runPlan {
	p := runPlan{k: k}
	build := []string{"test", "-c", "-o", os.DevNull}
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
	for _, pkg := range split {
		sp := splitPackage{
			prebuild: job{args: append(slices.Clone(build), pkg), label: pkg},
			whole:    job{args: append(slices.Clone(test), pkg), label: pkg},
		}
		for i := range k {
			shard := shardspec.Format(i, k)
			sp.shards = append(sp.shards, job{
				args:  append(slices.Clone(test), "-parallel", "1", pkg),
				shard: shard,
				label: pkg + " shard " + shard,
			})
		}
		p.split = append(p.split, sp)
	}
	return p
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
}

func newRunner(goCommand string, stdout, stderr io.Writer) *runner {
	return &runner{goCommand: goCommand, stdout: stdout, stderr: stderr, running: map[*exec.Cmd]bool{}}
}

func (r *runner) run(p runPlan) int {
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
	queue := make(chan job)
	for range p.k {
		wg.Go(func() {
			for j := range queue {
				r.runShard(j, p.json)
			}
		})
	}
	for _, sp := range p.split {
		if status, started := r.start(sp.prebuild, io.Discard, io.Discard, false); !started {
			break
		} else if status != 0 {
			// One unsplit run reports the failure once, in the format the
			// flags ask for.
			r.runWhole(sp.whole)
			continue
		}
		for _, j := range sp.shards {
			queue <- j
		}
	}
	close(queue)
	wg.Wait()
	return r.worst
}

func (r *runner) runWhole(j job) {
	lines := &lineWriter{out: &r.out}
	errLines := &lineWriter{out: &r.out}
	lines.w, errLines.w = r.stdout, r.stderr
	r.start(j, lines, errLines, true)
	lines.flush()
	errLines.flush()
}

func (r *runner) runShard(j job, json bool) {
	var stdout, stderr bytes.Buffer
	if !json {
		stdout.WriteString("ao-gotest: " + j.label + "\n")
	}
	if _, started := r.start(j, &stdout, &stderr, true); !started {
		return
	}
	r.out.Lock()
	_, errOut := r.stdout.Write(stdout.Bytes())
	_, errErr := r.stderr.Write(stderr.Bytes())
	r.out.Unlock()
	if errOut != nil || errErr != nil {
		r.record(1)
	}
}

// start runs j to completion and, when counted, records its status. It
// starts nothing once a signal has arrived.
func (r *runner) start(j job, stdout, stderr io.Writer, counted bool) (status int, started bool) {
	command := exec.Command(r.goCommand, j.args...)
	command.Stdout, command.Stderr = stdout, stderr
	command.Env = environ(j.shard)
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
	status = exitStatus(command, err)
	if counted {
		r.record(status)
	}
	return status, true
}

func (r *runner) record(status int) {
	r.mu.Lock()
	r.worst = max(r.worst, status)
	r.mu.Unlock()
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

// environ is this process's environment for a job: shardspec.Env names
// the job's shard, and is unset for the unsplit run.
func environ(shard string) []string {
	env := slices.DeleteFunc(os.Environ(), func(kv string) bool { return strings.HasPrefix(kv, shardspec.Env+"=") })
	if shard != "" {
		env = append(env, shardspec.Env+"="+shard)
	}
	return env
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
