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
	// EnvTranscriptHome is set only by a fully isolated harness boot. It is
	// the mock's stand-in for a provider home: the Claude adapter writes its
	// synthetic transcript there so a backend restart can exercise the real
	// cold-resume preflight, and the Codex adapter records each thread's
	// history mode there so a THROWAWAY RESUME (which is how every rollback
	// reaches the provider) still knows whether the thread is revertible.
	// Neither ever touches a developer's provider home; without this
	// variable both behaviours are simply absent.
	EnvTranscriptHome = "AO_HARNESS_TRANSCRIPT_HOME"
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
	// Kind: "registered", "user_input", "turn_started",
	// "turn_interrupted", "step_started", "step_completed",
	// "waiting_signal", "approval_pending", "approval_decided",
	// "history_cut", "scenario_done", "exiting".
	Kind string `json:"kind"`
	// Turn is the 1-based user-turn index (0 for lifecycle reports).
	Turn int `json:"turn,omitempty"`
	// Step is the 1-based step index within the turn.
	Step int `json:"step,omitempty"`
	// Detail is kind-specific: step action name, wait-gate name,
	// approval decision, exit code.
	Detail string `json:"detail,omitempty"`
	// Input is the user text the mock received on the wire, set on
	// ReportUserInput. It is the ONLY surface that answers "what did the
	// app actually send the provider" — the transcript stores what the
	// user typed, which is deliberately not the same string once the send
	// path expands a composer command. ReportHistoryCut reuses it for the
	// anchor turn id, which is the same question about a different frame.
	Input string `json:"input,omitempty"`
	// SessionRef is the provider session that received Input: Claude's
	// session id or Codex's thread id. A process id cannot answer this for
	// Codex because one app-server process may host multiple threads.
	SessionRef string `json:"sessionRef,omitempty"`
	// SessionConfig is set only on ReportSessionConfig reports.
	SessionConfig *SessionConfig `json:"sessionConfig,omitempty"`
}

// Report kinds. Tests await these via harness:mock events; renaming any
// is a breaking change to e2e helpers.
const (
	ReportRegistered = "registered"
	// ReportUserInput carries the text of one inbound user message, posted
	// by the protocol adapter the moment the app writes it — before the
	// turn's steps run, so a test can await the input and the turn's
	// completion independently.
	ReportUserInput       = "user_input"
	ReportTurnStarted     = "turn_started"
	ReportTurnInterrupted = "turn_interrupted"
	ReportStepStarted     = "step_started"
	ReportStepCompleted   = "step_completed"
	ReportWaitingSignal   = "waiting_signal"
	ReportApprovalPending = "approval_pending"
	ReportApprovalDecided = "approval_decided"
	ReportScenarioDone    = "scenario_done"
	ReportExiting         = "exiting"
	// ReportHistoryCut names the provider-history truncation the app just
	// asked for on a Codex thread: Detail is the method ("thread/revert"
	// or "thread/fork"), SessionRef the thread it named, Input the anchor
	// turn id. Posted BEFORE the answer, so a test can assert WHICH of
	// the two cuts the app chose without racing the response — the
	// choice is version- and history-mode-gated, and its only other
	// symptom (a thread id that did or did not change) cannot say
	// whether a refused revert was even attempted.
	ReportHistoryCut = "history_cut"
	// ReportSessionConfig carries the permission/sandbox configuration the
	// app actually launched this session with. Posted once per mock as soon
	// as it is observable — for Claude that is argv at boot, for Codex the
	// thread/start params.
	ReportSessionConfig = "session_config"
)

// SessionConfig is the permission/sandbox configuration a mock observed the
// app request. It exists so a test can assert what the app ASKED the provider
// for, which is the only part of runtime-mode enforcement AO owns — whether
// the real CLI then honours it is a provider-behaviour question answered by
// spikes, not by the harness.
//
// The fields are deliberately per-provider rather than a normalized mode: the
// point of the assertion is to catch a wrong mapping, and a normalized shape
// would launder exactly the mistake under test.
type SessionConfig struct {
	// PermissionMode is Claude's --permission-mode value ("" when the flag
	// was omitted, which is the supervised default).
	PermissionMode string `json:"permissionMode,omitempty"`
	// DisallowedTools lists Claude's --disallowedTools values in argv order.
	DisallowedTools []string `json:"disallowedTools,omitempty"`
	// Sandbox is Codex's thread/start `sandbox`.
	Sandbox string `json:"sandbox,omitempty"`
	// ApprovalPolicy is Codex's thread/start `approvalPolicy`.
	ApprovalPolicy string `json:"approvalPolicy,omitempty"`
}
