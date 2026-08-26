// Package scenario defines the declarative script format that drives
// ao-mockprovider — the fake Claude/Codex binary the agent test harness
// substitutes for real provider CLIs. A scenario says, per user turn,
// exactly which wire frames to emit and how (pacing, chunked partial
// writes, workspace mutations, approvals, stalls, crashes).
//
// The schema is shared by three consumers: the harness backend (embeds
// and serves the named library, validates uploads), ao-mockprovider
// (executes steps), and Go tests (author inline scenarios). Frames are
// carried as raw wire lines — Claude stream-json NDJSON or Codex
// app-server JSON-RPC — so the captured fixtures under
// docs/references/fixtures/ can be replayed verbatim; the schema never
// re-models provider wire shapes.
package scenario

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Provider selects which stdio protocol the mock speaks for a scenario.
const (
	ProviderClaude = "claude"
	ProviderCodex  = "codex"
)

// CurrentVersion is the schema version this package reads and writes.
// Bump only with a migration story for the embedded library.
const CurrentVersion = 1

// Scenario is one complete mock-provider script.
type Scenario struct {
	Version     int    `json:"version"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Provider    string `json:"provider"`

	// OnStart runs when the mock process boots, before any stdin
	// traffic. Claude scenarios must NOT emit system/init here: the
	// real CLI emits one init per user message (not at launch), and the
	// mock's claude adapter owns that frame — plus the user-envelope
	// replay echo — so every turn drives the app's turn machinery
	// correctly regardless of scenario authorship.
	OnStart []Step `json:"onStart,omitempty"`

	// Turns[i] runs in response to the (i+1)-th user turn: a stdin
	// user-message line for Claude, a turn/start request for Codex.
	Turns []Turn `json:"turns"`

	// AfterTurns picks the behaviour for user turns beyond len(Turns):
	// "repeatLast" re-runs the final turn's steps (default), "silent"
	// consumes the message and emits nothing (the turn hangs — useful
	// for interrupt/stall testing), "exit" ends the process cleanly.
	//
	// Every turn a scenario runs to completion reports scenario_done for
	// its own turn number, repeated turns included — so a per-turn await
	// works under repeatLast rather than only for the first turn.
	AfterTurns string `json:"afterTurns,omitempty"`

	// StartupDelayMs makes the app's COLD-START window drivable: the
	// adapter sleeps this long before writing the first frame that proves
	// the provider is up — Claude's first per-turn `system/init`, Codex's
	// `initialize` response. Once per process, not per turn: the thing
	// under test is a slow provider launch, not slow turns.
	//
	// Zero (the default) is no delay at all. Capped at MaxStartupDelayMs;
	// a scenario asking for more is a typo, and a mock that sleeps past
	// the app's own spawn timeout tests the timeout rather than the
	// window.
	StartupDelayMs int `json:"startupDelayMs,omitempty"`

	// ProviderVersion overrides the version the mock claims to be, which
	// is what every per-method version gate in the app reads: Codex's
	// `initialize` userAgent (`codex_cli_rs/<version>`, parsed by
	// internal/provider/codex/app_server_version.go) and Claude's
	// `system/init.claude_code_version`. The mock otherwise reports a
	// version above every gate, so a downgrade is the only way a spec can
	// exercise the fails-closed branch of one.
	//
	// It does NOT reach `--version`, the account probe, or one-shot text
	// generation: none of those invocations loads a scenario at all (they
	// answer and exit before the control channel is dialled), so a
	// scenario cannot speak for them. Pin those through the mock binary
	// itself if a spec ever needs to.
	//
	// A Codex scenario that declares its own `initialize` response
	// template still wins — the template is the more specific statement.
	ProviderVersion string `json:"providerVersion,omitempty"`

	// Codex maps JSON-RPC request methods to response templates for the
	// session-establishment handshake (initialize, thread/start,
	// thread/resume, ...). turn/start is answered from here too, then
	// the matching Turn's steps stream as notifications. Sensible
	// defaults cover the standard handshake; scenarios override only
	// what they need. Templates support the same ${VAR} substitution as
	// step lines, plus ${REQUEST_ID} for the JSON-RPC id.
	Codex *CodexOptions `json:"codex,omitempty"`
}

// CodexOptions carries the Codex-specific pieces of a scenario.
type CodexOptions struct {
	// Responses maps a JSON-RPC method name to a full response
	// template. ${REQUEST_ID} substitutes the request's id verbatim
	// (number or string).
	Responses map[string]string `json:"responses,omitempty"`
	// ThreadID seeds ${THREAD_ID}; defaults to "mock-codex-thread".
	// thread/resume echoes the requested id instead.
	ThreadID string `json:"threadId,omitempty"`
}

// Turn is the step list for one user turn.
type Turn struct {
	// Label surfaces in harness progress events (harness:mock) so
	// tests can await "turn N step K" boundaries by name.
	Label string `json:"label,omitempty"`
	Steps []Step `json:"steps"`
}

// Step is a tagged union: exactly one field may be set. A JSON object
// with zero or multiple set fields fails Validate — silent ambiguity in
// a test script wastes exactly the debugging time the harness exists to
// save.
type Step struct {
	// Emit writes wire frames to stdout with optional pacing.
	Emit *EmitStep `json:"emit,omitempty"`
	// Fixture streams lines from an NDJSON fixture file (path relative
	// to the fixture root the harness passes to the mock).
	Fixture *FixtureStep `json:"fixture,omitempty"`
	// DelayMs pauses between steps.
	DelayMs int `json:"delayMs,omitempty"`
	// WriteFile mutates the workspace (the mock's cwd), so diffs,
	// checkpoints, and git status reflect real file changes.
	WriteFile *WriteFileStep `json:"writeFile,omitempty"`
	// Approval emits an approval request and branches on the app's
	// decision (Claude: CanUseTool control_request; Codex: the
	// elicitation request configured in the step).
	Approval *ApprovalStep `json:"approval,omitempty"`
	// WaitSignal blocks until the harness control channel delivers an
	// "advance" command (HarnessMockAdvance) — the step-through
	// primitive for frame-accurate Playwright assertions.
	WaitSignal *WaitSignalStep `json:"waitSignal,omitempty"`
	// Stall stops emitting for the given duration (or forever when 0),
	// while stdin keeps draining — models a hung provider.
	Stall *StallStep `json:"stall,omitempty"`
	// Exit terminates the mock process mid-scenario — models a crash
	// (non-zero) or a clean provider shutdown (zero).
	Exit *ExitStep `json:"exit,omitempty"`
	// Repeat re-runs a nested step list, optionally forever — the
	// primitive behind indefinite steady-state streaming (the soak rig).
	Repeat *RepeatStep `json:"repeat,omitempty"`
}

// EmitStep writes one or more wire lines. Lines support ${VAR}
// substitution (see Vars). Pacing knobs exist to reproduce streaming
// timing bugs:
//
//   - DelayBetweenMs sleeps between lines (streaming cadence).
//   - ChunkBytes > 0 splits each line's bytes into flushed writes of
//     that size, ChunkIntervalMs apart — mid-line partial writes, the
//     input that shakes out NDJSON reassembly and reconnect bugs.
//   - Coalesce writes every line of the step in ONE write, the opposite
//     stress: real node CLIs batch their output under load, so an app
//     reader that only ever sees one frame per read is untested against
//     the shape it will actually meet.
type EmitStep struct {
	Lines           []string `json:"lines"`
	DelayBetweenMs  int      `json:"delayBetweenMs,omitempty"`
	ChunkBytes      int      `json:"chunkBytes,omitempty"`
	ChunkIntervalMs int      `json:"chunkIntervalMs,omitempty"`
	// Coalesce writes all of Lines as a single stdout write — several
	// NDJSON frames arriving in one read, which is what a provider CLI
	// under load actually produces and the only path the split-per-line
	// default never exercises.
	//
	// Mutually exclusive with the pacing knobs, which all exist to SPLIT
	// output: a coalesced write has no between-lines moment to delay at
	// and no sub-line boundary to chunk on, so declaring both is an
	// author contradiction rather than a resolvable combination.
	Coalesce bool `json:"coalesce,omitempty"`
}

// FixtureStep streams a captured wire log. FromLine/ToLine are
// 1-indexed inclusive; zero means "start"/"end". Blank lines and lines
// starting with '#' are skipped (fixture files carry commentary).
type FixtureStep struct {
	Path           string `json:"path"`
	FromLine       int    `json:"fromLine,omitempty"`
	ToLine         int    `json:"toLine,omitempty"`
	DelayBetweenMs int    `json:"delayBetweenMs,omitempty"`
}

// WriteFileStep writes Content to Path (workspace-relative, parent
// dirs created). Append toggles append vs truncate.
type WriteFileStep struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Append  bool   `json:"append,omitempty"`
}

// ApprovalStep emits an approval request, waits for the response, and
// runs the branch matching the decision. For Claude the request is a
// CanUseTool control_request built from ToolName/Input; the mock
// correlates the control_response by request id. OnAllow typically
// emits the tool_use + tool_result frames; OnDeny a denial result.
type ApprovalStep struct {
	// ToolName/Input shape the CanUseTool request (Claude) or the
	// command approval (Codex).
	ToolName string          `json:"toolName"`
	Input    json.RawMessage `json:"input,omitempty"`
	// ToolUseID/AgentID are the Claude-only correlation fields the real
	// CLI puts on `control_request/can_use_tool`: the id of the tool_use
	// awaiting the decision, and — when the asking tool call originated
	// inside a subagent — that subagent's agent id (its task_id). Both
	// are load-bearing downstream: triage resolves AgentID to the launch
	// tool_use so the prompt scopes to the agent's card rather than the
	// main thread. Captured shape:
	// docs/references/fixtures/claude/can_use_tool_agent_id_20260822.ndjson.
	// Omitted fields are left off the request, which is the top-level
	// (main-agent) shape. Both support ${VAR} substitution.
	ToolUseID string `json:"toolUseId,omitempty"`
	AgentID   string `json:"agentId,omitempty"`
	OnAllow   []Step `json:"onAllow,omitempty"`
	OnDeny    []Step `json:"onDeny,omitempty"`
	// TimeoutMs bounds the wait; 0 means wait forever. On timeout the
	// mock logs to stderr and continues with OnDeny.
	TimeoutMs int `json:"timeoutMs,omitempty"`
}

// WaitSignalStep blocks until an advance command arrives on the control
// channel. Name lets a scenario declare multiple distinct gates; an
// advance command targets a name (empty advances whatever gate is
// currently open).
type WaitSignalStep struct {
	Name string `json:"name,omitempty"`
}

// StallStep suspends output. DurationMs 0 stalls until the process is
// closed or a control advance arrives.
type StallStep struct {
	DurationMs int `json:"durationMs,omitempty"`
}

// ExitStep ends the process immediately with Code. FlushDelayMs gives
// already-written frames time to drain first.
type ExitStep struct {
	Code         int `json:"code"`
	FlushDelayMs int `json:"flushDelayMs,omitempty"`
}

// RepeatStep re-runs Steps. Count > 0 runs that many iterations; Count
// <= 0 loops until the turn is interrupted or the process dies — which
// is how a scenario models a turn that never completes (background
// agents streaming indefinitely) rather than a long finite script.
//
// Inside the body, ${ITER} carries the 1-based iteration number, so
// emitted message / tool ids stay unique across iterations. A nested
// repeat re-binds ${ITER} for its own body.
//
// An infinite repeat must pace itself: Validate requires a direct child
// that waits (delayMs, stall, waitSignal, approval, or an emit with
// delayBetweenMs). Without one, a scenario silently becomes an
// unthrottled writer that saturates the app's reader — a mistake worth
// catching at set time, not hours into a soak.
type RepeatStep struct {
	Count int    `json:"count,omitempty"`
	Steps []Step `json:"steps"`
}

// Vars is the substitution context for ${VAR} tokens in emit lines and
// Codex response templates. The mock populates it at runtime:
//
//	${SESSION_ID}  — Claude session id (new or from --resume)
//	${THREAD_ID}   — Codex thread id (scenario option or resumed id)
//	${TURN}        — 1-based index of the current user turn
//	${TURN_ID}     — Codex turn id ("turn-<TURN>")
//	${REQUEST_ID}  — Codex only: the JSON-RPC id being answered
//	${CWD}         — the mock's working directory (the workspace)
//	${ITER}        — inside a repeat body: the 1-based iteration number
//
// Two more carry what only the running mock knows about the turn's own user
// message, which no scenario file could name:
//
//	${USER_INPUT}  — the turn's user text (Codex: the `turn/start` input vec)
//	${CLIENT_ID}   — Codex only: the `clientUserMessageId` the caller stamped
//	                 on `turn/start`, which its `userMessage` echo has to
//	                 carry back as `clientId` or the app cannot consume the
//	                 pending send it registered by that id. Empty when the
//	                 caller supplied none.
//
// A steer's own echo is written by the adapter rather than a scenario step —
// its text and client id exist only at steer time — so it needs no variable.
type Vars map[string]string

// Substitute replaces ${VAR} tokens for every key present in v.
// Unknown tokens are left verbatim so provider payloads containing
// literal ${...} text survive.
func (v Vars) Substitute(line string) string {
	if len(v) == 0 || !strings.Contains(line, "${") {
		return line
	}
	out := line
	for key, val := range v {
		out = strings.ReplaceAll(out, "${"+key+"}", val)
	}
	return out
}

// MaxStartupDelayMs caps Scenario.StartupDelayMs at 30 seconds. The
// knob exists to drive the app's cold-start WINDOW; a delay longer than
// any spawn timeout the app applies stops testing the window and starts
// testing the timeout, which is a different scenario and should say so.
const MaxStartupDelayMs = 30_000

// providerVersionPattern bounds Scenario.ProviderVersion. Empty (the
// default: report the mock's own version) or a dotted numeric version.
// It is interpolated verbatim into wire strings the app then parses, so
// anything else would be injecting text into a userAgent rather than
// pinning a version.
var providerVersionPattern = regexp.MustCompile(`^(\d+(\.\d+)*)?$`)

// afterTurnsModes enumerates Scenario.AfterTurns values.
var afterTurnsModes = map[string]bool{
	"":           true, // default: repeatLast
	"repeatLast": true,
	"silent":     true,
	"exit":       true,
}

// Validate checks structural integrity: version, provider, step-union
// arity (recursively through approval branches), and per-step field
// sanity. It does NOT check that emitted lines are valid provider wire
// frames — fixtures are captured reality and inline lines are the
// author's responsibility; the harness's Go tests validate the shipped
// library against the real parsers instead.
func (s *Scenario) Validate() error {
	if s.Version != CurrentVersion {
		return fmt.Errorf("scenario %q: version %d unsupported (want %d)", s.Name, s.Version, CurrentVersion)
	}
	if s.Name == "" {
		return fmt.Errorf("scenario name must be non-empty")
	}
	if s.Provider != ProviderClaude && s.Provider != ProviderCodex {
		return fmt.Errorf("scenario %q: provider %q must be %q or %q", s.Name, s.Provider, ProviderClaude, ProviderCodex)
	}
	if !afterTurnsModes[s.AfterTurns] {
		return fmt.Errorf("scenario %q: afterTurns %q must be repeatLast, silent, or exit", s.Name, s.AfterTurns)
	}
	if s.StartupDelayMs < 0 || s.StartupDelayMs > MaxStartupDelayMs {
		return fmt.Errorf("scenario %q: startupDelayMs %d must be between 0 and %d", s.Name, s.StartupDelayMs, MaxStartupDelayMs)
	}
	if strings.TrimSpace(s.ProviderVersion) != s.ProviderVersion {
		return fmt.Errorf("scenario %q: providerVersion %q must not carry surrounding whitespace", s.Name, s.ProviderVersion)
	}
	if !providerVersionPattern.MatchString(s.ProviderVersion) {
		return fmt.Errorf(
			"scenario %q: providerVersion %q must be a dotted version like %q "+
				"(it is interpolated into a userAgent string the app parses)",
			s.Name, s.ProviderVersion, "0.147.0")
	}
	if len(s.Turns) == 0 && len(s.OnStart) == 0 {
		return fmt.Errorf("scenario %q: needs at least one turn or onStart step", s.Name)
	}
	for i, step := range s.OnStart {
		if err := step.validate(); err != nil {
			return fmt.Errorf("scenario %q: onStart step %d: %w", s.Name, i+1, err)
		}
	}
	for ti, turn := range s.Turns {
		if len(turn.Steps) == 0 {
			return fmt.Errorf("scenario %q: turn %d has no steps", s.Name, ti+1)
		}
		for si, step := range turn.Steps {
			if err := step.validate(); err != nil {
				return fmt.Errorf("scenario %q: turn %d step %d: %w", s.Name, ti+1, si+1, err)
			}
		}
	}
	return nil
}

func (st *Step) validate() error {
	set := 0
	if st.Emit != nil {
		set++
		if len(st.Emit.Lines) == 0 {
			return fmt.Errorf("emit: lines must be non-empty")
		}
		if st.Emit.ChunkBytes < 0 || st.Emit.DelayBetweenMs < 0 || st.Emit.ChunkIntervalMs < 0 {
			return fmt.Errorf("emit: pacing values must be >= 0")
		}
		if st.Emit.Coalesce && (st.Emit.ChunkBytes > 0 || st.Emit.DelayBetweenMs > 0 || st.Emit.ChunkIntervalMs > 0) {
			return fmt.Errorf(
				"emit: coalesce writes every line in one write, so it cannot be combined with " +
					"the pacing knobs that split output (delayBetweenMs, chunkBytes, chunkIntervalMs)")
		}
	}
	if st.Fixture != nil {
		set++
		if st.Fixture.Path == "" {
			return fmt.Errorf("fixture: path must be non-empty")
		}
		if st.Fixture.FromLine < 0 || st.Fixture.ToLine < 0 {
			return fmt.Errorf("fixture: line bounds must be >= 0")
		}
		if st.Fixture.ToLine > 0 && st.Fixture.FromLine > st.Fixture.ToLine {
			return fmt.Errorf("fixture: fromLine %d > toLine %d", st.Fixture.FromLine, st.Fixture.ToLine)
		}
	}
	if st.DelayMs != 0 {
		set++
		if st.DelayMs < 0 {
			return fmt.Errorf("delayMs must be > 0")
		}
	}
	if st.WriteFile != nil {
		set++
		if st.WriteFile.Path == "" {
			return fmt.Errorf("writeFile: path must be non-empty")
		}
	}
	if st.Approval != nil {
		set++
		if st.Approval.ToolName == "" {
			return fmt.Errorf("approval: toolName must be non-empty")
		}
		for i, sub := range st.Approval.OnAllow {
			if err := sub.validate(); err != nil {
				return fmt.Errorf("approval onAllow step %d: %w", i+1, err)
			}
		}
		for i, sub := range st.Approval.OnDeny {
			if err := sub.validate(); err != nil {
				return fmt.Errorf("approval onDeny step %d: %w", i+1, err)
			}
		}
	}
	if st.WaitSignal != nil {
		set++
	}
	if st.Stall != nil {
		set++
		if st.Stall.DurationMs < 0 {
			return fmt.Errorf("stall: durationMs must be >= 0")
		}
	}
	if st.Exit != nil {
		set++
	}
	if st.Repeat != nil {
		set++
		if len(st.Repeat.Steps) == 0 {
			return fmt.Errorf("repeat: steps must be non-empty")
		}
		for i, sub := range st.Repeat.Steps {
			if err := sub.validate(); err != nil {
				return fmt.Errorf("repeat step %d: %w", i+1, err)
			}
		}
		if st.Repeat.Count <= 0 && !stepsPace(st.Repeat.Steps) {
			return fmt.Errorf(
				"repeat: an unbounded repeat (count %d) needs a pacing step among its direct children "+
					"(delayMs, stall, waitSignal, approval, or an emit with delayBetweenMs) — "+
					"otherwise it writes wire lines as fast as the pipe accepts them",
				st.Repeat.Count)
		}
	}
	if set != 1 {
		return fmt.Errorf("step must set exactly one action, got %d", set)
	}
	return nil
}

// stepsPace reports whether a step list contains something that waits.
// Direct children only: a pacing step buried inside a nested repeat or
// approval branch is not a guarantee the outer loop ever yields, and a
// shallow rule is one an author can check by eye.
func stepsPace(steps []Step) bool {
	for _, st := range steps {
		switch {
		case st.DelayMs > 0,
			st.Stall != nil,
			st.WaitSignal != nil,
			st.Approval != nil,
			st.Emit != nil && st.Emit.DelayBetweenMs > 0:
			return true
		}
	}
	return false
}

// FixturePaths collects every fixture path the scenario references —
// onStart, every turn, and approval branches (recursively). Callers use
// it to fail a scenario install when a fixture file is missing, per the
// package invariant that scripts fail at set time, not inside a spawned
// mock.
func (s *Scenario) FixturePaths() []string {
	var out []string
	out = appendFixturePaths(out, s.OnStart)
	for _, turn := range s.Turns {
		out = appendFixturePaths(out, turn.Steps)
	}
	return out
}

func appendFixturePaths(out []string, steps []Step) []string {
	for _, st := range steps {
		if st.Fixture != nil {
			out = append(out, st.Fixture.Path)
		}
		if st.Approval != nil {
			out = appendFixturePaths(out, st.Approval.OnAllow)
			out = appendFixturePaths(out, st.Approval.OnDeny)
		}
		if st.Repeat != nil {
			out = appendFixturePaths(out, st.Repeat.Steps)
		}
	}
	return out
}

// Parse decodes and validates a scenario JSON document.
func Parse(data []byte) (*Scenario, error) {
	var s Scenario
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("parse scenario: %w", err)
	}
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return &s, nil
}
