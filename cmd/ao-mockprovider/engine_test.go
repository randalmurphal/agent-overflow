package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/harness/control"
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

// recordingReporter captures every control report the engine posts, so a
// unit test can assert on a surface whose real consumer is another
// process.
type recordingReporter struct {
	reporter *reporter
	mu       sync.Mutex
	reports  []control.Report
}

func newRecordingReporter() *recordingReporter {
	rec := &recordingReporter{}
	rec.reporter = &reporter{observe: func(rep control.Report) {
		rec.mu.Lock()
		rec.reports = append(rec.reports, rep)
		rec.mu.Unlock()
	}}
	return rec
}

func (r *recordingReporter) snapshot() []control.Report {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]control.Report(nil), r.reports...)
}

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

	// The stranded "other" advance dies WITH ITS TURN. Left buffered it
	// would release the first gate of some later turn, which reads as a
	// mock skipping a step for no reason.
	e.finishTurn(1)
	e.mu.Lock()
	stranded := len(e.pendingAdvances)
	e.mu.Unlock()
	if stranded != 0 {
		t.Fatalf("pendingAdvances after the turn ended = %d, want 0 (the stranded advance must be discarded)", stranded)
	}
}

// TestAdvanceReportsReleaseAndBuffering pins the two control reports a
// driving test reads to tell "my advance opened the gate" from "my
// advance did nothing" — the pair is otherwise unobservable outside the
// mock process, since a buffered advance looks exactly like one that was
// never delivered.
func TestAdvanceReportsReleaseAndBuffering(t *testing.T) {
	var buf bytes.Buffer
	e := newUnitEngine(t, &buf)
	rec := newRecordingReporter()
	e.rep = rec.reporter
	e.startTurn(1)

	// Buffered: no gate open at all.
	e.advance("early")
	// Buffered against a gate that does not match.
	released := make(chan struct{})
	go func() {
		e.runWaitSignal(1, &scenario.WaitSignalStep{Name: "g1"})
		close(released)
	}()
	waitForGate(t, e)
	e.advance("mismatch")
	e.advance("g1")
	select {
	case <-released:
	case <-time.After(2 * time.Second):
		t.Fatal("gate not released")
	}

	reports := rec.snapshot()
	var buffered []control.Report
	var releases []control.Report
	for _, rep := range reports {
		switch rep.Kind {
		case control.ReportAdvanceBuffered:
			buffered = append(buffered, rep)
		case control.ReportAdvanceReleased:
			releases = append(releases, rep)
		}
	}
	if len(buffered) != 2 {
		t.Fatalf("advance_buffered reports = %+v, want 2", buffered)
	}
	if buffered[0].Gate != "early" || buffered[0].OpenGate != "" {
		t.Errorf("first buffered report = %+v, want gate=early with no open gate", buffered[0])
	}
	if buffered[1].Gate != "mismatch" || buffered[1].OpenGate != "g1" {
		t.Errorf("second buffered report = %+v, want gate=mismatch openGate=g1", buffered[1])
	}
	if len(releases) != 1 || releases[0].Gate != "g1" {
		t.Fatalf("advance_released reports = %+v, want one for g1", releases)
	}
}

// TestBufferedAdvanceDoesNotCrossTurns is the leak this buffering used to
// have: an advance stranded in one turn released the first gate of a
// later one, minutes and turns away from the command that caused it.
func TestBufferedAdvanceDoesNotCrossTurns(t *testing.T) {
	var buf bytes.Buffer
	e := newUnitEngine(t, &buf)
	e.startTurn(1)
	e.advance("") // unnamed: releases whichever gate opens next, in TURN 1
	e.finishTurn(1)

	e.startTurn(2)
	defer e.finishTurn(2)
	done := make(chan struct{})
	go func() {
		e.runWaitSignal(2, &scenario.WaitSignalStep{Name: "g1"})
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("an advance buffered in turn 1 released a gate in turn 2")
	case <-time.After(50 * time.Millisecond):
	}
	e.advance("g1")
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("gate not released by its own turn's advance")
	}
}

// TestInterruptDiscardsBufferedAdvances: an interrupt ends the turn, so
// the advances it was holding end with it. Otherwise the abort leaves a
// live command behind that fires into the next turn.
func TestInterruptDiscardsBufferedAdvances(t *testing.T) {
	var buf bytes.Buffer
	e := newUnitEngine(t, &buf)
	e.adapter = &stubAdapter{}
	e.startTurn(1)
	e.advance("stranded")
	e.mu.Lock()
	buffered := len(e.pendingAdvances)
	e.mu.Unlock()
	if buffered != 1 {
		t.Fatalf("pendingAdvances before interrupt = %d, want 1", buffered)
	}
	if !e.interruptTurn("") {
		t.Fatal("active turn was not interrupted")
	}
	e.mu.Lock()
	buffered = len(e.pendingAdvances)
	e.mu.Unlock()
	if buffered != 0 {
		t.Fatalf("pendingAdvances after interrupt = %d, want 0", buffered)
	}
}

// TestAdvanceBufferIsCapped: the buffer tolerates a command RACING its
// gate, not a driver queueing dozens of them. Past the cap an advance is
// discarded with a report rather than parked where nothing reads it.
func TestAdvanceBufferIsCapped(t *testing.T) {
	var buf bytes.Buffer
	e := newUnitEngine(t, &buf)
	rec := newRecordingReporter()
	e.rep = rec.reporter
	e.startTurn(1)
	defer e.finishTurn(1)

	for i := 0; i < maxPendingAdvances+3; i++ {
		e.advance("g")
	}
	e.mu.Lock()
	buffered := len(e.pendingAdvances)
	e.mu.Unlock()
	if buffered != maxPendingAdvances {
		t.Fatalf("buffered advances = %d, want the cap %d", buffered, maxPendingAdvances)
	}
	drops := 0
	for _, rep := range rec.snapshot() {
		if rep.Kind == control.ReportAdvanceBuffered && rep.Detail == control.AdvanceDroppedDetail {
			drops++
		}
	}
	if drops != 3 {
		t.Fatalf("reported drops = %d, want 3", drops)
	}
}

// waitForGate blocks until the engine has a gate open.
func waitForGate(t *testing.T, e *engine) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		e.mu.Lock()
		open := e.gate != nil
		e.mu.Unlock()
		if open {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("gate never opened")
		}
		time.Sleep(time.Millisecond)
	}
}

// TestWaitSignalConsumesBufferedAdvance is the race tolerance the buffer
// exists for: an advance command that beats the gate it targets by a few
// milliseconds still releases it — WITHIN the turn it was issued in
// (TestBufferedAdvanceDoesNotCrossTurns covers the other side).
func TestWaitSignalConsumesBufferedAdvance(t *testing.T) {
	var buf bytes.Buffer
	e := newUnitEngine(t, &buf)
	e.startTurn(1)
	defer e.finishTurn(1)
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

// TestRepeatStepBoundedIterations pins the two things a bounded repeat
// owes its author: the body runs exactly Count times, and ${ITER} is
// rebound per iteration so emitted ids stay unique.
func TestRepeatStepBoundedIterations(t *testing.T) {
	var buf bytes.Buffer
	e := newUnitEngine(t, &buf)
	e.runSteps(e.varsForTurn(2), 2, []scenario.Step{{Repeat: &scenario.RepeatStep{
		Count: 3,
		Steps: []scenario.Step{{Emit: &scenario.EmitStep{Lines: []string{
			`{"turn":${TURN},"iter":${ITER},"cwd":"${CWD}"}`,
		}}}},
	}}}, true)

	got := outputLines(&buf)
	want := []string{
		`{"turn":2,"iter":1,"cwd":"/ws"}`,
		`{"turn":2,"iter":2,"cwd":"/ws"}`,
		`{"turn":2,"iter":3,"cwd":"/ws"}`,
	}
	if len(got) != len(want) {
		t.Fatalf("repeat emitted %d lines, want %d: %q", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i+1, got[i], want[i])
		}
	}
}

// TestRepeatStepLeavesCallerVarsUnchanged guards the map copy: the
// caller's vars are shared with concurrent readers (control "emit"
// commands snapshot them), so an in-place ${ITER} write would be a data
// race and would leak a stale iteration number after the loop.
func TestRepeatStepLeavesCallerVarsUnchanged(t *testing.T) {
	var buf bytes.Buffer
	e := newUnitEngine(t, &buf)
	vars := e.varsForTurn(1)
	e.runRepeat(vars, 1, &scenario.RepeatStep{
		Count: 2,
		Steps: []scenario.Step{{Emit: &scenario.EmitStep{Lines: []string{`{"i":${ITER}}`}}}},
	})
	if _, ok := vars["ITER"]; ok {
		t.Fatalf("runRepeat mutated the caller's vars: %v", vars)
	}
}

// TestRepeatStepUnboundedStopsOnInterrupt is what makes an infinite
// repeat safe to ship: a scenario that never completes must still end
// the moment the app interrupts the turn.
func TestRepeatStepUnboundedStopsOnInterrupt(t *testing.T) {
	var buf bytes.Buffer
	e := newUnitEngine(t, &buf)
	e.adapter = &stubAdapter{}
	e.startTurn(1)
	defer e.finishTurn(1)

	done := make(chan struct{})
	go func() {
		e.runRepeat(e.varsForTurn(1), 1, &scenario.RepeatStep{
			Count: 0,
			Steps: []scenario.Step{
				{DelayMs: 5},
				{Emit: &scenario.EmitStep{Lines: []string{`{"i":${ITER}}`}}},
			},
		})
		close(done)
	}()
	// Let a few iterations land so the interrupt lands mid-loop rather
	// than before it starts.
	time.Sleep(30 * time.Millisecond)
	if !e.interruptTurn("") {
		t.Fatal("active turn was not interrupted")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("unbounded repeat kept running after the turn was interrupted")
	}
	if len(outputLines(&buf)) == 0 {
		t.Fatal("unbounded repeat emitted nothing before the interrupt")
	}
}

// TestScenarioDoneFiresOncePerTurn is the fix for a latch that fired
// once per PROCESS.
//
// Under the default afterTurns:repeatLast, turns 2..N re-run the last
// scripted turn and finish exactly as turn 1 did — but the old
// doneReported bool meant only turn 1 ever announced it. A harness test
// that awaits the scenario boundary per turn (the normal shape: send,
// await, assert, send again) hung forever on the second send against a
// mock that had already done the work.
func TestScenarioDoneFiresOncePerTurn(t *testing.T) {
	var buf bytes.Buffer
	rec := newRecordingReporter()
	sc := &scenario.Scenario{
		Version:  scenario.CurrentVersion,
		Name:     "repeat-last",
		Provider: scenario.ProviderClaude,
		// One scripted turn, so turns 2 and 3 both land on repeatLast.
		Turns: []scenario.Turn{{Label: "only", Steps: []scenario.Step{{Emit: &scenario.EmitStep{Lines: []string{`{"t":${TURN}}`}}}}}},
	}
	e := newEngine(sc, t.TempDir(), t.TempDir(), newLineWriter(&buf), rec.reporter, scenario.Vars{"SESSION_ID": "s1"})
	e.exitFn = func(code int) { t.Fatalf("unexpected exit(%d)", code) }
	e.adapter = &stubAdapter{}

	for n := 1; n <= 3; n++ {
		e.runTurn(n)
	}

	var doneTurns []int
	for _, rep := range rec.snapshot() {
		if rep.Kind == control.ReportScenarioDone {
			doneTurns = append(doneTurns, rep.Turn)
		}
	}
	if len(doneTurns) != 3 {
		t.Fatalf("scenario_done turns = %v, want one per turn (1,2,3)", doneTurns)
	}
	for i, turn := range doneTurns {
		if turn != i+1 {
			t.Fatalf("scenario_done turns = %v, want them stamped 1,2,3 so a per-turn await can match", doneTurns)
		}
	}
}

// TestScenarioDoneDedupesWithinOneTurn: the per-turn dedupe still has to
// hold, because runTurn can reach the report from more than one path
// inside a single turn number.
func TestScenarioDoneDedupesWithinOneTurn(t *testing.T) {
	var buf bytes.Buffer
	rec := newRecordingReporter()
	sc := &scenario.Scenario{Version: scenario.CurrentVersion, Name: "unit", Provider: scenario.ProviderClaude}
	e := newEngine(sc, t.TempDir(), t.TempDir(), newLineWriter(&buf), rec.reporter, scenario.Vars{"SESSION_ID": "s1"})

	e.reportScenarioDone(2)
	e.reportScenarioDone(2)

	var count int
	for _, rep := range rec.snapshot() {
		if rep.Kind == control.ReportScenarioDone {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("scenario_done reported %d times for one turn, want 1", count)
	}
}

// TestScenarioDoneEntriesDoNotAccumulate: the dedupe map is bounded by
// the live turn, not by process lifetime. A soak runs unboundedly many
// turns, and a map that grew one entry per turn would be a leak with a
// fixed rate.
func TestScenarioDoneEntriesDoNotAccumulate(t *testing.T) {
	var buf bytes.Buffer
	e := newUnitEngine(t, &buf)
	e.adapter = &stubAdapter{}
	for n := 1; n <= 50; n++ {
		e.startTurn(n)
		e.reportScenarioDone(n)
		e.finishTurn(n)
	}
	e.mu.Lock()
	remaining := len(e.doneTurns)
	e.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("doneTurns retained %d entries after every turn finished", remaining)
	}
}

// TestCoalescedEmitIsOneWrite: a scenario asking for coalesce must reach
// the provider's stdin as ONE write, because the defect it reproduces
// (a reader that mishandles several NDJSON lines arriving in a single
// read) is invisible when each line gets its own syscall.
func TestCoalescedEmitIsOneWrite(t *testing.T) {
	rec := &recordingWriter{}
	sc := &scenario.Scenario{Version: scenario.CurrentVersion, Name: "unit", Provider: scenario.ProviderClaude}
	e := newEngine(sc, t.TempDir(), t.TempDir(), newLineWriter(rec), &reporter{}, scenario.Vars{"SESSION_ID": "s1"})

	lines := []string{`{"a":1}`, `{"b":"${SESSION_ID}"}`, `{"c":3}`}
	e.runEmit(e.varsForTurn(0), &scenario.EmitStep{Lines: lines, Coalesce: true})

	writes := rec.snapshot()
	if len(writes) != 1 {
		t.Fatalf("coalesced emit made %d writes, want 1: %q", len(writes), writes)
	}
	got := strings.Split(strings.TrimRight(writes[0], "\n"), "\n")
	want := []string{`{"a":1}`, `{"b":"s1"}`, `{"c":3}`}
	if len(got) != len(want) {
		t.Fatalf("coalesced write = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("coalesced write line %d = %q, want %q (substitution must still run)", i, got[i], want[i])
		}
	}
}

// TestUncoalescedEmitStaysOneWritePerLine guards the default: coalesce
// is opt-in, and every existing scenario depends on line-at-a-time
// delivery.
func TestUncoalescedEmitStaysOneWritePerLine(t *testing.T) {
	rec := &recordingWriter{}
	sc := &scenario.Scenario{Version: scenario.CurrentVersion, Name: "unit", Provider: scenario.ProviderClaude}
	e := newEngine(sc, t.TempDir(), t.TempDir(), newLineWriter(rec), &reporter{}, scenario.Vars{"SESSION_ID": "s1"})

	e.runEmit(e.varsForTurn(0), &scenario.EmitStep{Lines: []string{`{"a":1}`, `{"b":2}`, `{"c":3}`}})

	if writes := rec.snapshot(); len(writes) != 3 {
		t.Fatalf("plain emit made %d writes, want one per line: %q", len(writes), writes)
	}
}

// TestStartupDelayHoldsTheFirstFrameOnce: startupDelayMs exists to
// reproduce a slow provider start, which is only observable BEFORE the
// first frame. It must also be paid once — a per-frame sleep would turn
// a 5s spawn-delay scenario into a 5s-per-turn scenario.
func TestStartupDelayHoldsTheFirstFrameOnce(t *testing.T) {
	var buf bytes.Buffer
	sc := &scenario.Scenario{
		Version:        scenario.CurrentVersion,
		Name:           "slow-start",
		Provider:       scenario.ProviderClaude,
		StartupDelayMs: 40,
	}
	e := newEngine(sc, t.TempDir(), t.TempDir(), newLineWriter(&buf), &reporter{}, scenario.Vars{"SESSION_ID": "s1"})

	start := time.Now()
	e.awaitStartupDelay()
	first := time.Since(start)
	if first < 30*time.Millisecond {
		t.Fatalf("first frame waited %v, want roughly the scenario's 40ms", first)
	}

	start = time.Now()
	e.awaitStartupDelay()
	if second := time.Since(start); second > 20*time.Millisecond {
		t.Fatalf("the delay was paid again (%v); it must hold only the FIRST frame", second)
	}
}
