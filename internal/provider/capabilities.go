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
	case string(Claude):
		return Capabilities{}
	default:
		return Capabilities{}
	}
}
