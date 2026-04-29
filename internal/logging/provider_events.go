package logging

import "time"

// ProviderEventEntry is the raw provider I/O log schema.
type ProviderEventEntry struct {
	Timestamp string `json:"ts"`
	ThreadID  string `json:"threadId"`
	Direction string `json:"direction"`
	Provider  string `json:"provider"`
	Data      string `json:"data"`
}

// LogProviderEvent writes a raw provider stdin/stdout event as one NDJSON line.
// Timestamps use RFC3339Nano so events arriving in the same second can still
// be ordered — provider streams burst hundreds of frames in a few milliseconds
// during a turn, and ordering is what this log is for.
func (l *Logger) LogProviderEvent(entry ProviderEventEntry) error {
	if entry.Timestamp == "" {
		entry.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
	}
	return l.logValue(entry)
}
