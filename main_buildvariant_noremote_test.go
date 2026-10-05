//go:build noremote

package main

import (
	"errors"
	"testing"

	"agent-overflow/internal/buildvariant"
)

// Every boot whose purpose is remote access is refused before it starts;
// an ordinary boot, and an explicit loopback bind, are not.
func TestNoremoteRefusesRemoteBoots(t *testing.T) {
	refused := []struct {
		name             string
		serve, supervise bool
		flags            cliFlags
	}{
		{name: "serve", serve: true},
		{name: "supervise", supervise: true},
		{name: "--connect", flags: cliFlags{connect: "ws://127.0.0.1:1/?token=x"}},
		{name: "--frontend", flags: cliFlags{frontend: true}},
		{name: "--listen wildcard", flags: cliFlags{listenAddr: "0.0.0.0:0"}},
		{name: "--listen name", flags: cliFlags{listenAddr: "example.invalid:0"}},
	}
	for _, tc := range refused {
		err := refuseRemoteBoot(tc.serve, tc.supervise, tc.flags)
		if !errors.Is(err, buildvariant.ErrRemoteAccessUnavailable) {
			t.Errorf("%s: refuseRemoteBoot = %v, want the remote-access refusal", tc.name, err)
		}
	}
	// An omitted --listen host means loopback (defaultListenHost).
	for _, flags := range []cliFlags{{}, {listenAddr: "127.0.0.1:0"}, {listenAddr: "[::1]:0"}, {listenAddr: ":0"}} {
		if err := refuseRemoteBoot(false, false, flags); err != nil {
			t.Errorf("refuseRemoteBoot(%+v) = %v, want an ordinary boot", flags, err)
		}
	}
}
