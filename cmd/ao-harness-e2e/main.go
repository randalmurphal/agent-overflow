// Command ao-harness-e2e runs the Playwright E2E suite inside one owned
// process and memory boundary. It deliberately accepts Playwright arguments
// only. The command itself is fixed so `pnpm test` cannot accidentally run a
// shell command outside containment.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/harness/containment"
	"agent-overflow/internal/harness/governor"
	"agent-overflow/internal/harness/instanceinfo"
)

const (
	// Two Playwright workers, their browsers, two app backends, and the Go/Node
	// runners normally exceed 2 GiB. Six GiB leaves room for the complete gate
	// while the host-global reservation and available-memory floor still refuse
	// unsafe parallel launches.
	defaultMemoryLimit = 6 << 30
	monitorInterval    = 100 * time.Millisecond
)

type runMode uint8

const (
	modeTests runMode = iota
	modeFlow
	modeFreeze
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr *os.File) int {
	limit, mode, commandArgs, err := parseArgs(args)
	if err != nil {
		fmt.Fprintln(stderr, "ao-harness-e2e:", err)
		return 2
	}
	worktree, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(stderr, "ao-harness-e2e: resolve working directory:", err)
		return 1
	}
	testDir := worktree
	if _, err := os.Stat(filepath.Join(testDir, "package.json")); err != nil {
		candidate := filepath.Join(worktree, "e2e")
		if _, candidateErr := os.Stat(filepath.Join(candidate, "package.json")); candidateErr != nil {
			fmt.Fprintf(stderr, "ao-harness-e2e: no E2E package.json in %s or %s\n", worktree, candidate)
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

	group, enforcement, err := containment.PrepareWithFallback(limit)
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
		CeilingBytes: limit,
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

	command := containedCommand(mode, commandArgs)
	command.Dir = testDir
	command.Env = containedEnvironment()
	command.Stdout = stdout
	command.Stderr = stderr
	configureProcessGroup(command)
	if err := group.Configure(command); err != nil {
		fmt.Fprintln(stderr, "ao-harness-e2e: configure memory containment:", err)
		return 1
	}
	if err := command.Start(); err != nil {
		fmt.Fprintln(stderr, "ao-harness-e2e: start Playwright:", err)
		return 1
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
		return 1
	}
	if err := group.Adopt(command); err != nil {
		if killErr := terminateProcessTree(command, group); killErr != nil {
			fmt.Fprintln(stderr, "ao-harness-e2e: clean up after containment adoption failure:", killErr)
		}
		_ = command.Wait()
		fmt.Fprintln(stderr, "ao-harness-e2e: adopt Playwright into memory containment:", err)
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

func parseArgs(args []string) (uint64, runMode, []string, error) {
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	limit := uint64(defaultMemoryLimit)
	mode := modeTests
	modeSet := false
	commandArgs := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--test":
			if modeSet {
				return 0, 0, nil, fmt.Errorf("conflicting or repeated launcher mode %q", arg)
			}
			mode = modeTests
			modeSet = true
		case arg == "--flow":
			if modeSet {
				return 0, 0, nil, fmt.Errorf("conflicting or repeated launcher mode %q", arg)
			}
			mode = modeFlow
			modeSet = true
		case arg == "--freeze-repro":
			if modeSet {
				return 0, 0, nil, fmt.Errorf("conflicting or repeated launcher mode %q", arg)
			}
			mode = modeFreeze
			modeSet = true
		case arg == "--memory-limit-bytes":
			if i+1 >= len(args) {
				return 0, 0, nil, fmt.Errorf("--memory-limit-bytes needs a value")
			}
			i++
			parsed, err := strconv.ParseUint(args[i], 10, 64)
			if err != nil {
				return 0, 0, nil, fmt.Errorf("invalid --memory-limit-bytes %q: %w", args[i], err)
			}
			limit = parsed
		case strings.HasPrefix(arg, "--memory-limit-bytes="):
			parsed, err := strconv.ParseUint(strings.TrimPrefix(arg, "--memory-limit-bytes="), 10, 64)
			if err != nil {
				return 0, 0, nil, fmt.Errorf("invalid --memory-limit-bytes %q: %w", arg, err)
			}
			limit = parsed
		case arg == "--":
			// Accept the conventional launcher separator after the launcher's
			// own option, while keeping it out of Playwright's argv.
			continue
		default:
			commandArgs = append(commandArgs, arg)
		}
	}
	if limit == 0 {
		return 0, 0, nil, fmt.Errorf("--memory-limit-bytes must be positive")
	}
	return limit, mode, commandArgs, nil
}
