package app

import (
	"agent-overflow/internal/appupdate"
)

var (
	ErrUpdatesUnsupported     = appupdate.ErrUpdatesUnsupported
	ErrUpdateNotReady         = appupdate.ErrUpdateNotReady
	ErrNoUpdateToDownload     = appupdate.ErrNoUpdateToDownload
	ErrInvalidReleaseTag      = appupdate.ErrInvalidReleaseTag
	ErrUpdateBusy             = appupdate.ErrUpdateBusy
	ErrInvalidInstallStatus   = appupdate.ErrInvalidInstallStatus
	ErrNoInstallInFlight      = appupdate.ErrNoInstallInFlight
	ErrInstallVersionMismatch = appupdate.ErrInstallVersionMismatch
)

// UpdateAvailability is the result of CheckForUpdate. Supported is false on
// builds that can't self-update, in which case the frontend hides the update
// section entirely. When Supported && Available, the release fields describe
// the newer version the user may choose to install.
type UpdateAvailability = appupdate.UpdateAvailability

// ReleaseSummary describes one installable release for the version picker. Only
// releases that ship an asset for the running platform AND a checksum sidecar
// are surfaced — anything else can't be installed here, so it's omitted.
type ReleaseSummary = appupdate.ReleaseSummary

// CheckForUpdate asks the configured provider whether a newer release exists.
// It only reads metadata — nothing is downloaded or installed. Returns
// Supported=false (no error) on builds without an updater so the caller can
// quietly hide the UI. A failed release check comes back as CheckError on the
// result rather than an RPC error — the caller still gets every field that
// does not depend on the network.
//
//ao:scope host
//ao:route home
func (a *App) CheckForUpdate() (UpdateAvailability, error) {
	if a.updater == nil {
		return UpdateAvailability{CurrentVersion: a.version}, nil
	}
	return a.updater.CheckForUpdate()
}

// ListReleases returns the installable releases for this build's update target,
// newest first, so the frontend can offer a version picker. Read-only; LocalOnly.
//
//ao:scope host
//ao:route home
func (a *App) ListReleases() ([]ReleaseSummary, error) {
	if a.updater == nil {
		return nil, ErrUpdatesUnsupported
	}
	releases, err := a.updater.ListReleases()
	if err != nil {
		return nil, err
	}
	if releases == nil {
		releases = []ReleaseSummary{}
	}
	return releases, nil
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
//
//ao:scope host
//ao:route home
func (a *App) DownloadUpdate(tag string) error {
	if a.updater == nil {
		return ErrUpdatesUnsupported
	}
	if a.shuttingDown.Load() {
		return ErrShuttingDown
	}
	return a.updater.DownloadUpdate(tag)
}

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
//
//ao:scope host
//ao:route home
func (a *App) RestartToUpdate() error {
	if a.updater == nil {
		return ErrUpdatesUnsupported
	}
	return a.updater.RestartToUpdate()
}

// ReportUpdateInstallStatus is how the Windows launcher answers an
// updater:install directive. Its name is pinned by selfupdate.RPCReportStatus,
// which both sides of the wire import.
//
// stage is selfupdate.StatusProceeding (the launcher accepted the directive and
// is about to install and kill this process) or selfupdate.StatusFailed
// (terminal failure; the launcher stays on the old version and message says
// why). version must name the install actually in flight — a report that does
// not is refused with no state change, so a stale or malformed answer can never
// cancel a real install.
//
// A "proceeding" report does not end the install, it moves it into its second
// phase: the launcher has promised to kill this process, so the ACK deadline is
// replaced by the silence backstop that catches the promise going unkept. A
// "failed" report is terminal from either phase — the launcher can hit an
// install error after acknowledging, which is the ordinary way that second
// phase ends without a swap.
//
// LocalOnly: it mutates install state, clears an on-disk marker, and releases
// the updater's busy fence.
//
//ao:scope host
//ao:route home
func (a *App) ReportUpdateInstallStatus(stage, version, message string) error {
	if a.updater == nil {
		return ErrUpdatesUnsupported
	}
	return a.updater.ReportUpdateInstallStatus(stage, version, message)
}
