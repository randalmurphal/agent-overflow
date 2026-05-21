package appidentity

import "testing"

func TestSingleInstanceID(t *testing.T) {
	dev := SingleInstanceID("desktop", "dev")
	prod := SingleInstanceID("desktop", "prod")
	if dev == prod {
		t.Fatal("dev and prod IDs must differ")
	}
	if dev != "com.agentoverflow.desktop.dev" {
		t.Fatalf("dev ID = %q", dev)
	}
	if prod != "com.agentoverflow.desktop" {
		t.Fatalf("prod ID = %q", prod)
	}
}
