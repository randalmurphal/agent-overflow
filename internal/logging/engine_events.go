package logging

import "time"

// EngineEventEntry is the workflow run-lifecycle log schema: one line per
// engine-significant decision (a park and why, a cancel, a resume, a
// definition re-read, a rebuild action, a capacity note).
//
// It is a separate stream from the provider-event log because it answers a
// different question. The provider log is raw CLI I/O, enabled per debugging
// session; this one is the account of what the ENGINE did, and a park that
// happened last Tuesday has to be readable without having predicted on Monday
// that it would need logging. So it is always on.
//
// `Message` is engine-authored prose, never model output — a park's cause is
// a Go error's text. The durable, user-facing copy of that cause is the
// attempt row's `park_cause`; this line is the diagnostic trail around it,
// including for the parks that never reached an attempt row.
type EngineEventEntry struct {
	Timestamp string `json:"ts"`
	Event     string `json:"event"`
	ItemID    string `json:"itemId,omitempty"`
	ProjectID string `json:"projectId,omitempty"`
	PhaseID   string `json:"phaseId,omitempty"`
	Attempt   int    `json:"attempt,omitempty"`
	State     string `json:"state,omitempty"`
	Reason    string `json:"reason,omitempty"`
	Message   string `json:"message,omitempty"`
}

// LogEngineEvent writes one run-lifecycle record as an NDJSON line.
// Timestamps use RFC3339Nano: the engine's command loop settles several
// records inside one millisecond when a tree comes down, and their order is
// what reconstructs the teardown.
func (l *Logger) LogEngineEvent(entry EngineEventEntry) error {
	if entry.Timestamp == "" {
		entry.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
	}
	return l.logValue(entry)
}

// NewEngineEventLogger opens the engine log at
// <baseDir>/logs/engine-YYYY-MM-DD.ndjson with default rotation. Unlike the
// provider-event logger it takes no env gate: a run parks once, and there is
// no second chance to have turned the log on.
func NewEngineEventLogger(baseDir string) (*Logger, error) {
	return NewLogger(dailyLogPath(baseDir, "engine"), 0)
}
