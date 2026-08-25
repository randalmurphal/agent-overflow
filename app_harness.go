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
	}, nil
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
	if !json.Valid(payload) {
		return fmt.Errorf("payload is not valid JSON")
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
