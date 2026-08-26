// app_harness.go declares the Harness RPC receiver — the agent test
// harness's control surface. It is registered on the transport only by
// runHarness (main_harness.go); every other boot mode never constructs
// it, so none of these methods exist on the wire outside --harness.
// Registration marks the whole receiver LocalOnly, so even a LAN-bound
// harness refuses these methods for non-loopback peers.
//
// Naming: every method is prefixed Harness* so wire-level name dispatch
// ({type:"rpc", method:"HarnessInfo"}) can never collide with an App
// method, and so transcripts of harness runs are self-describing.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/harness"
	"agent-overflow/internal/harness/control"
	"agent-overflow/internal/notify"
	"agent-overflow/internal/slicesx"
	"agent-overflow/internal/store"
	"agent-overflow/internal/uitrace"
)

// Harness is the RPC receiver for the agent test harness. It wraps the
// live *App rather than duplicating its state — harness operations run
// through the same production code paths (thread CRUD, session
// lifecycle, event bus) the real UI exercises, per the repo principle
// that the harness must behave like the real app in every way except
// the provider processes.
type Harness struct {
	app   *App
	paths harnessPaths

	mu            sync.Mutex
	replayer      *harness.Replayer     // lazy; see replayerEngine
	recording     *harnessRecording     // the one in-flight bundle capture
	control       *control.Server       // mock-provider control channel
	scenarioRules []harnessScenarioRule // mock → scenario assignment
	// soakAutopilot latches how the --autopilot arming went. Empty means
	// this boot has no autopilot at all; see soakAutopilotState.
	soakAutopilot string
	// wireMethods is every method name the dispatcher registered, sorted.
	// Handed over by bootTransport once registration finishes; see
	// setWireMethods.
	wireMethods []string

	// mutate serializes the three RPCs that rewrite the harness's WORLD —
	// HarnessSeed, HarnessReset, and HarnessReplayBundle's restore. It is a
	// separate lock from `mu` above, which guards this struct's own fields
	// for microseconds; these operations run for seconds, spawn git, stop
	// sessions, and touch the store and the filesystem.
	//
	// The races it closes are real and silent: reset's
	// `RemoveAll(<dataRoot>/workspaces)` can delete a repo a concurrent
	// seed's CreateRepo just finished writing (leaving a project row
	// pointing at nothing), two resets both list the same projects and the
	// second DeleteProject fails on rows the first already cascaded, and a
	// bundle restore swapping the SQLite file under a running seed leaves
	// the seed's remaining inserts in a database nobody will read. Nothing
	// in the wire layer serializes RPCs — the transport dispatches each
	// frame on its own goroutine — so the guard has to live here.
	//
	// Deliberately NOT extended to the read RPCs or the scenario/mock
	// setters: those are short, independently locked, and blocking them
	// behind a multi-second reset would make a harness feel wedged.
	mutate sync.Mutex

	// Frontend bridge (app_harness_ui.go) and the perf run it arms
	// (app_harness_perf.go). Both carry their own locks: a ui query parks
	// for up to 10s and a perf run holds a sampler goroutine, neither of
	// which may sit on the mutex above.
	ui   harnessUIBridge
	perf harnessPerfState
}

// newHarness wires the receiver. Engines that need App subsystems
// (seeder, replayer, mock control) attach lazily as their features are
// invoked — App.Start hasn't run yet when this is constructed.
func newHarness(app *App, paths harnessPaths) *Harness {
	return &Harness{app: app, paths: paths}
}

// HarnessInfoResult is the self-description agents start from: where
// everything lives on disk, and how to reach the app. Fields are stable
// API for e2e helpers — extend, don't rename.
type HarnessInfoResult struct {
	Version      string `json:"version"`
	PID          int    `json:"pid"`
	DataRoot     string `json:"dataRoot"`
	DataDir      string `json:"dataDir"`
	HomeDir      string `json:"homeDir,omitempty"`
	MockProvider string `json:"mockProvider"`
	DBPath       string `json:"dbPath"`
	// EventLogDir holds the per-thread NDJSON event logs
	// (internal/observability/replay) — always enabled in harness mode,
	// and the raw material for wire-level replay recordings.
	EventLogDir string `json:"eventLogDir"`
	// UITracePath / FrontendErrorsPath are the frontend diagnostic
	// JSONL sinks (internal/uitrace). The render trace only receives
	// data when the frontend build enabled it (VITE_AGENT_OVERFLOW_UI_TRACE);
	// the error log is always on.
	UITracePath        string `json:"uiTracePath"`
	FrontendErrorsPath string `json:"frontendErrorsPath"`
	// SoakAutopilot reports how the --autopilot preset went on this boot:
	// "off" (not requested), "arming" (the pre-arm delay or the arming
	// itself is still in flight), "armed", or "failed: <reason>".
	//
	// It exists because the arming runs on a goroutine that starts AFTER
	// publishInstance has already stamped the instance as a soak — so a
	// failure used to be a single log line in launcher-soak.log and the
	// instance looked identical to a working one to every tool. Anything
	// deciding whether the soak is actually producing load reads this.
	SoakAutopilot string `json:"soakAutopilot"`
	// AssetsFreshness is the embedded-frontend-bundle verdict from boot:
	// "match" (embed equals the adjacent frontend/dist), "stale" (they
	// differ — rebuild the binary before trusting any measurement),
	// "unknown" (no on-disk dist to compare against), or "dev-server"
	// (FRONTEND_DEVSERVER_URL assets are being served instead of the
	// embed). `ao-harness health` flags "stale".
	AssetsFreshness string `json:"assetsFreshness,omitempty"`
}

// The four values HarnessInfoResult.SoakAutopilot can carry. A failure
// renders as soakAutopilotFailedPrefix + the reason, so a consumer tests
// for the prefix rather than an exact string.
const (
	soakAutopilotOff          = "off"
	soakAutopilotArming       = "arming"
	soakAutopilotArmed        = "armed"
	soakAutopilotFailedPrefix = "failed: "
)

// setSoakAutopilot latches the arming outcome. Called from the boot
// goroutine and from the arming goroutine, read by HarnessInfo on a
// transport goroutine — hence the lock.
func (h *Harness) setSoakAutopilot(state string) {
	h.mu.Lock()
	h.soakAutopilot = state
	h.mu.Unlock()
}

// soakAutopilotState renders the latch. An unset latch is "off": only a
// --autopilot boot ever writes one, so nothing having written means
// nothing was asked for.
func (h *Harness) soakAutopilotState() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.soakAutopilot == "" {
		return soakAutopilotOff
	}
	return h.soakAutopilot
}

// HarnessInfo reports the harness's identity and evidence paths.
func (h *Harness) HarnessInfo() (HarnessInfoResult, error) {
	dataDir := h.paths.DataDir
	return HarnessInfoResult{
		Version:            version,
		PID:                os.Getpid(),
		DataRoot:           h.paths.DataRoot,
		DataDir:            dataDir,
		HomeDir:            h.paths.HomeDir,
		MockProvider:       h.paths.MockProvider,
		DBPath:             filepath.Join(dataDir, "agent-overflow.db"),
		EventLogDir:        filepath.Join(dataDir, "replay"),
		UITracePath:        filepath.Join(dataDir, uitrace.DirName, uitrace.FileName),
		FrontendErrorsPath: filepath.Join(dataDir, uitrace.DirName, uitrace.ErrorFileName),
		SoakAutopilot:      h.soakAutopilotState(),
		AssetsFreshness:    h.paths.AssetsFreshness,
	}, nil
}

// setWireMethods records the dispatcher's registered method names. Called
// once by bootTransport, after BOTH receivers are registered and before
// the listener serves anything, so no RPC can observe a partial list.
//
// The list is pushed rather than pulled deliberately: the Harness holds
// an *App, not a *transport.Dispatcher, and giving it one would hand the
// harness surface a way to reach the wire layer directly — the exact
// back-channel the transport-boundary invariant forbids.
func (h *Harness) setWireMethods(names []string) {
	sorted := slices.Clone(names)
	slices.Sort(sorted)
	sorted = slices.Compact(sorted)
	h.mu.Lock()
	h.wireMethods = sorted
	h.mu.Unlock()
}

// HarnessListMethods reports every method name reachable on the wire,
// sorted — the App's bindings and the Harness surface in one list.
//
// It is the harness's self-description for callers that cannot read the
// Go source: a CLI can check whether the instance it just attached to
// actually has the RPC it is about to call, instead of discovering a
// version mismatch as an opaque "unknown method" mid-run.
//
// Names are the bare wire names ({"method":"HarnessInfo"}), not FQNs,
// because that is what a caller puts on the frame.
func (h *Harness) HarnessListMethods() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	// Cloned: the caller gets a value, and a mutating caller must not be
	// able to edit the registry's record of itself.
	return slicesx.OrEmpty(slices.Clone(h.wireMethods))
}

// HarnessSessionEnv returns the AO_* environment a thread's LIVE provider
// session carries, so an e2e spec can run the real `ao` binary against the real
// backend with the real credential.
//
// It is a read of the live registry, never a mint: a thread with no session
// returns nothing, which is exactly what a spawned process would see. The mock
// provider has no exec step and could not shell out on its own, so this is how
// the CLI path is exercised end to end without inventing a credential the app
// would not otherwise have issued.
func (h *Harness) HarnessSessionEnv(threadID string) (map[string]string, error) {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return nil, fmt.Errorf("thread id must be non-empty")
	}
	env := h.app.sessionAOEnv(threadID)
	if env == nil {
		return map[string]string{}, nil
	}
	return env, nil
}

// HarnessListThreadRows returns every non-archived thread ROW, drafts
// included — the read `App.ListThreads` deliberately cannot serve.
//
// The sidebar's list hides a thread until it has an item or a
// content-carrying draft, so a row that was created and never written to is
// invisible to every production read a spec can reach. That row is exactly
// what some regressions are about: an empty thread materialized as a side
// effect of an unrelated action (naming a workspace for a draft) is a bug
// whose only symptom is its existence. This is the assertion surface for
// "nothing was created", and for reading back what a just-materialized row
// was bound to.
//
// Read-only, and it uses the same store call the app's own internal callers
// use, so the harness is not learning the schema separately.
func (h *Harness) HarnessListThreadRows() ([]store.Thread, error) {
	if h.app == nil || h.app.store == nil {
		return nil, fmt.Errorf("store unavailable")
	}
	rows, err := h.app.store.ListThreads()
	if err != nil {
		return nil, err
	}
	return slicesx.OrEmpty(rows), nil
}

// HarnessEmit publishes a raw event onto the transport bus — the
// escape-hatch injection primitive for quick experiments that don't
// warrant a scenario. The payload is forwarded verbatim; the frontend
// treats it exactly like a backend-originated event on that channel,
// so a malformed payload exercises the frontend's real error handling
// (which is often the point).
func (h *Harness) HarnessEmit(channel string, payload json.RawMessage) error {
	if channel == "" {
		return fmt.Errorf("channel must be non-empty")
	}
	if len(payload) == 0 {
		return fmt.Errorf("payload must be non-empty JSON (use null to send an empty event)")
	}
	if err := requireValidJSON("payload", payload); err != nil {
		return err
	}
	// Deliberate escape hatch. NOT unregistrable: a caller that spells a
	// REGISTERED name inherits that row's audience, AudienceAny included —
	// only unrecognized names land on the fail-closed loopback-only
	// default. The real gate is reachability: this method exists only
	// under --harness/--soak, on a LocalOnly receiver, so only a loopback
	// caller holding the bootstrap token can reach it, and forging frames
	// is the harness's intended capability (2026-08-25 security review,
	// finding 3).
	h.app.emit(eventchan.Channel(channel), payload)
	return nil
}

// HarnessNotify exercises the production send helper and then synthesizes
// the activation that an OS click would produce. The synthetic step bypasses
// only OS presentation/click handling: target validation, backend emission,
// transport delivery, and frontend routing are the production path.
func (h *Harness) HarnessNotify(title, body string, target notify.Target) error {
	sendErr := h.app.notifyOS(title, body, target)
	activationErr := h.app.activateNotificationTarget(target)
	return errors.Join(sendErr, activationErr)
}
