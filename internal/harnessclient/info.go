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
}

// Info calls HarnessInfo on the instance.
func (c *Client) Info(ctx context.Context) (HarnessInfo, error) {
	var out HarnessInfo
	if err := c.CallInto(ctx, &out, "HarnessInfo"); err != nil {
		return HarnessInfo{}, err
	}
	return out, nil
}
