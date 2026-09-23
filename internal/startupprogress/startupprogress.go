// Package startupprogress is the wire contract of a backend that is still
// starting: the 503 body its readiness-gated /bootstrap.json answers until
// the boot finishes. internal/transport writes it, carriers pass it on,
// and the Windows launcher and the frontend read it.
//
// Stdlib-only by construction: the Windows launcher links this and does
// not link internal/transport.
package startupprogress

import (
	"encoding/json"
	"net/http"
)

// Reason is the body's `reason` while the backend starts.
const Reason = "starting"

// Progress is what a starting backend reports. Times are Unix
// milliseconds. UpdatedAt advances with every report and with the
// heartbeat of an open step, so a client can tell a slow step from a
// backend that stopped working.
type Progress struct {
	// Phase is the boot phase id (the `boot: phase=` log name).
	Phase string `json:"phase"`
	// Detail is the sentence a person reads for the phase.
	Detail string `json:"detail"`
	// Step and Steps count sub-steps inside the phase, such as pending
	// migrations. Both are zero when the phase has none.
	Step  int `json:"step"`
	Steps int `json:"steps"`
	// StartedAt is when this backend began starting.
	StartedAt int64 `json:"startedAt"`
	// UpdatedAt is the last report or heartbeat.
	UpdatedAt int64 `json:"updatedAt"`
	// UpdatingTo names the version this boot is finishing an in-app update
	// to. Empty on an ordinary start.
	UpdatingTo string `json:"updatingTo,omitempty"`
}

type body struct {
	Reason string `json:"reason"`
	Progress
}

// Write answers a bootstrap request for a starting backend with p.
func Write(w http.ResponseWriter, p Progress) {
	h := w.Header()
	h.Set("Cache-Control", "no-store, max-age=0")
	h.Set("Content-Type", "application/json")
	h.Set("Retry-After", "1")
	w.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(w).Encode(body{Reason: Reason, Progress: p})
}

// Parse reads a bootstrap answer. It reports true only for a 503 whose
// body is a starting report; a bare 503 and any other answer report false.
func Parse(status int, data []byte) (Progress, bool) {
	if status != http.StatusServiceUnavailable {
		return Progress{}, false
	}
	var b body
	if err := json.Unmarshal(data, &b); err != nil || b.Reason != Reason {
		return Progress{}, false
	}
	return b.Progress, true
}
