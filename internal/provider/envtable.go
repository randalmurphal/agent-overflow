package provider

import (
	"strconv"
	"strings"
	"sync"
)

// ReservedEnv names one environment variable Agent Overflow sets, defaults or
// clears on a provider process. Its only values are the entries this file
// declares and AOEnv, so a spawn path cannot pin, default or clear a variable
// the table does not list: there is no way to write one.
//
// The table is also what the settings layer refuses in a provider's custom
// environment (ReservedEnvRefusal). An entry with a refusal is one a user
// value would defeat or be silently discarded by; an entry without one is a
// default the user may override, a credential the user may supply, or a pin
// whose user value the app routes elsewhere.
type ReservedEnv struct{ entry *reservedEnvEntry }

type reservedEnvEntry struct {
	name string
	// provider is the settings key whose custom environment the entry
	// covers: "claude", "codex", "agy", or "" for every provider.
	provider string
	// refusal is why settings refuses a custom value; "" when it accepts one.
	refusal string
}

// Name is the variable's name. The zero ReservedEnv is the one value another
// package can write without the table, and it names no variable: Name panics
// on it, and BuildEnvironment and Fingerprint read every handle's Name.
func (e ReservedEnv) Name() string {
	if e.entry == nil {
		panic("provider: the zero ReservedEnv names no variable; use a handle declared in envtable.go or AOEnv")
	}
	return e.entry.name
}

var reservedEnvTable []*reservedEnvEntry

func reserve(providerKey, name, refusal string) ReservedEnv {
	entry := &reservedEnvEntry{name: name, provider: providerKey, refusal: refusal}
	reservedEnvTable = append(reservedEnvTable, entry)
	return ReservedEnv{entry}
}

// ReservedEnvPrefix is the namespace Agent Overflow uses for every variable it
// injects for its own contracts: the in-session marker, the test harness
// control channel, the mock provider and claude-tui's hook relay. The whole
// prefix is reserved, so a variable added to any of those contracts is
// covered the day it lands.
const ReservedEnvPrefix = "AO_"

const reservedPrefixRefusal = "the " + ReservedEnvPrefix + " prefix is how Agent Overflow passes its own session, harness, and hook-relay contract to provider processes"

var (
	aoEnvMu      sync.Mutex
	aoEnvEntries = map[string]*reservedEnvEntry{}
)

// AOEnv is the handle for a variable under ReservedEnvPrefix; one name always
// yields the same handle. Every caller passes one of Agent Overflow's own
// constant names, so a name outside the prefix is a programming error and
// panics rather than becoming a handle for a variable the table does not
// reserve.
func AOEnv(name string) ReservedEnv {
	if !strings.HasPrefix(name, ReservedEnvPrefix) {
		panic("provider: AOEnv(" + strconv.Quote(name) + "): the name lacks the " + ReservedEnvPrefix + " prefix")
	}
	aoEnvMu.Lock()
	defer aoEnvMu.Unlock()
	entry := aoEnvEntries[name]
	if entry == nil {
		entry = &reservedEnvEntry{name: name, refusal: reservedPrefixRefusal}
		aoEnvEntries[name] = entry
	}
	return ReservedEnv{entry}
}

// AOPins is m, whose names must all carry ReservedEnvPrefix (AOEnv panics on
// one that does not), as pins. It is how a boot-time environment such as the
// harness's mock-control credentials becomes EnvInput.Pins.
func AOPins(m map[string]string) map[ReservedEnv]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[ReservedEnv]string, len(m))
	for name, value := range m {
		out[AOEnv(name)] = value
	}
	return out
}

// pathName is the variable EnvInput.PathPrepend extends.
const pathName = "PATH"

const settingsBlockRefusal = "delivered through `--settings` (whose flagSettings source outranks the subprocess environment), so a value here would be silently ignored"

// Every provider.
var (
	// PATH is assembled by Agent Overflow: EnvInput.PathPrepend goes in
	// front of the inherited PATH. It has no handle, so no spawn can pin,
	// default or clear it beside the prefix.
	_ = reserve("", pathName, "PATH is assembled by Agent Overflow (the bundled `agent-overflow` CLI directory is prepended to the inherited PATH for every session); point at a different CLI with the provider's binary-path setting instead")
)

// Claude and claude-tui.
var (
	ClaudeConfigDirEnv              = reserve("claude", "CLAUDE_CONFIG_DIR", "Agent Overflow owns Claude's config home: every spawn clears this variable, and managed-account probes set it to a temporary home. Claude treats its PRESENCE (not its value) as \"non-default home\", which on macOS hashes into a different Keychain service")
	ClaudeSecureStorageConfigDirEnv = reserve("claude", "CLAUDE_SECURESTORAGE_CONFIG_DIR", "Claude 2.1.220+ keys its credential-storage (macOS Keychain service) naming off this variable when present, overriding CLAUDE_CONFIG_DIR — a custom value would make Agent Overflow's account probes write rotated tokens into the wrong account's storage")
	ClaudeEntrypointEnv             = reserve("claude", "CLAUDE_CODE_ENTRYPOINT", "Agent Overflow pins this to \"agent-overflow\" so its sessions stay listed in `claude --resume`")
	ClaudeAutoCompactPctEnv         = reserve("claude", "CLAUDE_AUTOCOMPACT_PCT_OVERRIDE", "set by the per-provider auto-compact threshold, "+settingsBlockRefusal)
	ClaudeAutoCompactWindowEnv      = reserve("claude", "CLAUDE_CODE_AUTO_COMPACT_WINDOW", "derived from the thread's context window and "+settingsBlockRefusal)
	ClaudeMaxSubagentSpawnDepthEnv  = reserve("claude", "CLAUDE_CODE_MAX_SUBAGENT_SPAWN_DEPTH", "set by the Claude subagent-limit setting, "+settingsBlockRefusal)
	ClaudeMaxConcurrentSubagentsEnv = reserve("claude", "CLAUDE_CODE_MAX_CONCURRENT_SUBAGENTS", "set by the Claude subagent-limit setting, "+settingsBlockRefusal)
	ClaudeToolMemoryLimitEnv        = reserve("claude", "CLAUDE_CODE_TOOL_MEMORY_LIMIT", "set by the Claude tool-memory-limit setting, "+settingsBlockRefusal)
	ClaudeResumeInterruptedTurnEnv  = reserve("claude", "CLAUDE_CODE_RESUME_INTERRUPTED_TURN", "Claude 2.1.236+ drops the interrupted turn's tool results from a resumed transcript when this variable is set, which Agent Overflow's resume-cursor mirror does not model, so the first start of a forked conversation under it can fail outright; the name stays unavailable until the mirror handles that case")
	ClaudeDisable1MContextEnv       = reserve("claude", "CLAUDE_CODE_DISABLE_1M_CONTEXT", "Claude 2.1.237 ignores the `[1m]` model suffix Agent Overflow appends to request the 1M-token context tier whenever this variable is set, so a value here would make the thread's context-window setting silently lie about the window the session actually got")
	ClaudeCrossSessionGateEnv       = reserve("claude", "CLAUDE_CODE_HARBOR_KITE", "set by the cross-session messaging setting, which is what opens the peer inbox; a value here would let a thread be discovered and messaged by other Claude sessions while the setting says it is off")
	ClaudePeerSessionNameEnv        = reserve("claude", "CLAUDE_CODE_SESSION_NAME", "Agent Overflow derives the peer-visible session name from the thread and passes it as `--name`, keeping it distinct per thread and in step with the thread title; a value here would name every thread the same thing to every peer")
	ClaudeUserDialogTimeoutEnv      = reserve("claude", "CLAUDE_CODE_USER_DIALOG_TIMEOUT_MS", "sets how long a HELD cross-session message waits before it is silently dropped; Agent Overflow always sends an explicit inbound policy so nothing is ever held, and a value here would only re-time a drop the user cannot see")
	ClaudeTaskListIDEnv             = reserve("claude", "CLAUDE_CODE_TASK_LIST_ID", "Agent Overflow pins this to the thread id so Claude's task list follows the thread across restarts and reverts; a value here would make every thread share one task list")
	ClaudeBaseURLEnv                = reserve("claude", "ANTHROPIC_BASE_URL", "")
	ClaudeTodoToolsEnv              = reserve("claude", "CLAUDE_CODE_ENABLE_TODO_TOOLS", "")
	ClaudeTodoReminderModeEnv       = reserve("claude", "CLAUDE_CODE_TODO_REMINDER_MODE", "")
	ClaudeBashMaxTimeoutEnv         = reserve("claude", "BASH_MAX_TIMEOUT_MS", "")
	ClaudeMCPDescriptionLengthEnv   = reserve("claude", "CLAUDE_CODE_MAX_MCP_DESCRIPTION_LENGTH", "")
	ClaudeMCPAutoBackgroundEnv      = reserve("claude", "CLAUDE_CODE_MCP_AUTO_BACKGROUND_MS", "")
)

// ClaudeMCPDescriptionLengthLimit is the value AO defaults
// ClaudeMCPDescriptionLengthEnv to. The variable caps, in characters, both an
// MCP server's instructions and each MCP tool description Claude Code shows
// the model; longer text is cut and marked truncated. Unset, the CLI caps
// instructions and up-front tool descriptions at 4096 and descriptions loaded
// through tool search at 16384 (2048 for instructions before 2.1.296); a value
// replaces both caps (2.1.300 bundle). This is the CLI's own tool-search cap,
// so deferred tool descriptions keep their limit and instructions and
// up-front descriptions rise to it. The app's MCP server instructions must fit
// under it; threadtools' instructions test enforces that.
const ClaudeMCPDescriptionLengthLimit = 16384

// ClaudeMCPDescriptionLengthValue is ClaudeMCPDescriptionLengthLimit as the
// variable's value.
var ClaudeMCPDescriptionLengthValue = strconv.Itoa(ClaudeMCPDescriptionLengthLimit)

// Codex.
var (
	CodexHomeEnv    = reserve("codex", "CODEX_HOME", "Agent Overflow owns Codex's home: every spawn clears this variable, and login / inactive-account probes set it to a temporary home holding that account's credentials")
	CodexReleaseEnv = reserve("codex", "CODEX_RELEASE", "")
)

// Antigravity.
var (
	AgyGeminiHomeEnv         = reserve("agy", "GEMINI_HOME", "Agent Overflow manages Antigravity's home isolation")
	AgyForceFileStorageEnv   = reserve("agy", "AGY_ACP_FORCE_FILE_STORAGE", "Agent Overflow controls Antigravity's credential storage isolation")
	AgyDisableAutoUpdateEnv  = reserve("agy", "AGY_CLI_DISABLE_AUTO_UPDATE", "Agent Overflow pins this so background auto-updates do not replace the running ACP server mid-session")
	AgySSHConnectionEnv      = reserve("agy", "SSH_CONNECTION", "")
	AgyAmbientCredentialEnvs = []ReservedEnv{
		reserve("agy", "GEMINI_API_KEY", ""),
		reserve("agy", "GOOGLE_API_KEY", ""),
		reserve("agy", "GOOGLE_APPLICATION_CREDENTIALS", ""),
		reserve("agy", "GOOGLE_CLOUD_PROJECT", ""),
		reserve("agy", "GOOGLE_CLOUD_LOCATION", ""),
		reserve("agy", "GOOGLE_GENAI_USE_VERTEXAI", ""),
		reserve("agy", "GCLOUD_PROJECT", ""),
		reserve("agy", "CLOUDSDK_CORE_PROJECT", ""),
	}
)

// ReservedEnvRefusal reports why the settings layer refuses name in the
// custom environment stored under providerKey ("claude", "codex" or "agy").
// Lookup is case-insensitive: settings refuses a reserved name in any case.
func ReservedEnvRefusal(providerKey, name string) (reason string, refused bool) {
	upper := strings.ToUpper(strings.TrimSpace(name))
	if strings.HasPrefix(upper, ReservedEnvPrefix) {
		return reservedPrefixRefusal, true
	}
	for _, entry := range reservedEnvTable {
		if entry.refusal != "" && entry.name == upper && (entry.provider == "" || entry.provider == providerKey) {
			return entry.refusal, true
		}
	}
	return "", false
}
