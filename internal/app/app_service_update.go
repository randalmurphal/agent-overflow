package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/appupdate"
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/supervise"
)

// The remote update trigger (docs/specs/remote-access.md §7, "Headless serve
// mode and remote update"; operator's copy: docs/architecture/serve-mode.md).
//
// Before this, a supervised serve host could only be updated by somebody at
// the machine: `agent-overflow service update`. That is the wrong requirement
// for a backend whose whole point is being somewhere else. This file is the
// owner's path to the same outcome from any client: pick a release, and the
// backend resolves it, downloads it verified, preflights it, stages it into
// the supervisor's layout, and asks the supervisor to run it. From there W8h1
// owns the rest — trial boot, commit or roll back — and the version that comes
// back publishes `service:update-outcome`.
//
// The ORDER is the safety property, and it is the same order the local command
// uses (internal/aocli's serviceUpdate):
//
//	resolving -> downloading -> verifying -> staging -> requested
//
// Verify BEFORE stage, always. Two different refusals live in that step and
// both have to happen while the artifact is still a temp file: a download that
// is not an Agent Overflow binary this host can run, and one that speaks an
// update protocol the installed supervisor does not. A staged version
// directory is immutable and is what `acceptUpdate` selects from, so putting
// either question after the staging would mean writing down a version the
// supervisor then has to refuse.
//
// The VERSION staged is the preflight's answer, not the tag's. A tag is what
// the release feed calls a build; the binary's own `__service-preflight` is
// what it calls itself, and the version directory has to be named for the
// second or a rollback would return to a directory holding something else.
//
// Nothing here is configured on a desktop boot or an unsupervised `serve`:
// ConfigureServiceUpdates runs only from a supervised serve host that can
// fetch its own artifact, and every RPC below answers honestly when it did
// not.

// ServiceUpdateDeps is what package main hands this seam at boot. Narrow
// functions and one value, so nothing here reaches for a supervisor channel,
// a release feed or an executable of its own.
type ServiceUpdateDeps struct {
	// Source resolves and downloads releases for THIS host's artifact. nil
	// means this host cannot fetch releases at all — no release artifact it
	// could install as a single file — which the status RPC says in words.
	Source *appupdate.ReleaseSource
	// Layout is the supervisor's directory tree: where a version is staged,
	// and the filesystem the download must land on so the stage is a local
	// copy rather than a cross-device move.
	Layout supervise.Layout
	// Preflight asks a downloaded file what it is, in its own process.
	// Injected rather than called directly so a test can describe a binary
	// instead of executing one; production passes supervise.PreflightBinary.
	Preflight func(ctx context.Context, binary string) (supervise.Preflight, error)
	// Log receives one line per flow transition. nil is silent, which is what
	// a test wants.
	Log func(format string, args ...any)
}

// serviceUpdateState is the App's half: the supervisor callback a supervised
// boot installs, plus the deps and the live flow this file owns. One mutex
// over all of it, because every field is read by the status RPC and written
// by the flow, and two locks would be two answers to "what is happening".
type serviceUpdateState struct {
	mu sync.Mutex
	// request asks the supervisor to run an already-staged version. Installed
	// by SetServiceUpdateRequester from a supervised `serve` boot and nil
	// everywhere else, which is what makes "this install has no supervisor" an
	// answer rather than a nil dereference.
	request func(target string) (string, error)
	// deps is what ConfigureServiceUpdates installed, zero when it did not run.
	deps ServiceUpdateDeps
	// busy is the one-flow-at-a-time fence. Claimed by RequestServiceUpdate
	// under mu and dropped by the goroutine that finishes.
	busy bool
	// status is the last published frame, minus the fields derived per read.
	status ServiceUpdateStatus
}

// The phases a flow moves through, in order. Strings on the wire because the
// client renders them and a number would be a lookup table in two languages.
const (
	serviceUpdatePhaseIdle        = "idle"
	serviceUpdatePhaseResolving   = "resolving"
	serviceUpdatePhaseDownloading = "downloading"
	serviceUpdatePhaseVerifying   = "verifying"
	serviceUpdatePhaseStaging     = "staging"
	serviceUpdatePhaseRequested   = "requested"
	serviceUpdatePhaseError       = "error"
)

// serviceUpdateProgressInterval throttles the download's status frames. The
// artifact is tens of MB and the provider ticks per write, so an unthrottled
// flow would publish thousands of frames onto a latest-only ring nobody reads
// more than four times a second. The terminal frame for each phase is
// published unconditionally, so the throttle can never swallow the last one.
const serviceUpdateProgressInterval = 250 * time.Millisecond

// serviceUpdateCheckTimeout bounds the metadata round trips: the boot-time
// passive check and the picker's ListServiceReleases. It is the release
// LISTING, not a download, so a host that cannot reach GitHub finds out in
// seconds instead of holding an RPC open.
const serviceUpdateCheckTimeout = 30 * time.Second

// serviceUpdateFlowTimeout bounds the whole resolve + download + verify +
// stage. Generous, because the artifact is tens of MB over whatever link the
// host has; bounded, because a stalled transfer must not hold the one-flow
// fence forever on a machine nobody can walk up to.
const serviceUpdateFlowTimeout = 20 * time.Minute

// ErrServiceUpdateBusy refuses a second flow while one is running. The
// supervisor accepts one update at a time and so does this: two downloads
// racing for one staging layout is a corrupted version directory with a
// friendly name.
var ErrServiceUpdateBusy = errors.New("app: this backend is already installing an update")

// ErrServiceUpdateUnavailable refuses a flow on a host with no release source.
// Distinct from errNoSupervisor: that one says nothing can apply an update
// here, this one says nothing can FETCH one, and the remedies differ.
var ErrServiceUpdateUnavailable = errors.New(
	"app: this backend cannot download releases for itself, so update it at the machine " +
		"with `agent-overflow service update`")

// ErrServiceUpdateAlreadyRunning refuses a request for the version already
// running. The supervisor would refuse it too, one trial and one database
// snapshot later; refusing it here costs nothing and says why.
//
// Asked TWICE, against two different facts. Before the download, against the
// tag, which is all a caller has. And after the preflight, against the
// version the downloaded binary reports of itself, which is the name its
// directory would take and the only one that can collide with a directory
// that already holds something.
var ErrServiceUpdateAlreadyRunning = errors.New("app: that version is the one already running")

// ErrServiceUpdateTrial refuses a request made of a TRIAL boot. A trial is a
// supervisor already mid-update: it accepts one at a time, so the request
// would be refused after the download and the stage, both wasted. The gate
// being parked is the fact this process has about being a trial.
var ErrServiceUpdateTrial = errors.New(
	"app: this backend is a trial of an update that has not committed yet, so wait for that one to settle")

// ServiceUpdateStatus is everything a client needs to render the update
// surface for one supervised machine. Flat and additive like every other wire
// shape here.
type ServiceUpdateStatus struct {
	// Supervised is whether a supervisor is attached, so the trigger can work
	// at all. False on a desktop install and on a `serve` started by hand.
	Supervised bool `json:"supervised"`
	// Available is Supervised AND a release source exists for this host.
	Available bool `json:"available"`
	// CurrentVersion is this build's version. Always answered, including on a
	// host that can do nothing else here.
	CurrentVersion string `json:"currentVersion"`
	// LatestVersion is the boot-time passive check's answer, empty when this
	// host is up to date, when the check failed, or when it never ran.
	LatestVersion string `json:"latestVersion,omitempty"`
	// LatestTag is that release's tag, which is what RequestServiceUpdate
	// takes.
	LatestTag string `json:"latestTag,omitempty"`
	// Phase is where the current or last flow got to: idle, resolving,
	// downloading, verifying, staging, requested, error.
	Phase string `json:"phase"`
	// TargetTag is the tag the current or last flow was asked for.
	TargetTag string `json:"targetTag,omitempty"`
	// TargetVersion is what that tag resolved to, once known.
	TargetVersion string `json:"targetVersion,omitempty"`
	// UpdateID is the supervisor's id for the accepted update, set at phase
	// `requested`. It is what the client correlates the service:update-outcome
	// frame against after the backend restarts.
	UpdateID string `json:"updateId,omitempty"`
	// Written and Total are the download's progress in bytes. Total is zero
	// when the server sent no length.
	Written int64 `json:"written,omitempty"`
	Total   int64 `json:"total,omitempty"`
	// Error is the last flow's failure, naming the step it failed at. Cleared
	// when a new flow starts.
	Error string `json:"error,omitempty"`
	// Unavailable says why Available is false on a supervised host, in a
	// sentence. Empty otherwise.
	Unavailable string `json:"unavailable,omitempty"`
}

// ConfigureServiceUpdates installs the remote update seam and starts the one
// passive release check this process makes.
//
// Bootstrap-boundary function rather than a method, for the reason every input
// in bootstrap.go is one: an exported method on App is a wire RPC by
// construction, and a caller that could point this backend's updater at a
// release feed of its choosing over the wire is not a thing to ship.
//
// Called only from a supervised `serve` boot whose host has an artifact it can
// install (main_serve.go). Every other boot leaves this unconfigured, and the
// RPCs below answer accordingly.
func ConfigureServiceUpdates(a *App, deps ServiceUpdateDeps) {
	if deps.Log == nil {
		deps.Log = func(string, ...any) {}
	}
	a.serviceUpdate.mu.Lock()
	a.serviceUpdate.deps = deps
	if a.serviceUpdate.status.Phase == "" {
		a.serviceUpdate.status.Phase = serviceUpdatePhaseIdle
	}
	a.serviceUpdate.mu.Unlock()

	if deps.Source == nil {
		return
	}
	// The passive check: ONE goroutine, one call, never retried. It is
	// deliberately NOT in the activation gate's parked set, and the membership
	// rule is why — the gate parks work a database restore could not undo, and
	// a read of a public release list undoes itself by ending. A trial that
	// learns there is a newer version has done nothing a rollback has to
	// reverse. Parking it would instead mean a trial's status surface reported
	// no known release for as long as the trial ran, which is a worse answer
	// for no property gained.
	go func() {
		ctx, cancel := context.WithTimeout(a.lifeCtx(), serviceUpdateCheckTimeout)
		defer cancel()
		latest, err := deps.Source.Latest(ctx)
		if err != nil {
			// Logged and dropped. A host that cannot reach the release feed at
			// boot is a host whose owner can still ask for a specific version
			// later, and a retry loop on an unattended machine is a network
			// call nobody asked for repeating forever.
			deps.Log("serve: could not check for a newer release: %v", err)
			return
		}
		if latest == nil {
			return
		}
		a.serviceUpdate.mu.Lock()
		a.serviceUpdate.status.LatestVersion = latest.Version
		a.serviceUpdate.status.LatestTag = latest.Tag
		status := a.serviceUpdateStatusLocked()
		a.serviceUpdate.mu.Unlock()
		deps.Log("serve: version %s is available (this backend is %s)", latest.Version, a.version)
		a.emit(eventchan.ServiceUpdateStatus, status)
	}()
}

// serviceUpdateStatusLocked builds the wire shape from the state plus the
// facts derived per read. Caller holds a.serviceUpdate.mu.
func (a *App) serviceUpdateStatusLocked() ServiceUpdateStatus {
	status := a.serviceUpdate.status
	status.CurrentVersion = a.version
	status.Supervised = a.serviceUpdate.request != nil
	status.Available = status.Supervised && a.serviceUpdate.deps.Source != nil
	if status.Phase == "" {
		status.Phase = serviceUpdatePhaseIdle
	}
	if status.Supervised && !status.Available {
		status.Unavailable = ErrServiceUpdateUnavailable.Error()
	}
	return status
}

// publishServiceUpdate records a mutation to the flow's state and publishes
// the resulting frame. mutate runs under the lock; the emit happens after it
// is released, so a subscriber's work never runs inside the fence a concurrent
// status read is waiting on.
func (a *App) publishServiceUpdate(mutate func(status *ServiceUpdateStatus)) {
	a.serviceUpdate.mu.Lock()
	mutate(&a.serviceUpdate.status)
	status := a.serviceUpdateStatusLocked()
	a.serviceUpdate.mu.Unlock()
	a.emit(eventchan.ServiceUpdateStatus, status)
}

// GetServiceUpdateStatus reports what this backend knows about updating
// itself. It NEVER touches the network: it is the read a client polls, the
// read the push channel mirrors, and a read that could block on GitHub would
// make the update surface unopenable on an offline host.
//
// Off a supervised host it answers Supervised:false and the current version,
// with no error — the client hides the surface rather than showing a failure
// for a machine that simply is not supervised.
//
// `access:admin` rather than `host`: choosing which build a machine runs is
// what a paired admin device is for, and `host` would leave the surface
// readable only from the machine this feature exists to save a trip to. It
// discloses this build's version, a release tag, a phase word and a byte
// count, all of which the same device could read from the release feed
// itself.
//
//ao:scope access:admin
//ao:route selected
func (a *App) GetServiceUpdateStatus() (ServiceUpdateStatus, error) {
	a.serviceUpdate.mu.Lock()
	defer a.serviceUpdate.mu.Unlock()
	return a.serviceUpdateStatusLocked(), nil
}

// ListServiceReleases returns the releases this host could install, newest
// first, for the version picker.
//
// A network read, and the only one on this surface a client drives: it runs
// when the picker opens, bounded by the check timeout so an unreachable feed
// fails in seconds. `access:admin` for the same reason
// GetServiceUpdateStatus is, and its answer is the public release list.
//
//ao:scope access:admin
//ao:route selected
func (a *App) ListServiceReleases() ([]ReleaseSummary, error) {
	a.serviceUpdate.mu.Lock()
	source := a.serviceUpdate.deps.Source
	a.serviceUpdate.mu.Unlock()
	if source == nil {
		return nil, ErrServiceUpdateUnavailable
	}
	ctx, cancel := context.WithTimeout(a.lifeCtx(), serviceUpdateCheckTimeout)
	defer cancel()
	releases, err := source.List(ctx)
	if err != nil {
		return nil, err
	}
	return wireReleaseSummaries(releases), nil
}

// RequestServiceUpdate downloads a release, proves it runs here, stages it,
// and asks this backend's supervisor to boot it.
//
// It returns as soon as the flow is claimed. The work is a multi-minute
// download and the process is about to be stopped by its own supervisor, so
// binding it to an RPC's lifetime would mean a client that switched networks
// mid-download cancelled its own update. The client follows
// `service:update-status` instead, and correlates the restart through
// `service:update-outcome`.
//
// `//ao:stepup`: this replaces the code the machine runs. It is the same class
// of act as minting a pairing link or rebinding the listener, and §4 puts
// those behind a fresh per-call proof — host presence, or a passkey assertion
// this backend verified moments ago.
//
// `access:admin` rather than `host`, deliberately. `host` would make this
// callable only from the machine, which is exactly `agent-overflow service
// update`, and would leave the wave with no remote trigger at all; the spec
// (§7) asks for one that "requires step-up plus artifact verification", which
// is what this is. The download is verified against the published checksum and
// preflighted before anything is staged.
//
//ao:scope access:admin
//ao:stepup
//ao:route selected
func (a *App) RequestServiceUpdate(ctx context.Context, tag string) error {
	if err := a.requireStepUp(ctx, "installing a different version of this backend"); err != nil {
		return err
	}
	if a.shuttingDown.Load() {
		return ErrShuttingDown
	}
	if a.activation.Parked() {
		return ErrServiceUpdateTrial
	}
	tag = strings.TrimSpace(tag)
	if !appupdate.ValidReleaseTag(tag) {
		return fmt.Errorf("%w: %q", ErrInvalidReleaseTag, tag)
	}
	// The version already running is refused HERE rather than by the
	// supervisor, which would refuse it too — one stop, one snapshot and one
	// trial later. Compared on the tag's version half because that is all the
	// caller has before the resolve.
	if running := strings.TrimPrefix(tag, "v"); running != "" && running == a.version {
		return ErrServiceUpdateAlreadyRunning
	}

	a.serviceUpdate.mu.Lock()
	if a.serviceUpdate.request == nil {
		a.serviceUpdate.mu.Unlock()
		return errNoSupervisor
	}
	deps := a.serviceUpdate.deps
	if deps.Source == nil {
		a.serviceUpdate.mu.Unlock()
		return ErrServiceUpdateUnavailable
	}
	if a.serviceUpdate.busy {
		a.serviceUpdate.mu.Unlock()
		return ErrServiceUpdateBusy
	}
	a.serviceUpdate.busy = true
	// The whole previous flow's record is replaced, not patched: a stale
	// target version or a stale error beside a fresh phase is a client
	// rendering two updates at once.
	a.serviceUpdate.status = ServiceUpdateStatus{
		LatestVersion: a.serviceUpdate.status.LatestVersion,
		LatestTag:     a.serviceUpdate.status.LatestTag,
		Phase:         serviceUpdatePhaseResolving,
		TargetTag:     tag,
	}
	status := a.serviceUpdateStatusLocked()
	a.serviceUpdate.mu.Unlock()
	a.emit(eventchan.ServiceUpdateStatus, status)

	go a.runServiceUpdate(deps, tag)
	return nil
}

// runServiceUpdate is the flow. It runs on a.lifeCtx so a shutdown ends it,
// and it drops the fence on every exit path.
func (a *App) runServiceUpdate(deps ServiceUpdateDeps, tag string) {
	defer func() {
		a.serviceUpdate.mu.Lock()
		a.serviceUpdate.busy = false
		a.serviceUpdate.mu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(a.lifeCtx(), serviceUpdateFlowTimeout)
	defer cancel()

	if err := a.serviceUpdateFlow(ctx, deps, tag); err != nil {
		deps.Log("serve: update to %s failed: %v", tag, err)
		a.publishServiceUpdate(func(status *ServiceUpdateStatus) {
			status.Phase = serviceUpdatePhaseError
			status.Error = err.Error()
		})
	}
}

// serviceUpdateFlow is the ordered work, with every failure returning a
// sentence that names the step it failed at. The supervisor is untouched
// until the last one.
func (a *App) serviceUpdateFlow(ctx context.Context, deps ServiceUpdateDeps, tag string) error {
	// The download lands beside the version directories it is about to become
	// one of, so the stage is a local copy on one filesystem rather than a
	// cross-device move that could tear. 0700 on the directory and the file:
	// an unattended host's data root is the owner's, and an executable nobody
	// has vouched for yet is not something to leave group-readable.
	if err := os.MkdirAll(deps.Layout.Root(), 0o700); err != nil {
		return fmt.Errorf("preparing the download directory: %w", err)
	}
	file, err := os.CreateTemp(deps.Layout.Root(), "download-*")
	if err != nil {
		return fmt.Errorf("preparing the download: %w", err)
	}
	downloaded := file.Name()
	// Removed on EVERY exit path, success included: once StageBinary has
	// copied it into its version directory this file is a duplicate of an
	// immutable one, and a failed flow must leave nothing behind that a later
	// one could mistake for an artifact.
	defer func() {
		if err := os.Remove(downloaded); err != nil && !errors.Is(err, os.ErrNotExist) {
			deps.Log("serve: could not remove the downloaded update %s: %v", downloaded, err)
		}
	}()
	if err := file.Chmod(0o700); err != nil {
		file.Close()
		return fmt.Errorf("preparing the download: %w", err)
	}

	a.publishServiceUpdate(func(status *ServiceUpdateStatus) {
		status.Phase = serviceUpdatePhaseDownloading
	})
	resolved, err := deps.Source.Fetch(ctx, tag, file, a.serviceUpdateProgress())
	if closeErr := file.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	if err != nil {
		// Both the resolve and the checksum comparison land here, and both
		// read the same way to the person looking at the screen: the release
		// they asked for is not something this host installed.
		return fmt.Errorf("downloading %s: %w", tag, err)
	}

	// Verify: the checksum was already compared inside Fetch, so what this
	// phase adds is the question only the file itself can answer. Running it
	// BEFORE the stage is the ordering rule — an artifact that is not an Agent
	// Overflow binary this host can run, or one that speaks an update protocol
	// the installed supervisor does not, is refused while it is still a temp
	// file rather than after a version directory exists for it.
	a.publishServiceUpdate(func(status *ServiceUpdateStatus) {
		status.Phase = serviceUpdatePhaseVerifying
		status.TargetVersion = resolved.Version
	})
	answer, err := deps.Preflight(ctx, downloaded)
	if err != nil {
		return fmt.Errorf("checking the downloaded %s: %w", resolved.AssetName, err)
	}
	if err := supervise.CheckPreflight(answer); err != nil {
		return err
	}
	// The version STAGED is the binary's own answer, not the tag's. A version
	// directory is named for what is inside it, or a rollback returns to a
	// directory holding something else.
	if err := supervise.ValidVersion(answer.Version); err != nil {
		return fmt.Errorf("the downloaded %s reports version %q, which cannot name a directory: %w",
			resolved.AssetName, answer.Version, err)
	}
	// The version a binary REPORTS is not the version its tag promised, and
	// only the first one names a directory. A download whose own answer is
	// the version already running would be staged straight over that
	// version's directory, because a version directory is replaced on the
	// premise that a version names one build. The supervisor would then
	// refuse the update as already running, one download too late, leaving
	// versions/<running> holding a different build than its name asserts and
	// a later rollback returning to it. Refuse here, before anything is
	// written, and name both halves so the mismatch is readable.
	if answer.Version == a.version {
		return fmt.Errorf(
			"%w: %s is tagged %s but reports version %s, which is the version already running",
			ErrServiceUpdateAlreadyRunning, resolved.AssetName, resolved.Tag, answer.Version)
	}

	a.publishServiceUpdate(func(status *ServiceUpdateStatus) {
		status.Phase = serviceUpdatePhaseStaging
		status.TargetVersion = answer.Version
	})
	if err := supervise.StageBinary(deps.Layout, answer.Version, downloaded); err != nil {
		return fmt.Errorf("staging version %s: %w", answer.Version, err)
	}

	// The supervisor's turn. Everything after this belongs to W8h1: it
	// accepts, stops this process, snapshots the database, and boots the
	// target as a trial.
	updateID, err := a.serviceUpdateRequest(answer.Version)
	if err != nil {
		return fmt.Errorf("asking the supervisor to run version %s: %w", answer.Version, err)
	}
	deps.Log("serve: update %s accepted; the supervisor is switching to version %s", updateID, answer.Version)
	a.publishServiceUpdate(func(status *ServiceUpdateStatus) {
		status.Phase = serviceUpdatePhaseRequested
		status.TargetVersion = answer.Version
		status.UpdateID = updateID
	})
	// And the status STAYS here. This process is about to be stopped by its
	// supervisor, which is the expected end of the flow, and a client that
	// polls in the seconds before that happens needs to see which update to
	// wait for rather than an idle surface.
	return nil
}

// serviceUpdateProgress returns the download's progress callback, throttled.
// The closure owns its last-emit stamp, so two flows could never share one
// (they cannot overlap either, but a timestamp on the App would outlive the
// flow that set it and delay the next one's first frame).
func (a *App) serviceUpdateProgress() func(written, total int64) {
	last := time.Time{}
	return func(written, total int64) {
		now := time.Now()
		if now.Sub(last) < serviceUpdateProgressInterval {
			return
		}
		last = now
		a.publishServiceUpdate(func(status *ServiceUpdateStatus) {
			status.Written = written
			status.Total = total
		})
	}
}
