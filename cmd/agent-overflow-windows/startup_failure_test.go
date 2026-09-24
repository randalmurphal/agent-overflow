//go:build windows

package main

import (
	"errors"
	"fmt"
	"html/template"
	"reflect"
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

// TestMigrationRetryPageCallsRetryMigration: the page a remembered failed
// upgrade shows escapes its copy and has one Retry button, which calls the
// bound RetryMigration by the name Wails registers it under. The other
// failure pages have no button and no script.
func TestMigrationRetryPageCallsRetryMigration(t *testing.T) {
	body := string(migrationRetryPageHTML("Title <b>", "Reason: <script>x</script>"))
	if !strings.Contains(body, "<h1>Title &lt;b&gt;</h1><p>Reason: &lt;script&gt;x&lt;/script&gt;</p>") {
		t.Fatalf("the copy is not escaped: %s", body)
	}
	elem := reflect.TypeOf((*launcherApp)(nil)).Elem()
	want := `var method = "` + template.JSEscapeString(fmt.Sprintf("%s.%s.RetryMigration", elem.PkgPath(), elem.Name())) + `";`
	if !strings.Contains(body, want) {
		t.Fatalf("the page does not call %s: %s", want, body)
	}
	if _, ok := reflect.TypeOf(&launcherApp{}).MethodByName("RetryMigration"); !ok {
		t.Fatal("launcherApp has no bound RetryMigration")
	}
	if n := strings.Count(body, `<button id="ao-retry"`); n != 1 {
		t.Fatalf("the page has %d Retry buttons", n)
	}
	if !strings.Contains(body, `"/wails/runtime"`) || !strings.Contains(body, `args: { "call-id": callId, methodName: method, args: [] }`) {
		t.Fatalf("the Retry call is not Wails' CallBinding shape: %s", body)
	}
	for _, page := range [][]byte{failurePageHTML("t", "d", "a"), startupFailureHTML(errLaunchFailed)} {
		if strings.Contains(string(page), "ao-retry") || strings.Contains(string(page), "<script") {
			t.Fatalf("a failure page without a remembered upgrade offers Retry: %s", page)
		}
	}
}

// TestRetryMigrationRunsOnlyTheOfferedLaunch: Retry needs the offer the
// page was shown for and the launch claim, and a refused Retry keeps the
// offer and releases nothing it did not take.
func TestRetryMigrationRunsOnlyTheOfferedLaunch(t *testing.T) {
	a := &launcherApp{}
	if err := a.RetryMigration(); err == nil || !strings.Contains(err.Error(), "no database upgrade to retry") {
		t.Fatalf("RetryMigration without an offer = %v", err)
	}
	if a.launching.Load() {
		t.Fatal("a refused Retry kept the launch claim")
	}
	a.launching.Store(true)
	a.migrationRetry.Store(&launchTarget{distro: "Ubuntu"})
	if err := a.RetryMigration(); err == nil || !strings.Contains(err.Error(), "already in progress") {
		t.Fatalf("RetryMigration during a launch = %v", err)
	}
	if a.migrationRetry.Load() == nil || !a.launching.Load() {
		t.Fatal("a Retry refused during a launch took the offer or released the claim")
	}
}
