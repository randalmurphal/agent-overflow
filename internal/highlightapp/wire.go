package highlightapp

import "agent-overflow/internal/highlight"

// Stateless highlighting is shared by execution-host and frontend-only RPC
// receivers. One wire shape keeps both compatible with the same SPA bundle.
type CodeRequest struct {
	Lang   string `json:"lang"`
	Source string `json:"source"`
}

type PatchRequest struct {
	Path  string `json:"path"`
	Patch string `json:"patch"`
}

type Result struct {
	Lang       string                  `json:"lang"`
	Lines      []highlight.EncodedLine `json:"lines"`
	Truncated  bool                    `json:"truncated"`
	Incomplete bool                    `json:"incomplete"`
	// Primed marks a contextual cache upgrade; consumers must not replace it
	// with a later stateless result for the same content.
	Primed bool `json:"primed,omitempty"`
}
