package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

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
	if status, ok := command.ProcessState.Sys().(syscall.WaitStatus); ok && waitedStatus(status) > 0 {
		return waitedStatus(status)
	}
	return 1
}

// waitedStatus is the status a shell gives a process that ended as status
// says: its exit code, or 128 and the signal that ended it.
func waitedStatus(status syscall.WaitStatus) int {
	if status.Signaled() {
		return 128 + int(status.Signal())
	}
	return status.ExitStatus()
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
