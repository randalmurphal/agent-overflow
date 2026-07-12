package scenario

import (
	"embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// library holds the named scenarios that ship inside the binary.
// Filename convention: <name>.json where <name> equals the scenario's
// Name field (enforced by TestLibraryIntegrity). Being embedded means a
// harness needs zero on-disk setup before its first mock session — the
// backend hands the JSON to the mock over the control channel.
//
//go:embed library/*.json
var libraryFS embed.FS

// LibraryEntry describes one shipped scenario for discovery RPCs.
type LibraryEntry struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Provider    string `json:"provider"`
}

// defaultScenarioByProvider names the scenario a mock gets when no rule
// was set — so a freshly booted harness streams a sensible reply on the
// very first message with zero configuration.
var defaultScenarioByProvider = map[string]string{
	ProviderClaude: "streaming-text",
	ProviderCodex:  "codex-basic",
}

// DefaultName returns the zero-config scenario name for a protocol.
func DefaultName(provider string) (string, error) {
	name, ok := defaultScenarioByProvider[provider]
	if !ok {
		return "", fmt.Errorf("no default scenario for provider %q", provider)
	}
	return name, nil
}

// LoadLibrary returns the raw JSON and parsed form of a shipped
// scenario by name.
func LoadLibrary(name string) (json.RawMessage, *Scenario, error) {
	data, err := libraryFS.ReadFile("library/" + name + ".json")
	if err != nil {
		names := strings.Join(libraryNamesOnly(), ", ")
		return nil, nil, fmt.Errorf("no library scenario %q (have: %s)", name, names)
	}
	s, err := Parse(data)
	if err != nil {
		// Embedded content is validated by tests; failing here means a
		// broken build, not bad user input.
		return nil, nil, fmt.Errorf("embedded scenario %q is invalid: %w", name, err)
	}
	return data, s, nil
}

// Library lists every shipped scenario, sorted by name.
func Library() ([]LibraryEntry, error) {
	entries, err := libraryFS.ReadDir("library")
	if err != nil {
		return nil, fmt.Errorf("read embedded scenario library: %w", err)
	}
	out := make([]LibraryEntry, 0, len(entries))
	for _, e := range entries {
		name := strings.TrimSuffix(e.Name(), ".json")
		_, s, err := LoadLibrary(name)
		if err != nil {
			return nil, err
		}
		out = append(out, LibraryEntry{Name: s.Name, Description: s.Description, Provider: s.Provider})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func libraryNamesOnly() []string {
	entries, err := libraryFS.ReadDir("library")
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, strings.TrimSuffix(e.Name(), ".json"))
	}
	sort.Strings(names)
	return names
}
