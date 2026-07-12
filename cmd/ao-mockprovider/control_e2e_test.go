package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"agent-overflow/internal/harness/control"
	"agent-overflow/internal/harness/scenario"
)

// TestControlChannelEndToEnd drives the full harness wire: an in-test
// control.Server assigns the scenario at registration, a waitSignal
// gate is released by a live advance command, live emit/exit commands
// work, and every progress report kind the harness awaits is observed.
func TestControlChannelEndToEnd(t *testing.T) {
	sc := &scenario.Scenario{
		Version:  scenario.CurrentVersion,
		Name:     "gated",
		Provider: scenario.ProviderClaude,
		Turns: []scenario.Turn{{Label: "gated-turn", Steps: []scenario.Step{
			{Emit: &scenario.EmitStep{Lines: []string{`{"mock":"before-gate"}`}}},
			{WaitSignal: &scenario.WaitSignalStep{Name: "gate1"}},
			{Emit: &scenario.EmitStep{Lines: []string{`{"mock":"after-gate"}`}}},
		}}},
	}
	scenarioJSON, err := json.Marshal(sc)
	if err != nil {
		t.Fatalf("marshal scenario: %v", err)
	}

	reports := make(chan control.Report, 128)
	var mockID string
	mockIDCh := make(chan string, 1)
	srv, err := control.NewServer(control.ServerConfig{
		Resolve: func(reg control.Registration) (control.Assignment, error) {
			if reg.Protocol != scenario.ProviderClaude || reg.ResumeRef != "sess-e2e" || reg.PID == 0 {
				t.Errorf("registration = %+v", reg)
			}
			return control.Assignment{ScenarioName: sc.Name, ScenarioJSON: scenarioJSON}, nil
		},
		OnReport: func(info control.MockInfo, rep control.Report) {
			select {
			case mockIDCh <- info.MockID:
			default:
			}
			reports <- rep
		},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})

	env := []string{
		control.EnvAddr + "=" + srv.Addr(),
		control.EnvToken + "=" + srv.Token(),
	}
	args := append(append([]string(nil), claudeSessionArgs...), "--resume", "sess-e2e")
	p := startMock(t, args, env, t.TempDir())

	expectReport(t, reports, control.ReportRegistered)
	select {
	case mockID = <-mockIDCh:
	case <-time.After(testTimeout):
		t.Fatal("mock id never observed")
	}

	p.send(userLine)
	expectReport(t, reports, control.ReportTurnStarted)
	// Adapter-owned per-turn frames precede the scenario steps: the
	// init (carrying the resumed session id) and the user echo.
	p.expectLineContaining(`"session_id":"sess-e2e"`, testTimeout)
	p.expectLineContaining(`"isReplay":true`, testTimeout)
	if got := p.expectLine(testTimeout); got != `{"mock":"before-gate"}` {
		t.Fatalf("pre-gate line = %q", got)
	}
	waiting := expectReport(t, reports, control.ReportWaitingSignal)
	if waiting.Detail != "gate1" {
		t.Fatalf("waiting_signal detail = %q, want gate1", waiting.Detail)
	}

	if err := srv.Command(mockID, control.Command{Type: control.CommandAdvance, Name: "gate1"}); err != nil {
		t.Fatalf("advance command: %v", err)
	}
	if got := p.expectLine(testTimeout); got != `{"mock":"after-gate"}` {
		t.Fatalf("post-gate line = %q", got)
	}
	expectReport(t, reports, control.ReportScenarioDone)

	// Live emit substitutes the mock's current vars.
	if err := srv.Command(mockID, control.Command{Type: control.CommandEmit, Lines: []string{`{"injected":"${SESSION_ID}/${TURN}"}`}}); err != nil {
		t.Fatalf("emit command: %v", err)
	}
	if got := p.expectLine(testTimeout); got != `{"injected":"sess-e2e/1"}` {
		t.Fatalf("injected line = %q", got)
	}

	// Live exit terminates with the requested code after reporting.
	if err := srv.Command(mockID, control.Command{Type: control.CommandExit, Code: 7}); err != nil {
		t.Fatalf("exit command: %v", err)
	}
	exiting := expectReport(t, reports, control.ReportExiting)
	if exiting.Detail != "7" {
		t.Fatalf("exiting detail = %q, want 7", exiting.Detail)
	}
	p.expectExit(7, testTimeout)
}

// expectReport drains reports until the wanted kind appears. Reports of
// other kinds in between (step_started/step_completed/...) are legal.
func expectReport(t *testing.T, reports <-chan control.Report, kind string) control.Report {
	t.Helper()
	deadline := time.After(testTimeout)
	var seen []string
	for {
		select {
		case rep := <-reports:
			if rep.Kind == kind {
				return rep
			}
			seen = append(seen, rep.Kind)
		case <-deadline:
			t.Fatalf("report %q never arrived; saw %v", kind, seen)
		}
	}
}
