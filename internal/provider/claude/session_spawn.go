package claude

import (
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"

	"agent-overflow/internal/logging"
	"agent-overflow/internal/provider"
)

// Config for creating a Claude session.
type Config struct {
	Binary      string // default: "claude"
	Model       string
	WorkDir     string
	Resume      string // session ID to resume, empty for new
	ResumeAt    string // transcript UUID to resume at inside Resume
	ForkSession bool
	// SystemPrompt REPLACES the CLI's default system prompt. It reaches the
	// process as `--system-prompt-file <path>` (a 0600 temp file written at
	// spawn and removed on Close) rather than as an argv value — the two
	// flags are wire-identical, and the file avoids both MAX_ARG_STRLEN and
	// /proc exposure. See WriteSystemPromptFile.
	SystemPrompt string
	// OutputSchema is the inline JSON schema passed to --json-schema when
	// the session process starts. It is sticky for every turn in the session.
	OutputSchema    string
	ReasoningEffort string
	FastMode        bool
	AllowedTools    []string
	// PermissionFlags carries the full permission flag sequence. Nil / empty
	// means "don't pass any permission-related flag".
	PermissionFlags []string
	// DisallowedTools names built-in tools to remove from the session via
	// `--disallowedTools`. Spawn-time only: no control_request can add or
	// drop a tool on a live session, so a change here always requires a
	// restart (PlanLiveUpdate enforces that by comparing this field).
	DisallowedTools []string
	// DisableTodoReminders exports CLAUDE_CODE_TODO_REMINDER_MODE=off so
	// the CLI stops nudging the model to use the todo tools (see
	// claudeTodoReminderModeEnvVar). Spawn-time only, like every session
	// env value; a change lands on the next session, and PlanLiveUpdate's
	// trailing DeepEqual reports it as a restart.
	DisableTodoReminders bool
	// The Claude-only settings-block axes (see `cliInlineSettings`).
	//
	// All four are SPAWN-TIME ONLY and deliberately NOT part of
	// `provider.SessionOptions`: the App stamps them onto the Config in
	// `spawnProviderSession` from Settings, the same way Binary and Env
	// are stamped. Keeping them off SessionOptions is what makes them
	// "next sessions only" — `PlanLiveUpdate` diffs
	// `ConfigFromOptions(prev)` against `ConfigFromOptions(next)`, and
	// neither carries these fields, so editing one never queues a restart
	// on a running session. That matches the prompt-override / tool-list
	// contract in docs/specs/prompt-tool-overrides.md.
	//
	// Every one treats its ZERO VALUE as "say nothing", so the CLI's own
	// resolution stands unless the user asked otherwise.
	//
	// OutputStyle selects a built-in output style ("Concise",
	// "Proactive", "Explanatory", "Learning").
	OutputStyle string
	// CrossSessionEnabled opens Claude Code's machine-wide peer inbox:
	// the spawn exports CLAUDE_CODE_HARBOR_KITE=1 and passes `--name`, so
	// other Claude sessions on this host can find this one with
	// `ListAgents` and address it with `SendMessage`.
	//
	// UNLIKE the four axes above it, this one and CrossSessionInbound DO
	// come from provider.SessionOptions (via ConfigFromOptions), because
	// the inbox binds once during setup and nothing rebinds it live: they
	// have to be visible to PlanLiveUpdate's trailing DeepEqual so a
	// settings change converges by DEFERRED RESTART instead of never
	// converging at all.
	CrossSessionEnabled bool
	// CrossSessionInbound is the peer-message policy. Agent Overflow
	// emits "accept" or "refuse" only, and emits it ALWAYS — "refuse"
	// when CrossSessionEnabled is false, so a remotely gated inbox
	// cannot deliver into a thread the user never opted in. Empty
	// reaches the wire only for a caller that stamps Config by hand.
	CrossSessionInbound string
	// PeerSessionName is what peers see this session called in
	// `ListAgents`, rendered as `--name` when the inbox is on.
	//
	// Spawn-stamped like OutputStyle and deliberately NOT carried by
	// ConfigFromOptions — the opposite treatment from the two fields
	// above, for the opposite reason. A thread title changes after the
	// first turn, and the CLI renames a live session for free
	// (`/rename`, a local command with no model call), so a name change
	// must never reach the DeepEqual and queue a restart. See
	// session_peer.go for the live path.
	PeerSessionName string
	// MaxSubagentSpawnDepth / MaxConcurrentSubagents cap subagent
	// fan-out via CLAUDE_CODE_MAX_SUBAGENT_SPAWN_DEPTH /
	// CLAUDE_CODE_MAX_CONCURRENT_SUBAGENTS in the block's env map. Both
	// are `min: 1` on the CLI side, so zero is unsendable and means
	// "CLI default".
	MaxSubagentSpawnDepth  int
	MaxConcurrentSubagents int
	// ToolMemoryLimit caps the memory cgroup Claude installs for its tool
	// subprocesses (CLAUDE_CODE_TOOL_MEMORY_LIMIT). Effective only when
	// the CLI runs on Linux — including the WSL backend — because the
	// implementation reads /proc/self/cgroup.
	ToolMemoryLimit string
	// Thinking is the extended-thinking axis: leave it to the CLI, turn it
	// off, or pin a fixed token budget, plus whether thinking text reaches
	// the wire.
	//
	// Unlike the five spawn-only fields above it DOES come from
	// `provider.SessionOptions` (via ConfigFromOptions), and that is the
	// point: PlanLiveUpdate diffs it and ApplyLiveUpdate lands the change
	// on a running process with one `set_max_thinking_tokens`. Only the
	// return to ThinkingDefault has no wire form and falls through to the
	// restart path.
	Thinking           ThinkingConfig
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
	// ContextWindow is the thread's resolved context window in tokens
	// (200000 or 1000000). When AutoCompactPercent is set, it is also
	// rendered into the `--settings` env block as
	// CLAUDE_CODE_AUTO_COMPACT_WINDOW — on claude ≥2.1.201 auto-compact
	// is gated on an explicitly resolved auto-compact window, and
	// without this var the pct override is silently ignored (see
	// inlineSettingsForCLI). Zero omits the var.
	ContextWindow int
	// Env carries per-session environment variables Claude Code does NOT
	// override at startup. NewSession fills in AO's defaults for names
	// the caller left unset (withClaudeSessionEnvDefaults: the
	// CLAUDE_CODE_ENTRYPOINT marker, the CLAUDE_CODE_ENABLE_TODO_TOOLS
	// opt-in, and — only when DisableTodoReminders is set —
	// CLAUDE_CODE_TODO_REMINDER_MODE=off). Anything Claude exposes via `settings.env` should go
	// through AutoCompactPercent's inline settings path instead.
	Env         map[string]string
	EventLogger *logging.Logger
	// MCPServers carries optional MCP server configs to register for
	// this session. Threaded through `--mcp-config <json>`; unless
	// MergeMCPServers is true, `--strict-mcp-config` isolates the session from
	// native discovery. Design uses strict isolation while interactive chat
	// merges AO's first-party workflow proposal tool with user servers.
	// Shape matches Claude Code's --mcp-config schema:
	//   {"mcpServers": {"<name>": {"url": "..."}}} for HTTP servers
	//   or {"mcpServers": {"<name>": {"command": "...", "args": [...]}}}
	//   for stdio servers. The map provided here is wrapped under
	//   "mcpServers" before serialization.
	MCPServers map[string]any
	// MergeMCPServers keeps native user/workspace MCP discovery enabled while
	// adding MCPServers. Design sessions leave this false to retain their
	// strict, isolated server set; interactive chat uses true for AO's
	// first-party workflow proposal tool.
	MergeMCPServers bool
}

// claudeSpawnEnv and claudeSpawnUnsetEnv are the two halves of a Claude
// child's environment, named so a test can assert what the child actually
// receives without spawning one. The pair is what makes "off means off"
// true: the overrides SET the gate when the feature is on, and the unset
// list REMOVES an inherited gate in both states — a variable AO never set
// but the host exported would otherwise bind the peer inbox behind the
// setting's back (see CrossSessionUnsetEnv).
func claudeSpawnEnv(cfg Config) map[string]string {
	return withClaudeSessionEnvDefaults(withClaudeCrossSessionEnv(cfg.Env, cfg), cfg.DisableTodoReminders)
}

func claudeSpawnUnsetEnv() []string {
	return append([]string{"CLAUDE_CONFIG_DIR", "CLAUDE_SECURESTORAGE_CONFIG_DIR"}, CrossSessionUnsetEnv()...)
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
//
// The value itself is `provider.ClaudeEntrypointOrigin`: the session
// importer reads it back out of transcripts, so writer and reader share
// one declaration rather than two literals that can drift apart.
const claudeCodeEntrypointEnvVar = "CLAUDE_CODE_ENTRYPOINT"

// claudeTodoToolsEnvVar opts the session into the TodoWrite / Task*
// tool surface. Claude ≥2.1.233 removes those tools for modern models
// (opus ≥4.8; sonnet/fable/mythos ≥5 — older families keep them
// unconditionally) unless the session opts back in: this variable
// (truthy: 1/true/yes/on), naming one of the five tools in
// --allowedTools, or a remote feature gate AO cannot depend on
// (spike-verified on 2.1.233: sonnet-5 init listed no
// TaskCreate/TaskUpdate/TaskGet/TaskList without the var, all four
// with it). AO's activity-rail todo list is built on the events those
// tools produce, so a session spawned without the opt-in silently
// loses the feature.
//
// Deliberately NOT in provider.ReservedEnvNames: it is a default, not
// a pin — a user who wants the vendor's stock tool surface can set it
// to "false" in the provider's custom environment, which lands in
// cfg.Env before withClaudeSessionEnvDefaults applies.
const claudeTodoToolsEnvVar = "CLAUDE_CODE_ENABLE_TODO_TOOLS"

// claudeTodoReminderModeEnvVar controls the CLI's periodic "track your
// work with the todo tools" nudge. "off" silences it while keeping the
// tools; unset, the CLI defaults the mode from a remote feature gate
// (baseline = nudge every 10 turns without a task write, verified on
// 2.1.233 binary analysis). Exported into the session only when the
// user turned reminders off in Settings (Config.DisableTodoReminders),
// and like the opt-in above it is a default, not a pin: a user value in
// the provider's custom environment wins.
const claudeTodoReminderModeEnvVar = "CLAUDE_CODE_TODO_REMINDER_MODE"

// withClaudeSessionEnvDefaults returns a copy of env with AO's
// session-environment defaults applied — each only when the caller did
// not already provide the name, so user custom env (and tests) can opt
// out per variable.
func withClaudeSessionEnvDefaults(env map[string]string, disableTodoReminders bool) map[string]string {
	defaults := []struct{ name, value string }{
		{claudeCodeEntrypointEnvVar, provider.ClaudeEntrypointOrigin},
		{claudeTodoToolsEnvVar, "true"},
	}
	if disableTodoReminders {
		defaults = append(defaults, struct{ name, value string }{claudeTodoReminderModeEnvVar, "off"})
	}
	missing := 0
	for _, d := range defaults {
		if _, ok := env[d.name]; !ok {
			missing++
		}
	}
	if missing == 0 {
		return env
	}
	merged := make(map[string]string, len(env)+missing)
	for k, v := range env {
		merged[k] = v
	}
	for _, d := range defaults {
		if _, ok := merged[d.name]; !ok {
			merged[d.name] = d.value
		}
	}
	return merged
}

// WriteSystemPromptFile materializes cfg.SystemPrompt for
// `--system-prompt-file`, returning "" for a session with no override.
//
// The two flags are wire-equivalent — `--system-prompt <text>` and
// `--system-prompt-file <path>` produce byte-identical API requests
// (verified on claude 2.1.234, docs/references/claude-wire.md
// §"System prompt assembly") — so this is purely about what argv can
// safely carry, and argv is the wrong channel for it twice over:
//
//   - Linux caps a single argv string at MAX_ARG_STRLEN (128KB, not
//     tunable). A rendered override that crosses it makes EVERY spawn fail
//     with E2BIG, which the user would see as a session that simply refuses
//     to start. The file has no such ceiling.
//   - argv is world-readable through /proc/<pid>/cmdline. A system prompt
//     carries workspace paths, git state, and whatever the user wrote into
//     it; the file is 0600 (os.CreateTemp's mode) and readable only by the
//     user running AO.
//
// The caller owns removal: Close for a live session, RemoveSystemPromptFile
// on every failed-spawn path.
//
// Exported for internal/provider/claudetui, which passes the same flag on
// the PTY launch (the interactive TUI honors `--system-prompt-file`
// identically — spike-verified 2.1.234). One writer means one temp-file
// name, one mode, and one removal contract for both Claude transports.
func WriteSystemPromptFile(prompt string) (string, error) {
	if prompt == "" {
		return "", nil
	}
	f, err := os.CreateTemp("", "ao-claude-system-prompt-*.txt")
	if err != nil {
		return "", fmt.Errorf("create system prompt file: %w", err)
	}
	path := f.Name()
	_, writeErr := f.WriteString(prompt)
	closeErr := f.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		RemoveSystemPromptFile(path)
		return "", fmt.Errorf("write system prompt file %s: %w", path, err)
	}
	return path, nil
}

// RemoveSystemPromptFile drops the temp file a session spawned with.
// Best-effort: the CLI has already read it, so a failure costs a stray file
// in the temp directory, not correctness — but it is logged rather than
// swallowed, because a removal that keeps failing means the temp directory
// is accumulating prompts.
func RemoveSystemPromptFile(path string) {
	if path == "" {
		return
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		log.Printf("claude: remove system prompt file %s: %v", path, err)
	}
}

// buildArgs constructs CLI flags from Config. systemPromptPath is the file
// WriteSystemPromptFile produced for cfg.SystemPrompt (empty when the
// session carries no override) — the prompt reaches the CLI by path, never
// as an argv value.
func buildArgs(cfg Config, systemPromptPath string) []string {
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
		// Always-on. An agent launched with an explicit
		// `run_in_background: false` — an INLINE, awaited agent — runs the
		// CLI's synchronous Task path, whose progress emitter drops every
		// content block that is not a tool_use or tool_result before it
		// reaches the parent stream (2.1.237 bundle: `if(!forwardSubagentText
		// && block.type!=="tool_use" && block.type!=="tool_result") continue`).
		// Its prose, thinking, and final answer exist only in the agent's own
		// sidechain JSONL, so the agent pane showed a wall of tool rows and
		// nothing else. Agents that default to background take a different
		// emitter that forwards every message, which is why only the inline
		// ones looked starved. The flag lifts the filter; the CLI has carried
		// it since 2.1.211 (bisected against published linux-x64 builds) and
		// it requires the print + stream-json output this arg list already
		// establishes. Verified live 2026-08-23: same inline Explore agent
		// emits `assistant/thinking` + `assistant/text` parented to the launch
		// with the flag, nothing without it. See
		// docs/references/claude-wire.md §"Subagent stream forwarding".
		"--forward-subagent-text",
		// Always-on provider-pushed transcript writes. Claude's local slash
		// command runner consumes a forked skill's ordinary stdout stream, and
		// a foreground agent moved to the background stops forwarding its
		// sidechain after the transition. transcript_mirror is the only live
		// wire surface that still carries those rows. AO never reads the path
		// named by a frame; it consumes the bounded entries supplied on stdout.
		// The flag is SDK-internal but is the exact surface Claude's own
		// ProcessTransport enables for SessionStore.
		"--session-mirror",
	}
	// The thinking axis. With nothing configured this renders exactly the
	// `--thinking-display summarized` this list used to carry inline: opt
	// thinking text onto the wire for every model, because Opus 4.7
	// defaults the underlying API `thinking.display` to `omitted`, which
	// silences `thinking_delta` events even though the thinking block is
	// still emitted (with a populated signature for multi-turn replay).
	// Older models default to `summarized` so it is a no-op there. Hidden
	// from `claude --help`; see docs/references/claude-wire.md
	// §"Extended thinking" for the full investigation.
	args = append(args, thinkingArgs(cfg.Thinking)...)
	// The peer-visible name. Empty (or cross-session off) renders
	// nothing, and the CLI then falls back to the cwd basename — which
	// would give every thread of a project the same address.
	args = append(args, peerSessionNameArgs(cfg)...)

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
	// The prompt itself never reaches argv — see WriteSystemPromptFile.
	if systemPromptPath != "" {
		args = append(args, "--system-prompt-file", systemPromptPath)
	}
	if cfg.OutputSchema != "" {
		args = append(args, "--json-schema", cfg.OutputSchema)
	}
	if cfg.ReasoningEffort != "" {
		args = append(args, "--effort", cfg.ReasoningEffort)
	}
	if settingsJSON, ok := inlineSettingsForCLI(cfg); ok {
		args = append(args, "--settings", settingsJSON)
	}
	if mcpJSON, ok := mcpConfigForCLI(cfg); ok {
		args = append(args, "--mcp-config", mcpJSON)
		if !cfg.MergeMCPServers {
			// Design-mode isolation: only load the servers passed above.
			args = append(args, "--strict-mcp-config")
		}
	}
	// PermissionFlags is either nil (default CLI prompting) or a complete
	// permission-related CLI flag sequence for the selected runtime mode.
	args = append(args, cfg.PermissionFlags...)
	for _, tool := range cfg.DisallowedTools {
		args = append(args, "--disallowedTools", tool)
	}
	if cfg.MaxTurns > 0 {
		args = append(args, "--max-turns", strconv.Itoa(cfg.MaxTurns))
	}
	for _, tool := range cfg.AllowedTools {
		args = append(args, "--allowedTools", tool)
	}

	return args
}
