// app_updater_wsl.go is the WSL half of in-app self-update: the process that
// can DOWNLOAD is not the process that can SWAP.
//
// The headless backend runs inside the distro and drives the same
// check/download/verify state machine the desktop build does, but the
// executable a swap would replace is the Windows launcher's, on a filesystem
// this process only reaches through /mnt/c. So the backend stages the verified
// `agent-overflow-wsl-amd64.exe` into the launcher's
// `%APPDATA%\agent-overflow\update` and emits an `updater:install` directive;
// the launcher re-verifies the digest, swaps, and relaunches. A marker written
// before the handoff lets the NEXT boot notice a swap that never happened —
// this process is gone by the time the launcher acts, so nothing here can
// observe the outcome directly.
//
// The handoff is therefore the whole risk, and it has two phases, each under
// its own deadline (armWSLInstallDeadlineLocked owns both, one timer field):
//
//	RestartToUpdate  → marker written, a.updater.busy claimed and held,
//	                   directive emitted, ACK deadline armed
//	  ├─ "proceeding" → acknowledged: marker and fence KEPT (the launcher is
//	  │                 about to kill this process), ACK deadline replaced by
//	  │                 the silence backstop
//	  │    ├─ process death .................. the success path; nothing to do
//	  │    ├─ "failed" ....................... unwind (the launcher hit an
//	  │    │                                   install error after accepting)
//	  │    └─ backstop expires ............... unwind (launcher gone / bridge
//	  │                                        dead, and nobody left to say so)
//	  ├─ "failed" ........................... unwind
//	  └─ ACK deadline expires ............... unwind
//
// "Unwind" is one thing everywhere (abandonWSLInstallLocked +
// emitWSLInstallFailure): clear the in-flight state, drop the marker, release
// the fence — all under the one lock — then emit a terminal updater:error.
// Every path is generation-guarded and idempotent, so a report and a deadline
// racing each other produce exactly one unwind and one event.
//
// Nothing in this file carries a build tag, deliberately. The gate is runtime
// state (the launcher's WSLENV-injected AppData path), not a build tag, so the
// whole flow compiles and is exercised by the ordinary `go test ./...` build
// rather than only under `-tags nogui`.
package main

import (
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"runtime"
	"time"

	"agent-overflow/internal/notify"
	"agent-overflow/internal/selfupdate"
	"agent-overflow/internal/wsldistro"

	"github.com/wailsapp/wails/v3/pkg/updater"
	"github.com/wailsapp/wails/v3/pkg/updater/providers/github"
)

const (
	// wslUpdaterPlatform is the release-asset platform token this backend
	// installs for. It is NOT runtime.GOOS: a linux/amd64 process fetches a
	// Windows .exe for the launcher to swap in, so the assets it can install
	// are the ones named for "wsl".
	wslUpdaterPlatform = "wsl"

	// wslInstallACKTimeout bounds how long RestartToUpdate waits for the
	// launcher to answer an install directive. The launcher acknowledges before
	// it starts any work, so this is generous; the point is that a launcher
	// that is wedged or not listening must not leave the UI stuck on
	// "restarting" with the updater fenced busy forever.
	wslInstallACKTimeout = 10 * time.Second

	// wslInstallBackstopTimeout bounds the silence AFTER the launcher has
	// acknowledged. The normal end of that silence is this process being killed
	// by the launcher's quit, so the timer only matters for the one case that
	// leaves nobody to tell us anything: the launcher hits an install error and
	// its "failed" report never lands (the bridge died in exactly that window).
	// It then stays alive on the old version while this backend holds the busy
	// fence and a marker on disk forever — the user cannot retry without
	// restarting the app, and the next boot reports a spurious "didn't apply".
	//
	// Derivation: after acknowledging, the launcher bounds its CheckAndInstall
	// at 2 minutes (cmd/agent-overflow-windows/update.go's updateInstallTimeout)
	// and reports failed immediately after, and if the swap DOES start its
	// force-exit watchdog fires 25s later. Three minutes therefore clears every
	// path the launcher can legitimately take; silence past it means the
	// launcher is gone or the bridge is dead. A successful swap kills this
	// process long before the timer matters.
	wslInstallBackstopTimeout = 3 * time.Minute
)

// wslUpdateMode is the WSL-side self-update configuration. Non-nil on App is
// the mode switch every WSL branch of the updater RPCs keys off. Set once by
// initWSLUpdater before the transport server starts and never mutated after,
// so readers need no lock.
type wslUpdateMode struct {
	// stagingDir is <WSLConfigDir()>/update — the launcher's own staging
	// directory, seen from this side of the boundary through /mnt/c.
	stagingDir string
	// markerDir is the backend's app-managed data dir (the same root
	// initStores resolves), holding the update-intent marker. Deliberately NOT
	// the Windows-side dir: the marker is this process's own record of what it
	// asked for, read by the next boot of this same backend, and it must
	// survive a launcher that never touches the staging dir at all.
	markerDir string
	// ackTimeout and backstopTimeout are the two install deadlines
	// (wslInstallACKTimeout / wslInstallBackstopTimeout in production; tests
	// inject short ones so both paths are asserted rather than slept through).
	//
	// ackTimeout must comfortably exceed the two statements between arming the
	// timer and emitting the directive, or a deadline could expire before the
	// launcher was ever told — the launcher would then get its report refused
	// as stale.
	ackTimeout      time.Duration
	backstopTimeout time.Duration
}

// initWSLUpdater configures the headless WSL backend's updater and reconciles
// whatever install the previous run handed to the launcher. Called from
// runHeadless BEFORE the transport server starts, so the updater RPC handlers
// observe App.updater.handle and App.updater.wsl without a race.
//
// Like the desktop initUpdater it is a no-op on any failure (logged): in-app
// updates simply stay unavailable while the app runs normally. Failing to set
// up the updater must never block startup.
func initWSLUpdater(a *App) {
	initWSLUpdaterIn(a, version, bootSettingsDir())
}

// initWSLUpdaterIn is the parameterized core of initWSLUpdater. The split keeps
// the two process-global inputs (the link-stamped version and the --data-dir
// derived app root) at the single production call site, so tests drive the real
// wiring without mutating package globals — the same shape ensureClientIDIn
// uses for the boot-time client id.
func initWSLUpdaterIn(a *App, currentVersion, markerDir string) {
	if currentVersion == "dev" {
		log.Printf("updater: disabled for dev build (version=%q)", currentVersion)
		return
	}
	// The launcher-injected AppData path is BOTH the feature gate and the
	// staging root. Without it there is no Windows-side directory to stage into
	// and no launcher listening for the directive, so a WSL backend started by
	// anything else (a manual `go run`, a dev shell) stays unsupported rather
	// than half-wired: it would download and verify a release nothing could
	// ever install.
	configDir, ok := wslConfigDir()
	if !ok {
		log.Printf("updater: WSL self-update unavailable — %s is not set, so this backend was not started by the Windows launcher", wsldistro.AppDataEnv)
		return
	}
	if markerDir == "" {
		log.Printf("updater: WSL self-update disabled — no app data dir resolves for the install marker")
		return
	}

	// No global client timeout: the same client streams the (tens-of-MB)
	// release binary, and http.Client.Timeout caps the WHOLE exchange including
	// the body read — a fixed cap would abort downloads on slow links. Per-call
	// deadlines via context (updaterCheckTimeout / updaterDownloadTimeout) bound
	// each operation instead; DefaultTransport still applies sane dial /
	// TLS-handshake timeouts. Shared with the targetable wrapper so list/by-tag
	// API calls use the same client.
	httpClient := &http.Client{}
	provider, err := github.New(github.Config{
		Repository:    updaterRepository,
		ChecksumAsset: updaterChecksumAsset,
		HTTPClient:    httpClient,
	})
	if err != nil {
		log.Printf("updater: github provider init failed: %v — in-app updates disabled", err)
		return
	}

	// One CheckRequest describes what this build asks the release feed for, and
	// it feeds both the Updater (which passes it to every Provider.Check) and
	// the targetable wrapper (whose ListReleases has no Updater to ask), so the
	// two can never disagree about which assets are installable here. Platform
	// and Arch MUST be set explicitly: Init defaults an empty Platform to
	// runtime.GOOS, which would silently target the linux desktop assets this
	// backend cannot install.
	req := updater.CheckRequest{
		CurrentVersion: currentVersion,
		Platform:       wslUpdaterPlatform,
		Arch:           runtime.GOARCH,
	}

	// targetable adds version selection (ListReleases + by-tag download) on top
	// of the stock latest-only provider; verifiedProvider still wraps it, so
	// every resolved release — latest or a specific tag — is checksum-verified
	// or rejected fail-closed.
	targetable := newTargetableProvider(provider, updaterRepository, updaterChecksumAsset, req, httpClient)

	u := updater.New(wslUpdaterHost{app: a})
	if err := u.Init(updater.Config{
		CurrentVersion: req.CurrentVersion,
		Platform:       req.Platform,
		Arch:           req.Arch,
		Providers:      []updater.Provider{verifiedProvider{inner: targetable}},
		Window:         updater.WindowNone, // there is no display server here at all
	}); err != nil {
		log.Printf("updater: init failed: %v — in-app updates disabled", err)
		return
	}

	mode := &wslUpdateMode{
		stagingDir:      filepath.Join(configDir, selfupdate.StagingDirName),
		markerDir:       markerDir,
		ackTimeout:      wslInstallACKTimeout,
		backstopTimeout: wslInstallBackstopTimeout,
	}
	a.updater.wsl = mode
	a.updater.handle = u
	a.updater.provider = targetable
	reconcileWSLUpdateMarker(a, currentVersion, mode)
	log.Printf("updater: configured for %s (current version %s, target %s/%s, staging %s)",
		updaterRepository, currentVersion, req.Platform, req.Arch, mode.stagingDir)
}

// reconcileWSLUpdateMarker closes the loop on an install the previous run handed
// to the launcher. The swap happens after this process is gone, so the only
// observer of whether it worked is the next boot's own version.
//
// Deliberately WSL-only rather than a shared boot step: the marker is written
// exclusively by restartToUpdateWSL, and the sweep half needs the Windows-side
// staging dir that only exists under the launcher. On desktop the swap helper
// runs while the framework still owns the flow, and its failures surface
// through the updater's own error path.
func reconcileWSLUpdateMarker(a *App, currentVersion string, mode *wslUpdateMode) {
	marker, err := selfupdate.LoadMarker(mode.markerDir)
	if err != nil {
		// A marker file exists but will not decode: an install WAS attempted
		// and we cannot tell which version it aimed at. Say so rather than
		// discard the evidence, then clear it — an unreadable file must not
		// re-accuse the launcher on every subsequent boot.
		log.Printf("updater: the update-intent marker in %s is unreadable: %v", mode.markerDir, err)
		a.setUpdateApplyFailure(fmt.Sprintf(
			"A previous update record was unreadable, so that update may not have applied — still running %s.",
			currentVersion))
		clearWSLUpdateResidue(mode)
		return
	}
	if marker == nil {
		return // nothing pending: the ordinary boot
	}
	if marker.ExpectedVersion == currentVersion {
		log.Printf("updater: update to %s applied", currentVersion)
		clearWSLUpdateResidue(mode)
		return
	}
	log.Printf("updater: update to %s did not apply — still running %s (staged at %s)",
		marker.ExpectedVersion, currentVersion, marker.StagedAt.Format(time.RFC3339))
	a.setUpdateApplyFailure(fmt.Sprintf("Update to %s didn't apply — still running %s.",
		marker.ExpectedVersion, currentVersion))
	clearWSLUpdateResidue(mode)
}

// clearWSLUpdateResidue drops both halves of a settled install: the marker (its
// question has been answered) and every staged artifact (either it was swapped
// in, or it is never going to be). Failures are logged, not fatal — a staging
// dir we could not clear costs disk, and the next download sweeps it again
// before staging.
func clearWSLUpdateResidue(mode *wslUpdateMode) {
	if err := selfupdate.ClearMarker(mode.markerDir); err != nil {
		log.Printf("updater: clear update marker: %v", err)
	}
	if err := selfupdate.SweepStagingDir(mode.stagingDir); err != nil {
		log.Printf("updater: sweep staging dir: %v", err)
	}
}

// setUpdateApplyFailure records the boot notice CheckForUpdate surfaces on
// UpdateAvailability.LastApplyFailure.
func (a *App) setUpdateApplyFailure(notice string) {
	a.updater.mu.Lock()
	a.updater.applyFailure = notice
	a.updater.mu.Unlock()
}

// notifyPendingUpdateApplyFailure presents the boot-detected "update didn't
// apply" notice as a native toast. It is a separate step from the boot check
// because the two have opposite ordering constraints: the updater must be
// configured BEFORE the transport server starts (so RPC handlers never see a
// half-wired App), while the notification pipe only works AFTER the event bus
// exists. runHeadless calls this once the bus is wired.
//
// Best-effort by design. The launcher's notification bridge may not have
// connected yet at this point in boot; it opens with a replay request from
// sequence 0 (wsllauncher.NotificationClient), so a toast emitted now is
// replayed to it on connect. Nothing here depends on that, though — the durable
// surface is UpdateAvailability.LastApplyFailure, which the Settings panel reads
// on every check for the life of the process.
func (a *App) notifyPendingUpdateApplyFailure() {
	a.updater.mu.Lock()
	notice := a.updater.applyFailure
	a.updater.mu.Unlock()
	if notice == "" {
		return
	}
	if err := a.notifyOS("Update didn't apply", notice, notify.Target{Kind: "none"}); err != nil {
		log.Printf("updater: could not present the update-apply notice: %v", err)
	}
}

// stageWSLUpdate copies the freshly downloaded and verified artifact out of the
// distro into the launcher's staging directory. It returns the terminal event
// its caller must emit — readiness or failure — rather than emitting it itself:
// the caller releases the a.updater.busy fence first, and a client that acts on
// "ready" the instant it lands would otherwise be refused a restart by a fence
// that is already on its way out.
//
// Called from DownloadUpdate's goroutine after DownloadAndInstall succeeds,
// still INSIDE that fence, so no concurrent check, download, or restart can
// observe the half-swapped state between the sweep and the copy.
//
// rel is the release identity captured under the same fence — it carries the
// asset filename and the SHASUMS256 digest, neither of which the Updater
// exposes after a download. StageCopy re-verifies the bytes it writes against
// that digest: the artifact crosses a filesystem this process does not own, and
// that check is the backend-side integrity gate on the hop.
func (a *App) stageWSLUpdate(rel *updater.Release) (terminal func()) {
	mode := a.updater.wsl
	filename, version, digest, err := releaseIdentity(rel)
	if err != nil {
		return a.failWSLStaging(err)
	}
	src := a.updater.handle.DownloadedPath()
	if src == "" {
		return a.failWSLStaging(errors.New("updater: the download reported success but staged no file"))
	}

	// One staged artifact at a time. The directive names a bare filename the
	// launcher resolves inside this directory, so a previous download's .exe
	// (or a temp file a crashed copy left) has no business surviving next to
	// the one we are about to hand over. Clearing App state first keeps the two
	// in step: from here until the copy lands, nothing is staged.
	a.updater.mu.Lock()
	a.updater.staged = nil
	a.updater.mu.Unlock()
	if err := selfupdate.SweepStagingDir(mode.stagingDir); err != nil {
		return a.failWSLStaging(err)
	}

	staged, err := selfupdate.StageCopy(src, mode.stagingDir, filename, digest)
	if err != nil {
		return a.failWSLStaging(err)
	}

	a.updater.mu.Lock()
	a.updater.staged = rel
	a.updater.mu.Unlock()

	log.Printf("updater: staged %s (%s) at %s for the Windows launcher", filename, version, staged)
	// The same channel and payload shape the desktop bridge forwards for
	// EventUpdateReady, because it means the same thing to the frontend: the
	// bytes are in place and the only step left is the user's restart.
	return func() { a.emit(updaterReadyChannel, rel) }
}

// failWSLStaging records a staging failure and returns the terminal event that
// reports it. The UI has been showing progress since download-started and the
// framework's own EventUpdateReady is suppressed on this path, so without this
// the panel would sit at "installing" forever.
func (a *App) failWSLStaging(cause error) func() {
	log.Printf("updater: staging the update for the Windows launcher failed: %v", cause)
	a.updater.mu.Lock()
	a.updater.staged = nil
	a.updater.mu.Unlock()
	return a.updaterErrorEmitter(updater.ErrorInfo{
		Stage:    updater.StageInstall,
		Message:  cause.Error(),
		Provider: selfupdate.ProviderName,
	})
}

// restartToUpdateWSL hands the staged artifact to the Windows launcher. It does
// NOT quit: the launcher owns the swap and kills this backend itself once it
// has re-verified the file, so the process must stay alive and reachable long
// enough to be told what happened.
//
// a.updater.busy is claimed here and released only by the install settling: the
// launcher's "failed" report, an expired deadline (ACK or backstop), or — the
// success path — never, because the launcher kills this process. Holding it in
// between is what keeps a second click, or a second --connect client, from
// emitting a competing directive while the first is in flight.
func (a *App) restartToUpdateWSL() error {
	mode := a.updater.wsl

	a.updater.mu.Lock()
	if a.updater.busy {
		a.updater.mu.Unlock()
		return ErrUpdateBusy
	}
	staged := a.updater.staged
	if staged == nil {
		a.updater.mu.Unlock()
		return ErrUpdateNotReady
	}
	filename, version, digest, err := releaseIdentity(staged)
	if err != nil {
		a.updater.mu.Unlock()
		return fmt.Errorf("restart to update: %w", err)
	}

	a.updater.busy = true
	// The marker goes down before the directive goes out. The launcher may kill
	// this process the moment it reads the directive, and a swap with no marker
	// on disk would leave the next boot unable to tell a successful update from
	// one that silently never applied. SaveMarker is atomic, so a failure here
	// leaves nothing behind and the fence is simply released again.
	if err := selfupdate.SaveMarker(mode.markerDir, selfupdate.Marker{
		ExpectedVersion: version,
		PriorVersion:    a.updater.handle.CurrentVersion(),
		StagedAt:        time.Now(),
	}); err != nil {
		a.updater.busy = false
		a.updater.mu.Unlock()
		return fmt.Errorf("restart to update: %w", err)
	}
	a.updater.install = staged
	// A fresh sequence starts unacknowledged even if a previous one ended in
	// the acknowledged phase. settleWSLInstallLocked already resets this; saying
	// so here is what makes the guarantee local to the handoff.
	a.updater.installAcked = false
	// Armed BEFORE the emit: the launcher can answer the moment the frame
	// lands, and an ACK that arrives before its own deadline exists would find
	// no install in flight and be refused as stale.
	a.armWSLInstallDeadlineLocked(mode.ackTimeout, fmt.Sprintf(
		"The Windows launcher did not respond to the install request within %s, so the update was not applied.",
		mode.ackTimeout))
	a.updater.mu.Unlock()

	log.Printf("updater: handing %s (%s) to the Windows launcher", filename, version)
	a.emit(selfupdate.ChannelInstall, selfupdate.InstallDirective{
		Filename: filename,
		SHA256:   hex.EncodeToString(digest),
		Version:  version,
	})
	return nil
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
func (a *App) ReportUpdateInstallStatus(stage, version, message string) error {
	if a.updater.wsl == nil {
		return ErrUpdatesUnsupported
	}
	switch stage {
	case selfupdate.StatusProceeding, selfupdate.StatusFailed:
	default:
		return fmt.Errorf("%w: %q", ErrInvalidInstallStatus, stage)
	}

	a.updater.mu.Lock()
	inflight := a.updater.install
	if inflight == nil {
		a.updater.mu.Unlock()
		return ErrNoInstallInFlight
	}
	if version != inflight.Version {
		a.updater.mu.Unlock()
		return fmt.Errorf("%w: reported %q, in flight %q", ErrInstallVersionMismatch, version, inflight.Version)
	}

	if stage == selfupdate.StatusProceeding {
		if a.updater.installAcked {
			// A duplicate acknowledgement. Idempotent, and deliberately does NOT
			// re-arm: a chatty or looping launcher must not be able to extend
			// the silence backstop indefinitely, which is the one thing that
			// would put the deadlock this backstop exists to prevent back on
			// the table.
			a.updater.mu.Unlock()
			log.Printf("updater: duplicate acknowledgement for the install of %s; ignoring", version)
			return nil
		}
		a.updater.installAcked = true
		// The marker stays on disk and a.updater.busy stays held on purpose: the
		// launcher has the file and is about to kill this process. That marker
		// is the only thing the NEXT boot can compare its own version against,
		// and the fence keeps a late second click from emitting a directive
		// into a process that is already being torn down.
		//
		// But "about to" is a promise, not a fact. The ACK deadline is replaced
		// by a longer one so that a launcher which acknowledges and then dies
		// (or loses the bridge before it can report the failure) does not leave
		// this backend fenced busy with a marker on disk until the user
		// restarts the app by hand.
		a.armWSLInstallDeadlineLocked(a.updater.wsl.backstopTimeout, fmt.Sprintf(
			"The Windows launcher went silent during the install; still running %s.",
			a.updater.handle.CurrentVersion()))
		a.updater.mu.Unlock()
		log.Printf("updater: the launcher acknowledged the install of %s; awaiting shutdown", version)
		return nil
	}

	gen := a.updater.installGen
	acted := a.abandonWSLInstallLocked(gen)
	a.updater.mu.Unlock()
	if acted {
		a.emitWSLInstallFailure(launcherFailureMessage(message))
	}
	return nil
}

// armWSLInstallDeadlineLocked puts the in-flight install under a deadline,
// replacing whatever deadline it was under before. Caller holds a.updater.mu.
//
// One timer field, not one per phase: the acknowledgement deadline and the
// post-acknowledgement silence backstop are the same thing at different points
// in the same install, so a phase change structurally cannot leave the previous
// phase's timer armed, and settleWSLInstallLocked has exactly one timer to stop.
//
// The generation bump lives here, not at the call sites, so every armed
// deadline carries a token no earlier deadline shares. Stop below is not
// enough on its own: a callback that has already fired is past stopping —
// merely parked on a.updater.mu — and with a shared generation it would pass the
// guard after the phase change that replaced it, unwinding an install the
// launcher had just been told to proceed with (marker gone, error emitted,
// swap continuing anyway). With the bump, the replaced callback finds the
// generation moved on and stands down.
func (a *App) armWSLInstallDeadlineLocked(after time.Duration, message string) {
	if a.updater.installTimer != nil {
		a.updater.installTimer.Stop()
	}
	a.updater.installGen++
	gen := a.updater.installGen
	a.updater.installTimer = time.AfterFunc(after, func() { a.failWSLInstallOnDeadline(gen, message) })
}

// failWSLInstallOnDeadline is the timer entry point for both install deadlines.
//
// It stands down once shutdown has begun. The backstop's whole subject is a
// process that is supposed to be dying, so it will routinely still be armed
// while the launcher's quit unwinds — and unwinding there would delete the
// marker that is precisely what the next boot needs to notice a swap that never
// applied, then emit a terminal error into a bus that is being torn down. A
// deadline expiring into a teardown has nothing useful to say.
func (a *App) failWSLInstallOnDeadline(gen uint64, message string) {
	if a.shuttingDown.Load() {
		log.Printf("updater: install deadline expired during shutdown; leaving the marker for the next boot to judge")
		return
	}
	a.failWSLInstall(gen, message)
}

// failWSLInstall unwinds an install handoff that will not happen. Used by both
// install deadlines; the launcher's own failure report takes the same two steps
// while already holding the lock.
func (a *App) failWSLInstall(gen uint64, message string) {
	a.updater.mu.Lock()
	acted := a.abandonWSLInstallLocked(gen)
	a.updater.mu.Unlock()
	if acted {
		a.emitWSLInstallFailure(message)
	}
}

// abandonWSLInstallLocked releases the in-flight install, drops its marker,
// and lifts the busy fence, reporting whether it was the one to do so. Caller
// holds a.updater.mu.
//
// gen is what makes the unwind idempotent across the races that matter: a
// deadline firing at the same moment the report it was waiting for arrives,
// and the sharper shapes where the fired callback loses a.updater.mu to a phase
// change (a "proceeding" re-arming the backstop) or to a report plus a
// subsequent RestartToUpdate. Every armed deadline gets its own generation
// (armWSLInstallDeadlineLocked owns the bump), so a replaced or settled
// deadline's callback finds the generation moved on and stands down instead of
// cancelling an install it no longer speaks for. The launcher's failure report
// reads the CURRENT generation under this same lock, so it always speaks for
// the install in flight.
//
// The marker drops here — under the lock, BEFORE the fence lifts — because the
// moment a.updater.busy is false a waiting RestartToUpdate can claim the fence and
// write a fresh marker; cleanup deferred past the unlock would delete that new
// install's marker and leave its swap invisible to the next boot. SaveMarker
// already runs under this lock, so marker I/O being a locked operation is the
// established shape, not a new cost.
//
// a.updater.staged is deliberately left alone: the artifact really is still staged
// on the Windows side, so a retry has something to hand over, and the next
// download sweeps it before staging its own.
func (a *App) abandonWSLInstallLocked(gen uint64) bool {
	if a.updater.install == nil || a.updater.installGen != gen {
		return false
	}
	a.settleWSLInstallLocked()
	if err := selfupdate.ClearMarker(a.updater.wsl.markerDir); err != nil {
		log.Printf("updater: clear update marker after a failed install: %v", err)
	}
	a.updater.busy = false
	return true
}

// settleWSLInstallLocked returns the install state to rest: no install in
// flight, no phase, no deadline armed. Caller holds a.updater.mu.
func (a *App) settleWSLInstallLocked() {
	if a.updater.installTimer != nil {
		a.updater.installTimer.Stop()
		a.updater.installTimer = nil
	}
	a.updater.install = nil
	a.updater.installAcked = false
}

// emitWSLInstallFailure is the unlocked tail of an abandoned install: the log
// line and the UI's terminal event. All state — the in-flight install, its
// marker, the busy fence — was already settled under the lock by
// abandonWSLInstallLocked; only the emit waits for the unlock, so it can never
// interleave with a new install claiming the fence.
func (a *App) emitWSLInstallFailure(message string) {
	log.Printf("updater: install handoff failed: %s", message)
	a.emit(updaterErrorChannel, updater.ErrorInfo{
		Stage:    updater.StageInstall,
		Message:  message,
		Provider: selfupdate.ProviderName,
	})
}

// launcherFailureMessage normalizes what the launcher said into something the
// UI can show. The launcher is a separate process whose message we do not
// control, so an empty one still has to read as a reason.
func launcherFailureMessage(message string) string {
	if message == "" {
		return "The Windows launcher could not install the update."
	}
	return message
}

// releaseIdentity extracts the three things the cross-process install contract
// needs from a resolved release, and refuses anything the launcher would reject
// anyway — validation runs through InstallDirective.Validate, so what gets
// staged is exactly what the far side will accept.
//
// Fail-closed on a missing digest: StageCopy's verification is the only
// integrity gate on the /mnt/c hop, and a release with nothing to verify
// against must never reach the staging directory. (verifiedProvider already
// rejects such releases at the provider boundary; this is the same rule
// enforced where the bytes actually cross.)
func releaseIdentity(rel *updater.Release) (filename, version string, digest []byte, err error) {
	if rel == nil {
		return "", "", nil, errors.New("updater: no resolved release identity to stage against")
	}
	if rel.Verification == nil || len(rel.Verification.Digest) == 0 {
		return "", "", nil, fmt.Errorf("updater: release %s carries no checksum to stage against", rel.Version)
	}
	directive := selfupdate.InstallDirective{
		Filename: rel.Artifact.Filename,
		SHA256:   hex.EncodeToString(rel.Verification.Digest),
		Version:  rel.Version,
	}
	if err := directive.Validate(); err != nil {
		return "", "", nil, err
	}
	return directive.Filename, directive.Version, rel.Verification.Digest, nil
}

// wslUpdaterHost is the updater.Host for the WSL backend. There is no Wails
// application here to route events through, so the host bridges straight onto
// the transport event bus.
type wslUpdaterHost struct{ app *App }

// Emit forwards a lifecycle event onto the channel the frontend subscribes to.
// EventUpdateReady is the one exception: on this path "ready" means the
// artifact reached the WINDOWS side, which has not happened yet when the
// framework fires it (it only knows the file is staged inside the distro).
// Forwarding it would offer the user a Restart button for bytes the launcher
// cannot see. stageWSLUpdate emits the real one after StageCopy.
func (h wslUpdaterHost) Emit(name string, data ...any) bool {
	if name == updater.EventUpdateReady {
		return true
	}
	channel, ok := updaterEventBridge[name]
	if !ok {
		// Not a failure — the framework emits several events we deliberately
		// don't bridge (check-started, update-available, no-update, meta), all
		// of them redundant with the CheckForUpdate RPC's return value.
		log.Printf("updater: unbridged event %s", name)
		return true
	}
	var payload any
	if len(data) > 0 {
		payload = data[0]
	}
	h.app.emit(channel, payload)
	return true
}

// OnEvent is unreachable in this configuration, verified against
// pkg/updater: the only caller is openSession, whose only callers are
// CheckAndInstall (which this backend never invokes — DownloadUpdate drives
// Check + DownloadAndInstall directly) and the periodic-check loop, which Init
// only starts when Config.CheckInterval > 0. Config leaves it zero here.
//
// It still returns a working remover rather than nil so that a future caller
// gets a no-op listener and a valid cancel, not a nil dereference. Nothing in
// this process ever emits the wails:updater:user:* events those listeners would
// wait on.
func (h wslUpdaterHost) OnEvent(string, func(any)) func() { return func() {} }

// OpenWindow cannot be reached under updater.WindowNone (classifyWindowOption
// routes to windowModeNone, which never calls it) and there is no display
// server inside the distro to open one on. Logs loudly and returns nil rather
// than half-succeeding, so a framework change that starts calling it shows up
// in the log instead of silently opening nothing.
func (h wslUpdaterHost) OpenWindow(updater.WindowOptions) updater.WindowHandle {
	log.Printf("updater: BUG — the WSL host was asked to open an update window; there is no display server here")
	return nil
}

// Quit is never expected: the WSL path hands the swap to the launcher and never
// calls Updater.Restart, which is the framework's only caller. If it ever fires,
// something is driving the updater's built-in restart path — which would spawn a
// swap helper against the wrong executable — so say so loudly and do nothing.
func (h wslUpdaterHost) Quit() {
	log.Printf("updater: BUG — the WSL host was asked to quit; the Windows launcher owns the swap and this process must not self-restart")
}

var _ updater.Host = wslUpdaterHost{}
