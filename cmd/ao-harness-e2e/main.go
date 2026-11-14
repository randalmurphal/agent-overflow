// Command ao-harness-e2e runs the Playwright E2E suite inside one owned
// process and memory boundary. It deliberately accepts Playwright arguments
// only. The command itself is fixed so `pnpm test` cannot accidentally run a
// shell command outside containment.
//
// On Linux the suite also runs in its own network namespace: loopback plus
// one private LAN interface and no route off the machine, so LAN-bind specs
// exercise a real non-loopback listener without reaching a real network.
// Started inside such a namespace already (ao-netns, as the Android smoke
// does so the emulator and adb share it), it runs there rather than in a
// new one. --host-network opts out for suites that need host services.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"agent-overflow/internal/harness/containment"
	"agent-overflow/internal/harness/governor"
	"agent-overflow/internal/harness/instanceinfo"
	"agent-overflow/internal/netisolate"
)

const (
	// Four Playwright workers, their browsers, four app backends, and the
	// Go/Node runners stay under 5 GiB. Six GiB leaves room for the complete
	// gate while the host-global reservation and available-memory floor still
	// refuse unsafe parallel launches.
	defaultMemoryLimit = 6 << 30
	monitorInterval    = 100 * time.Millisecond
)

type runMode uint8

const (
	modeTests runMode = iota
	modeFlow
	modeFreeze
)

type launchOptions struct {
	limit       uint64
	mode        runMode
	hostNetwork bool
	args        []string
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == netisolate.HelperArg {
		os.Exit(netisolate.RunHelper(os.Args[2:], os.Stderr))
	}
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr *os.File) int {
	opts, err := parseArgs(args)
	if err != nil {
		fmt.Fprintln(stderr, "ao-harness-e2e:", err)
		return 2
	}
	worktree, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(stderr, "ao-harness-e2e: resolve working directory:", err)
		return 1
	}
	// The suite dir is identified by its Playwright config, not by
	// package.json: the repo root carries a forwarder package.json too, so
	// a package.json probe run from the root resolved the ROOT as the
	// suite dir and tsc failed on the missing root tsconfig.
	testDir := worktree
	if _, err := os.Stat(filepath.Join(testDir, "playwright.config.ts")); err != nil {
		candidate := filepath.Join(worktree, "e2e")
		if _, candidateErr := os.Stat(filepath.Join(candidate, "playwright.config.ts")); candidateErr != nil {
			fmt.Fprintf(stderr, "ao-harness-e2e: no playwright.config.ts in %s or %s\n", worktree, candidate)
			return 1
		}
		testDir = candidate
	}
	worktree, err = filepath.Abs(worktree)
	if err != nil {
		fmt.Fprintln(stderr, "ao-harness-e2e: resolve working directory:", err)
		return 1
	}

	// Playwright (and the flow runner's node --experimental-transform-types)
	// STRIP types per file, never check them: a typo'd property inside a spec
	// helper's filter or predicate does not throw at runtime — it yields
	// undefined and lets an emptiness assertion pass vacuously. Typecheck the
	// whole suite first, before the memory reservation, so a type error fails
	// in seconds without holding the gate's reservation. Fixed argv, same
	// doctrine as the contained command; tsc's footprint does not need the
	// suite's containment any more than the `go run` hosting this launcher.
	typecheck := exec.Command("pnpm", "exec", "tsc", "--noEmit", "-p", ".")
	typecheck.Dir = testDir
	typecheck.Stdout = stdout
	typecheck.Stderr = stderr
	if err := typecheck.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			fmt.Fprintln(stderr, "ao-harness-e2e: E2E TypeScript check failed; fix the errors above")
			return exitErr.ExitCode()
		}
		fmt.Fprintln(stderr, "ao-harness-e2e: run E2E TypeScript check:", err)
		return 1
	}
	isolate := needsIsolation(opts.hostNetwork, netisolate.Inside())
	if isolate {
		if err := netisolate.Check(stderr); err != nil {
			fmt.Fprintln(stderr, "ao-harness-e2e:", err, "(--host-network is only for suites that need host services)")
			return 1
		}
	}

	group, enforcement, err := containment.PrepareWithFallback(opts.limit)
	if err != nil {
		fmt.Fprintln(stderr, "ao-harness-e2e: install memory containment:", err)
		return 1
	}
	defer func() {
		if closeErr := group.Close(); closeErr != nil {
			fmt.Fprintln(stderr, "ao-harness-e2e: close memory containment:", closeErr)
		}
	}()

	manager, err := governor.New(governor.Options{})
	if err != nil {
		fmt.Fprintln(stderr, "ao-harness-e2e: initialize memory governor:", err)
		return 1
	}
	runID := fmt.Sprintf("e2e-%d-%d", os.Getpid(), time.Now().UnixNano())
	lease, err := manager.Reserve(governor.Request{
		RunID:        runID,
		Worktree:     worktree,
		DataRoot:     worktree,
		OwnerPID:     os.Getpid(),
		CeilingBytes: opts.limit,
	})
	if err != nil {
		fmt.Fprintln(stderr, "ao-harness-e2e: reserve host memory:", err)
		return 1
	}
	defer func() {
		if releaseErr := manager.Release(lease); releaseErr != nil {
			fmt.Fprintln(stderr, "ao-harness-e2e: release host memory:", releaseErr)
		}
	}()
	if enforcement != "cgroup-v2" && enforcement != "kernel" {
		fmt.Fprintf(stderr, "ao-harness-e2e: using %s; host-floor watchdog is active\n", enforcement)
	}

	command := containedCommand(opts.mode, opts.args)
	command.Dir = testDir
	command.Env = containedEnvironment()
	command.Stdout = stdout
	command.Stderr = stderr
	configureProcessGroup(command)
	if isolate {
		if err := netisolate.Command(command); err != nil {
			fmt.Fprintln(stderr, "ao-harness-e2e: isolate network:", err)
			return 1
		}
	}
	// Playwright runs in its own process group, so a signal meant for the
	// suite reaches only this process. Registered before the start so none
	// can end the launcher between them and orphan the tree.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, interruptSignals...)
	defer signal.Stop(signals)
	identity, ok := startContained(command, group, stderr)
	if !ok {
		return 1
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stopOnce sync.Once
	stoppedForSafety := false
	stop := func(reason string, event governor.Event) {
		stopOnce.Do(func() {
			stoppedForSafety = true
			fmt.Fprintf(stderr, "ao-harness-e2e: stopping contained suite: %s rss=%d available=%d\n", reason, event.RSSBytes, event.AvailableBytes)
			if err := terminateProcessTreeVerified(command, group, identity); err != nil {
				fmt.Fprintln(stderr, "ao-harness-e2e: terminate contained suite:", err)
			}
		})
	}
	interrupted := haltOnSignal(ctx, signals, func(sig os.Signal) {
		stopOnce.Do(func() {
			// stderr is the only place to report; a failed write has nowhere else to go.
			_, _ = fmt.Fprintf(stderr, "ao-harness-e2e: stopping contained suite: received %v\n", sig)
			if err := terminateProcessTreeVerified(command, group, identity); err != nil {
				_, _ = fmt.Fprintln(stderr, "ao-harness-e2e: terminate contained suite:", err)
			}
		})
	})
	monitorDone := make(chan error, 1)
	go func() {
		err := manager.Monitor(ctx, lease, monitorInterval, nil, func(event governor.Event) {
			if event.Reason == governor.ReasonSafetyCeiling || event.Reason == governor.ReasonAvailableFloor {
				stop(event.Reason, event)
			}
		})
		if err != nil {
			stop("memory monitor error: "+err.Error(), governor.Event{Error: err.Error()})
		}
		monitorDone <- err
	}()

	runErr := command.Wait()
	cancel()
	monitorErr := <-monitorDone
	// Joins a teardown a signal started; nil when none arrived.
	sig := <-interrupted
	if !waitForProcessTree(command, group, 2*time.Second) {
		fmt.Fprintln(stderr, "ao-harness-e2e: Playwright left descendants running; terminating the owned process tree")
		if err := terminateProcessTreeVerified(command, group, identity); err != nil {
			fmt.Fprintln(stderr, "ao-harness-e2e: terminate owned process tree:", err)
		}
		if !waitForProcessTree(command, group, 5*time.Second) {
			if runErr == nil {
				runErr = fmt.Errorf("Playwright descendants survived teardown")
			}
		}
	}
	if monitorErr != nil {
		stop("memory monitor error: "+monitorErr.Error(), governor.Event{Error: monitorErr.Error()})
		if runErr == nil {
			runErr = monitorErr
		}
	}
	if stoppedForSafety && runErr == nil {
		runErr = errors.New("contained E2E suite stopped by the memory safety monitor")
	}
	if sig != nil {
		return signalExitCode(sig)
	}
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			return exitErr.ExitCode()
		}
		fmt.Fprintln(stderr, "ao-harness-e2e: Playwright:", runErr)
		return 1
	}
	return 0
}

// haltOnSignal calls halt with the first signal to arrive before ctx ends,
// then delivers that signal on the returned channel. The channel closes
// once halt has returned or ctx has ended, so receiving from it joins the
// teardown halt started.
func haltOnSignal(ctx context.Context, signals <-chan os.Signal, halt func(os.Signal)) <-chan os.Signal {
	got := make(chan os.Signal, 1)
	go func() {
		defer close(got)
		select {
		case sig := <-signals:
			halt(sig)
			got <- sig
		case <-ctx.Done():
		}
	}()
	return got
}

// signalExitCode is the status a shell reports for a process a signal ended.
func signalExitCode(sig os.Signal) int {
	if number, ok := sig.(syscall.Signal); ok {
		return 128 + int(number)
	}
	return 1
}

// startContained starts command inside group and records its identity after
// Adopt, once the command runs inside the boundary rather than a containment
// launcher. A failure is reported to stderr after the started tree is torn
// down.
func startContained(command *exec.Cmd, group containment.Group, stderr io.Writer) (instanceinfo.ProcessIdentity, bool) {
	if err := group.Configure(command); err != nil {
		fmt.Fprintln(stderr, "ao-harness-e2e: configure memory containment:", err)
		return instanceinfo.ProcessIdentity{}, false
	}
	if err := command.Start(); err != nil {
		fmt.Fprintln(stderr, "ao-harness-e2e: start Playwright:", err)
		return instanceinfo.ProcessIdentity{}, false
	}
	if err := group.Adopt(command); err != nil {
		if killErr := terminateProcessTree(command, group); killErr != nil {
			fmt.Fprintln(stderr, "ao-harness-e2e: clean up after containment adoption failure:", killErr)
		}
		_ = command.Wait()
		fmt.Fprintln(stderr, "ao-harness-e2e: adopt Playwright into memory containment:", err)
		return instanceinfo.ProcessIdentity{}, false
	}
	identity, err := instanceinfo.CaptureProcessIdentity(command.Process.Pid)
	if err != nil {
		if killErr := terminateProcessTree(command, group); killErr != nil {
			fmt.Fprintln(stderr, "ao-harness-e2e: clean up after identity failure:", killErr)
		}
		_ = command.Wait()
		if !waitForProcessTree(command, group, 5*time.Second) {
			fmt.Fprintln(stderr, "ao-harness-e2e: descendants survived identity failure cleanup")
		}
		fmt.Fprintln(stderr, "ao-harness-e2e: capture Playwright identity:", err)
		return instanceinfo.ProcessIdentity{}, false
	}
	return identity, true
}

func containedEnvironment() []string {
	const marker = "AO_E2E_CONTAINED="
	env := os.Environ()
	filtered := env[:0]
	for _, value := range env {
		if !strings.HasPrefix(value, marker) {
			filtered = append(filtered, value)
		}
	}
	filtered = append(filtered, marker+"1")
	return append(filtered, "AO_E2E_FUNCTIONAL_MANAGED=1")
}

func containedCommand(mode runMode, args []string) *exec.Cmd {
	if mode == modeFlow {
		return exec.Command("node", append([]string{"--experimental-transform-types", "scripts/run-functional-flow.ts"}, args...)...)
	}
	if mode == modeFreeze {
		fixed := []string{"exec", "playwright", "test", "--config=playwright.manual.config.ts", "freeze-repro.manual.spec.ts"}
		return exec.Command("pnpm", append(fixed, args...)...)
	}
	return exec.Command("pnpm", append([]string{"exec", "playwright", "test"}, args...)...)
}

func parseArgs(args []string) (launchOptions, error) {
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	opts := launchOptions{limit: defaultMemoryLimit, mode: modeTests, args: make([]string, 0, len(args))}
	modeSet := false
	setMode := func(arg string, mode runMode) error {
		if modeSet {
			return fmt.Errorf("conflicting or repeated launcher mode %q", arg)
		}
		opts.mode = mode
		modeSet = true
		return nil
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		var err error
		switch {
		case arg == "--test":
			err = setMode(arg, modeTests)
		case arg == "--flow":
			err = setMode(arg, modeFlow)
		case arg == "--freeze-repro":
			err = setMode(arg, modeFreeze)
		case arg == "--host-network":
			opts.hostNetwork = true
		case arg == "--memory-limit-bytes":
			if i+1 >= len(args) {
				return launchOptions{}, fmt.Errorf("--memory-limit-bytes needs a value")
			}
			i++
			opts.limit, err = strconv.ParseUint(args[i], 10, 64)
			if err != nil {
				err = fmt.Errorf("invalid --memory-limit-bytes %q: %w", args[i], err)
			}
		case strings.HasPrefix(arg, "--memory-limit-bytes="):
			opts.limit, err = strconv.ParseUint(strings.TrimPrefix(arg, "--memory-limit-bytes="), 10, 64)
			if err != nil {
				err = fmt.Errorf("invalid --memory-limit-bytes %q: %w", arg, err)
			}
		case arg == "--":
			// Accept the conventional launcher separator after the launcher's
			// own option, while keeping it out of Playwright's argv.
			continue
		default:
			opts.args = append(opts.args, arg)
		}
		if err != nil {
			return launchOptions{}, err
		}
	}
	if opts.limit == 0 {
		return launchOptions{}, fmt.Errorf("--memory-limit-bytes must be positive")
	}
	return opts, nil
}

// needsIsolation is whether the suite needs a namespace of its own: not when
// it was asked to use the host's, and not when it already runs in one, where
// a second would cut it off from the processes it shares that one with.
func needsIsolation(hostNetwork, inside bool) bool {
	return !hostNetwork && !inside
}
