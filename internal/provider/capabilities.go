package provider

type ModelCatalogSource string

const (
	// StaticModelCatalog — the shipped list is the whole answer.
	StaticModelCatalog ModelCatalogSource = "static"
	// CodexLiveModelCatalog — app-server `model/list` REPLACES the shipped
	// list; a model missing from it is authoritatively unavailable.
	CodexLiveModelCatalog ModelCatalogSource = "codex_live"
	// ClaudeProbeEnrichedCatalog — the shipped list is merged with the models
	// the zero-token account probe's `initialize` response reports. The wire
	// ENRICHES (capability flags) and EXTENDS (models we don't ship yet) the
	// lookup list and decides which rows pickers offer; rows it omits stay for
	// capability lookup, marked Unlisted. See internal/claudemodels.
	ClaudeProbeEnrichedCatalog ModelCatalogSource = "claude_probe"
	// ACPLiveModelCatalog — discovered live over ACP configOptions (zero-token
	// probe on the live or ephemeral ACP connection).
	ACPLiveModelCatalog ModelCatalogSource = "acp_live"
)

// IsLive reports whether the provider's model catalog is fetched live from the
// provider runtime (so remembered user choices outlive temporary catalog
// unavailability instead of being coerced against a static list).
func (s ModelCatalogSource) IsLive() bool {
	return s == CodexLiveModelCatalog || s == ACPLiveModelCatalog
}

type BackgroundTerminalCleaner string

const (
	NoBackgroundTerminalCleaner    BackgroundTerminalCleaner = ""
	CodexBackgroundTerminalCleaner BackgroundTerminalCleaner = "codex"
)

type ImageIngestion string

const (
	// InlineImageIngestion (the zero value, the safe default) sends image bytes
	// inline as base64 in the message body — headless Claude, whose Anthropic
	// Messages API has no local-path image source.
	InlineImageIngestion ImageIngestion = ""
	// PathImageIngestion sends each image's on-disk PATH and lets the provider read
	// the file itself — claude-tui pastes the path into the real TUI composer, and
	// Codex takes a `localImage` input item (which also earns Codex's native
	// numbered "<image name=…>" tag). The app resolves these attachments to a Path
	// rather than bytes (resolveSendMessageAttachments).
	PathImageIngestion ImageIngestion = "path"
)

// ResumeTranscript is where a provider keeps the transcript a resume reads,
// for a check before a start tears the running session down.
type ResumeTranscript string

const (
	// NoResumeTranscript: nothing to check before the start. Codex refuses
	// thread/resume of a thread it no longer has, after the start.
	NoResumeTranscript ResumeTranscript = ""
	// ClaudeProjectsTranscript: a session JSONL under the account's Claude
	// projects directory, which sessionfork.ResumeTranscriptGone checks.
	ClaudeProjectsTranscript ResumeTranscript = "claude_projects"
)

// RollbackGranularity describes the unit at which a provider can truncate its
// native conversation history during revert / edit-and-resend.
type RollbackGranularity string

const (
	RollbackNone RollbackGranularity = ""
	// RollbackTurn truncates on a turn boundary (Codex thread/revert & fork).
	RollbackTurn RollbackGranularity = "turn"
	// RollbackItem truncates at an exact user message item (Claude JSONL slice).
	RollbackItem RollbackGranularity = "item"
)

// Capabilities names provider-level behavior that app code can depend on.
// Concrete protocol routing still belongs at the call site or in
// provider-specific code, so fields that require provider-specific plumbing use
// typed values instead of generic booleans.
type Capabilities struct {
	ModelCatalog              ModelCatalogSource
	BackgroundTerminalCleaner BackgroundTerminalCleaner
	ImageIngestion            ImageIngestion
	// EnforcesRuntimeMode reports whether the provider's session config
	// actually applies the thread's RuntimeMode. False means the thread row's
	// runtime mode is inert for this provider — permissions are governed
	// somewhere we do not drive (claude-tui hands them to the real TUI).
	//
	// Callers that treat a runtime mode as a guarantee rather than a
	// preference — a workflow phase's `access` declaration is the one that
	// exists today — must refuse to start on a provider where this is false,
	// instead of running unenforced under a mode nothing reads.
	EnforcesRuntimeMode bool
	// ResumeTranscript says how a start can tell, before it stops anything,
	// that the provider no longer has the session it resumes.
	ResumeTranscript ResumeTranscript
	// UsageFamily normalizes dependent provider surfaces (like claude-tui) to
	// their parent billing/usage family.
	UsageFamily ProviderKind
	// StandardContextWindow and ExtendedContextWindow are the provider-level
	// fallback context windows when a model has no explicit window options.
	StandardContextWindow int
	ExtendedContextWindow int
	// SupportsAppManagedMCP reports whether the provider accepts AO's
	// loopback HTTP MCP servers (browser, remote tools, thread tools).
	SupportsAppManagedMCP bool
	// SupportsWorkflowMCP reports whether the provider accepts ao-workflow-tools.
	SupportsWorkflowMCP bool
	// SupportsAppendSystemPrompt reports whether the provider can append
	// instructions / agent guide text to its system prompt.
	SupportsAppendSystemPrompt bool
	// SupportsOwnMemory reports whether the provider has a native memory
	// feature that AO can toggle off for assistants.
	SupportsOwnMemory bool
	// RollbackGranularity reports whether and at what granularity the provider
	// supports anchored conversation rollback.
	RollbackGranularity RollbackGranularity
}

// SupportsConversationRollback reports whether the provider supports anchored
// conversation revert / edit-and-resend.
func (c Capabilities) SupportsConversationRollback() bool {
	return c.RollbackGranularity != RollbackNone
}

func CapabilitiesForProvider(providerName string) Capabilities {
	switch providerName {
	case string(Codex):
		return Capabilities{
			ModelCatalog:               CodexLiveModelCatalog,
			BackgroundTerminalCleaner:  CodexBackgroundTerminalCleaner,
			ImageIngestion:             PathImageIngestion,
			EnforcesRuntimeMode:        true,
			UsageFamily:                Codex,
			StandardContextWindow:      CodexStandardContextWindow,
			ExtendedContextWindow:      CodexExtendedContextWindow,
			SupportsAppManagedMCP:      true,
			SupportsWorkflowMCP:        true,
			SupportsAppendSystemPrompt: true,
			SupportsOwnMemory:          true,
			RollbackGranularity:        RollbackTurn,
		}
	case string(ClaudeTUI):
		// Same model catalog as headless claude (one binary, one login, so the
		// same probe answer enriches both) and no background-terminal cleaner,
		// but ingests images by pasting their on-disk path into the real TUI
		// composer. claudetui.ConfigFromOptions keeps only model / workdir /
		// resume / effort from the shared options — permission flags never reach
		// the TUI, which owns approvals itself — so RuntimeMode is inert here.
		return Capabilities{
			ModelCatalog:               ClaudeProbeEnrichedCatalog,
			ImageIngestion:             PathImageIngestion,
			ResumeTranscript:           ClaudeProjectsTranscript,
			UsageFamily:                Claude,
			StandardContextWindow:      ClaudeStandardContextWindow,
			ExtendedContextWindow:      ClaudeExtendedContextWindow,
			SupportsWorkflowMCP:        true,
			SupportsAppendSystemPrompt: true,
			SupportsOwnMemory:          true,
		}
	case string(Claude):
		// Headless: base64-inlines image bytes (the zero-value ingestion); named
		// explicitly so it reads as a known provider rather than a default
		// fall-through.
		return Capabilities{
			ModelCatalog:               ClaudeProbeEnrichedCatalog,
			EnforcesRuntimeMode:        true,
			ResumeTranscript:           ClaudeProjectsTranscript,
			UsageFamily:                Claude,
			StandardContextWindow:      ClaudeStandardContextWindow,
			ExtendedContextWindow:      ClaudeExtendedContextWindow,
			SupportsAppManagedMCP:      true,
			SupportsWorkflowMCP:        true,
			SupportsAppendSystemPrompt: true,
			SupportsOwnMemory:          true,
			RollbackGranularity:        RollbackItem,
		}
	case string(Agy):
		return Capabilities{
			ModelCatalog:               ACPLiveModelCatalog,
			EnforcesRuntimeMode:        true,
			UsageFamily:                Agy,
			StandardContextWindow:      AgyStandardContextWindow,
			ExtendedContextWindow:      AgyStandardContextWindow,
			SupportsAppManagedMCP:      true,
			SupportsWorkflowMCP:        true,
			SupportsAppendSystemPrompt: true,
			RollbackGranularity:        RollbackItem,
		}
	default:
		return Capabilities{}
	}
}
