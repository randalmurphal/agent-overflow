package app

import (
	"context"
	"testing"

	"agent-overflow/internal/settings"
)

// Tracing is wired once at start, so the value it started with is the one
// the session runs with until a restart, whatever the setting says since,
// including when nothing read it before the setting changed.
func TestObservabilityTracing_ReportsTheLaunchValueAcrossChanges(t *testing.T) {
	for _, launched := range []bool{false, true} {
		t.Run(map[bool]string{false: "off at start", true: "on at start"}[launched], func(t *testing.T) {
			app := &App{settings: settings.NewService(t.TempDir())}
			// A port nothing listens on: the exporter dials lazily and the
			// test exports nothing.
			if _, err := app.settings.Update(map[string]any{
				"observabilityTracingEnabled": launched,
				"observabilityOtlpEndpoint":   "127.0.0.1:1",
			}); err != nil {
				t.Fatalf("seed settings: %v", err)
			}
			if err := app.initObservability(context.Background(), t.TempDir()); err != nil {
				t.Fatalf("initObservability: %v", err)
			}
			t.Cleanup(func() {
				// Nothing to flush: a cancelled context skips the wait on the
				// unreachable collector.
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				_ = app.telemetry.Shutdown(ctx)
			})

			check := func(step string, want LaunchSetting) {
				t.Helper()
				if got, err := app.GetObservabilityTracing(); err != nil || got != want {
					t.Fatalf("%s: got (%+v, %v), want %+v", step, got, err, want)
				}
			}
			// The first read comes after a change: a page opened after a save.
			if _, err := app.settings.Update(map[string]any{"observabilityTracingEnabled": !launched}); err != nil {
				t.Fatalf("toggle: %v", err)
			}
			check("after a change", LaunchSetting{Value: !launched, Launched: launched})
			if _, err := app.settings.Update(map[string]any{"observabilityTracingEnabled": launched}); err != nil {
				t.Fatalf("toggle back: %v", err)
			}
			check("after changing back", LaunchSetting{Value: launched, Launched: launched})
		})
	}
}
