package logging

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// ProviderEventEntry is the raw provider I/O log schema. Data is the
// provider event embedded as a raw JSON value, not a string: provider
// frames ARE JSON, and re-escaping every byte of a streaming turn into
// a quoted string was ~24% of the backend's allocation while turns ran
// (measured 2026-08-24, 242MB over one 16-minute window). The encoder
// compacts the raw value, so multi-line input cannot break NDJSON
// framing. A non-JSON payload still logs — LogProviderEvent falls back
// to the old quoted-string form for it.
type ProviderEventEntry struct {
	Timestamp string          `json:"ts"`
	ThreadID  string          `json:"threadId"`
	Direction string          `json:"direction"`
	Provider  string          `json:"provider"`
	Data      json.RawMessage `json:"data"`
}

// LogProviderEvent writes a raw provider stdin/stdout event as one NDJSON line.
// Timestamps use RFC3339Nano so events arriving in the same second can still
// be ordered — provider streams burst hundreds of frames in a few milliseconds
// during a turn, and ordering is what this log is for.
func (l *Logger) LogProviderEvent(entry ProviderEventEntry) error {
	if entry.Timestamp == "" {
		entry.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
	}
	if !json.Valid(entry.Data) {
		quoted, err := json.Marshal(string(entry.Data))
		if err != nil {
			return fmt.Errorf("logging: quote non-JSON provider event: %w", err)
		}
		entry.Data = quoted
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
