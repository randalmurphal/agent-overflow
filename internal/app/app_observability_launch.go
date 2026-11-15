package app

// GetObservabilityTracing reports the OTLP tracing setting: saved, and as
// this session's exporters were built with it. initObservability wires
// tracing once at start and records that value; a change applies on the
// next start (Settings > Observability).
//
//ao:scope settings:read
//ao:route home
func (a *App) GetObservabilityTracing() (LaunchSetting, error) {
	saved := a.currentSettings().ObservabilityTracingEnabled
	return LaunchSetting{Value: saved, Launched: a.launchTracing.record(saved)}, nil
}
