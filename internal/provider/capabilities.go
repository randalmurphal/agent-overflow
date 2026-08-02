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
	// list is a picker shortlist that omits older-but-usable models, so it
	// ENRICHES (capability flags) and EXTENDS (models we don't ship yet) but
	// never subtracts. See internal/claudemodels.
	ClaudeProbeEnrichedCatalog ModelCatalogSource = "claude_probe"
)

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
}

func CapabilitiesForProvider(providerName string) Capabilities {
	switch providerName {
	case string(Codex):
		return Capabilities{
			ModelCatalog:              CodexLiveModelCatalog,
			BackgroundTerminalCleaner: CodexBackgroundTerminalCleaner,
			ImageIngestion:            PathImageIngestion,
			EnforcesRuntimeMode:       true,
		}
	case string(ClaudeTUI):
		// Same model catalog as headless claude (one binary, one login, so the
		// same probe answer enriches both) and no background-terminal cleaner,
		// but ingests images by pasting their on-disk path into the real TUI
		// composer. claudetui.ConfigFromOptions keeps only model / workdir /
		// resume / effort from the shared options — permission flags never reach
		// the TUI, which owns approvals itself — so RuntimeMode is inert here.
		return Capabilities{
			ModelCatalog:   ClaudeProbeEnrichedCatalog,
			ImageIngestion: PathImageIngestion,
		}
	case string(Claude):
		// Headless: base64-inlines image bytes (the zero-value ingestion); named
		// explicitly so it reads as a known provider rather than a default
		// fall-through.
		return Capabilities{
			ModelCatalog:        ClaudeProbeEnrichedCatalog,
			EnforcesRuntimeMode: true,
		}
	default:
		return Capabilities{}
	}
}
