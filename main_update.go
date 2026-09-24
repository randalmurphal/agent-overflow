package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	appservice "agent-overflow/internal/app"
	"agent-overflow/internal/platform"
	"agent-overflow/internal/startupprogress"
	"agent-overflow/internal/store"
	"agent-overflow/internal/supervise"
	"agent-overflow/internal/transport"
)

// The in-app update's WSL-side commands and the trial they start
// (docs/specs/app-update.md). The Windows launcher runs each command through
// wsl.exe with an explicit argv; its reports are supervise.UpdateEventPrefix
// lines on stdout, the last one the result. The steps live in
// internal/supervise; this file owns argv, the lock, signals and the exit
// code.

func isUpdateCommand(name string) bool {
	switch name {
	case supervise.UpdateSpaceCommand, supervise.UpdateSnapshotCommand, supervise.UpdateTrialRunCommand,
		supervise.UpdateRestoreCommand, supervise.UpdateDiscardCommand,
		supervise.UpdateTrialCommand:
		return true
	}
	return false
}

type updateCommandFlags struct {
	id       string
	dataDir  string
	to       string
	attempt  int
	hostFree *uint64
	reason   string
}

func parseUpdateCommandFlags(name string, args []string) (updateCommandFlags, error) {
	var flags updateCommandFlags
	set := flag.NewFlagSet(name, flag.ContinueOnError)
	set.SetOutput(io.Discard)
	set.StringVar(&flags.id, "id", "", "the update id")
	set.StringVar(&flags.dataDir, "data-dir", "", "the data root, as for a boot")
	set.StringVar(&flags.to, "to", "", "the version the trial runs")
	set.IntVar(&flags.attempt, "attempt", 0, "the trial's attempt number")
	set.StringVar(&flags.reason, "reason", "", "why the snapshot is being restored")
	hostFree := set.Int64("host-free", -1, "free bytes on the host drive that holds the data root's disk")
	if err := set.Parse(args); err != nil {
		return flags, fmt.Errorf("%s: %w", name, err)
	}
	if set.NArg() > 0 {
		return flags, fmt.Errorf("%s: unexpected argument %q", name, set.Arg(0))
	}
	if *hostFree >= 0 {
		free := uint64(*hostFree)
		flags.hostFree = &free
	}
	if name == supervise.UpdateTrialCommand {
		return flags, nil
	}
	if strings.TrimSpace(flags.id) == "" {
		return flags, fmt.Errorf("%s: --id is required", name)
	}
	if name == supervise.UpdateTrialRunCommand {
		if err := supervise.ValidVersion(flags.to); err != nil {
			return flags, fmt.Errorf("%s: --to: %w", name, err)
		}
		if flags.attempt < 1 {
			return flags, fmt.Errorf("%s: --attempt must be 1 or more", name)
		}
	}
	return flags, nil
}

// runUpdateCommand runs one command and returns the process exit code: 0 for
// a result of ok or prepared, 1 for any other result, 2 for bad argv.
func runUpdateCommand(name string, args []string) int {
	flags, err := parseUpdateCommandFlags(name, args)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	dataDirRoot = flags.dataDir
	relocateOffWindowsDriveMount()
	if name == supervise.UpdateTrialCommand {
		return runUpdateTrial()
	}

	// A write to a closed stdout must return EPIPE rather than kill this
	// process with its trial still running: the failed write is how a
	// command learns that the launcher is gone.
	signal.Notify(make(chan os.Signal, 1), syscall.SIGPIPE)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	out := os.Stdout
	dataDir := bootSettingsDir()
	command := supervise.UpdateCommand{
		DataDir:     dataDir,
		UpdateID:    flags.id,
		Out:         out,
		AcquireLock: acquireUpdateLock,
		SchemaVersion: func() (int, error) {
			return store.ReadSchemaVersion(filepath.Join(dataDir, supervise.DatabaseFiles()[0]))
		},
		Log: log.Printf,
	}
	if err := supervise.WriteUpdateEvent(out, command.Started()); err != nil {
		log.Printf("update: %s: report start: %v", name, err)
	}
	result := runUpdateStep(ctx, name, flags, command)
	if err := supervise.WriteUpdateEvent(out, result); err != nil {
		log.Printf("update: %s: report the result: %v", name, err)
	}
	log.Printf("update: %s %s: %s %s", name, flags.id, result.Outcome, result.Reason)
	switch result.Outcome {
	case supervise.UpdateOutcomeOK, supervise.UpdateOutcomePrepared:
		return 0
	}
	return 1
}

func runUpdateStep(ctx context.Context, name string, flags updateCommandFlags, command supervise.UpdateCommand) supervise.UpdateEvent {
	if command.DataDir == "" {
		return supervise.UpdateEvent{Type: supervise.UpdateEventResult, Outcome: supervise.UpdateOutcomeRefused,
			Reason: "cannot determine the data directory"}
	}
	switch name {
	case supervise.UpdateSpaceCommand:
		return command.Space(flags.hostFree)
	case supervise.UpdateSnapshotCommand:
		return command.Snapshot(ctx, flags.hostFree)
	case supervise.UpdateTrialRunCommand:
		executable, err := os.Executable()
		if err != nil {
			return supervise.UpdateEvent{Type: supervise.UpdateEventResult, Outcome: supervise.UpdateOutcomeRefused,
				Reason: fmt.Sprintf("cannot find this binary's own path: %v", err)}
		}
		trialArgs := []string{supervise.UpdateTrialCommand}
		if flags.dataDir != "" {
			trialArgs = append(trialArgs, "--data-dir", flags.dataDir)
		}
		return command.TrialRun(ctx, supervise.TrialRunOptions{
			Binary: executable, Args: trialArgs,
			TargetVersion: flags.to, Attempt: flags.attempt,
		})
	case supervise.UpdateRestoreCommand:
		reason := flags.reason
		if reason == "" {
			reason = "the update did not finish"
		}
		return command.Restore(ctx, reason)
	case supervise.UpdateDiscardCommand:
		return command.Discard(ctx)
	}
	return supervise.UpdateEvent{Type: supervise.UpdateEventResult, Outcome: supervise.UpdateOutcomeRefused,
		Reason: "unknown update command " + name}
}

// acquireUpdateLock is the backend lock for an update command. The lock's
// file is what the trial inherits.
func acquireUpdateLock(ctx context.Context, wait time.Duration) (*os.File, func(), error) {
	lock, err := waitForBackendInstanceLock(ctx, bootSettingsDir(), wait)
	if err != nil {
		return nil, nil, err
	}
	return lock.file, func() {
		if err := lock.file.Close(); err != nil {
			log.Printf("update: release the backend lock: %v", err)
		}
	}, nil
}

// heldTrialLock keeps the inherited lock's descriptor open for the trial's
// life. A collected *os.File closes it.
var heldTrialLock *os.File

// trialBackend is the boot a trial proves: start runs until prepared or
// failure, and shutdown is the ordered teardown after the supervisor asks
// the trial to stop.
type trialBackend interface {
	start(ctx context.Context) error
	shutdown()
}

// runUpdateTrial is the trial: the platform's backend boot with no window and
// no client, judged by the process that started it.
func runUpdateTrial() int {
	return runTrialProtocol(newTrialAppBackend)
}

// runTrialProtocol speaks the trial's side of the channel around a boot. It
// reports progress, then prepared or failed, and then waits for the
// supervisor to stop it or go away.
func runTrialProtocol(newBackend func(observe func(startupprogress.Progress)) trialBackend) int {
	conn, err := supervise.OpenChildChannel(os.LookupEnv, os.Unsetenv)
	if err != nil {
		log.Printf("update trial: %v", err)
		return 2
	}
	if conn == nil {
		log.Printf("update trial: started without a supervisor channel; run %s instead", supervise.UpdateTrialRunCommand)
		return 2
	}
	defer conn.Close()
	opening, err := conn.Receive()
	if err != nil || opening.Type != supervise.MsgActivate || !opening.Trial {
		log.Printf("update trial: the opening frame is not a trial's activate (%+v, %v)", opening, err)
		return 2
	}
	fail := func(reason string) int {
		log.Printf("update trial: %s", reason)
		if err := conn.Send(supervise.Message{Type: supervise.MsgFailed, Reason: reason}); err != nil {
			log.Printf("update trial: report the failure: %v", err)
		}
		return 1
	}
	lock, err := supervise.AdoptInheritedLock(os.LookupEnv, os.Unsetenv)
	if err != nil {
		return fail(err.Error())
	}
	if lock == nil {
		return fail("the trial was started without the data root's lock")
	}
	heldTrialLock = lock
	if err := conn.Send(supervise.Message{
		Type: supervise.MsgHello, ProtocolVersion: supervise.ProtocolVersion,
		Version: version, ReportsProgress: true,
	}); err != nil {
		log.Printf("update trial: greet the supervisor: %v", err)
		return 1
	}
	log.Printf("update trial: booting %s for update %s", version, opening.UpdateID)

	// Signals are caught from here on, so a stop during the boot cancels it
	// (a migration rolls back) and a stop after prepared runs the ordered
	// shutdown.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	// The supervisor closing the channel is the stop request of last resort:
	// a parent that died cannot send SIGTERM.
	supervisorGone := make(chan struct{})
	go func() {
		defer close(supervisorGone)
		for {
			if _, err := conn.Receive(); err != nil {
				return
			}
		}
	}()
	stopRequested := make(chan struct{})
	go func() {
		select {
		case sig := <-signals:
			log.Printf("update trial: received %s", sig)
		case <-supervisorGone:
			log.Printf("update trial: the supervisor closed the channel")
		}
		close(stopRequested)
	}()

	progress := supervise.NewProgressRelay(func(p startupprogress.Progress) error {
		return conn.Send(supervise.Message{Type: supervise.MsgProgress, Progress: &p})
	})
	backend := newBackend(progress.Report)
	bootCtx, bootCancel := context.WithCancel(context.Background())
	defer bootCancel()
	cancelBootOnShutdownRequest(bootCtx, bootCancel, stopRequested)
	startErr := backend.start(bootCtx)
	if err := progress.Close(); err != nil {
		log.Printf("update trial: forward progress: %v", err)
	}
	if startErr != nil {
		reason := startErr.Error()
		if bootCtx.Err() != nil {
			reason = "the trial was stopped before it finished starting"
		}
		code := fail(reason)
		backend.shutdown()
		return code
	}
	if err := conn.Send(supervise.Message{Type: supervise.MsgPrepared, UpdateID: opening.UpdateID}); err != nil {
		log.Printf("update trial: report prepared: %v", err)
	}
	log.Printf("update trial: prepared; waiting to be stopped")
	<-stopRequested
	backend.shutdown()
	return 0
}

// trialAppBackend is the production trial: the headless boot with
// unattended work parked, an ephemeral loopback listener that is not pinned,
// and none of the boot-time update reconciliation or notices, which belong
// to the published version's boot.
type trialAppBackend struct {
	app *App
	srv *transport.Server
}

func newTrialAppBackend(observe func(startupprogress.Progress)) trialBackend {
	syncShellEnvForBoot()
	appService := newApp()
	appservice.ParkUnattendedWork(appService.App)
	if platform.IsWSL() {
		// As runHeadless: nothing is advertised before the launcher reports
		// the Windows network, and a trial has no launcher connection.
		appservice.ExpectNativeNetwork(appService.App)
	}
	srv := bootTransport(appService, "127.0.0.1:0", bootTransportOptions{
		BackendLockHeldBySupervisor: true,
		IgnorePersistedNetwork:      true,
		NoPortPin:                   true,
		BootProgressObserver:        observe,
	})
	return &trialAppBackend{app: appService, srv: srv}
}

func (b *trialAppBackend) start(ctx context.Context) error { return b.app.Start(ctx) }

func (b *trialAppBackend) shutdown() { shutdownHeadless(b.app, b.srv) }
