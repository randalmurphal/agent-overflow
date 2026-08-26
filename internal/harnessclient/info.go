package harnessclient

import "context"

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
	AssetsFreshness string `json:"assetsFreshness,omitempty"`
}

// Info calls HarnessInfo on the instance.
func (c *Client) Info(ctx context.Context) (HarnessInfo, error) {
	var out HarnessInfo
	if err := c.CallInto(ctx, &out, "HarnessInfo"); err != nil {
		return HarnessInfo{}, err
	}
	return out, nil
}
