package otel

import "strings"

// ConfigFromFlags builds a Config from two user-settable fields: whether
// tracing is enabled and the OTLP endpoint string. Kept separate from the
// Settings struct so this package has no import dependency on internal/settings.
func ConfigFromFlags(enabled bool, endpoint string) Config {
	return Config{
		Enabled:     enabled,
		Endpoint:    strings.TrimSpace(endpoint),
		ServiceName: DefaultServiceName,
	}
}
