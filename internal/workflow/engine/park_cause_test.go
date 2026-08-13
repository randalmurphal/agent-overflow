package engine

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"agent-overflow/internal/workflow/def"
)

// recordingLog is the run-lifecycle sink under test. It is a slice rather than a
// file because the engine's contract is the record it emits, not where the app
// puts it.
type recordingLog struct {
	mu     sync.Mutex
	events []LogEvent
}

func (l *recordingLog) LogEngineEvent(event LogEvent) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, event)
}

func (l *recordingLog) matching(event string) []LogEvent {
	l.mu.Lock()
	defer l.mu.Unlock()
	var found []LogEvent
	for _, candidate := range l.events {
		if candidate.Event == event {
			found = append(found, candidate)
		}
	}
	return found
}

// A resource the project never sized is an engine-diagnosed park, and the phase
// attempt already exists when it happens — so the cause lands on that row rather
// than on a new one, and the envelope stays empty because no turn ran.
func TestUnsizedResourceParksWithItsCauseOnTheAttempt(t *testing.T) {
	h := newHarness(t, Config{}, map[string]def.Workflow{
		"flow": onePhaseWorkflow("flow", []string{"unsized"}, []def.Route{{To: "done"}}),
	}, []string{"p"}, nil)

	if err := h.engine.StartItem(testItem("item", "p", "flow", 0)); err == nil {
		t.Fatal("a phase claiming an unsized resource started")
	}
	requireItemState(t, h.store, "item", StateNeedsHuman, ReasonSetupFailed)
	requireParkCause(t, h.phaseAttempt(t, "item", "work", 1), `resource "unsized"`)
}

// A run whose workflow will not resolve has no phase to rest an attempt on, so
// the cause reaches the caller and the engine log and stops there. Zero attempt
// rows is the honest record of a run that never entered a phase — but a park
// with no record ANYWHERE is the undiagnosable case this log exists to close.
func TestUnresolvableDefinitionParksWithNoAttemptRowAndLogsItsCause(t *testing.T) {
	sink := &recordingLog{}
	h := newHarness(t, Config{Log: sink}, map[string]def.Workflow{}, []string{"p"}, nil)

	if err := h.engine.StartItem(testItem("item", "p", "missing", 0)); err == nil {
		t.Fatal("a run with no resolvable workflow started")
	}
	requireItemState(t, h.store, "item", StateNeedsHuman, ReasonSetupFailed)
	phases, err := h.store.ListWorkItemPhases("item")
	if err != nil {
		t.Fatal(err)
	}
	if len(phases) != 0 {
		t.Fatalf("phase rows = %+v, want none: the run never named a phase", phases)
	}
	parks := sink.matching(LogEventPark)
	if len(parks) != 1 {
		t.Fatalf("park log events = %+v, want exactly one", parks)
	}
	if parks[0].ItemID != "item" || parks[0].Reason != ReasonSetupFailed ||
		!strings.Contains(parks[0].Message, "missing") {
		t.Fatalf("park log event = %+v, want the unresolvable workflow named", parks[0])
	}
}

// Cancel and resume are the other two lifecycle decisions a reader reconstructs
// after the fact: which park a cancel ended, and whether a resume re-read the
// definition. Neither leaves a durable trace of its own — the run row shows only
// where it ended up — so the log is the whole record.
func TestCancelAndResumeAreLogged(t *testing.T) {
	sink := &recordingLog{}
	h := newHarness(t, Config{Log: sink}, map[string]def.Workflow{
		"flow": onePhaseWorkflow("flow", nil, []def.Route{{To: "done"}}),
	}, []string{"p"}, nil)
	if err := h.engine.StartItem(testItem("item", "p", "flow", 0)); err != nil {
		t.Fatal(err)
	}
	h.runner.complete(t, "item", Outcome{Kind: OutcomeStuck, Envelope: stuckEnvelope()})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, "item", StateNeedsHuman, ReasonStuck)

	if err := h.engine.Resume("item", "", false); err != nil {
		t.Fatal(err)
	}
	resumes := sink.matching(LogEventResume)
	if len(resumes) != 1 || resumes[0].ItemID != "item" || resumes[0].PhaseID != "work" {
		t.Fatalf("resume log events = %+v", resumes)
	}
	if err := h.engine.Cancel("item"); err != nil {
		t.Fatal(err)
	}
	cancels := sink.matching(LogEventCancel)
	if len(cancels) != 1 || cancels[0].ItemID != "item" || cancels[0].State != StateCancelled {
		t.Fatalf("cancel log events = %+v", cancels)
	}
}

// A park an AGENT authored carries no engine cause: the envelope it rested with
// is the account, and engine prose alongside it would be a second, competing
// one. This is the negative half of the contract every park-cause assertion
// above makes.
func TestAgentAuthoredParkRecordsNoEngineCause(t *testing.T) {
	h := newHarness(t, Config{}, map[string]def.Workflow{
		"flow": onePhaseWorkflow("flow", nil, []def.Route{{To: "done"}}),
	}, []string{"p"}, nil)
	if err := h.engine.StartItem(testItem("item", "p", "flow", 0)); err != nil {
		t.Fatal(err)
	}
	h.runner.complete(t, "item", Outcome{Kind: OutcomeStuck, Envelope: stuckEnvelope()})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	attempt := h.phaseAttempt(t, "item", "work", 1)
	if attempt.ParkCause != "" {
		t.Fatalf("agent-authored park carries an engine cause: %q", attempt.ParkCause)
	}
	if len(attempt.OutputEnvelope) == 0 {
		t.Fatal("agent-authored park lost the envelope it rested on")
	}
}

// A completed attempt that is reopened for repair drops the cause with the
// park: the row is being re-run, and a stale diagnosis on a live attempt reads
// as a park that already happened again.
func TestReopeningAParkedAttemptClearsItsCause(t *testing.T) {
	h := newHarness(t, Config{}, map[string]def.Workflow{
		"flow": onePhaseWorkflow("flow", []string{"unsized"}, []def.Route{{To: "done"}}),
	}, []string{"p"}, nil)
	if err := h.engine.StartItem(testItem("item", "p", "flow", 0)); err == nil {
		t.Fatal("a phase claiming an unsized resource started")
	}
	if cause := h.phaseAttempt(t, "item", "work", 1).ParkCause; cause == "" {
		t.Fatal("the setup-failed park recorded no cause")
	}
	if err := h.store.ReopenWorkItemPhase("item", "work", 1); err != nil {
		t.Fatal(err)
	}
	if cause := h.phaseAttempt(t, "item", "work", 1).ParkCause; cause != "" {
		t.Fatalf("reopened attempt still carries its park cause: %q", cause)
	}
}

// A cause past the byte bound is truncated with the fact stated, and cut on a
// rune boundary: the column is TEXT, and half a rune would be invalid UTF-8 in
// the store before any reader got the chance to quote it.
func TestParkCauseTextTruncatesOnARuneBoundary(t *testing.T) {
	if got := parkCauseText(nil); got != "" {
		t.Fatalf("nil cause = %q, want empty", got)
	}
	short := errors.New("branch already exists")
	if got := parkCauseText(short); got != short.Error() {
		t.Fatalf("short cause = %q, want it unchanged", got)
	}
	// Multi-byte runes straddling the bound in every alignment.
	for offset := range 3 {
		long := errors.New(strings.Repeat("x", offset) + strings.Repeat("é", MaxParkCauseBytes))
		got := parkCauseText(long)
		if !strings.HasSuffix(got, " …(cause truncated)") {
			t.Fatalf("offset %d: truncated cause does not say so: %q", offset, got)
		}
		if !utf8.ValidString(got) {
			t.Fatalf("offset %d: truncation produced invalid UTF-8", offset)
		}
		if len(got) > MaxParkCauseBytes+len(" …(cause truncated)") {
			t.Fatalf("offset %d: truncated cause is %d bytes, past the bound", offset, len(got))
		}
	}
}

// The resume log's note is the only record of WHICH resume a run took, and the
// shapes do materially different things: one continues the parked attempt on its
// own session, the other discards it and re-enters the phase from its inputs.
//
// `ContinuableReason` does not decide that — it only decides which PATH a bare
// resume takes, and the continuation path re-enters fresh wherever there is
// nothing to continue: a `driver: tool` phase never held a session, and an agent
// phase's thread can be deleted under it. A note taken from the request called
// every one of those a continuation, which is exactly the set a reader consults
// this log about.
func TestResumeLogNoteStatesWhichResumeHappened(t *testing.T) {
	toolPhase := def.Phase{
		ID: "check", Driver: def.DriverTool, Check: "test",
		Outputs: map[string]def.Variable{"ok": {Schema: def.JSONSchema{Type: "boolean"}}},
		Gate:    def.Gate{Routes: []def.Route{{To: "done"}}},
	}
	sink := &recordingLog{}
	h := newHarness(t, Config{Log: sink}, map[string]def.Workflow{
		"flow": onePhaseWorkflow("flow", nil, []def.Route{{To: "done"}}),
		"tool": {ID: "tool", Phases: []def.Phase{toolPhase}},
	}, []string{"project"}, nil)

	// A `stuck` park is not continuable at all, so a bare resume re-enters.
	if err := h.engine.StartItem(testItem("stuck-park", "project", "flow", 0)); err != nil {
		t.Fatal(err)
	}
	h.runner.complete(t, "stuck-park", Outcome{Kind: OutcomeStuck, Envelope: stuckEnvelope()})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, "stuck-park", StateNeedsHuman, ReasonStuck)
	if err := h.engine.Resume("stuck-park", "", false); err != nil {
		t.Fatal(err)
	}

	// A `paused` agent phase whose session is still there is the one shape that
	// really does continue.
	if err := h.engine.StartItem(testItem("warm", "project", "flow", 1)); err != nil {
		t.Fatal(err)
	}
	seedThread(t, h.store, "warm-thread")
	if err := h.store.AttachWorkItemPhaseRun("warm", "work", 1, "warm-thread", ""); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.PauseItem("warm"); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, "warm", StateNeedsHuman, ReasonPaused)
	if err := h.engine.Resume("warm", "", false); err != nil {
		t.Fatal(err)
	}

	// A `paused` TOOL phase took the continuation path and re-entered fresh: it
	// never held a session, which is the common case this note used to misreport.
	if err := h.engine.StartItem(testItem("tool-park", "project", "tool", 2)); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.PauseItem("tool-park"); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, "tool-park", StateNeedsHuman, ReasonPaused)
	if err := h.engine.Resume("tool-park", "", false); err != nil {
		t.Fatal(err)
	}

	// An agent phase whose thread the app no longer has does the same, and says
	// so differently: a deleted session is a fact worth correlating.
	if err := h.engine.StartItem(testItem("lost", "project", "flow", 3)); err != nil {
		t.Fatal(err)
	}
	if err := h.store.AttachWorkItemPhaseRun("lost", "work", 1, "deleted-thread", ""); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.PauseItem("lost"); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Resume("lost", "", false); err != nil {
		t.Fatal(err)
	}

	notes := map[string]string{}
	for _, event := range sink.matching(LogEventResume) {
		if previous, seen := notes[event.ItemID]; seen {
			t.Fatalf("run %s logged two resume notes: %q then %q", event.ItemID, previous, event.Message)
		}
		notes[event.ItemID] = event.Message
	}
	for _, want := range []struct{ run, contains string }{
		{"stuck-park", "fresh entry into the parked phase"},
		{"warm", "continuing the parked attempt on its own session"},
		{"tool-park", "fresh entry into the parked phase: the parked attempt held no provider session"},
		{"lost", "fresh entry into the parked phase: the attempt's provider session is unavailable"},
	} {
		if got := notes[want.run]; !strings.Contains(got, want.contains) {
			// Reported rather than fatal so one run's wording does not hide the
			// others: the branches fail independently.
			t.Errorf("run %s logged %q, want the resume it actually took (%q)", want.run, got, want.contains)
		}
	}
}
