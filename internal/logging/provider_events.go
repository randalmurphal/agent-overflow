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
func (l *Logger) LogProviderEvent(entry ProviderEventEntry) error {
	if entry.Timestamp == "" {
		entry.Timestamp = time.Now().UTC().Format(time.RFC3339)
	}
	return l.logValue(entry)
}
