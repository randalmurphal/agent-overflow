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

// Capabilities names provider-level behavior that app code can depend on.
// Concrete protocol routing still belongs at the call site or in
// provider-specific code, so fields that require provider-specific plumbing use
// typed values instead of generic booleans.
type Capabilities struct {
	ModelCatalog              ModelCatalogSource
	BackgroundTerminalCleaner BackgroundTerminalCleaner
}

func CapabilitiesForProvider(providerName string) Capabilities {
	switch providerName {
	case string(Codex):
		return Capabilities{
			ModelCatalog:              CodexLiveModelCatalog,
			BackgroundTerminalCleaner: CodexBackgroundTerminalCleaner,
		}
	case string(Claude), string(ClaudeTUI):
		// claude-tui exposes the same static catalog as headless claude and
		// has no background-terminal cleaner; named explicitly so it reads as
		// a known provider rather than falling through to the default.
		return Capabilities{}
	default:
		return Capabilities{}
	}
}
