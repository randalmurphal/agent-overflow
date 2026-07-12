// Package control is the private wire between the harness backend and
// the ao-mockprovider processes it spawns (indirectly, via the app's
// normal provider spawn path). The backend runs a loopback HTTP server;
// each mock dials it at boot using the address + token inherited from
// the backend's environment, registers itself, receives its scenario,
// then long-polls for live commands (advance a wait gate, inject
// frames, kill the process) and reports progress back — which the
// backend re-emits as harness:mock events so tests can await scenario
// step boundaries instead of sleeping.
//
// Deliberately not the app transport: the transport boundary invariant
// keeps that wire for UI clients only, and a second tiny listener means
// mock traffic can never interleave with (or authenticate as) a UI
// connection.
package control

import "encoding/json"

// Env variable names through which the backend hands mocks the control
// endpoint. Set by runHarness before App.Start so every provider spawn
// (os.Environ inheritance) carries them.
const (
	EnvAddr  = "AO_HARNESS_CONTROL"
	EnvToken = "AO_HARNESS_CONTROL_TOKEN"
)

// Registration is what a mock reports about itself at boot.
type Registration struct {
	// Protocol is scenario.ProviderClaude or scenario.ProviderCodex —
	// which stdio protocol the mock detected from its argv.
	Protocol string `json:"protocol"`
	// Cwd is the mock's working directory — the workspace the app
	// spawned it in. The only cross-process key that correlates a mock
	// to a thread.
	Cwd string `json:"cwd"`
	PID int    `json:"pid"`
	// ResumeRef carries Claude's --resume value or Codex's
	// thread/resume id when the app is resuming, so scenario ${VAR}
	// substitution and assignment logic can react.
	ResumeRef string `json:"resumeRef,omitempty"`
}

// RegisterResponse hands the mock its identity and script.
type RegisterResponse struct {
	// MockID names this mock instance in commands, reports, and
	// harness:mock events ("mock-1", "mock-2", ... in spawn order).
	MockID string `json:"mockId"`
	// Scenario is the full scenario JSON the mock must execute.
	Scenario json.RawMessage `json:"scenario"`
	// FixtureRoot resolves relative FixtureStep paths.
	FixtureRoot string `json:"fixtureRoot"`
}

// Command is a live instruction to a running mock.
type Command struct {
	// Type: "advance" (release a waitSignal/stall gate), "emit" (write
	// the given wire lines immediately), "exit" (terminate with Code).
	Type string `json:"type"`
	// Name targets a named waitSignal gate; empty releases whichever
	// gate is open.
	Name string `json:"name,omitempty"`
	// Lines for "emit" — substituted against the mock's current Vars.
	Lines []string `json:"lines,omitempty"`
	// Code for "exit".
	Code int `json:"code,omitempty"`
}

// Command types.
const (
	CommandAdvance = "advance"
	CommandEmit    = "emit"
	CommandExit    = "exit"
)

// Report is a progress event from mock to backend. The backend wraps
// it (with MockID, Cwd) into a harness:mock bus event.
type Report struct {
	// Kind: "registered", "turn_started", "step_started",
	// "step_completed", "waiting_signal", "approval_pending",
	// "approval_decided", "scenario_done", "exiting".
	Kind string `json:"kind"`
	// Turn is the 1-based user-turn index (0 for lifecycle reports).
	Turn int `json:"turn,omitempty"`
	// Step is the 1-based step index within the turn.
	Step int `json:"step,omitempty"`
	// Detail is kind-specific: step action name, wait-gate name,
	// approval decision, exit code.
	Detail string `json:"detail,omitempty"`
}

// Report kinds. Tests await these via harness:mock events; renaming any
// is a breaking change to e2e helpers.
const (
	ReportRegistered      = "registered"
	ReportTurnStarted     = "turn_started"
	ReportStepStarted     = "step_started"
	ReportStepCompleted   = "step_completed"
	ReportWaitingSignal   = "waiting_signal"
	ReportApprovalPending = "approval_pending"
	ReportApprovalDecided = "approval_decided"
	ReportScenarioDone    = "scenario_done"
	ReportExiting         = "exiting"
)
