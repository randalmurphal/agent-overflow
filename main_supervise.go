package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"agent-overflow/internal/supervise"
)

// The `supervise` boot: the stable process a service manager selects
// (docs/specs/remote-access.md §7, "Headless serve mode and remote
// update"). Operator-facing walkthrough: docs/architecture/serve-mode.md.
//
// It runs no backend of its own. It reads the durable launch state, spawns
// the version that state selects as a `serve` child with an IPC pipe pair
// inherited, and owns every transition an update makes: the pending record,
// the quiescent database snapshot, the trial that must report prepared, the
// commit, and the rollback. Every one of those lives in internal/supervise;
// what is here is the executable-only half — argv, the process's own
// identity, signals, and the exit code the service manager reads.
//
// A supervisor is OPTIONAL forever. `agent-overflow serve` started by hand
// runs exactly as it did before this mode existed, and answers nothing on a
// channel it was never handed.

// runSupervise selects a version and runs it until it exits or we are
// signalled.
//
// The exit code is the CHILD's, deliberately. `Restart=on-failure` was chosen
// for the serve unit because a clean exit is the operator stopping the backend
// and a supervisor that restarts one of those cannot be stopped
// (internal/serviceinstall/AGENTS.md). Inserting a process between systemd and
// the backend must not change what that rule means, so this one mirrors what
// it supervised: a clean child exits us cleanly, a crashed child exits us
// non-zero, and the unit's policy applies to the pair exactly as it applied to
// the one.
// bootArgs are the flags after the verb, already parsed and validated by
// main(): --data-dir has been applied to dataDirRoot, and every flag naming a
// different mode was refused there.
func runSupervise(bootArgs []string) {
	if runtime.GOOS == "windows" {
		// The same answer internal/serviceinstall gives, for the same reason:
		// on Windows this app is a launcher that already supervises its
		// backend inside WSL, and a second supervisor outside it would own a
		// launch state neither half reads.
		fatalf("supervise: on Windows, Agent Overflow is a launcher that already supervises its " +
			"backend inside WSL. Run `agent-overflow supervise` inside the WSL distribution, " +
			"using the Linux binary there.")
	}
	dataDir := bootSettingsDir()
	if dataDir == "" {
		fatalf("supervise: cannot determine the data directory. Name one with --data-dir.")
	}
	lock, err := acquireBackendInstanceLock(dataDir)
	if err != nil {
		fatalf("supervise: %v", err)
	}
	heldBackendLock = lock
	executable, err := os.Executable()
	if err != nil {
		fatalf("supervise: cannot find this binary's own path: %v", err)
	}

	supervisor, err := supervise.New(supervise.Config{
		OwnsDataRoot:   true,
		DataDir:        dataDir,
		SelfExecutable: executable,
		SelfVersion:    version,
		// The child is this same argv with the verb swapped: `serve` plus
		// every flag the operator passed through. One list, so a unit that
		// pins a bind pins the backend's bind and not the supervisor's idea
		// of one.
		ChildArgs: append([]string{serveVerb}, bootArgs...),
		Log:       log.Printf,
	})
	if err != nil {
		fatalf("supervise: %v", err)
	}

	// SIGTERM and SIGINT stop the child and then us. The child gets the same
	// graceful shutdown it would get if the signal had been delivered to it
	// directly — the supervisor forwards a SIGTERM and waits — so `systemctl
	// stop` still flushes SQLite and closes provider sessions.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := supervisor.Run(ctx); err != nil {
		// Every error out of Run is either the child's exit status or a state
		// this supervisor refuses to guess about. Both belong in the journal
		// verbatim, and both exit non-zero so the service manager surfaces
		// them rather than treating the stop as deliberate.
		fatalf("supervise: %v", err)
	}
}
