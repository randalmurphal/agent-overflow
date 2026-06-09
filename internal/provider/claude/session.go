package claude

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"agent-overflow/internal/logging"
	"agent-overflow/internal/provider"

	"github.com/google/uuid"
)

// controlRequestPrefix is the first bytes of a Claude control_request
// NDJSON line. Gating the ExitPlanMode handler on this prefix saves a
// json.Unmarshal on every assistant/user/stream_event line (which is
// the hot path during streaming). False positives are benign — the
// subsequent Request.Subtype / ToolName check still filters.
var controlRequestPrefix = []byte(`{"type":"control_request"`)

// controlResponsePrefix matches the CLI's reply to our outbound
// control_requests (stop_task, set_permission_mode). Prefix-gating
// it the same way as controlRequestPrefix keeps streaming deltas off
// the secondary json.Unmarshal path.
var controlResponsePrefix = []byte(`{"type":"control_response"`)

// controlCancelRequestPrefix matches the CLI's "abandon this prior
// control_request" notification. The CLI emits this for any in-flight
// can_use_tool callback that an interrupt aborts (the SDK fires the
// AbortSignal on the pending callback; the CLI side wires that to a
// control_cancel_request on stdout — see Python SDK
// _internal/query.py:272-278). We must NOT write a control_response
// for these — the CLI has already given up on the request — so the
// prefix gate routes them to a separate cleanup-only handler.
var controlCancelRequestPrefix = []byte(`{"type":"control_cancel_request"`)

var (
	leafAssistantPrefix = []byte(`{"type":"assistant"`)
	leafUserPrefix      = []byte(`{"type":"user"`)
	leafResultPrefix    = []byte(`{"type":"result"`)
)

type controlRequestEnvelope struct {
	Type      string `json:"type"`
	RequestID string `json:"request_id"`
	Request   struct {
		Subtype  string          `json:"subtype"`
		ToolName string          `json:"tool_name"`
		Input    json.RawMessage `json:"input"`
	} `json:"request"`
}

// DefaultControlRequestTimeout bounds how long outbound Claude control
// requests wait for the CLI's control_response before returning a timeout
// error. The verified stop_task spike observed sub-100ms round-trips on
// Claude CLI 2.1.112; ten seconds is a generous ceiling that still fails
// loudly if the CLI is wedged.
const DefaultControlRequestTimeout = 10 * time.Second

// Compile-time guarantee that *Session satisfies the provider.Session
// interface the app layer calls into. Changing any of the methods in a
// way that breaks the contract is caught at build time.
var _ provider.Session = (*Session)(nil)

// Session manages a Claude Code CLI subprocess.
type Session struct {
	proc      *provider.Process
	threadID  string
	sessionID string
	workDir   string
	model     string
	onEvent   func(provider.ProviderEvent)
	cancel    context.CancelFunc
	closing   atomic.Bool
	readDone  chan struct{}
	// basePermissionMode is the runtime/access mode restored after a plan
	// turn. currentPermissionMode mirrors the last successful
	// set_permission_mode request so we avoid redundant control round-trips.
	permissionModeMu      sync.RWMutex
	basePermissionMode    string
	currentPermissionMode string
	interactionMode       provider.InteractionMode
	// parser holds per-session NDJSON parse state so tool_use / tool_result
	// pairs can share metadata (e.g. the `is_background` flag) across the
	// two messages that carry them.
	parser *Parser
	// leafTracker tracks the canonical settled top-level Claude transcript
	// UUID. Unresolved server-side tool-use rows are kept out of this leaf
	// so a later user send can be forced back onto the real continuation.
	leafTracker *claudeLeafTracker
	// replayMu guards the expected parent for the next AO-authored replay
	// user echo. Claude stream-json gives us no send-time parent override,
	// so the replay echo is the earliest confirmation that the live process
	// attached the user message to the expected transcript leaf.
	replayMu               sync.Mutex
	expectedReplayParent   string
	expectedReplayWasRisky bool
	// approvalsMu guards pendingApprovals, resolvedApprovals, and
	// approvalsClosed.
	approvalsMu sync.Mutex
	// pendingApprovals maps approval request IDs to the in-flight request
	// metadata needed to resolve, cancel, or drain it.
	pendingApprovals map[string]*pendingApproval
	// approvalDedup tracks request IDs already answered so duplicate
	// responses return ErrApprovalAlreadyResolved (Bug B9) instead of
	// writing a second control_response to the CLI. Guarded by approvalsMu.
	approvalDedup provider.ApprovalDeduper
	// approvalsClosed is set when Close has resolved all pending requests
	// so late-arriving approvals do not register new ones.
	approvalsClosed bool
	// controlRequestTimeout overrides DefaultControlRequestTimeout when non-zero.
	// Tests set this to a short window so a non-responsive fake CLI
	// doesn't stall the suite. Production leaves it zero.
	controlRequestTimeout time.Duration
	// controlRequestMu guards pendingControlRequests and controlRequestSeq.
	controlRequestMu sync.Mutex
	// pendingControlRequests maps an outbound control_request id to the
	// channel the read loop delivers the matching control_response on.
	// Entry is created before the write, then drained by readLoop or by the
	// caller on timeout / cancellation.
	pendingControlRequests map[string]chan *controlResponseResult
	// controlRequestSeq is a per-session counter so concurrent outbound
	// control_requests never collide on request_id. The session pointer is mixed
	// in (by map lifetime — a second session allocates a fresh map)
	// so request_ids don't need to be globally unique, only unique
	// within a single CLI subprocess.
	controlRequestSeq uint64
}

// controlResponseResult carries the outcome of an outbound control_request
// round-trip from the read loop back to the waiting caller. Exactly one of
// errMsg or ok is set: ok=true on subtype=success, errMsg populated on
// subtype=error. A nil pointer means the session closed before the response
// arrived. payload carries the inner `response.response` object — set for
// subtype=success when the request shape returns structured data (e.g.
// mcp_authenticate's {authUrl, requiresUserAction}); empty otherwise.
type controlResponseResult struct {
	ok      bool
	errMsg  string
	payload json.RawMessage
}

// pendingApproval tracks a single in-flight interactive request so user
// responses, provider cancels, and session close all resolve the same
// request ID exactly once.
type pendingApproval struct {
	resolveKind        provider.EventKind
	userInputQuestions []provider.UserInputQuestion
}

// Config for creating a Claude session.
type Config struct {
	Binary          string // default: "claude"
	Model           string
	WorkDir         string
	Resume          string // session ID to resume, empty for new
	ResumeAt        string // transcript UUID to resume at inside Resume
	ForkSession     bool
	SystemPrompt    string
	ReasoningEffort string
	FastMode        bool
	AllowedTools    []string
	// PermissionFlags carries the full permission flag sequence. Nil / empty
	// means "don't pass any permission-related flag".
	PermissionFlags    []string
	BasePermissionMode string
	InteractionMode    provider.InteractionMode
	MaxTurns           int
	// AutoCompactPercent is the autocompact threshold (1-90) the CLI
	// should use for this session, or 0 to inherit Claude's default.
	// Values >90 are clamped to 90 by `inlineSettingsForCLI` (matches
	// the upstream `normalizeAutoCompactPercent` contract and Claude's
	// own buffer-based cap). Threaded through `--settings '{"env":
	// {"CLAUDE_AUTOCOMPACT_PCT_OVERRIDE":...}}'` rather than the
	// subprocess env because Claude Code reapplies its own
	// `~/.claude/settings.json` `env` block over inherited values
	// during init (managedEnv.ts → applySafeConfigEnvironmentVariables);
	// the `flagSettings` source ranks above `userSettings` so the
	// inline env here wins. Verified against claude 2.1.118.
	AutoCompactPercent int
	// Env carries per-session environment variables Claude Code does NOT
	// override at startup — currently just CLAUDE_CODE_ENTRYPOINT (the
	// CLI's `initializeEntrypoint` skips the rewrite when the value is
	// anything other than the literal `"cli"`). Anything Claude exposes
	// via `settings.env` should go through AutoCompactPercent's inline
	// settings path instead.
	Env         map[string]string
	EventLogger *logging.Logger
	// MCPServers carries optional MCP server configs to register for
	// this session. Threaded through `--mcp-config <json>` plus
	// `--strict-mcp-config` so only the supplied servers are loaded
	// (no .mcp.json discovery from the workdir, no user-settings
	// MCP servers leaking into the agent context). Currently used
	// for the design-mode Codex HTTP MCP server, which Claude
	// consumes the same way Codex does after the v42 design rewrite.
	// Shape matches Claude Code's --mcp-config schema:
	//   {"mcpServers": {"<name>": {"url": "..."}}} for HTTP servers
	//   or {"mcpServers": {"<name>": {"command": "...", "args": [...]}}}
	//   for stdio servers. The map provided here is wrapped under
	//   "mcpServers" before serialization.
	MCPServers map[string]any
}

// NewSession spawns a Claude CLI process and starts the stdout reader goroutine.
// The init event arrives after the first Send() call, not on spawn.
func NewSession(ctx context.Context, threadID string, cfg Config, onEvent func(provider.ProviderEvent)) (*Session, error) {
	binary := cfg.Binary
	if binary == "" {
		binary = "claude"
	}

	args := buildArgs(cfg)

	childCtx, cancel := context.WithCancel(ctx)

	proc, err := provider.Spawn(childCtx, provider.SpawnConfig{
		Binary:      binary,
		Args:        args,
		Dir:         cfg.WorkDir,
		Env:         withClaudeCodeEntrypoint(cfg.Env),
		EventLogger: cfg.EventLogger,
		ThreadID:    threadID,
		Provider:    string(provider.Claude),
	})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("claude: spawn: %w", err)
	}

	s := &Session{
		proc:                  proc,
		threadID:              threadID,
		sessionID:             cfg.Resume,
		workDir:               cfg.WorkDir,
		model:                 cfg.Model,
		onEvent:               onEvent,
		cancel:                cancel,
		readDone:              make(chan struct{}),
		parser:                NewParser(),
		leafTracker:           newClaudeLeafTracker(cfg.ResumeAt),
		basePermissionMode:    normalizeClaudePermissionMode(cfg.BasePermissionMode),
		currentPermissionMode: normalizeClaudePermissionMode(cfg.BasePermissionMode),
		interactionMode:       cfg.InteractionMode,
	}
	// Seed the parser with the configured model so result usage can be
	// priced even if the init envelope lands late. The init handler still
	// overrides this when Claude echoes a different model (auto-reroute).
	s.parser.SetModel(cfg.Model)

	go s.readLoop()

	return s, nil
}

// claudeCodeEntrypointEnv tags spawned `claude` processes so the
// resulting session JSONL header records `entrypoint: agent-overflow`
// instead of the auto-detected `sdk-cli` (which the CLI's resume
// picker filters out — see [docs/references/claude.md](../../../docs/references/claude.md)).
//
// The CLI's `initializeEntrypoint` only rewrites the env value when it
// equals the literal string `"cli"`; any other preset value (including
// custom strings) survives the early return. Setting `agent-overflow`
// keeps the session resumable from a normal `claude --resume` while
// cleanly identifying our client in telemetry.
const (
	claudeCodeEntrypointEnvVar = "CLAUDE_CODE_ENTRYPOINT"
	claudeCodeEntrypointValue  = "agent-overflow"
)

// withClaudeCodeEntrypoint returns a copy of env with
// CLAUDE_CODE_ENTRYPOINT set to "agent-overflow", unless the caller
// already provided an explicit value (tests can opt out).
func withClaudeCodeEntrypoint(env map[string]string) map[string]string {
	if _, ok := env[claudeCodeEntrypointEnvVar]; ok {
		return env
	}
	merged := make(map[string]string, len(env)+1)
	for k, v := range env {
		merged[k] = v
	}
	merged[claudeCodeEntrypointEnvVar] = claudeCodeEntrypointValue
	return merged
}

// buildArgs constructs CLI flags from Config.
func buildArgs(cfg Config) []string {
	args := []string{
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		// Route tool-use approval through the CanUseTool control protocol on
		// stdin/stdout. Required for parseControlRequest to receive
		// can_use_tool events with permission_suggestions.
		"--permission-prompt-tool", "stdio",
		// Emit finer-grained content_block_delta envelopes (Gap 4: partial
		// messages). These already flow through parseStreamEvent, so no new
		// routing is needed; the flag simply increases stream fidelity.
		"--include-partial-messages",
		// Always-on. The CLI replay echo gives us a wire-confirmation point
		// that triage's pending-send correlation pairs with the AO-initiated
		// send. Without the flag, AO has no signal that the model actually
		// received the message; with the flag, the wire echoes user text
		// whose `isReplay:true` envelope we promote to `EventUserText`. The
		// flag is purely additive — non-replay user envelopes (tool_result
		// blocks) are unchanged.
		"--replay-user-messages",
		// Opt thinking text onto the wire for every model. Opus 4.7
		// defaults the underlying API `thinking.display` to `omitted`,
		// which silences `thinking_delta` events even though the
		// thinking block is still emitted (with a populated signature
		// for multi-turn replay). Older models default to `summarized`
		// so this flag is a no-op there. Hidden from `claude --help`;
		// see docs/references/claude-wire.md §"Extended thinking" for
		// the full investigation.
		"--thinking-display", "summarized",
	}

	if cfg.Model != "" {
		args = append(args, "--model", cfg.Model)
	}
	if cfg.Resume != "" {
		args = append(args, "--resume", cfg.Resume)
	}
	if cfg.Resume != "" && cfg.ResumeAt != "" {
		args = append(args, "--resume-session-at", cfg.ResumeAt)
	}
	if cfg.ForkSession {
		args = append(args, "--fork-session")
	}
	if cfg.SystemPrompt != "" {
		args = append(args, "--system-prompt", cfg.SystemPrompt)
	}
	if cfg.ReasoningEffort != "" {
		args = append(args, "--effort", cfg.ReasoningEffort)
	}
	if settingsJSON, ok := inlineSettingsForCLI(cfg); ok {
		args = append(args, "--settings", settingsJSON)
	}
	if mcpJSON, ok := mcpConfigForCLI(cfg); ok {
		// --strict-mcp-config means: only load the servers we pass on
		// the command line. No user-settings discovery, no .mcp.json
		// in the workdir. The agent only sees what we register.
		args = append(args, "--mcp-config", mcpJSON, "--strict-mcp-config")
	}
	// PermissionFlags is either nil (default CLI prompting) or a complete
	// permission-related CLI flag sequence for the selected runtime mode.
	args = append(args, cfg.PermissionFlags...)
	if cfg.MaxTurns > 0 {
		args = append(args, "--max-turns", strconv.Itoa(cfg.MaxTurns))
	}
	for _, tool := range cfg.AllowedTools {
		args = append(args, "--allowedTools", tool)
	}

	return args
}

func normalizeClaudePermissionMode(mode string) string {
	switch mode {
	case "acceptEdits", "bypassPermissions", "plan":
		return mode
	default:
		return "default"
	}
}

func (s *Session) desiredPermissionModeForTurn(mode provider.InteractionMode) string {
	if provider.NormalizeInteractionMode(string(mode)) == provider.ModePlan {
		return "plan"
	}
	return s.basePermissionMode
}

func (s *Session) getCurrentPermissionMode() string {
	s.permissionModeMu.RLock()
	defer s.permissionModeMu.RUnlock()
	return normalizeClaudePermissionMode(s.currentPermissionMode)
}

func (s *Session) setCurrentPermissionMode(mode string) {
	s.permissionModeMu.Lock()
	s.currentPermissionMode = normalizeClaudePermissionMode(mode)
	s.permissionModeMu.Unlock()
}

func (s *Session) setPermissionMode(ctx context.Context, mode string) error {
	mode = normalizeClaudePermissionMode(mode)
	if mode == s.getCurrentPermissionMode() {
		return nil
	}
	opName := "set permission mode " + mode
	res, err := s.sendControlRequest(ctx, opName, map[string]any{
		"subtype": "set_permission_mode",
		"mode":    mode,
	})
	if err != nil {
		return err
	}
	if err := interpretControlResponse(res, opName); err != nil {
		return err
	}
	s.setCurrentPermissionMode(mode)
	return nil
}

// SetInteractionMode applies a chat/plan mode change to the live Claude
// permission mode. The next Send also sets this defensively, but exposing the
// operation lets the app reflect a user toggling Plan Mode while the session is
// already running.
func (s *Session) SetInteractionMode(ctx context.Context, mode provider.InteractionMode) error {
	normalized := provider.NormalizeInteractionMode(string(mode))
	if err := s.setPermissionMode(ctx, s.desiredPermissionModeForTurn(normalized)); err != nil {
		return err
	}
	s.interactionMode = normalized
	return nil
}

// Send sends a user message. The message is written as a JSON object to stdin.
// There is intentionally no idle watchdog on the response channel — Claude
// may legitimately sit silent for long periods while waiting on a pending
// can_use_tool prompt or thinking through a hard request. The user-facing
// Stop button is the authoritative way to abort.
func (s *Session) Send(ctx context.Context, content string, opts provider.SendOptions) error {
	// Validate the client-supplied message id before any side effect
	// (permission-mode control_request, stdin write) so a malformed id
	// fails the send loudly instead of poisoning the session JSONL with a
	// uuid the revert path can never match. Reject non-canonical forms too:
	// the caller (app_send.go) stamps this exact string on the user row and
	// the message checkpoint, and the revert path matches the JSONL `uuid`
	// against that stored string byte-for-byte. Normalizing here instead
	// would desync the envelope from the pre-stamped row; sending a
	// non-canonical id as-is would bet on the CLI never canonicalizing.
	// Requiring canonical input keeps row, checkpoint, envelope, and the
	// echoed JSONL uuid identical, and turns the parsed value into a real
	// check rather than a discarded result.
	if opts.UserMessageUUID != "" {
		parsed, err := uuid.Parse(opts.UserMessageUUID)
		if err != nil {
			return fmt.Errorf("claude: invalid user message uuid %q: %w", opts.UserMessageUUID, err)
		}
		if parsed.String() != opts.UserMessageUUID {
			return fmt.Errorf("claude: user message uuid %q is not in canonical form (want %q)", opts.UserMessageUUID, parsed.String())
		}
	}
	interactionMode := opts.InteractionMode
	if interactionMode == "" {
		interactionMode = s.interactionMode
	}
	if err := s.setPermissionMode(ctx, s.desiredPermissionModeForTurn(interactionMode)); err != nil {
		return err
	}
	attachments := opts.Attachments

	message := map[string]any{
		"role": "user",
	}
	blocks := make([]map[string]any, 0, 1+len(attachments))
	if len(attachments) == 0 || strings.TrimSpace(content) != "" {
		blocks = append(blocks, map[string]any{
			"type": "text",
			"text": content,
		})
	}
	for _, attachment := range attachments {
		blocks = append(blocks, map[string]any{
			"type": "image",
			"source": map[string]any{
				"type":       "base64",
				"media_type": attachment.MimeType,
				"data":       base64.StdEncoding.EncodeToString(attachment.Data),
			},
		})
	}
	if len(blocks) == 0 {
		return fmt.Errorf("claude: user message requires text or image content")
	}
	message["content"] = blocks

	s.recordExpectedReplayParent()

	msg := map[string]any{
		"type":    "user",
		"message": message,
	}
	// Stamp the client-minted message id as the envelope's top-level
	// `uuid`. Verified behaviour (claude v2.1.150, --input-format
	// stream-json persistent mode — AO's exact flags): the CLI persists
	// this exact value as the user entry's `uuid` in its session JSONL and
	// echoes it back on the --replay-user-messages envelope, assigning only
	// `parentUuid` itself. AO relies on this so a revert can slice the
	// transcript by a uuid it knew at send time, before the replay echo
	// arrives. This is an undocumented binary contract — if the CLI version
	// moves and revert-by-uuid starts falling back to the ordinal walk,
	// re-spike per docs/references/spike-policy.md before assuming this
	// still holds. Verified behaviour + write-timing data are captured in
	// docs/references/claude-wire.md §"Outbound user message".
	if opts.UserMessageUUID != "" {
		msg["uuid"] = opts.UserMessageUUID
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("claude: marshal user message: %w", err)
	}
	if err := s.proc.WriteLine(data); err != nil {
		s.clearExpectedReplayParent()
		return err
	}
	return nil
}

// Interrupt aborts the current turn by sending a control_request with
// subtype "interrupt" and waiting for the CLI's control_response. Per
// claude-wire.md §control_request, the CLI's interrupt handler stops
// the model and reaps in-flight foreground tool subprocesses;
// backgrounded tasks (Bash run_in_background:true, Task subagents)
// survive by design and are stopped individually via stop_task.
//
// If the CLI never acks (timeout or caller-context cancellation), the
// error surfaces to the caller — the failure is the CLI's to fix
// (every Anthropic SDK uses the same control_request primitive). We
// deliberately do NOT escalate to a process kill here: a kill would
// take down backgrounded tasks too, inverting the documented
// foreground-only behaviour and silently masking a Claude Code bug.
func (s *Session) Interrupt(ctx context.Context) error {
	res, err := s.sendControlRequest(ctx, "interrupt", map[string]any{
		"subtype": "interrupt",
	})
	if err != nil {
		return err
	}
	return interpretControlResponse(res, "interrupt")
}

// StopTask kills a backgrounded Claude task (Bash with
// run_in_background:true or a Task subagent) by sending a `stop_task`
// control_request and awaiting the matching control_response. The
// `task_id` argument is the id the CLI emitted on `system/task_started`
// — the Claude control protocol accepts the same id for both task
// types.
//
// On success the CLI replies with
// `{"type":"control_response","response":{"subtype":"success", ...}}`
// and fires a follow-up `system/task_updated` with
// `patch.status:"killed"` on the normal event stream (routed through
// triage just like a natural terminal). On error the response carries
// `subtype:"error"` with a human-readable message that StopTask wraps
// into the returned error.
//
// Returns a timeout error after controlRequestTimeout (or ctx.Done) if the
// CLI never answers.
func (s *Session) StopTask(ctx context.Context, taskID string) error {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return fmt.Errorf("claude: stop_task: empty task_id")
	}
	opName := "stop_task " + taskID
	res, err := s.sendControlRequest(ctx, opName, map[string]any{
		"subtype": "stop_task",
		"task_id": taskID,
	})
	if err != nil {
		return err
	}
	return interpretControlResponse(res, opName)
}

// sendControlRequest is the shared round-trip for every outbound
// control_request the session originates (interrupt, stop_task,
// set_permission_mode). It allocates a request_id, registers the
// pending response channel, marshals + writes the envelope, and
// blocks on either ctx.Done, the configured timeout, or the matching
// control_response. Errors are wrapped with "claude: <opName>: ..."
// so callers don't repeat the prefix; the raw result is returned for
// the caller to interpret (success vs error subtype) — usually via
// interpretControlResponse, except where the caller has additional
// per-success side effects (e.g. setPermissionMode caching the new
// mode).
func (s *Session) sendControlRequest(ctx context.Context, opName string, request map[string]any) (*controlResponseResult, error) {
	requestID := s.allocateControlRequestID()
	ch := make(chan *controlResponseResult, 1)
	if !s.registerControlRequest(requestID, ch) {
		return nil, fmt.Errorf("claude: %s: session closing", opName)
	}

	msg := map[string]any{
		"type":       "control_request",
		"request_id": requestID,
		"request":    request,
	}
	data, err := json.Marshal(msg)
	if err != nil {
		s.releaseControlRequest(requestID)
		return nil, fmt.Errorf("claude: marshal %s: %w", opName, err)
	}
	if err := s.proc.WriteLine(data); err != nil {
		s.releaseControlRequest(requestID)
		return nil, fmt.Errorf("claude: write %s: %w", opName, err)
	}

	timeout := s.controlRequestTimeout
	if timeout <= 0 {
		timeout = DefaultControlRequestTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		s.releaseControlRequest(requestID)
		return nil, fmt.Errorf("claude: %s: %w", opName, ctx.Err())
	case <-timer.C:
		s.releaseControlRequest(requestID)
		return nil, fmt.Errorf("claude: %s: timeout after %s", opName, timeout)
	case res, ok := <-ch:
		// deliverControlResponse already removed the entry under lock;
		// nothing for us to release here.
		if !ok || res == nil {
			return nil, fmt.Errorf("claude: %s: session closed before response", opName)
		}
		return res, nil
	}
}

// interpretControlResponse converts a delivered control_response into
// the standard success-or-wrapped-error pair every caller needs. Used
// directly by Interrupt and StopTask; setPermissionMode inlines the
// equivalent logic because it has a per-success side effect to run.
func interpretControlResponse(res *controlResponseResult, opName string) error {
	if res.ok {
		return nil
	}
	if res.errMsg == "" {
		return fmt.Errorf("claude: %s: provider returned unspecified error", opName)
	}
	return fmt.Errorf("claude: %s: %s", opName, res.errMsg)
}

// allocateControlRequestID generates a request_id unique within the
// session. Format is a short "so-<n>" prefix so logs and wire samples
// make it clear the id originated here.
func (s *Session) allocateControlRequestID() string {
	s.controlRequestMu.Lock()
	s.controlRequestSeq++
	seq := s.controlRequestSeq
	s.controlRequestMu.Unlock()
	return fmt.Sprintf("so-%d", seq)
}

// registerControlRequest stores the pending channel under the request_id.
// Returns false when Close has run (the closing flag flipped and the
// pending map has been drained) so late control callers fail fast
// instead of parking on a channel nobody will deliver to.
//
// The closing check happens UNDER controlRequestMu so the clearPendingControlRequests
// / registerControlRequest pair serialises correctly: if Close wins the
// lock first, the registration fails; if a concurrent control request wins
// it first, the entry is visible to the subsequent clearPendingControlRequests
// drain. Without this ordering, a late registration could leak a
// pending entry past Close.
func (s *Session) registerControlRequest(requestID string, ch chan *controlResponseResult) bool {
	s.controlRequestMu.Lock()
	defer s.controlRequestMu.Unlock()
	if s.closing.Load() {
		return false
	}
	if s.pendingControlRequests == nil {
		s.pendingControlRequests = make(map[string]chan *controlResponseResult)
	}
	s.pendingControlRequests[requestID] = ch
	return true
}

// releaseControlRequest removes the pending entry and drains the channel so
// a late read-loop delivery lands in a discarded buffer. Called from
// timeout / cancel / error branches so the map never leaks entries and
// the single-slot channel never blocks a reader that already gave up.
func (s *Session) releaseControlRequest(requestID string) {
	s.controlRequestMu.Lock()
	ch, ok := s.pendingControlRequests[requestID]
	if ok {
		delete(s.pendingControlRequests, requestID)
	}
	s.controlRequestMu.Unlock()
	if !ok {
		return
	}
	select {
	case <-ch:
	default:
	}
}

// deliverControlResponse is the read-loop-side half: it matches an
// inbound control_response to a pending outbound control_request and delivers the
// result. Unknown request_ids are returned as (false) so the caller
// can log once and drop.
func (s *Session) deliverControlResponse(requestID string, res *controlResponseResult) bool {
	s.controlRequestMu.Lock()
	ch, ok := s.pendingControlRequests[requestID]
	if ok {
		delete(s.pendingControlRequests, requestID)
	}
	s.controlRequestMu.Unlock()
	if !ok {
		return false
	}
	select {
	case ch <- res:
	default:
		// Channel already drained by timeout — nothing to do.
	}
	return true
}

// clearPendingControlRequests closes every outstanding control-request waiter so
// Close doesn't strand a caller. Mirrors clearPendingApprovals.
func (s *Session) clearPendingControlRequests() {
	s.controlRequestMu.Lock()
	pending := s.pendingControlRequests
	s.pendingControlRequests = nil
	s.controlRequestMu.Unlock()
	for _, ch := range pending {
		// A nil send signals "session closing" — the caller returns a
		// clean error rather than hanging forever waiting on a
		// control_response the dead subprocess will never emit.
		select {
		case ch <- nil:
		default:
		}
	}
}

// RespondToApproval sends a tool-use approval decision back to the CLI.
// Accepts both Codex-native values (accept, acceptForSession, decline, cancel)
// and legacy values (allow, allow_session, deny) for backward compatibility.
// When resp.UpdatedInput or resp.UpdatedPermissions are non-empty and the
// decision is an allow, the raw JSON is forwarded to the CLI as the
// Claude-SDK-compatible CanUseTool response fields.
//
// Responding twice for the same RequestID returns ErrApprovalAlreadyResolved
// (Bug B9).
func (s *Session) RespondToApproval(ctx context.Context, resp provider.ApprovalResponse) error {
	if !s.claimApproval(resp.RequestID, provider.EventApprovalResolved) {
		return ErrApprovalAlreadyResolved
	}
	data, err := buildApprovalResponse(resp)
	if err != nil {
		return err
	}
	return s.proc.WriteLine(data)
}

func (s *Session) RespondToUserInput(ctx context.Context, resp provider.UserInputResponse) error {
	decision, err := provider.NormalizeUserInputDecision(resp.Decision)
	if err != nil {
		return err
	}
	questions := s.pendingUserInputQuestions(resp.RequestID)
	if !s.claimApproval(resp.RequestID, provider.EventUserInputResolved) {
		return ErrApprovalAlreadyResolved
	}
	approval := provider.ApprovalResponse{
		RequestID: resp.RequestID,
		Decision:  decision,
	}
	inputFields := map[string]any{
		"answers": claudeAskUserQuestionAnswers(questions, resp.Answers),
	}
	if len(questions) > 0 {
		inputFields["questions"] = questions
	}
	input, err := json.Marshal(inputFields)
	if err != nil {
		return fmt.Errorf("claude: marshal user input answers: %w", err)
	}
	approval.UpdatedInput = input
	data, err := buildApprovalResponse(approval)
	if err != nil {
		return err
	}
	return s.proc.WriteLine(data)
}

// claudeAskUserQuestionAnswers projects the user's selections into the shape
// Claude Code's AskUserQuestion tool consumes: question key -> answer string,
// with multi-select answers comma-joined.
//
// The comma-join is Claude Code's contract, not a lossy shortcut we picked
// (verified against the installed CLI's embedded schema, 2.1.168): the tool's
// result schema is `answers: record(string, string)` and documents
// "multi-select answers are comma-separated", so the model only ever sees a
// joined string. The injection point we actually write to (the permission
// component's updatedInput.answers) accepts an array too, but preprocesses it
// by joining with the identical ", " before validating as a string -- so
// sending map[string][]string here is accepted-but-equivalent, a no-op rather
// than a fix. There is no lossless multi-select form at the model layer; do
// not "fix" this into an array.
//
// Structured fidelity for display/history is preserved on a separate path that
// never reaches the model: triage's mergeUserInputAnswersIntoLaunch persists
// the raw per-question arrays onto item.meta.answers, which the AskUserQuestion
// history card prefers over this joined echo. That path keeps comma-containing
// labels and custom free-text intact for the UI. This function feeds the model
// only.
func claudeAskUserQuestionAnswers(questions []provider.UserInputQuestion, answers map[string]provider.UserInputAnswer) map[string]string {
	out := make(map[string]string, len(answers))
	used := make(map[string]struct{}, len(answers))
	keyCounts := claudeAskUserQuestionKeyCounts(questions)
	for _, question := range questions {
		answer, sourceKey, ok := answerForClaudeQuestion(question, answers)
		if !ok {
			continue
		}
		key := claudeAskUserQuestionAnswerKey(question, sourceKey, keyCounts)
		out[key] = strings.Join([]string(answer), ", ")
		used[sourceKey] = struct{}{}
	}
	for key, answer := range answers {
		if _, ok := used[key]; ok {
			continue
		}
		out[key] = strings.Join([]string(answer), ", ")
	}
	return out
}

func claudeAskUserQuestionKeyCounts(questions []provider.UserInputQuestion) map[string]int {
	counts := make(map[string]int, len(questions)*3)
	for _, question := range questions {
		for _, key := range []string{question.Question, question.Header, question.ID} {
			key = strings.TrimSpace(key)
			if key != "" {
				counts[key]++
			}
		}
	}
	return counts
}

func claudeAskUserQuestionAnswerKey(question provider.UserInputQuestion, sourceKey string, keyCounts map[string]int) string {
	for _, key := range []string{question.Question, question.Header, question.ID} {
		key = strings.TrimSpace(key)
		if key != "" && keyCounts[key] == 1 {
			return key
		}
	}
	if sourceKey != "" {
		return sourceKey
	}
	return strings.TrimSpace(question.ID)
}

func answerForClaudeQuestion(question provider.UserInputQuestion, answers map[string]provider.UserInputAnswer) (provider.UserInputAnswer, string, bool) {
	for _, key := range []string{question.ID, question.Header, question.Question} {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if answer, ok := answers[key]; ok {
			return answer, key, true
		}
	}
	return nil, "", false
}

// ErrApprovalAlreadyResolved is returned by RespondToApproval when the
// request ID has already been answered so callers can surface a clear
// message instead of silently shadowing the previous decision.
var ErrApprovalAlreadyResolved = fmt.Errorf("claude: approval already resolved: %w", provider.ErrStaleInteractiveRequest)

// trackPendingApproval registers a pending interactive request.
func (s *Session) trackPendingApproval(requestID string, resolveKind provider.EventKind) {
	s.trackPendingApprovalWithQuestions(requestID, resolveKind, nil)
}

func (s *Session) trackPendingApprovalWithQuestions(requestID string, resolveKind provider.EventKind, questions []provider.UserInputQuestion) {
	if requestID == "" {
		return
	}
	s.approvalsMu.Lock()
	if s.approvalsClosed {
		s.approvalsMu.Unlock()
		return
	}
	if s.pendingApprovals == nil {
		s.pendingApprovals = make(map[string]*pendingApproval)
	}
	s.pendingApprovals[requestID] = &pendingApproval{
		resolveKind:        resolveKind,
		userInputQuestions: append([]provider.UserInputQuestion(nil), questions...),
	}
	// Starting a new pending request re-opens the ID in case the provider
	// re-sent the request after a response.
	s.approvalDedup.Forget(requestID)
	s.approvalsMu.Unlock()
}

func (s *Session) pendingUserInputQuestions(requestID string) []provider.UserInputQuestion {
	s.approvalsMu.Lock()
	defer s.approvalsMu.Unlock()
	pending := s.pendingApprovals[requestID]
	if pending == nil || len(pending.userInputQuestions) == 0 {
		return nil
	}
	return append([]provider.UserInputQuestion(nil), pending.userInputQuestions...)
}

// claimApproval returns true when the caller is the first to answer the
// approval for requestID. False means either we already answered (Bug B9
// dedup) or the session is closing.
func (s *Session) claimApproval(requestID string, expectedKind provider.EventKind) bool {
	s.approvalsMu.Lock()
	defer s.approvalsMu.Unlock()
	if s.approvalDedup.IsResolved(requestID) {
		return false
	}
	pending, hadPending := s.pendingApprovals[requestID]
	if !hadPending || pending.resolveKind != expectedKind {
		return false
	}
	delete(s.pendingApprovals, requestID)
	s.approvalDedup.MarkResolved(requestID)
	return true
}

// clearPendingApprovals resolves every outstanding interactive request
// with a "lost" decision. It also drops the dedup set: once Close has
// been called, no duplicate response can land at the provider, so the
// memory cost of keeping the IDs around is pure overhead.
func (s *Session) clearPendingApprovals() {
	s.approvalsMu.Lock()
	s.approvalsClosed = true
	pending := s.pendingApprovals
	s.pendingApprovals = nil
	s.approvalDedup.Reset()
	s.approvalsMu.Unlock()
	for requestID, p := range pending {
		// Decision "lost" signals session-ended-mid-prompt to triage
		// (internal/triage/approvals.go:198 maps it to status=errored
		// in the store). Different from the user-driven "cancel" the
		// control_cancel_request handler emits — that one means the
		// CLI itself abandoned the request after an interrupt; this
		// one means the session is going away.
		metaFields := map[string]any{
			"requestId": requestID,
			"decision":  "lost",
		}
		if p.resolveKind == provider.EventUserInputResolved {
			// Frontend expects answers on UserInputResolved events; empty
			// map keeps the type contract clean even when no user reply
			// was ever submitted.
			metaFields["answers"] = map[string]any{}
		}
		meta, _ := json.Marshal(metaFields)
		s.onEvent(provider.ProviderEvent{
			Kind:      p.resolveKind,
			ThreadID:  s.threadID,
			ItemID:    requestID,
			Meta:      meta,
			Timestamp: time.Now(),
		})
	}
}

// SessionID returns the provider's session identifier.
// Only valid after the init event has been received.
func (s *Session) SessionID() string {
	return s.sessionID
}

// CanonicalLeafUUID returns the latest settled top-level Claude transcript
// UUID observed by this live session.
func (s *Session) CanonicalLeafUUID() string {
	if s == nil || s.leafTracker == nil {
		return ""
	}
	return s.leafTracker.canonicalLeaf()
}

// RequiresResumeAtBeforeUserSend reports whether the live Claude process has
// emitted an unresolved server-side tool-use row after a completed turn. In
// that state AO must restart Claude with --resume-session-at before writing a
// new user message, because stream-json stdin has no parent override.
func (s *Session) RequiresResumeAtBeforeUserSend() bool {
	if s == nil || s.leafTracker == nil {
		return false
	}
	return s.leafTracker.requiresResumeAtBeforeUserSend()
}

func (s *Session) recordExpectedReplayParent() {
	if s == nil {
		return
	}
	var parent string
	var risky bool
	if s.leafTracker != nil {
		parent = s.leafTracker.canonicalLeaf()
		risky = s.leafTracker.requiresResumeAtBeforeUserSend()
	}
	s.replayMu.Lock()
	s.expectedReplayParent = parent
	s.expectedReplayWasRisky = risky
	s.replayMu.Unlock()
}

func (s *Session) takeExpectedReplayParent() (parent string, wasRisky bool) {
	if s == nil {
		return "", false
	}
	s.replayMu.Lock()
	defer s.replayMu.Unlock()
	parent = s.expectedReplayParent
	wasRisky = s.expectedReplayWasRisky
	s.expectedReplayParent = ""
	s.expectedReplayWasRisky = false
	return parent, wasRisky
}

func (s *Session) clearExpectedReplayParent() {
	if s == nil {
		return
	}
	s.replayMu.Lock()
	s.expectedReplayParent = ""
	s.expectedReplayWasRisky = false
	s.replayMu.Unlock()
}

// PID returns the OS process id (and process-group id) of the Claude
// subprocess, or 0 when no process is live.
func (s *Session) PID() int {
	if s.proc == nil {
		return 0
	}
	return s.proc.PID()
}

// Close shuts down the CLI process gracefully.
// Closes stdin first for graceful shutdown, then cancels the context as fallback.
func (s *Session) Close() error {
	s.closing.Store(true)
	s.clearPendingApprovals()
	s.clearPendingControlRequests()
	err := s.proc.Close()
	s.cancel()
	if s.readDone != nil {
		<-s.readDone
	}
	// Release parser-owned state so the dedup sets
	// (completedToolUseIDs, completedTasks, backgroundToolUses, etc.)
	// don't linger after the readLoop exits.
	s.parser.Close()
	return err
}

// readLoop reads stdout NDJSON lines and dispatches them as ProviderEvents.
func (s *Session) readLoop() {
	defer func() {
		if s.readDone != nil {
			defer close(s.readDone)
		}

		// Release any control_request callers still parked on a pending
		// control_response. If the subprocess died on its own (io.EOF,
		// crash) Close won't be the path that drains the map, so the
		// caller would otherwise sit idle until its own timeout fires.
		// Signalling here surfaces "session closed before response"
		// within a handful of milliseconds of the subprocess exit.
		s.clearPendingControlRequests()

		// If the CLI exits while an approval or user-input request is
		// waiting, resolve it as lost so the frontend prompt does not linger.
		s.clearPendingApprovals()

		if !s.closing.Load() {
			// Any read-loop exit while we weren't the one closing is
			// abnormal — including a clean exit-code-0 without a
			// host-initiated close. Triage gates synthesizing the
			// truncated turn-complete on this "error" signal, so a
			// missed emission leaves the FE working indicator stuck.
			// WaitProcessExitErr can return nil for a clean exit or for
			// a 100ms reap timeout; MarshalProcessExitMeta handles both.
			exitErr := provider.WaitProcessExitErr(s.proc)
			s.onEvent(provider.ProviderEvent{
				Kind:      provider.EventSessionStatus,
				ThreadID:  s.threadID,
				Content:   "error",
				Meta:      provider.MarshalProcessExitMeta(exitErr),
				Timestamp: time.Now(),
			})
		}

		s.onEvent(provider.ProviderEvent{
			Kind:      provider.EventSessionStatus,
			ThreadID:  s.threadID,
			Content:   "disconnected",
			Timestamp: time.Now(),
		})
	}()

	for {
		line, err := s.proc.ReadLine()
		if err != nil {
			if err != io.EOF {
				meta, _ := json.Marshal(map[string]any{"fatal": true})
				s.onEvent(provider.ProviderEvent{
					Kind:      provider.EventError,
					ThreadID:  s.threadID,
					Content:   fmt.Sprintf("claude: read error: %v", err),
					Meta:      meta,
					Timestamp: time.Now(),
				})
			}
			return
		}

		if s.leafTracker != nil && shouldTrackClaudeLeafLine(line) {
			s.leafTracker.ingestLine(line)
		}

		// Gate control_request pre-handling on a byte-prefix check so
		// every streaming text_delta line doesn't pay an extra
		// json.Unmarshal. ParseLine below still handles the line if
		// the gate skips this branch.
		if bytes.HasPrefix(line, controlRequestPrefix) {
			var raw controlRequestEnvelope
			if err := json.Unmarshal(line, &raw); err != nil {
				log.Printf("claude: control_request handling error: %v", err)
			} else if raw.Type == "control_request" {
				handled, fatalMessage, err := s.handleControlRequest(raw)
				if err != nil {
					if fatalMessage != "" {
						meta, _ := json.Marshal(map[string]any{"fatal": true})
						s.onEvent(provider.ProviderEvent{
							Kind:      provider.EventError,
							ThreadID:  s.threadID,
							Content:   fmt.Sprintf("%s: %v", fatalMessage, err),
							Meta:      meta,
							Timestamp: time.Now(),
						})
						_ = s.proc.Close()
						return
					}
					log.Printf("claude: control_request handling error: %v", err)
				}
				if handled {
					continue
				}
			}
		}

		// Same prefix gating for control_response — the CLI emits these
		// only in reply to our outbound control_requests. Parse once
		// here and deliver to the waiting caller so we don't pay a
		// second json.Unmarshal on the streaming hot path.
		if bytes.HasPrefix(line, controlResponsePrefix) {
			s.handleControlResponseLine(line)
			continue
		}

		// control_cancel_request: the CLI is abandoning a prior
		// can_use_tool callback (typically because of an interrupt).
		// Drain our pending approval / user-input state so the
		// frontend panel clears immediately. We DO NOT write a
		// response — the CLI is not waiting for one.
		if bytes.HasPrefix(line, controlCancelRequestPrefix) {
			s.handleControlCancelRequestLine(line)
			continue
		}

		events, err := s.parser.ParseLine(s.threadID, line)
		if err != nil {
			log.Printf("claude: parse error: %v (line: %s)", err, string(line[:min(len(line), 200)]))
			continue
		}

		for _, evt := range events {
			if evt.Kind == provider.EventInit && evt.Meta != nil {
				var info provider.SessionInfo
				if json.Unmarshal(evt.Meta, &info) == nil && info.SessionID != "" {
					s.sessionID = info.SessionID
				}
			}
			if evt.Kind == provider.EventApprovalRequest && evt.ItemID != "" {
				s.trackPendingApproval(evt.ItemID, provider.EventApprovalResolved)
			}
			if evt.Kind == provider.EventUserInputRequest && evt.ItemID != "" {
				var request provider.UserInputRequest
				_ = json.Unmarshal(evt.Meta, &request)
				s.trackPendingApprovalWithQuestions(evt.ItemID, provider.EventUserInputResolved, request.Questions)
			}
			if evt.Kind == provider.EventTurnComplete && s.leafTracker != nil {
				s.leafTracker.markTurnComplete()
			}
			s.onEvent(evt)
			if evt.Kind == provider.EventUserText {
				s.verifyReplayParent(evt)
			}
		}
	}
}

func (s *Session) verifyReplayParent(evt provider.ProviderEvent) {
	expectedParent, wasRisky := s.takeExpectedReplayParent()
	if expectedParent == "" {
		return
	}
	providerItemID, parentUUID := replayProviderIDs(evt.Meta)
	if parentUUID == "" && wasRisky && providerItemID != "" && s.sessionID != "" {
		if parent, found, err := findReplayUserParent(s.sessionID, s.workDir, providerItemID); err != nil {
			s.emitReplayParentError(fmt.Sprintf("Claude replay omitted parentUuid and AO could not verify the transcript parent: %v", err))
			return
		} else if found {
			parentUUID = parent
		}
	}
	if parentUUID == "" && wasRisky {
		s.emitReplayParentError("Claude replay omitted parentUuid and AO could not verify the transcript parent")
		return
	}
	if parentUUID == "" || parentUUID == expectedParent {
		return
	}
	s.emitReplayParentError(fmt.Sprintf("Claude attached the user message to transcript parent %s, expected %s", parentUUID, expectedParent))
}

func shouldTrackClaudeLeafLine(line []byte) bool {
	return bytes.HasPrefix(line, leafAssistantPrefix) ||
		bytes.HasPrefix(line, leafUserPrefix) ||
		bytes.HasPrefix(line, leafResultPrefix)
}

func replayProviderIDs(meta json.RawMessage) (providerItemID, parentUUID string) {
	if len(meta) == 0 {
		return "", ""
	}
	var fields struct {
		ProviderItemID string `json:"provider_item_id"`
		ParentUUID     string `json:"parent_uuid"`
	}
	if err := json.Unmarshal(meta, &fields); err != nil {
		return "", ""
	}
	return strings.TrimSpace(fields.ProviderItemID), strings.TrimSpace(fields.ParentUUID)
}

func (s *Session) emitReplayParentError(message string) {
	meta, _ := json.Marshal(map[string]any{
		"fatal": true,
		"code":  "claude_context_parent_mismatch",
	})
	s.onEvent(provider.ProviderEvent{
		Kind:      provider.EventError,
		ThreadID:  s.threadID,
		Content:   message,
		Meta:      meta,
		Timestamp: time.Now(),
	})
	if s.proc != nil {
		_ = s.proc.Close()
	}
}

func (s *Session) maybeHandleFullAccessToolRequest(line []byte) (bool, error) {
	var raw controlRequestEnvelope
	if err := json.Unmarshal(line, &raw); err != nil {
		return false, err
	}
	return s.handleFullAccessToolRequest(raw)
}

func (s *Session) handleControlRequest(raw controlRequestEnvelope) (handled bool, fatalMessage string, err error) {
	handled, err = s.handleExitPlanModeRequest(raw)
	if err != nil || handled {
		if err != nil && handled {
			return handled, "claude: exit plan mode response failed", err
		}
		return handled, "", err
	}
	handled, err = s.handleFullAccessToolRequest(raw)
	if err != nil && handled {
		return handled, "claude: full-access approval response failed", err
	}
	return handled, "", err
}

func (s *Session) handleFullAccessToolRequest(raw controlRequestEnvelope) (bool, error) {
	if s.getCurrentPermissionMode() != "bypassPermissions" {
		return false, nil
	}
	if raw.Type != "control_request" || raw.Request.Subtype != "can_use_tool" {
		return false, nil
	}
	switch raw.Request.ToolName {
	case "AskUserQuestion", "ExitPlanMode":
		return false, nil
	}

	msg := map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"subtype":    "success",
			"request_id": raw.RequestID,
			"response": map[string]any{
				"behavior": "allow",
			},
		},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return true, fmt.Errorf("claude: marshal full-access approval response: %w", err)
	}
	if err := s.proc.WriteLine(data); err != nil {
		return true, fmt.Errorf("claude: send full-access approval response: %w", err)
	}
	return true, nil
}

func (s *Session) maybeHandleExitPlanModeRequest(line []byte) (bool, error) {
	var raw controlRequestEnvelope
	if err := json.Unmarshal(line, &raw); err != nil {
		return false, err
	}
	return s.handleExitPlanModeRequest(raw)
}

func (s *Session) handleExitPlanModeRequest(raw controlRequestEnvelope) (bool, error) {
	if raw.Type != "control_request" || raw.Request.Subtype != "can_use_tool" || raw.Request.ToolName != "ExitPlanMode" {
		return false, nil
	}

	planMarkdown := extractExitPlanModePlan(raw.Request.Input)
	if planMarkdown != "" {
		s.onEvent(provider.ProviderEvent{
			Kind:      provider.EventProposedPlan,
			ThreadID:  s.threadID,
			ItemID:    raw.RequestID,
			ItemType:  raw.Request.ToolName,
			Content:   planMarkdown,
			Timestamp: time.Now(),
		})
	}

	msg := map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"subtype":    "success",
			"request_id": raw.RequestID,
			"response": map[string]any{
				"behavior": "deny",
				"message":  "The client captured your proposed plan. Stop here and wait for the user's feedback or implementation request in a later turn.",
			},
		},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return true, fmt.Errorf("claude: marshal exit plan mode response: %w", err)
	}
	if err := s.proc.WriteLine(data); err != nil {
		return true, fmt.Errorf("claude: send exit plan mode response: %w", err)
	}
	return true, nil
}

// handleControlResponseLine decodes a `control_response` NDJSON line
// and routes it to the waiting control-request caller by request_id. Called
// only from the read loop's prefix-gated branch, so all the work
// happens off the streaming hot path.
//
// Unknown request_ids are logged and dropped — the CLI might emit a
// duplicate or late reply after the session has already released its
// pending entry; silently discarding it keeps the read loop alive
// while still leaving a breadcrumb. These lines are rare in practice
// (one per out-of-band reply) so the log isn't rate-limited. Malformed
// JSON is likewise logged (not fatal): a garbled control_response
// shouldn't take the subprocess down.
func (s *Session) handleControlResponseLine(line []byte) {
	var raw struct {
		Type     string `json:"type"`
		Response struct {
			Subtype   string          `json:"subtype"`
			RequestID string          `json:"request_id"`
			Error     string          `json:"error"`
			Response  json.RawMessage `json:"response"`
		} `json:"response"`
	}
	if err := json.Unmarshal(line, &raw); err != nil {
		log.Printf("claude: malformed control_response line: %v", err)
		return
	}
	if raw.Type != "control_response" {
		// Prefix-only false positive (e.g. an unrelated envelope whose
		// serialized bytes happened to start with `{"type":"control_response`
		// — shouldn't happen in practice, but the check is cheap).
		return
	}

	requestID := raw.Response.RequestID
	if requestID == "" {
		log.Printf("claude: control_response missing request_id: %s", string(line[:min(len(line), 200)]))
		return
	}

	res := &controlResponseResult{}
	switch raw.Response.Subtype {
	case "success":
		res.ok = true
		res.payload = raw.Response.Response
	case "error":
		res.errMsg = raw.Response.Error
	default:
		// The CLI only emits success / error per the wire reference;
		// unknown subtypes get recorded as errors so the waiting caller
		// surfaces a clear message rather than silently hanging.
		res.errMsg = fmt.Sprintf("unexpected control_response subtype %q", raw.Response.Subtype)
	}

	if !s.deliverControlResponse(requestID, res) {
		log.Printf("claude: control_response with no pending request_id %q (subtype=%s)", requestID, raw.Response.Subtype)
	}
}

// handleControlCancelRequestLine processes a CLI-originated
// `control_cancel_request` envelope. The CLI emits these when an
// interrupt aborts an in-flight `can_use_tool` callback — the request
// is no longer being awaited on the CLI side, so we must clear the
// matching pending approval / user-input state without writing a
// control_response.
//
// The cancellation payload mirrors t3-code's AbortSignal handlers:
// pending approvals resolve as `decision: "cancel"` (matching
// ClaudeAdapter.ts:2764 — "User cancelled tool execution."), pending
// user-inputs resolve with empty `answers: {}` (matching
// ClaudeAdapter.ts:2612). The frontend panel listens for the matching
// EventApprovalResolved / EventUserInputResolved kind and clears.
func (s *Session) handleControlCancelRequestLine(line []byte) {
	var raw struct {
		Type      string `json:"type"`
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(line, &raw); err != nil {
		log.Printf("claude: malformed control_cancel_request line: %v", err)
		return
	}
	if raw.Type != "control_cancel_request" {
		// Prefix-only false positive. Cheap to verify; cheap to drop.
		return
	}
	requestID := raw.RequestID
	if requestID == "" {
		log.Printf("claude: control_cancel_request missing request_id: %s", string(line[:min(len(line), 200)]))
		return
	}
	s.cancelPendingApproval(requestID)
}

// cancelPendingApproval clears the pending approval / user-input entry
// for requestID and emits the matching resolved event so the frontend
// panel disappears. Idempotent: if the request is already resolved or
// unknown, the call is a no-op.
func (s *Session) cancelPendingApproval(requestID string) {
	s.approvalsMu.Lock()
	if s.approvalDedup.IsResolved(requestID) {
		s.approvalsMu.Unlock()
		return
	}
	pending, ok := s.pendingApprovals[requestID]
	if !ok {
		s.approvalsMu.Unlock()
		return
	}
	delete(s.pendingApprovals, requestID)
	s.approvalDedup.MarkResolved(requestID)
	resolveKind := pending.resolveKind
	s.approvalsMu.Unlock()

	metaFields := map[string]any{
		"requestId": requestID,
	}
	switch resolveKind {
	case provider.EventUserInputResolved:
		metaFields["answers"] = map[string]any{}
		metaFields["decision"] = "cancel"
	default:
		metaFields["decision"] = "cancel"
	}
	meta, _ := json.Marshal(metaFields)
	s.onEvent(provider.ProviderEvent{
		Kind:      resolveKind,
		ThreadID:  s.threadID,
		ItemID:    requestID,
		Meta:      meta,
		Timestamp: time.Now(),
	})
}
