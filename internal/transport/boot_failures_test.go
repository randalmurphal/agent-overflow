package transport

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
)

// TestBootPhaseFailuresReachEveryHello: a boot phase the reporter is told
// failed is named, with its detail and error, in the hello of every
// connection from then on, before and after MarkReady. The error is
// bounded, a failure with no phase open is filed under the last phase
// reported, and a boot with no failure sends no field.
func TestBootPhaseFailuresReachEveryHello(t *testing.T) {
	f := newServerFixtureWith(t, func(cfg *Config) { cfg.RequireReadyForBootstrap = true })
	r := NewStartupReporter(f.srv, "")
	readHello := func() (helloFrame, string) {
		t.Helper()
		conn := f.dial(t)
		defer conn.CloseNow()
		raw := readFirstFrame(t, conn)
		var hello helloFrame
		if err := json.Unmarshal(raw, &hello); err != nil {
			t.Fatal(err)
		}
		return hello, string(raw)
	}
	if _, raw := readHello(); strings.Contains(raw, "bootFailures") {
		t.Fatalf("a boot with no failure sent %s", raw)
	}

	end := r.BeginBootPhase("app.recover_crashed_turns", "Settling interrupted turns")
	r.BootPhaseFailed(errors.New("database is locked"))
	end()
	want := []BootFailure{{Phase: "app.recover_crashed_turns", Detail: "Settling interrupted turns", Error: "database is locked"}}
	if early, raw := readHello(); !slices.Equal(early.BootFailures, want) {
		t.Fatalf("hello before MarkReady = %s, want the failed phase", raw)
	}

	endOuter := r.BeginBootPhase("app.init_subsystems", "Starting services")
	endInner := r.BeginBootPhase("app.sweep_crashed_worktree_setups", "Settling worktree setups")
	r.BootPhaseFailed(errors.New(strings.Repeat("é", 2*maxBootFailureErrorRunes)))
	endInner()
	endOuter()
	r.BootPhaseFailed(errors.New("late"))
	f.srv.MarkReady()
	want = append(want,
		BootFailure{Phase: "app.sweep_crashed_worktree_setups", Detail: "Settling worktree setups",
			Error: strings.Repeat("é", maxBootFailureErrorRunes) + "…"},
		BootFailure{Phase: "app.init_subsystems", Detail: "Starting services", Error: "late"})
	if late, raw := readHello(); !slices.Equal(late.BootFailures, want) {
		t.Fatalf("hello after MarkReady = %s, want every failure in order", raw)
	}

	var nilReporter *StartupReporter
	nilReporter.BootPhaseFailed(errors.New("nothing to report to"))
}
