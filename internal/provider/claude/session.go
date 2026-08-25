package claude

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"agent-overflow/internal/provider"
)

// Compile-time guarantee that *Session satisfies the provider.Session
// interface the app layer calls into. Changing any of the methods in a
// way that breaks the contract is caught at build time.
var _ provider.Session = (*Session)(nil)

// Session manages a Claude Code CLI subprocess.
type Session struct {
	proc     *provider.Process
	threadID string
	// sessionID is the CLI's session identifier — seeded from Config.Resume
	// at construction and overwritten by every `system/init` on the READ
	// LOOP, while SessionID() and the replay-parent lookup in
	// session_send.go read it from binding goroutines. Atomic for the same
	// reason codex's codexThreadID is: the two sides share no lock.
	sessionID atomic.Pointer[string]
	workDir   string
	onEvent   func(provider.ProviderEvent)
	cancel    context.CancelFunc
	closing   atomic.Bool
	readDone  chan struct{}
	// systemPromptPath is the temp file cfg.SystemPrompt was written to for
	// `--system-prompt-file`, or "" when the session carries no override.
	// Removed by Close; see WriteSystemPromptFile for why the prompt does
	// not travel in argv.
	systemPromptPath string
	// allowsBypassPermissions records whether the process was spawned with
	// --allow-dangerously-skip-permissions. The CLI rejects a live
	// set_permission_mode escalation to bypassPermissions without it
	// (verified on 2.1.205), so ApplyLiveUpdate routes that transition to
	// the restart path instead. Written once at construction.
	allowsBypassPermissions bool
	// spawnedWithFastModeOptIn records whether the process was spawned with
	// the `--settings '{"fastMode":true}'` SDK opt-in. Without it the CLI
	// answers `/fast` with "Fast mode is not available in the Agent SDK"
	// (fast_mode_disabled_reason "sdk_opt_in_required", verified 2.1.219),
	// so ApplyLiveUpdate routes a live fast-mode ENABLE on such a process to
	// the restart path — the respawn is what adds the opt-in. Written once
	// at construction, like allowsBypassPermissions.
	spawnedWithFastModeOptIn bool
	// configModelMu guards configModel AND requestedEffort (below) — the
	// two halves of "what AO has asked this process to run", read together
	// by the get_settings override projection, so they take one lock.
	//
	// configModel is the model string this session is currently configured
	// to run — seeded from Config.Model at spawn and updated by every acked
	// set_model. ApplyLiveUpdate reads it to refuse an /effort apply against
	// a model that declares no reasoning tiers (the planner never produces
	// one, but the gate must not live in caller discipline alone).
	configModelMu sync.Mutex
	configModel   string
	// requestedEffort is the reasoning tier AO has ASKED this session to
	// run — seeded from Config.ReasoningEffort and advanced by each
	// /effort send. Send-side intent, deliberately not a report of what the
	// CLI adopted: its only consumer is the get_settings read-back, which
	// compares it against what a project `.claude/settings.json` carries to
	// answer "is the repository overriding what AO asked for". Guarded by
	// configModelMu, which already covers the other half of that question.
	requestedEffort string
	// advertisedCommandsMu guards advertisedCommands: the provider-executed
	// slash-command NAMES this session most recently advertised — seeded by
	// `system/init.slash_commands` and REPLACED wholesale by
	// `system/commands_changed` (the wire contract is replace, never merge).
	// Written on the read loop; read by ApplyLiveUpdate on binding
	// goroutines to gate the /effort and /fast live applies. Nil until the
	// first init lands, and supportsSlashCommand treats nil as "unknown" —
	// callers fall back to the restart path rather than assuming a command
	// exists.
	advertisedCommandsMu sync.Mutex
	advertisedCommands   map[string]struct{}
	// liveStateMu guards the wire-observed facts about the running process
	// that are neither launch config nor parser state: the CLI version it
	// reported on `system/init`, whether it answered `get_settings` with an
	// unsupported-subtype error, the settings readback's `applied` values,
	// and any project-level settings source found overriding AO's intent.
	// Written on the read loop and by control round-trips on binding
	// goroutines; read by ApplyLiveUpdate and the app-side confirmation
	// path.
	liveStateMu            sync.Mutex
	cliVersion             string
	getSettingsUnsupported bool
	appliedSettings        *AppliedSettings
	settingsOverrides      []SettingsOverrideNotice
	// capabilities / capabilitiesLogged hold `system/init.capabilities`.
	// Behaviour and lifecycle live in session_capabilities.go; they sit
	// here only because the fields must be declared with the struct.
	capabilities       map[string]struct{}
	capabilitiesLogged bool
	// basePermissionMode is the runtime/access mode restored after a plan
	// turn. currentPermissionMode mirrors the last successful
	// set_permission_mode request so we avoid redundant control round-trips.
	// Both are guarded by permissionModeMu: basePermissionMode is mutable
	// post-construction via setBasePermissionMode (live runtime-mode
	// changes).
	permissionModeMu      sync.RWMutex
	basePermissionMode    string
	currentPermissionMode string
	interactionMode       provider.InteractionMode
	// parser holds per-session NDJSON parse state so tool_use / tool_result
	// pairs can share metadata (e.g. the `is_background` flag) across the
	// two messages that carry them.
	parser *Parser
	// issuedCommands is the ledger of command uuids THIS app put on the
	// wire, and the whole basis for telling a peer-started turn from one
	// of ours (session_peer.go). Mutex-guarded: Send writes it from caller
	// goroutines while the lifecycle parse reads it from the read loop.
	issuedCommands issuedCommandUUIDs
	// suppressedCommands is the ledger of command uuids whose `<synthetic>`
	// output must NOT become a transcript row — AO's own bookkeeping
	// commands, and commands whose only output restates state AO renders
	// itself (command_result_suppression.go). Same shape and same
	// concurrency story as issuedCommands, and released by the same
	// terminal lifecycle frame.
	suppressedCommands suppressedCommandUUIDs
	// directCommands records every command-shaped send that is intentionally
	// entering Claude's native command router. transcript_mirror uses this
	// ledger to turn a forked built-in command into a Skill launch without a
	// maintained command-name list.
	directCommands directSlashCommands
	// crossSessionEnabled records whether this process joined Claude
	// Code's machine-wide peer inbox at spawn. Immutable for the
	// session's life — the inbox binds once during setup and no control
	// request rebinds it — which is why the settings axis converges by
	// deferred restart.
	crossSessionEnabled bool
	// peerRenameSendMu serializes the whole stage-and-write of a
	// `/rename`, so the LAST rename to stage is also the last to reach
	// stdin. Held across Session.Send, and never acquired while
	// peerNameMu is held. Without it two concurrent renames can interleave
	// as stage A, stage+send B, send A: the CLI then ends on A while the
	// cache holds B, and every later reconcile no-ops because the wanted
	// name already matches.
	peerRenameSendMu sync.Mutex
	// peerSessionName is the name the CLI has CONFIRMED it answers to —
	// read back out of the `/rename` command's own output, not assumed from
	// the request (session_peer.go parsePeerRenameAssignedName). peerNameSeq
	// is the staging order of the rename that put it there.
	// pendingPeerRenames holds every rename still in flight, keyed by its
	// command uuid, so each terminal frame resolves ITS OWN rename rather
	// than whichever one happened to occupy a single slot. A name is
	// promoted only when its bracket completes: a `cancelled` /
	// `discarded` rename left the registry on the OLD name, and caching
	// the new one would make every later reconcile no-op forever.
	// Promotion is monotonic in staging order (peerNameSeq), which is what
	// keeps a late frame for a superseded rename from reinstating a stale
	// name over a newer one.
	// All of these are guarded together because the rename path runs off
	// the read loop and the settle runs on it.
	//
	// peerRenameSettledName is the last name AO REQUESTED whose bracket the
	// CLI completed, which is a different question from peerSessionName and
	// has to be tracked separately: the CLI can complete a `/rename` under a
	// name of its own choosing (a collision yields to a suffixed variant), so
	// "what do peers address" and "is there any point sending this name
	// again" stop being the same string exactly when it matters.
	peerNameMu            sync.Mutex
	peerSessionName       string
	peerRenameSettledName string
	peerNameSeq           uint64
	peerRenameSeq         uint64
	pendingPeerRenames    map[string]pendingPeerRename
	// leafTracker tracks the canonical settled top-level Claude transcript
	// UUID. Unresolved server-side tool-use rows are kept out of this leaf
	// so a later user send can be forced back onto the real continuation.
	leafTracker *claudeLeafTracker
	// replayMu guards the expected parents for AO-authored replay user
	// echoes, keyed by the client-minted message uuid the echo carries back
	// as provider_item_id. Claude stream-json gives us no send-time parent
	// override, so the replay echo is the earliest confirmation that the
	// live process attached the user message to the expected transcript
	// leaf. Keyed rather than a single slot because senders overlap: a
	// config-command send (live_update.go) can land between a queued user
	// message and its echo, and a slot would cross the two verifications.
	// expectedReplayOrder tracks insertion order for the size cap — an
	// entry whose echo never arrives (the CLI cancelled the queued
	// message) would otherwise leak.
	replayMu             sync.Mutex
	expectedReplayByUUID map[string]replayExpectation
	expectedReplayOrder  []string
	// approvals is the outstanding-interactive-request ledger: which
	// can_use_tool / AskUserQuestion requests are unanswered, and who is
	// allowed to answer each one. Shared with codex — it owns its own leaf
	// lock and the answered-id dedup that makes a second response return
	// ErrApprovalAlreadyResolved (Bug B9) instead of writing another
	// control_response to the CLI. The Claude-specific halves (the
	// AskUserQuestion answer projection, the control_cancel_request wiring)
	// stay in session_approvals.go.
	approvals provider.ApprovalRegistry
	// controlRequestTimeout overrides DefaultControlRequestTimeout when non-zero.
	// Tests set this to a short window so a non-responsive fake CLI
	// doesn't stall the suite. Production leaves it zero.
	controlRequestTimeout time.Duration
	// controlRequestMu guards pendingControlRequests and controlRequestSeq.
	controlRequestMu sync.Mutex
	// pendingControlRequests maps an outbound control_request id to the
	// channel the read loop delivers the matching control_response on,
	// plus whether the request was an `interrupt` (the read loop flags
	// the parser on a successful interrupt ack so the following result
	// envelope classifies as user-aborted; see Parser.MarkInterruptAcked).
	// Entry is created before the write, then drained by readLoop or by the
	// caller on timeout / cancellation.
	pendingControlRequests map[string]pendingControlRequest
	// controlRequestSeq is a per-session counter so concurrent outbound
	// control_requests never collide on request_id. The session pointer is mixed
	// in (by map lifetime — a second session allocates a fresh map)
	// so request_ids don't need to be globally unique, only unique
	// within a single CLI subprocess.
	controlRequestSeq uint64
}

// NewSession spawns a Claude CLI process and starts the stdout reader goroutine.
// The init event arrives after the first Send() call, not on spawn.
func NewSession(ctx context.Context, threadID string, cfg Config, onEvent func(provider.ProviderEvent)) (*Session, error) {
	if onEvent == nil {
		return nil, fmt.Errorf("claude: onEvent callback is required")
	}
	binary := cfg.Binary
	if binary == "" {
		binary = "claude"
	}

	systemPromptPath, err := WriteSystemPromptFile(cfg.SystemPrompt)
	if err != nil {
		return nil, fmt.Errorf("claude: %w", err)
	}
	args := buildArgs(cfg, systemPromptPath)

	childCtx, cancel := context.WithCancel(ctx)

	proc, err := provider.Spawn(childCtx, provider.SpawnConfig{
		Binary:      binary,
		Args:        args,
		Dir:         cfg.WorkDir,
		Env:         claudeSpawnEnv(cfg),
		UnsetEnv:    claudeSpawnUnsetEnv(),
		EventLogger: cfg.EventLogger,
		ThreadID:    threadID,
		Provider:    string(provider.Claude),
	})
	if err != nil {
		cancel()
		RemoveSystemPromptFile(systemPromptPath)
		return nil, fmt.Errorf("claude: spawn: %w", err)
	}

	s := &Session{
		proc:                     proc,
		systemPromptPath:         systemPromptPath,
		threadID:                 threadID,
		workDir:                  cfg.WorkDir,
		onEvent:                  onEvent,
		cancel:                   cancel,
		readDone:                 make(chan struct{}),
		parser:                   NewParser(),
		leafTracker:              newClaudeLeafTracker(cfg.ResumeAt),
		allowsBypassPermissions:  slices.Contains(cfg.PermissionFlags, "--allow-dangerously-skip-permissions"),
		spawnedWithFastModeOptIn: cfg.FastMode,
		configModel:              cfg.Model,
		requestedEffort:          cfg.ReasoningEffort,
		basePermissionMode:       normalizeClaudePermissionMode(cfg.BasePermissionMode),
		currentPermissionMode:    normalizeClaudePermissionMode(cfg.BasePermissionMode),
		interactionMode:          cfg.InteractionMode,
		crossSessionEnabled:      cfg.CrossSessionEnabled,
		peerSessionName:          SanitizePeerSessionName(cfg.PeerSessionName),
		peerRenameSettledName:    SanitizePeerSessionName(cfg.PeerSessionName),
	}
	if cfg.Resume != "" {
		s.setSessionID(cfg.Resume)
	}
	// Seed the parser with the configured model so result usage can be
	// priced even if the init envelope lands late. The init handler still
	// overrides this when Claude echoes a different model (auto-reroute).
	s.parser.SetModel(cfg.Model)
	// ParseLine feeds the leaf tracker from its own decoded map so
	// assistant/user/result lines aren't unmarshaled twice per line.
	s.parser.leafTracker = s.leafTracker
	// The command_lifecycle parse asks the session whether AO minted a
	// given command uuid; a uuid we never issued on a cross-session-enabled
	// process is a turn another Claude session started (session_peer.go).
	s.parser.peerTurns = s

	go s.readLoop()

	return s, nil
}

// SessionID returns the provider's session identifier.
// Only valid after the init event has been received.
func (s *Session) SessionID() string {
	if id := s.sessionID.Load(); id != nil {
		return *id
	}
	return ""
}

// setSessionID records the CLI-reported session id. Called from the
// constructor (Config.Resume seed) and the read loop's init handler.
func (s *Session) setSessionID(id string) {
	s.sessionID.Store(&id)
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
	// After the process is gone: the CLI reads the file at startup, but
	// removing it while a wedged process might still be starting up would
	// turn a slow spawn into a missing system prompt.
	RemoveSystemPromptFile(s.systemPromptPath)
	// Release parser-owned state so the dedup sets
	// (completedToolUseIDs, completedTasks, backgroundToolUses, etc.)
	// don't linger after the readLoop exits.
	s.parser.Close()
	return err
}
