package app

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/providerstatus"
)

// providerBinaryWatchInterval is how often the watcher stats the provider
// binaries. 60s is a background housekeeping cadence, not a live signal:
// the thing it detects (`npm i -g`, `brew upgrade`, a CLI self-update)
// takes longer than that to run, and the user's next act is a send, not a
// stopwatch. A quiet tick is two stats and nothing else — the version
// subprocess runs only when the bytes on disk actually changed.
const providerBinaryWatchInterval = 60 * time.Second

// providerBinaryDetectFn is the version-probe seam. Production leaves it at
// provider.DetectProvider, which spawns `<binary> --version`; watcher tests
// install a fake so a test binary can never resolve a real provider CLI
// (root AGENTS.md §Permanent invariants).
var providerBinaryDetectFn = provider.DetectProvider

// appProviderBinaryWatchState is the watcher's whole memory: what each
// provider binary looked like the last time its version was successfully
// read, and which live threads are currently flagged stale because of it.
//
// It is process-local by nature — a stale session is a fact about a running
// subprocess, so it dies with the process and is never persisted.
type appProviderBinaryWatchState struct {
	mu sync.Mutex
	// installed is keyed by provider name. An entry exists only for a
	// binary whose version was read successfully; a failed read leaves the
	// previous entry (or none) in place so the next tick retries.
	installed map[string]providerBinaryVersion
	// stale is keyed by thread id, holding the two versions the banner
	// shows. GetThreadLiveState reads it so a reconnecting webview
	// converges without waiting for the next push.
	stale map[string]staleProviderBinary
}

// providerBinaryIdentity is "the bytes we last looked at", cheap enough to
// re-derive every tick. Path is included because a version manager switch
// can repoint the symlink at an equally-old file with an equally-old mtime.
// Comparable by ==, which is the whole point of the tick; mtime is carried
// as unix nanos rather than a time.Time so that comparison is a plain value
// compare and not time.Time's wall/monotonic/location equality.
type providerBinaryIdentity struct {
	path        string
	size        int64
	modUnixNano int64
}

type providerBinaryVersion struct {
	identity providerBinaryIdentity
	// version is the parsed token of the last SUCCESSFUL probe, which is
	// what a later probe is compared against.
	version string
}

// staleProviderBinary is one flagged thread's two versions, in the wire
// spelling the banner and GetThreadLiveState both use, plus the provider
// that owns them — the clear event needs it after the session is gone.
type staleProviderBinary struct {
	provider  string
	session   string
	installed string
	// token pins the flag to the session that earned it. A hydration read
	// answers only while that exact session is still live, so a restart or
	// reap that lands between two ticks cannot resurrect a withdrawn banner.
	token string
}

// startProviderBinaryWatcher runs the provider-binary upgrade watcher.
// Starts at app boot, exits when lifeCtx is cancelled (Shutdown step 1b).
// It never mutates the session map — it only reads a snapshot of it and
// emits — so unlike the idle reaper it needs no stop channel or WaitGroup.
//
// The first tick is at t = providerBinaryWatchInterval, not t=0: nothing can
// have been upgraded in the first minute of a process that just resolved
// those binaries, and boot already has enough subprocesses in flight.
func (a *App) startProviderBinaryWatcher() {
	ctx := a.lifeCtx()
	go func() {
		ticker := time.NewTicker(providerBinaryWatchInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				a.sweepProviderBinaries()
			}
		}
	}()
}

// sweepProviderBinaries is one tick: refresh what each provider binary
// reports about itself, then reconcile every live session against it.
func (a *App) sweepProviderBinaries() {
	if a.shuttingDown.Load() || a.settings == nil {
		return
	}
	for _, providerName := range []string{string(provider.Claude), string(provider.Codex)} {
		a.refreshInstalledProviderVersion(providerName)
	}
	a.reconcileStaleProviderSessions()
}

// refreshInstalledProviderVersion re-reads one provider binary's version if
// and only if the file behind it changed. An in-place upgrade keeps the
// configured path AND the resolved path (npm rewrites the file the shim
// points at), which is why identity is (resolved path, size, mtime) and not
// the path alone.
//
// Every failure leaves the previous entry untouched, so the next tick sees
// a mismatched identity and retries. That is what makes a torn read — a
// half-written binary caught mid-upgrade — self-healing instead of a
// permanently wrong "installed version".
func (a *App) refreshInstalledProviderVersion(providerName string) {
	identity, ok := a.resolveProviderBinaryIdentity(providerName)
	if !ok {
		return
	}
	previous, known := a.providerBinaries.lookupInstalled(providerName)
	if known && previous.identity == identity {
		return
	}

	configured := a.providerBinaryPath(providerName)
	status := providerBinaryDetectFn(providerName, configured)
	version := providerstatus.VersionToken(status.Version)
	if version == "" {
		// Not a version we can compare anything against. Report it and
		// retry next tick rather than recording a blank as truth.
		log.Printf(
			"provider binary watch: %s at %s reported no usable version (status %q: %s)",
			providerName, identity.path, status.Status, status.Message,
		)
		return
	}
	if !known {
		// Baseline. Nothing to compare against yet: this is what the
		// binary was when the app started watching it.
		a.providerBinaries.storeInstalled(providerName, providerBinaryVersion{identity: identity, version: version})
		return
	}
	if version == previous.version {
		// The bytes moved but the version did not (a reinstall of the same
		// release, a touched mtime). Record the new identity so the next
		// tick is a stat again.
		a.providerBinaries.storeInstalled(providerName, providerBinaryVersion{identity: identity, version: version})
		return
	}

	if err := a.refreshProviderCatalogAfterUpgrade(providerName); err != nil {
		// The catalog is still describing the old binary. Leaving the old
		// entry in place means the next tick re-probes and retries the
		// refresh; committing it here would strand the model picker on a
		// version that is gone.
		log.Printf("provider binary watch: %s %s -> %s: refresh catalog: %v",
			providerName, previous.version, version, err)
		return
	}
	log.Printf("provider binary watch: %s upgraded %s -> %s; model catalog refreshed",
		providerName, previous.version, version)
	a.providerBinaries.storeInstalled(providerName, providerBinaryVersion{identity: identity, version: version})
}

// resolveProviderBinaryIdentity stats the binary a provider would spawn.
// Stat only — a tick that finds nothing changed must cost no subprocess.
//
// A path that does not resolve is not an error here: an uninstalled provider
// is a normal state the startup detect probe already reports through the
// banner, and this watcher has nothing to say about it.
func (a *App) resolveProviderBinaryIdentity(providerName string) (providerBinaryIdentity, bool) {
	configured := a.providerBinaryPath(providerName)
	if configured == "" {
		return providerBinaryIdentity{}, false
	}
	resolved, err := exec.LookPath(configured)
	if err != nil {
		return providerBinaryIdentity{}, false
	}
	// A version manager (nvm, volta, mise) puts a symlink chain in front of
	// the real file, and an upgrade repoints the chain without touching the
	// shim. Follow it so size/mtime describe the file that will actually run.
	if target, err := filepath.EvalSymlinks(resolved); err == nil {
		resolved = target
	}
	info, err := os.Stat(resolved)
	if err != nil {
		log.Printf("provider binary watch: stat %s binary %s: %v", providerName, resolved, err)
		return providerBinaryIdentity{}, false
	}
	return providerBinaryIdentity{
		path:        resolved,
		size:        info.Size(),
		modUnixNano: info.ModTime().UnixNano(),
	}, true
}

// refreshProviderCatalogAfterUpgrade re-reads everything the app cached
// about the old binary. Recheck invalidates the probe cache first, so the
// re-probe is a cache miss and `provider:account` re-emits — which is what
// makes the model picker refresh. A bare probe would be a cache hit and
// emit nothing.
func (a *App) refreshProviderCatalogAfterUpgrade(providerName string) error {
	switch providerName {
	case string(provider.Codex):
		// The Codex model cache is keyed by binary path, and an in-place
		// upgrade does not change the path — so the entry must be dropped
		// explicitly or `model/list` answers for the previous build.
		a.refreshCodexModelCatalog()
		_, err := a.providerDiscoveryService().RecheckCodexAccount()
		return err
	case string(provider.Claude):
		_, err := a.providerDiscoveryService().RecheckClaudeAccount()
		return err
	}
	return fmt.Errorf("no catalog refresh for provider %q", providerName)
}

// reconcileStaleProviderSessions compares every live session's self-reported
// build against the binary on disk and emits only on transitions.
//
// Absence is never staleness: a session that has not reported a version yet
// (no `system/init` has landed, or the handshake carried no user agent) and
// a provider whose version could not be read are both "unknown", and unknown
// never raises a banner.
func (a *App) reconcileStaleProviderSessions() {
	live := a.sessionManager().runtime.Snapshot()
	// Allocated only if something is actually stale: the common tick finds
	// nothing and should cost no map.
	var current map[string]staleProviderBinary
	for threadID, entry := range live {
		installed, known := a.providerBinaries.lookupInstalled(entry.Provider)
		if !known {
			continue
		}
		sessionVersion := liveSessionCLIVersion(entry)
		if !providerstatus.BinaryStale(sessionVersion, installed.version) {
			continue
		}
		if current == nil {
			current = make(map[string]staleProviderBinary, 1)
		}
		current[threadID] = staleProviderBinary{
			provider:  entry.Provider,
			session:   providerstatus.VersionToken(sessionVersion),
			installed: installed.version,
			token:     entry.Token,
		}
	}

	entered, cleared := a.providerBinaries.reconcileStale(current)
	for threadID, versions := range entered {
		a.emitProviderStatus(providerstatus.Event{
			Provider: versions.provider,
			// Status is deliberately empty: this event speaks the `kind`
			// vocabulary, which the frontend router resolves before it
			// ever looks at `status`.
			Kind:     providerstatus.KindBinaryStale,
			ThreadID: threadID,
			Message: fmt.Sprintf(
				"The %s CLI was updated to %s while this session was running %s. Restart the session to use the new version.",
				versions.provider, versions.installed, versions.session,
			),
			Version:          versions.installed,
			SessionVersion:   versions.session,
			InstalledVersion: versions.installed,
			// The action is ReconnectSession, which is a button, not a URL.
			Actionable: true,
		})
	}
	for threadID, providerName := range cleared {
		// "ready" is the existing clear-the-banner signal, and a
		// thread-scoped one clears exactly this pane. No kind: there is
		// nothing new to say, only something to withdraw.
		a.emitProviderStatus(providerstatus.Event{
			Provider: providerName,
			Status:   "ready",
			ThreadID: threadID,
		})
	}
}

// liveSessionCLIVersion asks a live session what build its own process
// reported. claude-tui has no such accessor — the TUI never states a version
// on a channel AO reads — so it is permanently unknown rather than
// permanently stale.
func liveSessionCLIVersion(entry session) string {
	switch {
	case entry.Claude != nil:
		return entry.Claude.CLIVersion()
	case entry.Codex != nil:
		return entry.Codex.AppServerVersion()
	default:
		return ""
	}
}

// staleProviderBinaryVersions returns the flagged versions for one thread.
// Empty strings when the thread is not currently flagged, which is what
// GetThreadLiveState hydrates a non-stale pane with.
func (a *App) staleProviderBinaryVersions(threadID string) (sessionVersion, installedVersion string) {
	a.providerBinaries.mu.Lock()
	versions, ok := a.providerBinaries.stale[threadID]
	a.providerBinaries.mu.Unlock()
	if !ok {
		return "", ""
	}
	// The flag is a claim about one running process. If that session is gone
	// or has been replaced since the last tick, the claim is void even though
	// the tick has not yet noticed.
	live, active := a.sessionManager().get(threadID)
	if !active || live.Token != versions.token {
		return "", ""
	}
	return versions.session, versions.installed
}

// forgetStale drops a thread's flag the moment its session leaves, so the
// window between a disconnect and the next tick cannot hand a reconnecting
// pane a banner about a process that no longer exists. No event is owed: the
// disconnect that triggers this is the frontend's clear.
func (s *appProviderBinaryWatchState) forgetStale(threadID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.stale, threadID)
	if len(s.stale) == 0 {
		s.stale = nil
	}
}

func (s *appProviderBinaryWatchState) lookupInstalled(providerName string) (providerBinaryVersion, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.installed[providerName]
	return entry, ok
}

func (s *appProviderBinaryWatchState) storeInstalled(providerName string, entry providerBinaryVersion) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.installed == nil {
		s.installed = make(map[string]providerBinaryVersion, 2)
	}
	s.installed[providerName] = entry
}

// reconcileStale swaps in the tick's flagged set and reports the two
// transitions worth an event: threads that just became stale (or whose
// versions changed while stale), and threads that stopped being stale —
// including the ones whose session simply ended, since a pane whose session
// is gone must not keep a banner about it.
//
// The cleared map carries each thread's provider name so the caller can emit
// without a second lookup into a session map the thread has already left.
func (s *appProviderBinaryWatchState) reconcileStale(
	current map[string]staleProviderBinary,
) (entered map[string]staleProviderBinary, cleared map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for threadID, versions := range current {
		if previous, ok := s.stale[threadID]; ok && previous == versions {
			continue
		}
		if entered == nil {
			entered = make(map[string]staleProviderBinary, len(current))
		}
		entered[threadID] = versions
	}
	for threadID, versions := range s.stale {
		if _, ok := current[threadID]; ok {
			continue
		}
		if cleared == nil {
			cleared = make(map[string]string, len(s.stale))
		}
		cleared[threadID] = versions.provider
	}

	if len(current) == 0 {
		s.stale = nil
		return entered, cleared
	}
	s.stale = current
	return entered, cleared
}
