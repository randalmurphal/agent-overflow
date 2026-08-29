package harnessclient

import "context"

// SupportedHarnessProtocolRevision is the revision this client can safely
// drive. A command must refuse state-changing operations when the backend
// reports another revision.
const SupportedHarnessProtocolRevision = 1

// HarnessInfo mirrors main.HarnessInfoResult — the one result shape this
// package types, because every other caller of it is looking for a PATH
// and a mistyped field name would silently yield "". Everything else
// stays json.RawMessage: a CLI that typed each RPC result would become a
// second copy of the app's wire surface, which is exactly the drift the
// generic path avoids.
type HarnessInfo struct {
	Version      string `json:"version"`
	PID          int    `json:"pid"`
	DataRoot     string `json:"dataRoot"`
	DataDir      string `json:"dataDir"`
	HomeDir      string `json:"homeDir,omitempty"`
	MockProvider string `json:"mockProvider"`
	DBPath       string `json:"dbPath"`
	// EventLogDir holds the per-thread NDJSON event logs that back
	// wire-level replay recordings.
	EventLogDir string `json:"eventLogDir"`
	// UITracePath receives frames only when the frontend build enabled
	// the render trace (UI_TRACE=1); FrontendErrorsPath is always on.
	UITracePath        string `json:"uiTracePath"`
	FrontendErrorsPath string `json:"frontendErrorsPath"`
	// SoakAutopilot is "armed", "off", or "failed: <reason>". A soak whose
	// autopilot failed to arm looks exactly like a healthy idle instance
	// from outside — a live backend, a seeded database, no traffic — which
	// is the one shape an hours-long run must not silently be. Empty means
	// a backend too old to answer, which every reader treats as "unknown"
	// rather than "off".
	SoakAutopilot string `json:"soakAutopilot,omitempty"`
	// AssetsFreshness is the embedded-bundle verdict from boot: "match",
	// "stale", "unknown", or "dev-server". "stale" means the binary was
	// built against a different frontend/dist than the one on disk and
	// every measurement describes the old bundle.
	AssetsFreshness string                `json:"assetsFreshness,omitempty"`
	AssetsDigest    string                `json:"assetsDigest,omitempty"`
	FrontendPages   []HarnessPageIdentity `json:"frontendPages,omitempty"`
}

// HarnessPageIdentity identifies the exact frontend document currently
// registered with a harness. Supervisors use PageID to bind UI queries and
// Origin/Marker to prove page ownership before mutating or measuring it.
type HarnessPageIdentity struct {
	PageID string `json:"pageId"`
	Marker string `json:"marker"`
	Origin string `json:"origin"`
}

// Info calls HarnessInfo on the instance.
func (c *Client) Info(ctx context.Context) (HarnessInfo, error) {
	var out HarnessInfo
	if err := c.CallInto(ctx, &out, "HarnessInfo"); err != nil {
		return HarnessInfo{}, err
	}
	return out, nil
}

// HarnessCapabilities is the versioned control-surface contract returned by
// main.HarnessCapabilities. It is typed because the CLI must validate the
// revision and named controls before it mutates or instruments an instance.
type HarnessCapabilities struct {
	ProtocolRevision int                       `json:"protocolRevision"`
	Methods          []string                  `json:"methods"`
	Meters           []string                  `json:"meters"`
	Actions          []string                  `json:"actions"`
	Queries          []string                  `json:"queries"`
	Workloads        []string                  `json:"workloads"`
	Build            HarnessBuildCapabilities  `json:"build"`
	Assets           HarnessAssetsCapabilities `json:"assets"`
}

type HarnessBuildCapabilities struct {
	Version string `json:"version"`
	Stamp   string `json:"stamp,omitempty"`
}

type HarnessAssetsCapabilities struct {
	Freshness string `json:"freshness"`
	Digest    string `json:"digest,omitempty"`
}

// Capabilities calls HarnessCapabilities on the instance.
func (c *Client) Capabilities(ctx context.Context) (HarnessCapabilities, error) {
	var out HarnessCapabilities
	if err := c.CallInto(ctx, &out, "HarnessCapabilities"); err != nil {
		return HarnessCapabilities{}, err
	}
	return out, nil
}
