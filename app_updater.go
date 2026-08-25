package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	"github.com/wailsapp/wails/v3/pkg/updater"
)

// In-app self-update surface, shared by both builds. The Wails app.Updater
// singleton backs the native desktop path (initUpdater in
// app_updater_desktop.go); the headless WSL backend builds its own Updater over
// the same providers and stages the artifact for the Windows launcher to swap
// (initWSLUpdater in app_updater_wsl.go). Tests leave a.updater.handle nil, so every
// method here guards that case and reports the feature unsupported rather than
// panicking.
//
// Trust model: integrity-only. Releases are verified against the SHA-256
// SHASUMS256 sidecar published alongside each GitHub release (not cryptographic
// signatures). verifiedProvider below fails the check closed if that sidecar is
// missing, so an update never installs unverified. The sidecar is fetched over
// the same TLS channel as the binary and is NOT independently signed, so it
// guards against corruption and partial/missing assets — not against an attacker
// who can publish a matching {binary, SHASUMS256} pair. The trust root is
// therefore the release-publishing pipeline (the workflow's GITHUB_TOKEN and
// maintainer credentials); if that ever needs hardening, Wails supports an
// ed25519 PublicKey + signature in updater.Config.
//
// UX contract: nothing is downloaded, installed, or restarted without an
// explicit user action. CheckForUpdate only reads release metadata;
// DownloadUpdate and RestartToUpdate are each driven by a distinct button.

const (
	// updaterCheckTimeout bounds a single CheckForUpdate round trip to GitHub.
	updaterCheckTimeout = 30 * time.Second
	// updaterDownloadTimeout bounds the whole download+verify+stage flow. The
	// release binary is tens of MB; this leaves generous headroom for slow
	// links while still failing a genuinely stuck transfer.
	updaterDownloadTimeout = 15 * time.Minute
)

// updaterRepository is the GitHub "owner/repo" the updater polls for releases.
// Releases are matched by asset filename (agent-overflow-<platform>-<arch>[.ext],
// where platform is the running GOOS on desktop and "wsl" on the WSL backend)
// and verified against the SHASUMS256 sidecar.
const updaterRepository = "randalmurphal/agent-overflow"

// updaterChecksumAsset is the exact release-asset name the GitHub provider
// fetches and parses for SHA-256 digests. Must match the file written by
// scripts/package-release-assets.sh; a mismatch makes the provider fall open
// (no verification), which verifiedProvider then rejects.
const updaterChecksumAsset = "SHASUMS256"

// updaterEventBridge maps each Wails updater lifecycle event onto the transport
// channel the Svelte UI subscribes to. Both hosts forward through this one
// table so the two builds cannot drift on channel names.
//
// The desktop path receives these on the Wails application event bus
// (pkg/updater/events.go); EventManager.Emit stores a single argument as
// CustomEvent.Data, so e.Data is already the typed payload (updater.Progress
// for progress, *updater.Release for the lifecycle events, updater.ErrorInfo
// for errors) and we forward it verbatim. The WSL host receives the same
// payloads as Host.Emit's first variadic argument.
//
// Check results (update-available / no-update) are deliberately NOT bridged:
// the frontend drives Check via the CheckForUpdate RPC and uses its return
// value, so re-emitting them as events would be redundant.
var updaterEventBridge = map[string]string{
	updater.EventDownloadStarted:  "updater:download-started",
	updater.EventDownloadProgress: "updater:progress",
	updater.EventVerifying:        "updater:verifying",
	updater.EventInstalling:       "updater:installing",
	updater.EventUpdateReady:      "updater:ready",
	updater.EventError:            "updater:error",
}

// updaterReadyChannel and updaterErrorChannel name the two channels the App
// emits on directly rather than through a host bridge: the WSL path raises its
// own readiness (the framework's fires too early there), and both paths raise
// errors for the failures the framework returns instead of emitting.
//
// They are resolved from updaterEventBridge so there is still exactly one
// name↔channel table, and mustBridgedChannel makes deleting a row from it a
// startup panic rather than a silent emit onto the empty channel.
var (
	updaterReadyChannel = mustBridgedChannel(updater.EventUpdateReady)
	updaterErrorChannel = mustBridgedChannel(updater.EventError)
)

func mustBridgedChannel(event string) string {
	channel, ok := updaterEventBridge[event]
	if !ok || channel == "" {
		panic("updater: " + event + " has no transport channel in updaterEventBridge")
	}
	return channel
}

var (
	// ErrUpdatesUnsupported is returned by the updater RPCs on builds that
	// can't self-update: the headless WSL backend (no Wails application) and
	// unstamped "dev" builds. The frontend treats it as "hide the update UI".
	ErrUpdatesUnsupported = errors.New("app: in-app updates are not available in this build")
	// ErrUpdateNotReady is returned by RestartToUpdate when no downloaded
	// update has been staged yet (DownloadUpdate has not completed).
	ErrUpdateNotReady = errors.New("app: no downloaded update is ready to install")
	// ErrNoUpdateToDownload is returned by DownloadUpdate when CheckForUpdate
	// has not found a newer release to download.
	ErrNoUpdateToDownload = errors.New("app: no update is available to download")
	// ErrInvalidReleaseTag is returned by DownloadUpdate when the caller passes
	// a tag that fails validation (defense-in-depth before it reaches a URL).
	ErrInvalidReleaseTag = errors.New("app: invalid release tag")
	// ErrUpdateBusy is returned by DownloadUpdate when another download/install
	// is already in flight (the updater handles one at a time).
	ErrUpdateBusy = errors.New("app: an update is already being installed")

	// The next three are the WSL install-handoff refusals, returned to the
	// Windows launcher by ReportUpdateInstallStatus. Each leaves the in-flight
	// install state untouched, so a malformed or stale report can never cancel
	// an install that is genuinely under way.
	//
	// ErrInvalidInstallStatus rejects a stage outside the selfupdate vocabulary.
	ErrInvalidInstallStatus = errors.New("app: unknown update install status")
	// ErrNoInstallInFlight rejects a report that matches no outstanding
	// directive: one that arrives before any RestartToUpdate, a duplicate after
	// the first report settled the install, or one that lost the race with the
	// acknowledgement timeout.
	ErrNoInstallInFlight = errors.New("app: no update install is awaiting a launcher report")
	// ErrInstallVersionMismatch rejects a report naming a different release
	// than the one in flight — a stale directive the launcher acted on late.
	ErrInstallVersionMismatch = errors.New("app: update install report names a different release")
)

// UpdateAvailability is the result of CheckForUpdate. Supported is false on
// builds that can't self-update, in which case the frontend hides the update
// section entirely. When Supported && Available, the release fields describe
// the newer version the user may choose to install.
type UpdateAvailability struct {
	Supported      bool   `json:"supported"`
	Available      bool   `json:"available"`
	CurrentVersion string `json:"currentVersion"`
	LatestVersion  string `json:"latestVersion,omitempty"`
	ReleaseName    string `json:"releaseName,omitempty"`
	ReleaseNotes   string `json:"releaseNotes,omitempty"`
	// LastApplyFailure is the boot-detected notice that a previously staged
	// update never got applied (WSL only: the Windows launcher owns that swap
	// and this process is gone by the time it runs, so the NEXT boot is the
	// only observer). Empty on every ordinary launch. It is process-lifetime
	// state recomputed from the on-disk marker at boot, so it persists across
	// re-checks within a session and clears on the next boot whose running
	// version matches what the marker expected — a re-check must not make a
	// failed install look successful.
	LastApplyFailure string `json:"lastApplyFailure,omitempty"`
	// CheckError, when non-empty, reports that the release check itself failed
	// (network down, GitHub unreachable, rate-limited). It is result state
	// rather than an RPC error so the fields the backend knows WITHOUT the
	// network — Supported, CurrentVersion, and above all LastApplyFailure —
	// still reach the panel: the boot-detected "didn't apply" notice must not
	// vanish behind an offline check.
	CheckError string `json:"checkError,omitempty"`
}

// CheckForUpdate asks the configured provider whether a newer release exists.
// It only reads metadata — nothing is downloaded or installed. Returns
// Supported=false (no error) on builds without an updater so the caller can
// quietly hide the UI. A failed release check comes back as CheckError on the
// result rather than an RPC error — the caller still gets every field that
// does not depend on the network.
func (a *App) CheckForUpdate() (UpdateAvailability, error) {
	if a.updater.handle == nil {
		return UpdateAvailability{Supported: false, CurrentVersion: version}, nil
	}

	a.updater.mu.Lock()
	defer a.updater.mu.Unlock()

	// A download/install is in flight (only reachable from a second --connect
	// client — the same client's UI blocks checks during a download). Running
	// Check now would retarget the provider and overwrite the pending release
	// the installer is about to use, so report the current state without
	// probing the network. The busy client's next check, after the install
	// settles, returns the authoritative answer.
	if a.updater.busy {
		return UpdateAvailability{
			Supported:        true,
			CurrentVersion:   a.updater.handle.CurrentVersion(),
			LastApplyFailure: a.updater.applyFailure,
		}, nil
	}

	// The passive check always reports the newest release: clear any tag a
	// prior DownloadUpdate aimed the provider at, so rolling back to an older
	// version doesn't make a later check report that older version as "latest".
	if a.updater.provider != nil {
		a.updater.provider.SetTarget("")
	}

	ctx, cancel := context.WithTimeout(a.lifeCtx(), updaterCheckTimeout)
	defer cancel()

	rel, err := a.updater.handle.Check(ctx)
	if err != nil {
		// The stash mirrors what the updater would install NOW; a failed
		// resolve means we no longer know, so drop it rather than let a stale
		// identity outlive the release it described.
		a.updater.pending = nil
		return UpdateAvailability{
			Supported:        true,
			CurrentVersion:   a.updater.handle.CurrentVersion(),
			LastApplyFailure: a.updater.applyFailure,
			CheckError:       fmt.Sprintf("check for update: %v", err),
		}, nil
	}
	// Stash the resolved identity (nil when up to date) under the same lock
	// that just ran the Check, so the updater's pending release and our record
	// of it can never be observed out of step. The WSL staging path is the only
	// reader; on desktop this is one small snapshot per check and nothing more.
	a.updater.pending = snapshotRelease(rel)

	out := UpdateAvailability{
		Supported:        true,
		CurrentVersion:   a.updater.handle.CurrentVersion(),
		LastApplyFailure: a.updater.applyFailure,
	}
	if rel != nil {
		out.Available = true
		out.LatestVersion = rel.Version
		out.ReleaseName = rel.Name
		out.ReleaseNotes = rel.Notes
	}
	return out, nil
}

// snapshotRelease copies the parts of a resolved release the App retains, so
// nothing we hold aliases a struct the Wails updater owns and may reuse. nil in,
// nil out — "up to date" is a legitimate resolve result.
//
// Metadata is carried by reference on purpose: the map is populated at resolve
// time and never mutated afterwards, and it is part of the payload the desktop
// bridge already forwards on updater:ready.
func snapshotRelease(rel *updater.Release) *updater.Release {
	if rel == nil {
		return nil
	}
	out := *rel
	if rel.Verification != nil {
		v := *rel.Verification
		v.Digest = append([]byte(nil), rel.Verification.Digest...)
		v.Signature = append([]byte(nil), rel.Verification.Signature...)
		out.Verification = &v
	}
	return &out
}

// DownloadUpdate downloads, verifies, and stages a release, then leaves it
// pending a user-driven restart. It returns as soon as the work is launched:
// the download blocks for seconds-to-minutes, so it runs off the RPC goroutine
// and the frontend tracks progress + the terminal (ready / error) state via the
// bridged updater:* events. Decoupling from the RPC lifecycle also means a
// WebSocket reconnect mid-download doesn't abandon the install.
//
// tag selects which release to install:
//
//   - "" installs the pending release a prior CheckForUpdate already found
//     (the latest). This requires StateAvailable now, so the common misuse
//     (download with no prior check) fails fast and synchronously.
//   - a specific tag (e.g. "v0.0.7") aims the provider at that exact release
//     and resolves it in the goroutine below — including an OLDER version, so
//     the user can roll back. The newer-than-current gate is deliberately
//     skipped for an explicit pick; integrity verification still applies.
//
// Only one download runs at a time: it claims a.updater.busy under a.updater.mu and
// returns ErrUpdateBusy if another is already in flight. The busy flag also
// fences a concurrent CheckForUpdate out of re-targeting the provider while the
// chosen release is being resolved and installed.
func (a *App) DownloadUpdate(tag string) error {
	if a.updater.handle == nil {
		return ErrUpdatesUnsupported
	}
	if a.shuttingDown.Load() {
		return ErrShuttingDown
	}
	if tag != "" && !validReleaseTag(tag) {
		return fmt.Errorf("%w: %q", ErrInvalidReleaseTag, tag)
	}

	// Claim the updater under a.updater.mu so this whole resolve+install is
	// serialized against CheckForUpdate (and a second DownloadUpdate). The
	// empty-tag precondition is re-checked here, holding the lock, so a
	// concurrent check can't flip the state between the guard and the claim.
	a.updater.mu.Lock()
	if a.updater.busy {
		a.updater.mu.Unlock()
		return ErrUpdateBusy
	}
	if tag == "" {
		// Empty tag installs the already-staged latest; require StateAvailable
		// so a download with no prior check fails fast. A specific tag resolves
		// its own pending release below, so it has no such precondition.
		if st := a.updater.handle.State(); st != updater.StateAvailable {
			a.updater.mu.Unlock()
			return fmt.Errorf("%w (state=%s)", ErrNoUpdateToDownload, st)
		}
	}
	a.updater.busy = true
	// Copy the resolved identity out under the busy fence. From here on the
	// goroutine works from this local, so a CheckForUpdate that lands after the
	// install settles (the busy flag only fences it until then) cannot retarget
	// the release the staging step is about to name and verify against. The
	// by-tag path below overwrites it with its own resolve.
	pending := a.updater.pending
	a.updater.mu.Unlock()

	go func() {
		// terminal, when set, is the event that ends this flow for the
		// frontend. It fires from the deferred block AFTER a.updater.busy drops,
		// because a client's very next action can test that same fence: the WSL
		// RestartToUpdate refuses while a download holds it, and a UI that acts
		// on "ready" the instant it lands must not be told an update is still
		// installing by the goroutine that just finished installing it.
		var terminal func()
		defer func() {
			a.updater.mu.Lock()
			a.updater.busy = false
			a.updater.mu.Unlock()
			if terminal != nil {
				terminal()
			}
		}()

		if tag != "" {
			// Retarget + resolve under the lock so a racing CheckForUpdate can't
			// reset the provider target between SetTarget and Check. a.updater.busy
			// (still set) keeps that check from running its own Check until the
			// install finishes, so the pending release stays this tag's. Bound
			// the resolve by the short check timeout — not the download timeout
			// — so the lock (which a concurrent check may wait on) is held only
			// for the metadata round trip, never the multi-minute download.
			rctx, rcancel := context.WithTimeout(a.lifeCtx(), updaterCheckTimeout)
			a.updater.mu.Lock()
			if a.updater.provider != nil {
				a.updater.provider.SetTarget(tag)
			}
			rel, err := a.updater.handle.Check(rctx)
			// Both resolve paths stash the same way, still holding the lock the
			// Check ran under: this one and CheckForUpdate's. If only one did,
			// the WSL staging step below could copy freshly downloaded bytes
			// under a stale release's filename and digest.
			a.updater.pending = snapshotRelease(rel)
			pending = a.updater.pending
			a.updater.mu.Unlock()
			rcancel()
			// Check errors are RETURNED by the updater, not emitted, so on
			// failure we surface our own updater:error — the frontend has
			// already flipped to "downloading" and would otherwise hang with no
			// terminal event. A nil release means the tag isn't installable on
			// this platform (no matching asset).
			if err != nil {
				log.Printf("updater: resolve %s failed: %v", tag, err)
				terminal = a.updaterErrorEmitter(updater.ErrorInfo{Stage: updater.StageCheck, Message: err.Error(), Provider: "github"})
				return
			}
			if rel == nil {
				log.Printf("updater: resolve %s returned no installable release", tag)
				terminal = a.updaterErrorEmitter(updater.ErrorInfo{Stage: updater.StageCheck, Message: fmt.Sprintf("release %s is not installable on this platform", tag), Provider: "github"})
				return
			}
		}

		ctx, cancel := context.WithTimeout(a.lifeCtx(), updaterDownloadTimeout)
		defer cancel()
		// DownloadAndInstall emits EventVerifying / EventInstalling /
		// EventUpdateReady on success and EventError on failure; the bridge
		// forwards all of them to the frontend, so the only thing left to do
		// with the returned error is log it for the server-side record.
		if err := a.updater.handle.DownloadAndInstall(ctx); err != nil {
			log.Printf("updater: download/install failed: %v", err)
			return
		}
		// WSL mode: the verified artifact is still inside the distro, where the
		// Windows launcher that performs the swap cannot reach it. Copying it
		// across /mnt/c is what "ready" means here, so the WSL host suppresses
		// the framework's EventUpdateReady and stageWSLUpdate emits the
		// updater:ready the frontend acts on once the bytes have landed —
		// verified again on the far side. Desktop mode never enters this branch
		// and keeps the bridged event.
		if a.updater.wsl != nil {
			terminal = a.stageWSLUpdate(pending)
		}
	}()
	return nil
}

// updaterErrorEmitter defers one updater:error emission. Used for the terminal
// events DownloadUpdate's goroutine raises itself (the ones the framework does
// not emit for us), so they land on the same after-the-fence edge as the
// success event.
func (a *App) updaterErrorEmitter(info updater.ErrorInfo) func() {
	return func() { a.emit(updaterErrorChannel, info) }
}

// restartExitWatchdogDelay is how long RestartToUpdate lets the graceful
// shutdown run before force-exiting the process. Bounded on both sides:
// it must exceed the ~24s worst-case sum of app_shutdown.go's sequential
// per-step timeouts (3+2+2+2+5+5+5s) so a slow-but-healthy teardown is
// never cut short of its session-close/orphan-reaper/store-close steps,
// and it must stay under the swap helper's 30s parent-exit timeout —
// past that, the helper aborts the whole update.
const restartExitWatchdogDelay = 25 * time.Second

// RestartToUpdate swaps in the staged update and relaunches. It spawns the
// detached swap helper and asks Wails to begin its normal shutdown, so the
// transport drains and stores flush before the process exits; the helper then
// replaces the binary (or .app bundle) and starts the new version. This quits
// the running app, so it is only ever wired to an explicit button.
//
// The WSL backend cannot do any of that — the executable being replaced is the
// Windows launcher's, on a filesystem this process only sees through /mnt/c —
// so it hands the staged artifact to the launcher instead and lets the launcher
// kill it. See restartToUpdateWSL.
func (a *App) RestartToUpdate() error {
	if a.updater.handle == nil {
		return ErrUpdatesUnsupported
	}
	if a.updater.wsl != nil {
		return a.restartToUpdateWSL()
	}
	if a.updater.handle.DownloadedPath() == "" {
		return ErrUpdateNotReady
	}
	return a.restartWithExitWatchdog(a.updater.handle.Restart)
}

// restartWithExitWatchdog arms a force-exit watchdog around the restart
// dance. The swap helper waits for THIS process to exit and aborts the
// update if it is still alive after 30s. A wedged graceful shutdown would
// therefore not just hang the app — it silently cancels the update and
// leaves a windowless zombie the user has to Force Quit (observed in the
// field on macOS: helper log "parent did not exit within timeout —
// aborting swap"). The process is going away either way, SQLite runs WAL
// (crash-safe), and provider session files are the authoritative recovery
// source, so a hard exit that lets the swap proceed strictly beats a
// zombie that cancels it. Disarmed only when the helper spawn itself
// fails and the app intentionally stays alive on the old version.
func (a *App) restartWithExitWatchdog(restart func(ctx context.Context) error) error {
	disarm := a.armRestartExitWatchdog(restartExitWatchdogDelay)
	if err := restart(a.lifeCtx()); err != nil {
		disarm()
		return fmt.Errorf("restart to update: %w", err)
	}
	return nil
}

// armRestartExitWatchdog schedules a hard process exit after delay and
// returns a disarm function. Fires through a.updater.restartExitFn (os.Exit when
// nil) so tests can observe the trigger without dying.
func (a *App) armRestartExitWatchdog(delay time.Duration) (disarm func()) {
	timer := time.AfterFunc(delay, func() {
		log.Printf("updater: graceful shutdown did not finish within %s of RestartToUpdate — force-exiting so the swap helper can proceed", delay)
		exitFn := a.updater.restartExitFn
		if exitFn == nil {
			exitFn = os.Exit
		}
		exitFn(0)
	})
	return func() { timer.Stop() }
}

// verifiedProvider wraps an updater.Provider and fails closed when a release
// arrives without verification material. The stock GitHub provider populates
// Release.Verification only when its checksum-sidecar lookup succeeds; on any
// miss (sidecar absent, asset-name mismatch) it returns a Release with nil
// Verification, and the Updater would then install it WITHOUT any integrity
// check. That silent fall-open is exactly the failure mode this codebase
// refuses to ship, so we turn it into a hard error at the provider boundary —
// before the Updater ever sees an unverifiable release.
type verifiedProvider struct {
	inner updater.Provider
}

func (p verifiedProvider) Name() string { return p.inner.Name() }

func (p verifiedProvider) Check(ctx context.Context, req updater.CheckRequest) (*updater.Release, error) {
	rel, err := p.inner.Check(ctx, req)
	if err != nil || rel == nil {
		// err: propagate as-is. rel == nil: caller is up to date — there is
		// nothing to verify, so this is the normal "no update" path.
		return rel, err
	}
	if rel.Verification == nil || len(rel.Verification.Digest) == 0 {
		return nil, fmt.Errorf("updater: release %s ships no checksum to verify against; refusing to install unverified", rel.Version)
	}
	return rel, nil
}

func (p verifiedProvider) Download(ctx context.Context, rel *updater.Release, dst io.Writer, onProgress func(written, total int64)) error {
	return p.inner.Download(ctx, rel, dst, onProgress)
}
