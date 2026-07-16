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

// In-app self-update surface. The Wails app.Updater singleton is configured
// for the native desktop path only (see initUpdater in app_updater_desktop.go);
// the headless WSL backend and tests leave a.updater nil, so every method here
// guards that case and reports the feature unsupported rather than panicking.
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
}

// CheckForUpdate asks the configured provider whether a newer release exists.
// It only reads metadata — nothing is downloaded or installed. Returns
// Supported=false (no error) on builds without an updater so the caller can
// quietly hide the UI.
func (a *App) CheckForUpdate() (UpdateAvailability, error) {
	if a.updater == nil {
		return UpdateAvailability{Supported: false, CurrentVersion: version}, nil
	}

	a.updaterMu.Lock()
	defer a.updaterMu.Unlock()

	// A download/install is in flight (only reachable from a second --connect
	// client — the same client's UI blocks checks during a download). Running
	// Check now would retarget the provider and overwrite the pending release
	// the installer is about to use, so report the current state without
	// probing the network. The busy client's next check, after the install
	// settles, returns the authoritative answer.
	if a.updaterBusy {
		return UpdateAvailability{Supported: true, CurrentVersion: a.updater.CurrentVersion()}, nil
	}

	// The passive check always reports the newest release: clear any tag a
	// prior DownloadUpdate aimed the provider at, so rolling back to an older
	// version doesn't make a later check report that older version as "latest".
	if a.updaterProvider != nil {
		a.updaterProvider.SetTarget("")
	}

	ctx, cancel := context.WithTimeout(a.lifeCtx(), updaterCheckTimeout)
	defer cancel()

	rel, err := a.updater.Check(ctx)
	if err != nil {
		return UpdateAvailability{}, fmt.Errorf("check for update: %w", err)
	}

	out := UpdateAvailability{Supported: true, CurrentVersion: a.updater.CurrentVersion()}
	if rel != nil {
		out.Available = true
		out.LatestVersion = rel.Version
		out.ReleaseName = rel.Name
		out.ReleaseNotes = rel.Notes
	}
	return out, nil
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
// Only one download runs at a time: it claims updaterBusy under updaterMu and
// returns ErrUpdateBusy if another is already in flight. The busy flag also
// fences a concurrent CheckForUpdate out of re-targeting the provider while the
// chosen release is being resolved and installed.
func (a *App) DownloadUpdate(tag string) error {
	if a.updater == nil {
		return ErrUpdatesUnsupported
	}
	if a.shuttingDown.Load() {
		return ErrShuttingDown
	}
	if tag != "" && !validReleaseTag(tag) {
		return fmt.Errorf("%w: %q", ErrInvalidReleaseTag, tag)
	}

	// Claim the updater under updaterMu so this whole resolve+install is
	// serialized against CheckForUpdate (and a second DownloadUpdate). The
	// empty-tag precondition is re-checked here, holding the lock, so a
	// concurrent check can't flip the state between the guard and the claim.
	a.updaterMu.Lock()
	if a.updaterBusy {
		a.updaterMu.Unlock()
		return ErrUpdateBusy
	}
	if tag == "" {
		// Empty tag installs the already-staged latest; require StateAvailable
		// so a download with no prior check fails fast. A specific tag resolves
		// its own pending release below, so it has no such precondition.
		if st := a.updater.State(); st != updater.StateAvailable {
			a.updaterMu.Unlock()
			return fmt.Errorf("%w (state=%s)", ErrNoUpdateToDownload, st)
		}
	}
	a.updaterBusy = true
	a.updaterMu.Unlock()

	go func() {
		defer func() {
			a.updaterMu.Lock()
			a.updaterBusy = false
			a.updaterMu.Unlock()
		}()

		if tag != "" {
			// Retarget + resolve under the lock so a racing CheckForUpdate can't
			// reset the provider target between SetTarget and Check. updaterBusy
			// (still set) keeps that check from running its own Check until the
			// install finishes, so the pending release stays this tag's. Bound
			// the resolve by the short check timeout — not the download timeout
			// — so the lock (which a concurrent check may wait on) is held only
			// for the metadata round trip, never the multi-minute download.
			rctx, rcancel := context.WithTimeout(a.lifeCtx(), updaterCheckTimeout)
			a.updaterMu.Lock()
			if a.updaterProvider != nil {
				a.updaterProvider.SetTarget(tag)
			}
			rel, err := a.updater.Check(rctx)
			a.updaterMu.Unlock()
			rcancel()
			// Check errors are RETURNED by the updater, not emitted, so on
			// failure we surface our own updater:error — the frontend has
			// already flipped to "downloading" and would otherwise hang with no
			// terminal event. A nil release means the tag isn't installable on
			// this platform (no matching asset).
			if err != nil {
				log.Printf("updater: resolve %s failed: %v", tag, err)
				a.emit("updater:error", updater.ErrorInfo{Stage: updater.StageCheck, Message: err.Error(), Provider: "github"})
				return
			}
			if rel == nil {
				log.Printf("updater: resolve %s returned no installable release", tag)
				a.emit("updater:error", updater.ErrorInfo{Stage: updater.StageCheck, Message: fmt.Sprintf("release %s is not installable on this platform", tag), Provider: "github"})
				return
			}
		}

		ctx, cancel := context.WithTimeout(a.lifeCtx(), updaterDownloadTimeout)
		defer cancel()
		// DownloadAndInstall emits EventVerifying / EventInstalling /
		// EventUpdateReady on success and EventError on failure; the bridge
		// forwards all of them to the frontend, so the only thing left to do
		// with the returned error is log it for the server-side record.
		if err := a.updater.DownloadAndInstall(ctx); err != nil {
			log.Printf("updater: download/install failed: %v", err)
		}
	}()
	return nil
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
func (a *App) RestartToUpdate() error {
	if a.updater == nil {
		return ErrUpdatesUnsupported
	}
	if a.updater.DownloadedPath() == "" {
		return ErrUpdateNotReady
	}
	return a.restartWithExitWatchdog(a.updater.Restart)
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
// returns a disarm function. Fires through a.restartExitFn (os.Exit when
// nil) so tests can observe the trigger without dying.
func (a *App) armRestartExitWatchdog(delay time.Duration) (disarm func()) {
	timer := time.AfterFunc(delay, func() {
		log.Printf("updater: graceful shutdown did not finish within %s of RestartToUpdate — force-exiting so the swap helper can proceed", delay)
		exitFn := a.restartExitFn
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
