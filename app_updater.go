package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
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

// DownloadUpdate downloads, verifies, and stages the release found by a prior
// CheckForUpdate. It returns as soon as the work is launched: the download
// blocks for seconds-to-minutes, so it runs off the RPC goroutine and the
// frontend tracks progress + the terminal (ready / error) state via the
// bridged updater:* events. Decoupling from the RPC lifecycle also means a
// WebSocket reconnect mid-download doesn't abandon the install.
//
// The synchronous guards give the caller immediate feedback for the common
// misuse (no prior check, unsupported build); the updater itself serialises
// concurrent downloads and re-validates the pending release before streaming.
func (a *App) DownloadUpdate() error {
	if a.updater == nil {
		return ErrUpdatesUnsupported
	}
	if a.shuttingDown.Load() {
		return ErrShuttingDown
	}
	if st := a.updater.State(); st != updater.StateAvailable {
		// StateAvailable is the only phase from which a download is valid; any
		// other phase means no checked-and-pending release (or a flow already
		// in progress). Surfaced synchronously so the button can react.
		return fmt.Errorf("%w (state=%s)", ErrNoUpdateToDownload, st)
	}

	go func() {
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
	if err := a.updater.Restart(a.lifeCtx()); err != nil {
		return fmt.Errorf("restart to update: %w", err)
	}
	return nil
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
