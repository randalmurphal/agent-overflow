package main

import (
	"slices"
	"strconv"
	"testing"
	"time"

	"agent-overflow/internal/harness/control"
	"agent-overflow/internal/harness/scenario"
)

// startReportingMock boots the real binary with a live control channel and
// returns the reports it posts.
func startReportingMock(t *testing.T, sc *scenario.Scenario, extraEnv ...string) (*mockProc, <-chan control.Report) {
	t.Helper()
	reports := make(chan control.Report, 64)
	p, _ := startMockOnControl(t, sc, []string{"app-server"}, func(_ control.MockInfo, report control.Report) { reports <- report }, extraEnv...)
	return p, reports
}

// resumedHistory resumes threadID in a fresh process and returns the inputs
// it reports the thread's history holds.
func resumedHistory(t *testing.T, sc *scenario.Scenario, threadID string, env ...string) []string {
	t.Helper()
	p, reports := startReportingMock(t, sc, env...)
	p.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	p.expectLineContaining(`"id":1`, testTimeout)
	p.send(`{"jsonrpc":"2.0","id":2,"method":"thread/resume","params":{"threadId":"` + threadID + `"}}`)
	p.expectLineContaining(`"id":2`, testTimeout)
	p.closeStdinAndExpectExit(0, testTimeout)
	// The exiting report is the process's last.
	var inputs []string
	for {
		select {
		case report := <-reports:
			switch report.Kind {
			case control.ReportHistoryResumed:
				if report.SessionRef != threadID {
					t.Fatalf("history_resumed for %q, want %q", report.SessionRef, threadID)
				}
				inputs = append(inputs, report.Input)
			case control.ReportExiting:
				return inputs
			}
		case <-time.After(testTimeout):
			t.Fatalf("no exiting report; resumed history so far %q", inputs)
		}
	}
}

// A Codex thread holds every input it was given, echoed or not, so a process
// that resumes it reports them: what the resumed agent already holds. Without
// KeepHistory nothing is recorded.
func TestCodexKeepHistoryReportsTheInputsAResumedThreadHolds(t *testing.T) {
	for _, keep := range []bool{true, false} {
		home := t.TempDir()
		env := control.EnvTranscriptHome + "=" + home
		sc := codexScenario()
		sc.Codex = &scenario.CodexOptions{ThreadID: "th-kept", KeepHistory: keep}

		p, _ := startReportingMock(t, sc, env)
		p.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
		p.expectLineContaining(`"id":1`, testTimeout)
		p.send(`{"jsonrpc":"2.0","id":2,"method":"thread/start","params":{}}`)
		p.expectLineContaining(`"id":2`, testTimeout)
		for i, text := range []string{"first message", "the completion"} {
			p.send(`{"jsonrpc":"2.0","id":` + strconv.Itoa(3+i) + `,"method":"turn/start","params":{"threadId":"th-kept","input":[{"type":"text","text":"` + text + `"}]}}`)
			p.expectLineContaining(`"method":"turn/completed"`, testTimeout)
		}
		p.closeStdinAndExpectExit(0, testTimeout)

		got := resumedHistory(t, sc, "th-kept", env)
		want := []string{"first message", "the completion"}
		if !keep {
			want = nil
		}
		if !slices.Equal(got, want) {
			t.Fatalf("keepHistory=%v: resumed history %q, want %q", keep, got, want)
		}
	}
}
