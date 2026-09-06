package app

import (
	"agent-overflow/internal/appidentity"
	"agent-overflow/internal/eventchan"
	"testing"
)

func TestDeviceNameFacadeUsesConfiguredInstallationAndEmits(t *testing.T) {
	identity := appidentity.NewDeviceName(t.TempDir())
	a := &App{configDir: t.TempDir()}
	SetDeviceNameIdentity(a, identity)
	var emitted string
	a.testEmitHook = func(name string, data any) {
		if name == eventchan.BackendNameChanged.String() {
			emitted = data.(map[string]string)["name"]
		}
	}
	if err := a.SetDeviceName(" Studio "); err != nil {
		t.Fatal(err)
	}
	if name, err := identity.Get(); err != nil || name != "Studio" {
		t.Fatalf("name=%q error=%v", name, err)
	}
	if a.backendDisplayName() != "Studio" || emitted != "Studio" {
		t.Fatalf("advertised=%q emitted=%q", a.backendDisplayName(), emitted)
	}
}
