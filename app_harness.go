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
	"sync"

	"agent-overflow/internal/harness"
	"agent-overflow/internal/harness/control"
	"agent-overflow/internal/notify"
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
	h.app.emit(channel, payload)
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
