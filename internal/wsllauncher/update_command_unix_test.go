//go:build !windows

package wsllauncher

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/startupprogress"
	"agent-overflow/internal/supervise"
)

// fakeWSL stands in for wsl.exe: it checks the `-d <distro> --exec` prefix
// and runs the rest of the argv directly, so a signal the runner sends with
// `kill` reaches the real process.
type fakeWSL struct {
	t     *testing.T
	mu    sync.Mutex
	calls [][]string
}

func (f *fakeWSL) runner(ctx context.Context, name string, args ...string) *exec.Cmd {
	f.mu.Lock()
	f.calls = append(f.calls, append([]string{name}, args...))
	f.mu.Unlock()
	if name != "wsl.exe" || len(args) < 4 || args[0] != "-d" || args[1] != "Ubuntu" || args[2] != "--exec" {
		f.t.Errorf("unexpected wsl.exe argv %q %q", name, args)
		return exec.CommandContext(ctx, "false")
	}
	return exec.CommandContext(ctx, args[3], args[4:]...)
}

func (f *fakeWSL) signals() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, call := range f.calls {
		if len(call) >= 6 && call[4] == "kill" {
			out = append(out, call[5])
		}
	}
	return out
}

// commandScript writes a payload that runs body with an `ev` helper for
// report lines.
func commandScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent-overflow")
	script := "#!/bin/sh\nev() { printf '" + supervise.UpdateEventPrefix + "%s\\n' \"$1\"; }\n" + body + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

const (
	startedLine   = `ev "{\"type\":\"started\",\"pid\":$$}"`
	heartbeatLine = `ev '{"type":"progress","liveness":true,"progress":{"phase":"store.open","detail":"Opening the database"}}'`
	progressLine  = `ev '{"type":"progress","progress":{"phase":"update.snapshot","detail":"Backing up the database"}}'`
	failOnTerm    = `trap 'ev "{\"type\":\"result\",\"outcome\":\"failed\",\"reason\":\"the trial was interrupted\"}"; exit 1' TERM`
)

func testRunner(f *fakeWSL, rule supervise.StallRule) UpdateCommandRunner {
	return UpdateCommandRunner{
		Distro: "Ubuntu", Command: f.runner, Rule: rule,
		TermGrace: 2 * time.Second, KillGrace: 2 * time.Second,
		Logf: func(string, ...any) {},
	}
}

func TestUpdateCommandRunnerReturnsTheResult(t *testing.T) {
	f := &fakeWSL{t: t}
	payload := commandScript(t, startedLine+"\n"+progressLine+"\necho a log line\nev '{\"type\":\"result\",\"outcome\":\"ok\"}'")
	var details []string
	result, err := testRunner(f, supervise.StallRule{Window: 5 * time.Second, Ceiling: 10 * time.Second}).Run(
		t.Context(), payload, supervise.UpdateSnapshotCommand, []string{"--id", "u1"},
		func(p startupprogress.Progress) { details = append(details, p.Detail) })
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != supervise.UpdateOutcomeOK {
		t.Fatalf("result = %+v", result)
	}
	if strings.Join(details, "|") != "Backing up the database" {
		t.Fatalf("progress = %q", details)
	}
	want := []string{"wsl.exe", "-d", "Ubuntu", "--exec", payload, supervise.UpdateSnapshotCommand, "--id", "u1"}
	if strings.Join(f.calls[0], " ") != strings.Join(want, " ") {
		t.Fatalf("argv = %q, want %q", f.calls[0], want)
	}
}

func TestUpdateCommandRunnerStopsACommandThatOnlyHeartbeats(t *testing.T) {
	f := &fakeWSL{t: t}
	payload := commandScript(t, failOnTerm+"\n"+startedLine+"\nwhile :; do "+heartbeatLine+"; sleep 0.05; done")
	var heartbeats int
	_, err := testRunner(f, supervise.StallRule{Window: 400 * time.Millisecond, Ceiling: 10 * time.Second}).Run(
		t.Context(), payload, supervise.UpdateTrialRunCommand, nil,
		func(startupprogress.Progress) { heartbeats++ })
	var stopped *UpdateCommandStoppedError
	if !errors.As(err, &stopped) {
		t.Fatalf("err = %v, want a stop", err)
	}
	if !strings.Contains(stopped.Reason, "stopped making progress for 400ms (last step: Opening the database)") {
		t.Fatalf("reason = %q", stopped.Reason)
	}
	if stopped.Result == nil || stopped.Result.Outcome != supervise.UpdateOutcomeFailed {
		t.Fatalf("the result reported while stopping was lost: %+v", stopped.Result)
	}
	if heartbeats == 0 {
		t.Fatal("heartbeats were not delivered")
	}
	if got := f.signals(); len(got) != 1 || got[0] != "-TERM" {
		t.Fatalf("signals = %q, want one SIGTERM", got)
	}
}

func TestUpdateCommandRunnerKeepsACommandThatReportsProgress(t *testing.T) {
	f := &fakeWSL{t: t}
	payload := commandScript(t, startedLine+"\ni=0\nwhile [ $i -lt 10 ]; do "+progressLine+"; sleep 0.1; i=$((i+1)); done\nev '{\"type\":\"result\",\"outcome\":\"ok\"}'")
	result, err := testRunner(f, supervise.StallRule{Window: 400 * time.Millisecond, Ceiling: 10 * time.Second}).Run(
		t.Context(), payload, supervise.UpdateSnapshotCommand, nil, nil)
	if err != nil || result.Outcome != supervise.UpdateOutcomeOK {
		t.Fatalf("result = %+v, %v", result, err)
	}
	if got := f.signals(); len(got) != 0 {
		t.Fatalf("a progressing command was signalled: %q", got)
	}
}

func TestUpdateCommandRunnerStopsAtTheCeiling(t *testing.T) {
	f := &fakeWSL{t: t}
	payload := commandScript(t, failOnTerm+"\n"+startedLine+"\nwhile :; do "+progressLine+"; sleep 0.05; done")
	_, err := testRunner(f, supervise.StallRule{Window: 5 * time.Second, Ceiling: 500 * time.Millisecond}).Run(
		t.Context(), payload, supervise.UpdateTrialRunCommand, nil, nil)
	var stopped *UpdateCommandStoppedError
	if !errors.As(err, &stopped) || !strings.Contains(stopped.Reason, "did not finish within 500ms") {
		t.Fatalf("err = %v", err)
	}
}

func TestUpdateCommandRunnerEscalatesToSIGKILL(t *testing.T) {
	f := &fakeWSL{t: t}
	payload := commandScript(t, "trap '' TERM\n"+startedLine+"\nwhile :; do "+heartbeatLine+"; sleep 0.05; done")
	runner := testRunner(f, supervise.StallRule{Window: 300 * time.Millisecond, Ceiling: 10 * time.Second})
	runner.TermGrace = 300 * time.Millisecond
	started := time.Now()
	_, err := runner.Run(t.Context(), payload, supervise.UpdateTrialRunCommand, nil, nil)
	var stopped *UpdateCommandStoppedError
	if !errors.As(err, &stopped) {
		t.Fatalf("err = %v", err)
	}
	if got := f.signals(); len(got) != 2 || got[0] != "-TERM" || got[1] != "-KILL" {
		t.Fatalf("signals = %q, want SIGTERM then SIGKILL", got)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("the stop took %s", elapsed)
	}
}

func TestUpdateCommandRunnerKillsWSLWhenThePIDIsUnknown(t *testing.T) {
	f := &fakeWSL{t: t}
	payload := commandScript(t, "exec sleep 30")
	started := time.Now()
	_, err := testRunner(f, supervise.StallRule{Window: 300 * time.Millisecond, Ceiling: 10 * time.Second}).Run(
		t.Context(), payload, supervise.UpdateSnapshotCommand, nil, nil)
	var stopped *UpdateCommandStoppedError
	if !errors.As(err, &stopped) {
		t.Fatalf("err = %v", err)
	}
	if got := f.signals(); len(got) != 0 {
		t.Fatalf("signals = %q, want none without a pid", got)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("the stop took %s", elapsed)
	}
}

func TestUpdateCommandRunnerFailsACommandWithoutAResult(t *testing.T) {
	f := &fakeWSL{t: t}
	payload := commandScript(t, startedLine+"\nexit 3")
	_, err := testRunner(f, supervise.StallRule{Window: 5 * time.Second, Ceiling: 10 * time.Second}).Run(
		t.Context(), payload, supervise.UpdateRestoreCommand, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "exited without a result") || !strings.Contains(err.Error(), "exit status 3") {
		t.Fatalf("err = %v", err)
	}
}

func TestUpdateCommandRunnerStopsOnCancel(t *testing.T) {
	f := &fakeWSL{t: t}
	payload := commandScript(t, failOnTerm+"\n"+startedLine+"\nwhile :; do "+progressLine+"; sleep 0.05; done")
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(300*time.Millisecond, cancel)
	_, err := testRunner(f, supervise.StallRule{Window: 5 * time.Second, Ceiling: 10 * time.Second}).Run(
		ctx, payload, supervise.UpdateTrialRunCommand, nil, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if got := f.signals(); len(got) != 1 || got[0] != "-TERM" {
		t.Fatalf("signals = %q", got)
	}
}

func TestUpdatePayloadsCommitAndRemove(t *testing.T) {
	f := &fakeWSL{t: t}
	dir := t.TempDir()
	record := supervise.LauncherRecord{
		Distro:        "Ubuntu",
		StablePayload: filepath.Join(dir, "agent-overflow"),
		StagedPayload: filepath.Join(dir, "agent-overflow.update-u1"),
	}
	write := func(path, contents string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(contents), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	read := func(path string) string {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	write(record.StablePayload, "old")
	write(record.StagedPayload, "new")
	payloads := UpdatePayloads{Runner: UpdateCommandRunner{Command: f.runner}}
	for range 2 {
		if err := payloads.CommitPayload(t.Context(), record); err != nil {
			t.Fatal(err)
		}
		if got := read(record.StablePayload); got != "new" {
			t.Fatalf("stable = %q after commit", got)
		}
		if _, err := os.Stat(record.StagedPayload); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("staged payload remains: %v", err)
		}
	}
	write(record.StagedPayload, "newer")
	for range 2 {
		if err := payloads.RemoveStagedPayload(t.Context(), record); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(record.StagedPayload); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staged payload remains: %v", err)
	}
	if got := read(record.StablePayload); got != "new" {
		t.Fatalf("removing the staged payload touched the stable one: %q", got)
	}
}

func TestUpdatePayloadsPreflight(t *testing.T) {
	f := &fakeWSL{t: t}
	payload := commandScript(t, `echo "a log line"; echo '{"protocolVersion":1,"version":"2.0.0"}'`)
	answer, err := UpdatePayloads{Runner: UpdateCommandRunner{Command: f.runner}}.Preflight(t.Context(), "Ubuntu", payload)
	if err != nil || answer.Version != "2.0.0" || answer.ProtocolVersion != 1 {
		t.Fatalf("preflight = %+v, %v", answer, err)
	}
}
