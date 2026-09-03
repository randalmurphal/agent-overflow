package app

import (
	"context"
	"slices"
	"testing"

	"agent-overflow/internal/settings"
	"agent-overflow/internal/transport"
)

// callerCtx is a connection that declared a durable client id and no session
// — the shape every local page channel presents, and the one uiStateScope
// turns into a "client:<id>" bucket.
func callerCtx(clientID string) context.Context {
	ctx, _ := transport.WithConnState(context.Background(), transport.ConnPrincipal{
		Client: transport.ClientIdentity{DeviceID: clientID},
	})
	return ctx
}

// withTierStore wires residency onto a test App the same way app_startup.go
// does, minus the boot around it.
func withTierStore(t *testing.T, app *App) *App {
	t.Helper()
	app.settings.AttachTierStore(app.store, "client:"+EnsureClientIDIn(t.TempDir()))
	return app
}

// The headline of the wave: one backend, two screens, two font sizes — and
// one shared answer for the user-tier keys.
func TestGetSettingsResolvesTheDeviceTierPerCaller(t *testing.T) {
	app := withTierStore(t, newTestAppWithStore(t))
	phone, desk := callerCtx("client-phone-0001"), callerCtx("client-desk-0001")

	if _, err := app.UpdateSettings(phone, map[string]any{"fontSize": 17}); err != nil {
		t.Fatalf("phone UpdateSettings: %v", err)
	}
	if _, err := app.UpdateSettings(desk, map[string]any{"fontSize": 20, "confirmDelete": false}); err != nil {
		t.Fatalf("desk UpdateSettings: %v", err)
	}

	phoneSettings, err := app.GetSettings(phone)
	if err != nil {
		t.Fatalf("phone GetSettings: %v", err)
	}
	deskSettings, err := app.GetSettings(desk)
	if err != nil {
		t.Fatalf("desk GetSettings: %v", err)
	}
	if phoneSettings.FontSize != 17 || deskSettings.FontSize != 20 {
		t.Fatalf("font sizes = phone %d / desk %d, want 17 / 20", phoneSettings.FontSize, deskSettings.FontSize)
	}
	if phoneSettings.ConfirmDelete || deskSettings.ConfirmDelete {
		t.Fatal("the user-tier write did not reach both callers")
	}

	// A caller with no connection behind it — a background saga, a test —
	// reads the device defaults rather than one screen's choice.
	sessionless, err := app.GetSettings(context.Background())
	if err != nil {
		t.Fatalf("sessionless GetSettings: %v", err)
	}
	if sessionless.FontSize != settings.DefaultSettings.FontSize {
		t.Fatalf("sessionless FontSize = %d, want the default %d", sessionless.FontSize, settings.DefaultSettings.FontSize)
	}
}

// One patch, three destinations, checked through the store the app really
// writes: settings.json keeps the host key, the reserved user scope keeps the
// user key, the caller's own bucket keeps the device key.
func TestUpdateSettingsRoutesEachKeyToItsTier(t *testing.T) {
	app := withTierStore(t, newTestAppWithStore(t))
	phone := callerCtx("client-phone-0002")

	if _, err := app.UpdateSettings(phone, map[string]any{
		"retention":     map[string]any{"days": 7},
		"confirmDelete": false,
		"fontSize":      17,
	}); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}

	userRows, err := app.store.GetUIState(settings.UserScope)
	if err != nil {
		t.Fatalf("read the user scope: %v", err)
	}
	if got := userRows["confirmDelete"]; got != "false" {
		t.Errorf("%s[confirmDelete] = %q, want false", settings.UserScope, got)
	}
	deviceRows, err := app.store.GetUIState("client:client-phone-0002")
	if err != nil {
		t.Fatalf("read the caller's bucket: %v", err)
	}
	if got := deviceRows["fontSize"]; got != "17" {
		t.Errorf("caller bucket[fontSize] = %q, want 17", got)
	}
	if _, misfiled := deviceRows["confirmDelete"]; misfiled {
		t.Error("a user-tier key landed in the caller's bucket")
	}
	// The host key stays where a host key lives, and nothing else joins it.
	if got := app.currentSettings().Retention.Days; got != 7 {
		t.Errorf("Retention.Days = %d, want 7", got)
	}
}

// The recent list is device-tier, so a thread created from a screen is
// remembered by THAT screen.
func TestCreateThreadAttributesRecentWorkspacesToTheCaller(t *testing.T) {
	app := withTierStore(t, newTestAppWithStore(t))
	dir := t.TempDir()
	project, err := app.CreateProject(dir)
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	phone := callerCtx("client-phone-0003")
	if _, err := app.CreateThread(phone, CreateThreadOptions{
		ProjectID: project.ID,
		Provider:  "claude",
		Model:     "claude-sonnet-4-6",
	}); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	phoneSettings, err := app.GetSettings(phone)
	if err != nil {
		t.Fatalf("phone GetSettings: %v", err)
	}
	if !slices.Contains(phoneSettings.RecentWorkspaces, project.Path) {
		t.Fatalf("phone RecentWorkspaces = %v, want %s", phoneSettings.RecentWorkspaces, project.Path)
	}
	deskSettings, err := app.GetSettings(callerCtx("client-desk-0003"))
	if err != nil {
		t.Fatalf("desk GetSettings: %v", err)
	}
	if len(deskSettings.RecentWorkspaces) != 0 {
		t.Fatalf("another screen inherited the recent list: %v", deskSettings.RecentWorkspaces)
	}
}
