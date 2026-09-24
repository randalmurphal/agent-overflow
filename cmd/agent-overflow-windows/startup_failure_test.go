//go:build windows

package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/startupprogress"
	"agent-overflow/internal/wsllauncher"
)

func TestStartupFailurePageUsesObservedCauseWithoutLeakingErrorContent(t *testing.T) {
	const secret = `private-token-123<script>alert("server body")</script>`
	stall := func(p startupprogress.Progress) error {
		return &wsllauncher.BackendStalledError{Progress: p, Quiet: 30 * time.Second, Last: errors.New(secret)}
	}
	silent := func(p startupprogress.Progress) error {
		return &wsllauncher.BackendStalledError{Progress: p, Quiet: 30 * time.Second, Unresponsive: true, Last: errors.New(secret)}
	}
	for _, tc := range []struct {
		name, want string
		err        error
		forwarding bool
	}{
		{"spawn", "process could not start inside WSL", errLaunchFailed, false},
		{"migrations pending", "Agent Overflow could not upgrade the database.</h1><p>This version upgrades the database only after backing it up",
			errors.Join(&wsllauncher.MigrationsPendingError{MigrationsPending: startupprogress.MigrationsPending{Database: 118, Build: 119, Pending: 1}}, errors.New(secret)), false},
		{"unreachable", "No HTTP response arrived", wsllauncher.ErrBackendUnreachable, true},
		{"invalid manifest", "startup response was not valid", wsllauncher.ErrInvalidBootstrap, false},
		{"not ready", "HTTP 503", wsllauncher.ErrBackendNotReady, false},
		{"stalled", "Startup stalled in phase store.migrate (Applying migration 3 of 7 v101) and made no progress for 30 seconds.",
			stall(startupprogress.Progress{Phase: "store.migrate", Detail: "Applying migration 3 of 7 v101", Step: 3, Steps: 7}), false},
		{"stalled update", "The update to v1.2.3 stalled.</h1><p>Finishing the update stalled in phase store.migrate (Applying migration 3 of 7 v101)",
			stall(startupprogress.Progress{Phase: "store.migrate", Detail: "Applying migration 3 of 7 v101", UpdatingTo: "1.2.3"}), false},
		{"stalled phase is escaped", "phase &lt;b&gt;", stall(startupprogress.Progress{Phase: "<b>"}), false},
		{"stopped responding", "Backend stopped responding.</h1><p>The backend stopped responding during startup, in phase store.migrate (Applying migration 3 of 7 v101), for 30 seconds.",
			silent(startupprogress.Progress{Phase: "store.migrate", Detail: "Applying migration 3 of 7 v101"}), false},
		{"stopped responding during an update", "The update to v1.2.3 stopped responding.</h1><p>The backend stopped responding while finishing the update, in phase store.migrate",
			silent(startupprogress.Progress{Phase: "store.migrate", Detail: "Applying migration 3 of 7 v101", UpdatingTo: "1.2.3"}), false},
		{"HTTP rejection", "HTTP 404", wsllauncher.BootstrapHTTPError{StatusCode: 404, URL: secret}, false},
		{"HTTP startup failure", "HTTP 500", wsllauncher.BootstrapHTTPError{StatusCode: 500, URL: secret}, false},
		{"invalid status", "local backend could not be opened", wsllauncher.BootstrapHTTPError{StatusCode: -1234, URL: secret}, false},
		{"other failure", "local backend could not be opened", errors.New(secret), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := string(startupFailureHTML(fmt.Errorf("%s: %w", secret, tc.err)))
			if !strings.Contains(body, tc.want) {
				t.Fatalf("page does not explain %q: %s", tc.want, body)
			}
			if strings.Contains(body, "localhostForwarding") != tc.forwarding {
				t.Fatalf("forwarding guidance does not match observed failure: %s", body)
			}
			if !strings.Contains(body, `%APPDATA%\agent-overflow\launcher.log`) {
				t.Fatalf("page lost the launcher log pointer: %s", body)
			}
			for _, forbidden := range []string{secret, "private-token", "<script>", "<b>", "fresh port", "already retried", "backend is running", "Windows reached", "-1234"} {
				if strings.Contains(body, forbidden) {
					t.Fatalf("page leaked or claimed %q", forbidden)
				}
			}
		})
	}
}
