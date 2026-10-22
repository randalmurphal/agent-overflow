package claudetui

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/testutil/mockexec"
)

var testSessionStartHook = provider.SessionStartHook{Label: "the memory", Command: `'/opt/ao' deliver '/h'`, Heading: "## Memory", Ending: "(End of memory)", Stamp: "(Delivered at "}

// testHook is a copy of testSessionStartHook that runs at a resume when
// resume is set.
func testHook(resume bool) *provider.SessionStartHook {
	hook := testSessionStartHook
	hook.Resume = resume
	return &hook
}

// The hook joins the relay's SessionStart entry as a matcher group of its
// own, and nothing else in the block changes.
func TestHookSettingsRegisterTheSessionStartHook(t *testing.T) {
	decode := func(cfg Config) map[string][]any {
		t.Helper()
		rendered, err := hookSettingsJSON(cfg)
		if err != nil {
			t.Fatalf("hookSettingsJSON: %v", err)
		}
		var block struct {
			Hooks map[string][]any `json:"hooks"`
		}
		if err := json.Unmarshal([]byte(rendered), &block); err != nil {
			t.Fatalf("settings %s: %v", rendered, err)
		}
		return block.Hooks
	}
	without := decode(Config{HookCmd: "/opt/ao"})
	hook := testHook(true)
	with := decode(Config{HookCmd: "/opt/ao", SessionStartHook: hook})
	if len(without["SessionStart"]) != 1 {
		t.Fatalf("relay-only SessionStart = %v", without["SessionStart"])
	}
	got, _ := json.Marshal(with["SessionStart"])
	want, _ := json.Marshal(append(append([]any{}, without["SessionStart"]...), claude.SessionStartHookGroup(*hook)))
	if string(got) != string(want) {
		t.Fatalf("SessionStart = %s\nwant           %s", got, want)
	}
	for event, entries := range without {
		if event == "SessionStart" {
			continue
		}
		a, _ := json.Marshal(entries)
		b, _ := json.Marshal(with[event])
		if string(a) != string(b) {
			t.Errorf("%s changed: %s, was %s", event, b, a)
		}
	}
}

// sessionStartTestSession is newWiredSession with the hook and without its
// feed loop, so the recorder holds only what the SessionStart check emits.
func sessionStartTestSession(t *testing.T, resume string) (*Session, *eventRecorder) {
	t.Helper()
	return sessionStartTestSessionWith(t, resume, testHook(false), nil)
}

func sessionStartTestSessionWith(t *testing.T, resume string, hook *provider.SessionStartHook, receipts *provider.SessionStartReceipts) (*Session, *eventRecorder) {
	t.Helper()
	delivery, err := claude.NewSessionStartDelivery(hook, receipts, resume)
	if err != nil {
		t.Fatalf("NewSessionStartDelivery: %v", err)
	}
	rec := &eventRecorder{}
	s := &Session{
		threadID:          testThread,
		sessionID:         "sess-1",
		parser:            newSessionParser(),
		feed:              make(chan json.RawMessage, 16),
		done:              make(chan struct{}),
		logf:              func(string, ...any) {},
		onEvent:           rec.record,
		sessionStart:      delivery,
		sessionStartSince: time.Now(),
	}
	s.rec = newReconstructor(s.feedEnvelope)
	return s, rec
}

// requestBody is a main request whose user message carries texts, each a
// text block of its own.
func requestBody(texts ...string) []byte {
	content := make([]any, 0, len(texts))
	for _, text := range texts {
		content = append(content, map[string]any{"type": "text", "text": text})
	}
	body, _ := json.Marshal(map[string]any{"model": "claude-opus-4-7", "messages": []any{map[string]any{"role": "user", "content": content}}})
	return body
}

// hookBlock is the system reminder Claude Code sends a SessionStart run's
// contexts in (2.1.261 to 2.1.283).
func hookBlock(contexts ...string) string {
	return "<system-reminder>\nSessionStart hook additional context: " + strings.Join(contexts, "\n") + "\n</system-reminder>"
}

const memoryContext = "## Memory\n\n- a\n\n(End of memory)"

// stampedAt is context as a run at at delivers it, stamped under its
// heading.
func stampedAt(context string, at time.Time) string {
	heading, rest, _ := strings.Cut(context, "\n")
	return heading + "\n(Delivered at " + at.UTC().Format("2006-01-02T15:04:05.000Z07:00") + "; it replaces any earlier copy.)\n" + rest
}

// The context of a run after the expectation opened, and of one before it.
var (
	memoryNow = stampedAt(memoryContext, time.Now().Add(time.Minute))
	memoryOld = stampedAt(memoryContext, time.Now().Add(-time.Hour))
)

func onlyWarning(t *testing.T, events []provider.ProviderEvent, prefix string) {
	t.Helper()
	if len(events) != 1 || events[0].Kind != provider.EventError || !strings.HasPrefix(events[0].Content, prefix) {
		t.Fatalf("events = %+v, want one warning %q", events, prefix)
	}
	var meta map[string]any
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil || meta["fatal"] != false {
		t.Fatalf("warning meta = %s", events[0].Meta)
	}
}

// The first main request after a fresh start carries the hook's context; a
// request without it is reported once.
func TestTheFirstMainRequestSettlesTheStart(t *testing.T) {
	s, rec := sessionStartTestSession(t, "")
	s.observeMainRequest(requestBody(hookBlock("user hook output", memoryNow)))
	if events := rec.take(); len(events) > 0 {
		t.Fatalf("events after the hook delivered: %+v", events)
	}

	// A later block without the hook's context, here one a prompt quotes,
	// leaves the earlier block's delivery standing.
	s, rec = sessionStartTestSession(t, "")
	s.observeMainRequest(requestBody(hookBlock(memoryNow), hookBlock("user hook output")))
	if events := rec.take(); len(events) > 0 {
		t.Fatalf("events after an earlier block delivered: %+v", events)
	}

	s, rec = sessionStartTestSession(t, "")
	s.observeMainRequest(requestBody(hookBlock("user hook output")))
	onlyWarning(t, rec.take(), "the memory did not reach this session after its start: ")
	s.observeMainRequest(requestBody())
	if events := rec.take(); len(events) > 0 {
		t.Fatalf("a later request settled again: %+v", events)
	}

	// A resumed session runs a hook without Resume only after a compaction.
	s, rec = sessionStartTestSession(t, "sess-1")
	s.observeMainRequest(requestBody())
	if events := rec.take(); len(events) > 0 {
		t.Fatalf("a resumed session warned: %+v", events)
	}
}

// A resume runs the hook when its Resume is set, and only this run's
// context counts: the earlier runs' blocks a resumed transcript carries,
// under the same marker, are stamped before the session launched.
func TestTheFirstMainRequestSettlesAResume(t *testing.T) {
	s, rec := sessionStartTestSessionWith(t, "sess-1", testHook(true), nil)
	s.observeMainRequest(requestBody(hookBlock(memoryOld), hookBlock(memoryOld)))
	onlyWarning(t, rec.take(), "the memory did not reach this session after its resume: ")

	s, rec = sessionStartTestSessionWith(t, "sess-1", testHook(true), nil)
	s.observeMainRequest(requestBody(hookBlock(memoryOld), hookBlock(memoryNow), "the user's prompt"))
	if events := rec.take(); len(events) > 0 {
		t.Fatalf("events after the hook delivered: %+v", events)
	}
}

// The hook's exact context, read out of a block that joins it with the
// user's own hook output, is kept under the session's id with the request
// that settles it, the latest when two runs qualify.
func TestTheMainRequestKeepsTheReceipt(t *testing.T) {
	var kept []string
	s, rec := sessionStartTestSessionWith(t, "", testHook(false), &provider.SessionStartReceipts{
		Keep: func(sessionRef, context string) error {
			kept = append(kept, sessionRef+" "+context)
			return nil
		},
	})
	later := stampedAt(strings.Replace(memoryContext, "- a", "- b", 1), time.Now().Add(2*time.Minute))
	s.observeMainRequest(requestBody(hookBlock(memoryNow), hookBlock("user hook output", `a "quoted" user line`, later)))
	if events := rec.take(); len(events) > 0 {
		t.Fatalf("events: %+v", events)
	}
	if want := []string{"sess-1 " + later}; strings.Join(kept, "\n") != strings.Join(want, "\n") {
		t.Fatalf("kept %q, want %q", kept, want)
	}
}

// PostCompact arms the compaction; the next main request settles it, and
// the start's context a compaction kept is not its delivery.
func TestTheMainRequestAfterACompactionSettlesIt(t *testing.T) {
	s, rec := sessionStartTestSession(t, "sess-1")
	s.finalizeCompaction("auto", "the summary")
	if s.sessionStart.Expecting() != provider.SessionStartAfterCompaction {
		t.Fatal("PostCompact did not arm the hook")
	}
	s.observeMainRequest(requestBody("the summary", hookBlock(memoryNow)))
	if events := rec.take(); len(events) > 0 {
		t.Fatalf("events after the hook delivered: %+v", events)
	}

	// The session launched before the start's run; the compaction came after
	// it.
	s, rec = sessionStartTestSession(t, "")
	s.sessionStartSince = time.Now().Add(-2 * time.Hour)
	s.observeMainRequest(requestBody(hookBlock(memoryOld)))
	if events := rec.take(); len(events) > 0 {
		t.Fatalf("events after the start delivered: %+v", events)
	}
	s.finalizeCompaction("manual", "")
	s.observeMainRequest(requestBody("an older summary", hookBlock(memoryOld)))
	onlyWarning(t, rec.take(), "the memory did not reach this session after its compaction: ")
}

// Only a hook block's context stamped after the expectation opened counts:
// a summary that quotes the context, an earlier run's block, a context
// without a stamp or with one that does not read as a time, a heading after
// the block's JSON string ends, or a block whose string does not decode is
// not a delivery.
func TestOnlyAStampedHookBlockDelivers(t *testing.T) {
	const missing = "the memory did not reach this session after its compaction: "
	for name, tc := range map[string]struct {
		texts []string
		want  string
	}{
		"quoted in a summary":      {[]string{"Summary: the agent read " + memoryNow}, missing},
		"an earlier run's block":   {[]string{hookBlock(memoryOld)}, missing},
		"no stamp":                 {[]string{hookBlock(memoryContext)}, missing},
		"a stamp that is no time":  {[]string{hookBlock(strings.Replace(memoryNow, "T", "x", 1))}, missing},
		"after the block's string": {[]string{hookBlock("user hook output"), memoryNow}, missing},
		"past an escaped quote":    {[]string{hookBlock(`a "quoted" word`, memoryNow)}, ""},
		"cut short":                {[]string{hookBlock(strings.TrimSuffix(memoryNow, "(End of memory)"))}, ""},
	} {
		s, rec := sessionStartTestSession(t, "sess-1")
		s.finalizeCompaction("auto", "")
		s.observeMainRequest(requestBody(tc.texts...))
		events := rec.take()
		if tc.want == "" {
			if len(events) > 0 {
				t.Errorf("%s: events %+v", name, events)
			}
			continue
		}
		if len(events) != 1 || !strings.HasPrefix(events[0].Content, tc.want) {
			t.Errorf("%s: events %+v, want a warning %q", name, events, tc.want)
		}
	}

	// A block whose string does not decode is logged.
	s, rec := sessionStartTestSession(t, "sess-1")
	var logged []string
	s.logf = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	s.finalizeCompaction("auto", "")
	s.observeMainRequest([]byte(`{"messages":[{"role":"user","content":"` + sessionStartMarker + `## Memory\x"}]}`))
	onlyWarning(t, rec.take(), missing)
	if !slices.ContainsFunc(logged, func(line string) bool {
		return strings.Contains(line, "SessionStart context block in a model request is not a JSON string")
	}) {
		t.Fatalf("logged %q", logged)
	}
}

// NewSession follows the hook from a fresh start, and from a resume when
// its Resume is set, and refuses a hook the delivery cannot tell apart
// before it launches anything.
func TestNewSessionFollowsTheHook(t *testing.T) {
	binary := mockexec.Write(t, t.TempDir()+"/claude", "#!/bin/sh\nexec sleep 30\n")
	for name, tc := range map[string]struct {
		resume string
		hook   *provider.SessionStartHook
		want   provider.SessionStartEvent
	}{
		"fresh":            {"", testHook(false), provider.SessionStartAfterStart},
		"resume":           {"sess-1", testHook(false), ""},
		"resume of a hook": {"sess-1", testHook(true), provider.SessionStartAfterResume},
	} {
		rec := &eventRecorder{}
		s, err := NewSession(context.Background(), testThread, Config{
			Binary: binary, WorkDir: t.TempDir(), HookCmd: "/opt/ao", Resume: tc.resume, SessionStartHook: tc.hook,
		}, rec.record)
		if err != nil {
			t.Fatalf("%s: NewSession: %v", name, err)
		}
		if got := s.sessionStart.Expecting(); got != tc.want {
			t.Errorf("%s: expecting %q, want %q", name, got, tc.want)
		}
		if err := s.Close(); err != nil {
			t.Errorf("%s: Close: %v", name, err)
		}
	}

	_, err := NewSession(context.Background(), testThread, Config{
		Binary: binary, WorkDir: t.TempDir(), HookCmd: "/opt/ao",
		SessionStartHook: &provider.SessionStartHook{Label: "a", Command: "a"},
	}, func(provider.ProviderEvent) {})
	if err == nil || !strings.Contains(err.Error(), "heading, ending and stamp are all required") {
		t.Fatalf("NewSession with a hook without a heading: %v", err)
	}
}

// Only a main-loop request reaches the SessionStart check, with its body as
// the model receives it: a compaction summarizer's and a subagent's requests
// carry another context.
func TestGatewayObservesOnlyMainRequests(t *testing.T) {
	post := func(drive *recordingDriver, headers map[string]string) {
		t.Helper()
		gw := newTestGateway(t, sseUpstream(t, http.StatusOK, nil).URL, drive, nil)
		resp := postGatewayWithHeaders(t, gw, http.MethodPost, "/v1/messages", agentReqBody, headers)
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}

	main := newRecordingDriver()
	post(main, nil)
	if observed := main.observedMainRequests(); len(observed) != 1 || string(observed[0]) != agentReqBody {
		t.Fatalf("main request observed %q", observed)
	}

	summarizer := newRecordingDriver()
	summarizer.rec.armCompaction()
	post(summarizer, nil)
	if got := summarizer.captureBeginsCount(); got != 1 {
		t.Fatalf("capture begins = %d", got)
	}
	if observed := summarizer.observedMainRequests(); len(observed) != 0 {
		t.Fatalf("the summarizer's request was observed: %q", observed)
	}

	subagent := newRecordingDriver()
	post(subagent, map[string]string{agentIDHeader: "aid-abc123"})
	if got := subagent.subagentBegins(); got != 1 {
		t.Fatalf("subagent begins = %d", got)
	}
	if observed := subagent.observedMainRequests(); len(observed) != 0 {
		t.Fatalf("a subagent's request was observed: %q", observed)
	}
}
