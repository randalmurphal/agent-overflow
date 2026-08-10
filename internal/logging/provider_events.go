package logging

import (
	"os"
	"strings"
	"time"
)

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

// NewProviderEventLogger returns a provider-events Logger when the
// AGENT_OVERFLOW_DEBUG env var enables the "provider" topic, or
// (nil, nil) when logging is disabled. The log lands under
// <baseDir>/logs/provider-events-YYYY-MM-DD.ndjson with default rotation.
func NewProviderEventLogger(baseDir string) (*Logger, error) {
	if !providerEventLoggingEnabled(os.Getenv("AGENT_OVERFLOW_DEBUG")) {
		return nil, nil
	}

	return NewLogger(dailyLogPath(baseDir, "provider-events"), 0)
}

func providerEventLoggingEnabled(value string) bool {
	for _, topic := range strings.Split(value, ",") {
		switch strings.TrimSpace(strings.ToLower(topic)) {
		case "all", "provider":
			return true
		}
	}
	return false
}
