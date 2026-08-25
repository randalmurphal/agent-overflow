package main

import (
	"testing"

	"agent-overflow/internal/transport"
)

// TestProductionReceiversRegisterWithoutCollision registers the two real
// receivers (App, Harness) on one dispatcher exactly as bootTransport does.
// Name-based dispatch shares one namespace across receivers, so a future
// App method that shadows a Harness method (or vice versa) must fail here
// at `make go-test` time rather than at `--harness` boot.
func TestProductionReceiversRegisterWithoutCollision(t *testing.T) {
	dispatcher := transport.NewDispatcher()
	if _, err := dispatcher.Register(&App{}, transport.RegisterOptions{
		Package:   "main",
		TypeName:  "App",
		AllowList: transport.NewMethodAllowList(),
	}); err != nil {
		t.Fatalf("register App: %v", err)
	}
	if _, err := dispatcher.Register(&Harness{}, transport.RegisterOptions{
		Package:   "main",
		TypeName:  "Harness",
		LocalOnly: true,
	}); err != nil {
		t.Fatalf("register Harness: %v", err)
	}
}
