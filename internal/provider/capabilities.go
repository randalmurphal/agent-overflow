package provider

type ModelCatalogSource string

const (
	StaticModelCatalog    ModelCatalogSource = "static"
	CodexLiveModelCatalog ModelCatalogSource = "codex_live"
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
}

func CapabilitiesForProvider(providerName string) Capabilities {
	switch providerName {
	case string(Codex):
		return Capabilities{
			ModelCatalog:              CodexLiveModelCatalog,
			BackgroundTerminalCleaner: CodexBackgroundTerminalCleaner,
			ImageIngestion:            PathImageIngestion,
		}
	case string(ClaudeTUI):
		// Same static model catalog as headless claude and no background-terminal
		// cleaner, but ingests images by pasting their on-disk path into the real
		// TUI composer.
		return Capabilities{ImageIngestion: PathImageIngestion}
	case string(Claude):
		// Headless: base64-inlines image bytes (the zero-value ingestion); named
		// explicitly so it reads as a known provider rather than a default
		// fall-through.
		return Capabilities{}
	default:
		return Capabilities{}
	}
}
