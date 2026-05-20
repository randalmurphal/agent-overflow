package mcp

// Status mirrors the wire-level state both providers report for MCP
// connections, plus the AO-side "haven't probed yet" pseudo-state.
// String values match the JSON the bindings expose to the frontend.
type Status string

const (
	StatusUnknown   Status = "unknown"
	StatusReady     Status = "ready"
	StatusNeedsAuth Status = "needs-auth"
	StatusFailed    Status = "failed"
)

// Transport names used by the probe layer to choose the stdio or http
// handshake path. Both providers expose extra transport synonyms
// (Codex's "streamable_http", Claude's "sse") which the adapter
// packages collapse to the corresponding probe transport before
// constructing a Spec.
const (
	TransportStdio = "stdio"
	TransportHTTP  = "http"
	TransportSSE   = "sse"
)

// Spec is the provider-agnostic projection the probe layer consumes.
// Both claudeconfig.Server and codexconfig.Server provide a ToSpec
// method that flattens their own shape into this one. Provider is
// kept on the spec so the probe cache can key by provider:name
// (the same server name can exist in both providers' configs).
type Spec struct {
	Provider  string
	Name      string
	Transport string
	Enabled   bool
	Command   string
	Args      []string
	Env       map[string]string
	URL       string
	Headers   map[string]string
	BearerEnv string
}

// CacheKey is the probe cache's identity for the spec. The provider
// prefix is what keeps a Claude entry and a Codex entry with the same
// server name from colliding in the cache. Empty Provider yields
// `:name`, which still works as a unique key for tests that don't
// care about provider scoping.
func (s Spec) CacheKey() string {
	return s.Provider + ":" + s.Name
}
