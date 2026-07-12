package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/harness/scenario"
)

// recordingWriter captures each Write call separately so chunked
// emission can be asserted at the syscall boundary.
type recordingWriter struct {
	mu     sync.Mutex
	writes []string
}

func (r *recordingWriter) Write(p []byte) (int, error) {
	r.mu.Lock()
	r.writes = append(r.writes, string(p))
	r.mu.Unlock()
	return len(p), nil
}

func (r *recordingWriter) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.writes...)
}

// stubAdapter answers approval requests instantly (or never, when
// respond is false).
type stubAdapter struct {
	respond bool
	allow   bool
}

func (s *stubAdapter) sendApproval(*scenario.ApprovalStep, scenario.Vars) (<-chan bool, func(), error) {
	ch := make(chan bool, 1)
	if s.respond {
		ch <- s.allow
	}
	return ch, func() {}, nil
}

func (s *stubAdapter) sendInterruptedTurn(scenario.Vars) {}

func newUnitEngine(t *testing.T, buf *bytes.Buffer) *engine {
	t.Helper()
	sc := &scenario.Scenario{Version: scenario.CurrentVersion, Name: "unit", Provider: scenario.ProviderClaude}
	e := newEngine(sc, t.TempDir(), t.TempDir(), newLineWriter(buf), &reporter{}, scenario.Vars{
		"SESSION_ID": "s1",
		"CWD":        "/ws",
	})
	e.exitFn = func(code int) { t.Fatalf("unexpected exit(%d)", code) }
	return e
}

func outputLines(buf *bytes.Buffer) []string {
	out := strings.TrimRight(buf.String(), "\n")
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

func TestEmitStepSubstitutesVars(t *testing.T) {
	var buf bytes.Buffer
	e := newUnitEngine(t, &buf)
	e.runSteps(e.varsForTurn(3), 3, []scenario.Step{{Emit: &scenario.EmitStep{Lines: []string{
		`{"session":"${SESSION_ID}","turn":${TURN},"turnId":"${TURN_ID}","cwd":"${CWD}","keep":"${UNKNOWN}"}`,
	}}}}, true)

	got := outputLines(&buf)
	want := `{"session":"s1","turn":3,"turnId":"turn-3","cwd":"/ws","keep":"${UNKNOWN}"}`
	if len(got) != 1 || got[0] != want {
		t.Fatalf("emit output = %q, want [%q]", got, want)
	}
}

func TestChunkedWriteSplitsAndReassembles(t *testing.T) {
	rec := &recordingWriter{}
	w := newLineWriter(rec)
	w.writeLine("abcdefgh", 3, 0)

	writes := rec.snapshot()
	if len(writes) != 3 || writes[0] != "abc" || writes[1] != "def" || writes[2] != "gh\n" {
		t.Fatalf("chunked writes = %q, want [abc def gh\\n]", writes)
	}
	if strings.Join(writes, "") != "abcdefgh\n" {
		t.Fatalf("reassembled = %q", strings.Join(writes, ""))
	}
}

func TestChunkedWritesDoNotInterleaveAcrossGoroutines(t *testing.T) {
	var buf bytes.Buffer
	w := newLineWriter(&buf)
	lineA := strings.Repeat("a", 50)
	lineB := strings.Repeat("b", 50)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); w.writeLine(lineA, 7, 1) }()
	go func() { defer wg.Done(); w.writeLine(lineB, 7, 1) }()
	wg.Wait()

	lines := outputLines(&buf)
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2: %q", len(lines), lines)
	}
	for _, line := range lines {
		if line != lineA && line != lineB {
			t.Fatalf("interleaved line: %q", line)
		}
	}
}

func TestFixtureWindowingUsesOriginalLineNumbers(t *testing.T) {
	var buf bytes.Buffer
	e := newUnitEngine(t, &buf)
	fixture := "# capture header\nline one ${SESSION_ID}\n\nline two\nline three\n"
	path := filepath.Join(e.fixtureRoot, "cap.ndjson")
	if err := os.WriteFile(path, []byte(fixture), 0o644); err != nil {
		t.Fatal(err)
	}

	// FromLine/ToLine count the comment and blank lines too.
	e.runSteps(e.varsForTurn(1), 1, []scenario.Step{{Fixture: &scenario.FixtureStep{
		Path: "cap.ndjson", FromLine: 2, ToLine: 4,
	}}}, true)

	got := outputLines(&buf)
	want := []string{"line one s1", "line two"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("fixture output = %q, want %q", got, want)
	}
}

func TestFixtureMissingFileSkipsStepAndContinues(t *testing.T) {
	var buf bytes.Buffer
	e := newUnitEngine(t, &buf)
	e.runSteps(e.varsForTurn(1), 1, []scenario.Step{
		{Fixture: &scenario.FixtureStep{Path: "nope.ndjson"}},
		{Emit: &scenario.EmitStep{Lines: []string{"after"}}},
	}, true)
	got := outputLines(&buf)
	if len(got) != 1 || got[0] != "after" {
		t.Fatalf("output after missing fixture = %q, want [after]", got)
	}
}

func TestWriteFileCreatesNestedAndAppends(t *testing.T) {
	var buf bytes.Buffer
	e := newUnitEngine(t, &buf)
	e.runSteps(e.varsForTurn(1), 1, []scenario.Step{
		{WriteFile: &scenario.WriteFileStep{Path: "sub/dir/f.txt", Content: "one\n"}},
		{WriteFile: &scenario.WriteFileStep{Path: "sub/dir/f.txt", Content: "two\n", Append: true}},
	}, true)

	data, err := os.ReadFile(filepath.Join(e.cwd, "sub", "dir", "f.txt"))
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	if string(data) != "one\ntwo\n" {
		t.Fatalf("content = %q, want %q", data, "one\ntwo\n")
	}

	// Truncate (Append=false) replaces.
	e.runStep(e.varsForTurn(1), 1, scenario.Step{WriteFile: &scenario.WriteFileStep{Path: "sub/dir/f.txt", Content: "fresh"}})
	data, _ = os.ReadFile(filepath.Join(e.cwd, "sub", "dir", "f.txt"))
	if string(data) != "fresh" {
		t.Fatalf("truncated content = %q, want fresh", data)
	}
}

func TestWriteFileRejectsEscapingPaths(t *testing.T) {
	outside := t.TempDir()
	var buf bytes.Buffer
	e := newUnitEngine(t, &buf)

	abs := filepath.Join(outside, "abs.txt")
	e.runStep(e.varsForTurn(1), 1, scenario.Step{WriteFile: &scenario.WriteFileStep{Path: abs, Content: "x"}})
	if _, err := os.Stat(abs); !os.IsNotExist(err) {
		t.Fatalf("absolute path was written: %v", err)
	}

	e.runStep(e.varsForTurn(1), 1, scenario.Step{WriteFile: &scenario.WriteFileStep{Path: "../escape.txt", Content: "x"}})
	if _, err := os.Stat(filepath.Join(e.cwd, "..", "escape.txt")); !os.IsNotExist(err) {
		t.Fatalf("parent-escaping path was written: %v", err)
	}

	// dir/../.. sneaking above the root must also be rejected.
	if _, err := normalizeWorkspaceRel("a/../../b.txt"); err == nil {
		t.Fatal("a/../../b.txt accepted")
	}
	// Interior .. that stays inside the workspace is fine.
	if rel, err := normalizeWorkspaceRel("a/../b.txt"); err != nil || rel != "b.txt" {
		t.Fatalf("a/../b.txt = %q, %v", rel, err)
	}
}

func TestApprovalBranches(t *testing.T) {
	step := scenario.Step{Approval: &scenario.ApprovalStep{
		ToolName: "Bash",
		OnAllow:  []scenario.Step{{Emit: &scenario.EmitStep{Lines: []string{"allowed"}}}},
		OnDeny:   []scenario.Step{{Emit: &scenario.EmitStep{Lines: []string{"denied"}}}},
	}}

	run := func(adapter protocolAdapter, timeoutMs int) []string {
		var buf bytes.Buffer
		e := newUnitEngine(t, &buf)
		e.adapter = adapter
		st := step
		ap := *st.Approval
		ap.TimeoutMs = timeoutMs
		st.Approval = &ap
		e.runStep(e.varsForTurn(1), 1, st)
		return outputLines(&buf)
	}

	if got := run(&stubAdapter{respond: true, allow: true}, 0); len(got) != 1 || got[0] != "allowed" {
		t.Fatalf("allow branch = %q", got)
	}
	if got := run(&stubAdapter{respond: true, allow: false}, 0); len(got) != 1 || got[0] != "denied" {
		t.Fatalf("deny branch = %q", got)
	}
	// Timeout falls through to OnDeny.
	if got := run(&stubAdapter{respond: false}, 20); len(got) != 1 || got[0] != "denied" {
		t.Fatalf("timeout branch = %q", got)
	}
}

func TestWaitSignalGateAdvance(t *testing.T) {
	var buf bytes.Buffer
	e := newUnitEngine(t, &buf)

	done := make(chan struct{})
	go func() {
		e.runWaitSignal(1, &scenario.WaitSignalStep{Name: "g1"})
		close(done)
	}()

	// Wait for the gate to open, then release it.
	deadline := time.Now().Add(2 * time.Second)
	for {
		e.mu.Lock()
		open := e.gate != nil
		e.mu.Unlock()
		if open {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("gate never opened")
		}
		time.Sleep(time.Millisecond)
	}

	e.advance("other") // wrong name buffers, must not release
	select {
	case <-done:
		t.Fatal("gate released by mismatched advance")
	case <-time.After(20 * time.Millisecond):
	}

	e.advance("g1")
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("gate not released by matching advance")
	}
}

func TestWaitSignalConsumesBufferedAdvance(t *testing.T) {
	var buf bytes.Buffer
	e := newUnitEngine(t, &buf)
	e.advance("") // arrives before the gate opens

	done := make(chan struct{})
	go func() {
		e.runWaitSignal(1, &scenario.WaitSignalStep{Name: "g1"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("buffered advance was not consumed")
	}
}

func TestWaitSignalInterruptBeforeGateOpenDoesNotBlock(t *testing.T) {
	var buf bytes.Buffer
	e := newUnitEngine(t, &buf)
	e.adapter = &stubAdapter{}
	e.startTurn(1)
	defer e.finishTurn(1)
	if !e.interruptTurn("") {
		t.Fatal("active turn was not interrupted")
	}

	done := make(chan struct{})
	go func() {
		e.runWaitSignal(1, &scenario.WaitSignalStep{Name: "late-gate"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("waitSignal installed a gate after the turn was interrupted")
	}
}

func TestApprovalInterruptDoesNotBlockOnStoppedTimer(t *testing.T) {
	var buf bytes.Buffer
	e := newUnitEngine(t, &buf)
	e.adapter = &stubAdapter{respond: false}
	e.startTurn(1)
	defer e.finishTurn(1)

	done := make(chan struct{})
	go func() {
		e.runApproval(e.varsForTurn(1), 1, &scenario.ApprovalStep{ToolName: "Bash", TimeoutMs: 5_000})
		close(done)
	}()
	if !e.interruptTurn("") {
		t.Fatal("approval turn was not interrupted")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("approval interrupt blocked while stopping its timer")
	}
}

func TestAfterTurnsPolicies(t *testing.T) {
	turnLines := []string{`turn ${TURN}`}
	base := func() *scenario.Scenario {
		return &scenario.Scenario{
			Version:  scenario.CurrentVersion,
			Name:     "after",
			Provider: scenario.ProviderClaude,
			Turns: []scenario.Turn{{Steps: []scenario.Step{
				{Emit: &scenario.EmitStep{Lines: turnLines}},
			}}},
		}
	}

	t.Run("repeatLast", func(t *testing.T) {
		var buf bytes.Buffer
		e := newUnitEngine(t, &buf)
		e.sc = base()
		e.runTurn(1)
		e.runTurn(2)
		got := outputLines(&buf)
		if len(got) != 2 || got[0] != "turn 1" || got[1] != "turn 2" {
			t.Fatalf("repeatLast output = %q", got)
		}
	})

	t.Run("silent", func(t *testing.T) {
		var buf bytes.Buffer
		e := newUnitEngine(t, &buf)
		e.adapter = &stubAdapter{}
		sc := base()
		sc.AfterTurns = "silent"
		e.sc = sc
		done := make(chan struct{})
		go func() {
			e.runTurn(2)
			close(done)
		}()
		deadline := time.Now().Add(2 * time.Second)
		for {
			e.mu.Lock()
			active := e.activeTurn == 2
			e.mu.Unlock()
			if active {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("silent turn never became active")
			}
			time.Sleep(time.Millisecond)
		}
		if !e.interruptTurn("") {
			t.Fatal("silent turn was not interruptible")
		}
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("silent turn did not finish after interrupt")
		}
		if got := outputLines(&buf); len(got) != 0 {
			t.Fatalf("silent output = %q", got)
		}
	})

	t.Run("exit", func(t *testing.T) {
		var buf bytes.Buffer
		e := newUnitEngine(t, &buf)
		sc := base()
		sc.AfterTurns = "exit"
		e.sc = sc
		var code = -1
		e.exitFn = func(c int) { code = c }
		e.runTurn(2)
		if code != 0 {
			t.Fatalf("exit code = %d, want 0", code)
		}
	})
}

func TestExitStepUsesConfiguredCode(t *testing.T) {
	var buf bytes.Buffer
	e := newUnitEngine(t, &buf)
	var code = -1
	e.exitFn = func(c int) { code = c }
	e.runStep(e.varsForTurn(1), 1, scenario.Step{Exit: &scenario.ExitStep{Code: 3}})
	if code != 3 {
		t.Fatalf("exit code = %d, want 3", code)
	}
}
