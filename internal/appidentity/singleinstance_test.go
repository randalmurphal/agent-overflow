package appidentity

import "testing"

func TestAppTitle(t *testing.T) {
	if got := AppTitle("dev"); got != "Agent Overflow (dev)" {
		t.Fatalf("dev title = %q", got)
	}
	if got := AppTitle("prod"); got != "Agent Overflow" {
		t.Fatalf("prod title = %q", got)
	}
	if got := AppTitle(""); got != "Agent Overflow" {
		t.Fatalf("empty mode title = %q", got)
	}
}

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
