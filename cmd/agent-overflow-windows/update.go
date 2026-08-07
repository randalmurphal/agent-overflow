//go:build windows

// update.go is the launcher's half of the Windows/WSL self-update split. The
// WSL backend downloads and digest-verifies the new launcher .exe and stages it
// into %APPDATA%\agent-overflow\update through /mnt/c, then emits an
// InstallDirective on selfupdate.ChannelInstall — it cannot swap a running
// Windows executable, and we can. This file receives that directive and drives
// the stock Wails updater against the staged file, which re-verifies the bytes,
// spawns the detached swap helper, and quits us so the helper can proceed.
//
// The wire half (subscription, decode, validation, status RPCs) lives in
// internal/wsllauncher so it is exercised by ordinary Linux unit tests; what is
// left here is the part that genuinely needs a Windows process to be true.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"agent-overflow/internal/selfupdate"
	"agent-overflow/internal/wsldistro"
	"agent-overflow/internal/wsllauncher"

	"github.com/wailsapp/wails/v3/pkg/updater"
)

// updateInstallTimeout bounds the verify-and-stage step. The "download" is a
// chunked copy of an already-local file, so this is generous by two orders of
// magnitude; it exists so a wedged filesystem (a disconnected /mnt/c share, a
// scanner holding the artifact) surfaces as a failed install the user can retry
// instead of an install that never answers.
const updateInstallTimeout = 2 * time.Minute

// updateRestartExitWatchdogDelay is how long we let the graceful shutdown run
// after Restart before force-exiting. The swap helper waits for THIS process to
// exit and aborts the whole update if we are still alive after 30s, so a wedged
// shutdown would silently cancel the swap and leave the user on the old version
// with no error to show for it. 25s keeps the normal quit path (OnShutdown's
// 5s-bounded WSL teardown plus run()'s belt-and-braces Stop, ~6s worst case)
// far inside the window while staying under the helper's abort. Mirrors the
// backend's restartExitWatchdogDelay, which is bounded by the same 30s.
const updateRestartExitWatchdogDelay = 25 * time.Second

// errInstallAckRefused marks the one failure the launcher must NOT report back:
// the backend answered our acknowledgement with a rejection, which means it has
// already unwound this install and surfaced the failure to the user itself.
var errInstallAckRefused = errors.New("the backend refused the install acknowledgement")

// handleUpdateInstall acts on one directive from the backend. It runs on the
// notification client's dispatch goroutine, so it may block for the length of
// the install.
func (a *launcherApp) handleUpdateInstall(directive selfupdate.InstallDirective) {
	// A second directive while one is running is a protocol anomaly, not a
	// user-visible failure of THIS install: the backend emits one per click and
	// stops waiting the moment we report "proceeding". Reporting "failed" here
	// would raise an error toast for an install that is in fact still on its
	// way to swapping the binary. Log and drop; the backend's own ACK timeout
	// is what covers a directive nobody ever acts on.
	if !a.updateInstalling.CompareAndSwap(false, true) {
		log.Printf("updater: dropping install directive for %s; another install is already in progress", directive.Version)
		return
	}

	err := a.installStagedUpdate(directive)
	if err == nil {
		// The swap helper is running and the app is quitting. Nothing left to
		// report, and the guard stays claimed for the rest of this process.
		return
	}
	if errors.Is(err, errInstallAckRefused) {
		// The backend already released its install state and told the user the
		// update failed. Posting StatusFailed would be refused for the same
		// reason and would add nothing the user has not already seen — and
		// swapping would contradict the error they are looking at.
		log.Printf("updater: abandoning install of %s: %v", directive.Version, err)
	} else {
		log.Printf("updater: install %s failed: %v", directive.Version, err)
		if reportErr := a.reportUpdateInstallStatus(selfupdate.StatusFailed, directive.Version, err.Error()); reportErr != nil {
			log.Printf("updater: report install failure: %v", reportErr)
		}
	}
	// The swap never started, so the launcher stays alive on the current
	// version and the user can retry.
	a.updateInstalling.Store(false)
}

// installStagedUpdate verifies the staged artifact and restarts into it. It
// returns nil only once the swap helper is running and the app is quitting —
// after that point the process is on its way out and there is nothing left to
// report.
func (a *launcherApp) installStagedUpdate(directive selfupdate.InstallDirective) error {
	dir, ok := wsldistro.WSLConfigDir()
	if !ok {
		return errors.New(`resolve %APPDATA%\agent-overflow`)
	}
	// InstallDirective.Validate already guarantees a bare file name, but the
	// join is what makes that structural: the wire names a file inside our
	// staging directory, never a path.
	staged := filepath.Join(dir, selfupdate.StagingDirName, directive.Filename)
	info, err := os.Stat(staged)
	if err != nil {
		return fmt.Errorf("stat staged update: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("staged update %s is not a regular file", staged)
	}
	digest, err := directive.Digest()
	if err != nil {
		return err
	}

	// Acknowledge before the work starts: this is what cancels the backend's
	// ACK deadline. The answer decides whether the swap happens at all —
	// wsllauncher.ClassifyInstallAck documents each branch.
	ackErr := a.reportUpdateInstallStatus(selfupdate.StatusProceeding, directive.Version, "")
	switch outcome := wsllauncher.ClassifyInstallAck(ackErr); outcome {
	case wsllauncher.InstallAckAccepted:
	case wsllauncher.InstallAckRefused:
		// The backend answered and rejected us, so the report provably did not
		// take effect: its ACK deadline already unwound the install and showed
		// the user an error, or this directive is stale. Do not swap.
		return fmt.Errorf("%w: %v", errInstallAckRefused, ackErr)
	case wsllauncher.InstallAckUndelivered:
		// No answer is ambiguous, not negative: the report may have landed with
		// only its response lost, leaving the backend holding its side open for
		// this swap. Continuing risks a spurious "the launcher did not respond"
		// error followed by a successful restart; aborting risks stranding a
		// backend waiting for a swap that never comes. The user asked for the
		// swap — proceed.
		log.Printf("updater: acknowledgement of %s went unanswered (%v); installing anyway", directive.Version, ackErr)
	}

	// A fresh Updater per attempt: Init is one-shot (ErrAlreadyConfigured), and
	// directives arrive at runtime and repeat after a failure, so the
	// application's app.Updater singleton cannot serve them. Each attempt gets
	// an immutable provider built around its own directive.
	u := updater.New(&launcherUpdaterHost{app: a})
	if err := u.Init(updater.Config{
		CurrentVersion: payloadVersion,
		// StagedFileProvider ignores the CheckRequest's version gating — the
		// user picked this exact build, rollbacks included. Platform/Arch are
		// still set so the release the Updater reports is truthful.
		Providers: []updater.Provider{selfupdate.NewStagedFileProvider(staged, directive.Version, digest)},
		Platform:  "wsl",
		Arch:      runtime.GOARCH,
		Window:    updater.WindowNone,
	}); err != nil {
		return fmt.Errorf("configure updater: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), updateInstallTimeout)
	defer cancel()
	// CheckAndInstall streams the staged file through the Updater's hash and
	// compares it against the directive's digest — that IS the launcher-side
	// integrity gate. The file crossed a filesystem the backend does not own
	// between staging and now, so re-verifying here is the point, and a
	// separate pre-hash would only read the bytes twice.
	if err := u.CheckAndInstall(ctx); err != nil {
		return fmt.Errorf("verify staged update: %w", err)
	}
	if u.DownloadedPath() == "" {
		return errors.New("updater staged nothing to restart into")
	}

	disarm := armUpdateExitWatchdog(updateRestartExitWatchdogDelay)
	if err := u.Restart(ctx); err != nil {
		// The helper never spawned; we stay alive on the current version, so
		// the force-exit must not fire.
		disarm()
		return fmt.Errorf("restart into staged update: %w", err)
	}
	return nil
}

// reportUpdateInstallStatus posts one status frame back to the backend over the
// launcher's existing bridge connection.
func (a *launcherApp) reportUpdateInstallStatus(stage, version, message string) error {
	a.mu.Lock()
	client := a.notificationClient
	ctx := a.notificationContext
	a.mu.Unlock()
	if client == nil {
		return errors.New("backend bridge is not connected")
	}
	return client.ReportUpdateInstallStatus(ctx, stage, version, message)
}

// armUpdateExitWatchdog schedules a hard process exit and returns a disarm
// function. SQLite in the WSL backend runs WAL and the Job Object tears the
// child down on our exit either way, so a hard exit that lets the swap proceed
// strictly beats a wedged shutdown that cancels it and leaves the user with a
// windowless process to kill by hand.
func armUpdateExitWatchdog(delay time.Duration) (disarm func()) {
	timer := time.AfterFunc(delay, func() {
		log.Printf("updater: graceful shutdown did not finish within %s of the update restart — force-exiting so the swap helper can proceed", delay)
		os.Exit(0)
	})
	return func() { timer.Stop() }
}

// launcherUpdaterHost is the updater.Host the launcher drives the install
// through. There is no update UI here: the user already saw the whole flow in
// the backend's own updater panel and clicked "Restart to update", so this side
// is headless by design (updater.WindowNone).
type launcherUpdaterHost struct {
	app *launcherApp
}

// Emit records the Updater's lifecycle and progress events in launcher.log.
// They describe a local file copy that the user is already watching from the
// backend's UI, so they are diagnostic only — the launcher has no window of its
// own to route them to.
func (h *launcherUpdaterHost) Emit(name string, data ...any) bool {
	if len(data) == 0 {
		log.Printf("updater: %s", name)
		return true
	}
	log.Printf("updater: %s %+v", name, data)
	return true
}

// OnEvent is reached under WindowNone: openSession registers the six
// updater:user:* / updater:window:ready listeners regardless of window mode
// (verified in pkg/updater/window_lifecycle.go — the windowModeNone branch
// falls through to the same sess.cancel append). Nothing in this process emits
// those events, so the listeners can never fire; the returned remover is a
// no-op that windowSession.close calls on teardown.
func (h *launcherUpdaterHost) OnEvent(string, func(payload any)) func() {
	return func() {}
}

// OpenWindow is unreachable under updater.WindowNone — classifyWindowOption
// maps it to windowModeNone, the one branch of openSession that never calls
// the host. If it is ever reached, the Config grew a window we did not intend;
// say so loudly rather than returning a nil handle the Updater would panic on.
func (h *launcherUpdaterHost) OpenWindow(opts updater.WindowOptions) updater.WindowHandle {
	log.Printf("updater: BUG: the headless launcher updater was asked to open a window %q; refusing", opts.Title)
	return noopUpdaterWindow{}
}

// Quit hands off to the launcher's normal shutdown, the same path the user's
// window close takes: OnShutdown cancels the notification bridge and stops the
// WSL child, then run() returns and the process exits — which is what the swap
// helper is blocked waiting for.
func (h *launcherUpdaterHost) Quit() {
	h.app.wails.Quit()
}

// noopUpdaterWindow keeps OpenWindow's contract (a non-nil handle) without
// putting anything on screen.
type noopUpdaterWindow struct{}

func (noopUpdaterWindow) EmitEvent(string, ...any) bool { return true }
func (noopUpdaterWindow) Show()                         {}
func (noopUpdaterWindow) Close()                        {}
