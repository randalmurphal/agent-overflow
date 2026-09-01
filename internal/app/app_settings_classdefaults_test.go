package app

import (
	"context"
	"slices"
	"testing"

	"agent-overflow/internal/identity"
	"agent-overflow/internal/settings"
)

// internal/settings mirrors identity.DeviceClass as plain strings rather than
// importing it, so that package stays free of a database dependency
// (classdefaults.go argues the direction). This package holds both, which
// makes it the one place the two sets can be compared — the same shape as the
// provider deny-list's root-package cross-check.
//
// It fails in BOTH directions on purpose. A class added to identity and not
// mirrored here has no row in the defaults table; a class mirrored here that
// identity never declares is a row no device can ever resolve to.
func TestSettingsDeviceClassesMirrorTheIdentityVocabulary(t *testing.T) {
	mirrored := make([]string, 0, len(settings.DeviceClasses))
	for _, class := range settings.DeviceClasses {
		mirrored = append(mirrored, string(class))
	}
	declared := make([]string, 0, len(identity.DeviceClasses))
	for _, class := range identity.DeviceClasses {
		declared = append(declared, string(class))
	}
	if !slices.Equal(mirrored, declared) {
		t.Fatalf("settings.DeviceClasses = %v, identity.DeviceClasses = %v: the two must agree, value for value and in order", mirrored, declared)
	}

	// And every identity value converts. settingsDeviceClass falls back to
	// desktop for anything undeclared, so a drift would be silent without
	// this half.
	for _, class := range identity.DeviceClasses {
		if got := settingsDeviceClass(string(class)); got != settings.DeviceClass(class) {
			t.Errorf("settingsDeviceClass(%q) = %q, want the same class", class, got)
		}
	}
}

// An unreadable class is desktop, not nothing. The column is CHECK-constrained
// so only a zero-valued row can produce this, and a screen we cannot classify
// is most likely this machine's own kind.
func TestAnUnreadableDeviceClassResolvesToDesktop(t *testing.T) {
	for _, class := range []string{"", "wristwatch", "DESKTOP"} {
		if got := settingsDeviceClass(class); got != settings.DeviceDesktop {
			t.Errorf("settingsDeviceClass(%q) = %q, want %q", class, got, settings.DeviceDesktop)
		}
	}
}

// End to end through the RPC the frontend actually calls: a paired PHONE
// reads lowPowerMode on, from the class table, having written nothing.
func TestAPairedPhoneReadsItsClassDefaultThroughGetSettings(t *testing.T) {
	app := withTierStore(t, identityApp(t))
	device, session := pairDeviceOfClass(t, app, identity.DevicePhone, "A phone", "thumb-phone-defaults")

	got, err := app.GetSettings(sessionCtx(session.ID, ""))
	if err != nil {
		t.Fatalf("phone GetSettings: %v", err)
	}
	if !got.LowPowerMode {
		t.Fatal("a paired phone read lowPowerMode off; §6 says its class ships it on")
	}
	if settings.DefaultSettings.LowPowerMode {
		t.Fatal("the global default is already on; this test proves nothing")
	}

	// Resolved at read: nothing was written into the phone's bucket to make
	// that answer, so the phone tracks the table rather than a stale copy.
	rows, err := app.store.GetUIState("device:" + device.ID)
	if err != nil {
		t.Fatalf("read the phone's bucket: %v", err)
	}
	if _, written := rows["lowPowerMode"]; written {
		t.Errorf("the class default was persisted into the phone's bucket: %v", rows)
	}

	// A paired BROWSER on the same backend is unaffected: the class is the
	// discriminator, not the fact of being paired.
	_, browserSession := pairDevice(t, app, "A browser", "thumb-browser-defaults")
	browser, err := app.GetSettings(sessionCtx(browserSession.ID, ""))
	if err != nil {
		t.Fatalf("browser GetSettings: %v", err)
	}
	if browser.LowPowerMode {
		t.Error("a paired browser picked up the phone class's default")
	}
}

// The phone can say no, and the no sticks. This is the case that fails
// outright if mutate probes the global default instead of the class-resolved
// one: the patch would move nothing, persist nothing, and read back as true.
func TestAPairedPhoneCanTurnItsClassDefaultOff(t *testing.T) {
	app := withTierStore(t, identityApp(t))
	device, session := pairDeviceOfClass(t, app, identity.DevicePhone, "A phone", "thumb-phone-optout")
	ctx := sessionCtx(session.ID, "")

	next, err := app.UpdateSettings(ctx, map[string]any{"lowPowerMode": false})
	if err != nil {
		t.Fatalf("phone UpdateSettings: %v", err)
	}
	if next.LowPowerMode {
		t.Fatal("the write's own return value still reports the class default")
	}
	got, err := app.GetSettings(ctx)
	if err != nil {
		t.Fatalf("phone GetSettings: %v", err)
	}
	if got.LowPowerMode {
		t.Fatal("the phone's explicit false did not outrank its class default")
	}
	rows, err := app.store.GetUIState("device:" + device.ID)
	if err != nil {
		t.Fatalf("read the phone's bucket: %v", err)
	}
	if rows["lowPowerMode"] != "false" {
		t.Fatalf("phone bucket[lowPowerMode] = %q, want an explicit false", rows["lowPowerMode"])
	}

	// And clearing the row returns the phone to its CLASS default, not to the
	// global one. DeleteUIState is the clear that exists: settings rows share
	// the bucket, spelled as the settings JSON key.
	if err := app.DeleteUIState(ctx, []string{"lowPowerMode"}); err != nil {
		t.Fatalf("DeleteUIState: %v", err)
	}
	cleared, err := app.GetSettings(ctx)
	if err != nil {
		t.Fatalf("phone GetSettings after clear: %v", err)
	}
	if !cleared.LowPowerMode {
		t.Fatal("clearing fell through to the global default instead of the phone class's")
	}
}

// Every screen that is not a paired device resolves as DESKTOP, whose row is
// empty — so this wave changed nothing for the local page channel, the
// launch-credential clients, or an in-process caller.
func TestLocalScreensAndSessionlessCallersResolveAsDesktop(t *testing.T) {
	app := withTierStore(t, identityApp(t))

	for name, ctx := range map[string]context.Context{
		"a local page-channel screen": callerCtx("client-desk-0101"),
		"a sessionless caller":        context.Background(),
		"the local channel's session": sessionCtx(localChannelSession(t, app).ID, "client-webview-0101"),
	} {
		got, err := app.GetSettings(ctx)
		if err != nil {
			t.Fatalf("%s GetSettings: %v", name, err)
		}
		if got.LowPowerMode != settings.DefaultSettings.LowPowerMode {
			t.Errorf("%s read lowPowerMode = %t, want the global default %t",
				name, got.LowPowerMode, settings.DefaultSettings.LowPowerMode)
		}
	}
}

// §6: "a device-tier frame prompts each client to re-read and each gets its
// own values". The frame names keys and never values, so what each device
// gets back is decided entirely by its own re-read — which is now
// class-resolved. One phone's write must not move the browser beside it, and
// the phone's class default must survive the write.
func TestADeviceTierFrameLetsEachDeviceReadItsOwnClassResolvedValues(t *testing.T) {
	app := withTierStore(t, identityApp(t))
	broadcasts := captureSettingsBroadcasts(t, app)

	_, phoneSession := pairDeviceOfClass(t, app, identity.DevicePhone, "A phone", "thumb-phone-frame")
	_, browserSession := pairDevice(t, app, "A browser", "thumb-browser-frame")
	phone := sessionCtx(phoneSession.ID, "")
	browser := sessionCtx(browserSession.ID, "")

	if _, err := app.UpdateSettings(phone, map[string]any{"fontSize": 17}); err != nil {
		t.Fatalf("phone UpdateSettings: %v", err)
	}
	if len(broadcasts.events) != 1 {
		t.Fatalf("emitted %d frames, want one: %+v", len(broadcasts.events), broadcasts.events)
	}
	if got := broadcasts.events[0]; got.Tier != "device" || !slices.Equal(got.Keys, []string{"fontSize"}) {
		t.Fatalf("frame = %+v, want the device tier naming fontSize", got)
	}

	// Each device re-reads for itself, exactly as the frame asks it to.
	phoneAfter, err := app.GetSettings(phone)
	if err != nil {
		t.Fatalf("phone re-read: %v", err)
	}
	browserAfter, err := app.GetSettings(browser)
	if err != nil {
		t.Fatalf("browser re-read: %v", err)
	}
	if phoneAfter.FontSize != 17 {
		t.Errorf("phone FontSize = %d, want its own 17", phoneAfter.FontSize)
	}
	if browserAfter.FontSize != settings.DefaultSettings.FontSize {
		t.Errorf("browser FontSize = %d, want the default %d — the phone's write reached it",
			browserAfter.FontSize, settings.DefaultSettings.FontSize)
	}
	if !phoneAfter.LowPowerMode {
		t.Error("the phone lost its class default across an unrelated device-tier write")
	}
	if browserAfter.LowPowerMode {
		t.Error("the browser gained the phone class's default")
	}
}
