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

var testSessionStartHooks = []provider.SessionStartHook{
	{Label: "instructions.md", Command: `'/opt/ao' __agent-home-context 'instructions.md' '/h'`, Heading: "## Your standing instructions", Ending: "[End of instructions.md]", Stamp: "[Delivered at "},
	{Label: "MEMORY.md", Command: `'/opt/ao' __agent-home-context 'MEMORY.md' '/h'`, Heading: "## Your memory index", Ending: "[End of MEMORY.md]", Stamp: "[Delivered at "},
}

// The hooks join the relay's SessionStart entry, each as a matcher group of
// its own, and nothing else in the block changes.
func TestHookSettingsRegisterTheSessionStartHooks(t *testing.T) {
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
	hooks := resumingHooks(1)
	with := decode(Config{HookCmd: "/opt/ao", SessionStartHooks: hooks})
	if len(without["SessionStart"]) != 1 {
		t.Fatalf("relay-only SessionStart = %v", without["SessionStart"])
	}
	got, _ := json.Marshal(with["SessionStart"])
	want, _ := json.Marshal(append(append([]any{}, without["SessionStart"]...), claude.SessionStartHookGroups(hooks)...))
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

// resumingHooks are the test hooks, those at indexes running at a resume.
func resumingHooks(indexes ...int) []provider.SessionStartHook {
	hooks := append([]provider.SessionStartHook{}, testSessionStartHooks...)
	for _, i := range indexes {
		hooks[i].Resume = true
	}
	return hooks
}

// sessionStartTestSession is newWiredSession with the hooks and without its
// feed loop, so the recorder holds only what the SessionStart check emits.
func sessionStartTestSession(t *testing.T, resume string) (*Session, *eventRecorder) {
	t.Helper()
	return sessionStartTestSessionWith(t, resume, testSessionStartHooks, nil)
}

func sessionStartTestSessionWith(t *testing.T, resume string, hooks []provider.SessionStartHook, receipts *provider.SessionStartReceipts) (*Session, *eventRecorder) {
	t.Helper()
	delivery, err := claude.NewSessionStartDelivery(hooks, receipts, resume)
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
		sessionStartHooks: hooks,
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

const (
	instructionsContext = "## Your standing instructions\n\nRole\n\n[End of instructions.md]"
	memoryContext       = "## Your memory index\n\n- a\n\n[End of MEMORY.md]"
)

// stampedAt is context as a run at at delivers it, stamped under its
// heading.
func stampedAt(context string, at time.Time) string {
	heading, rest, _ := strings.Cut(context, "\n")
	return heading + "\n[Delivered at " + at.UTC().Format("2006-01-02T15:04:05.000Z07:00") + "; it replaces any earlier copy.]\n" + rest
}

// The contexts of a run after the expectation opened, and of one before it.
var (
	instructionsNow = stampedAt(instructionsContext, time.Now().Add(time.Minute))
	memoryNow       = stampedAt(memoryContext, time.Now().Add(time.Minute))
	instructionsOld = stampedAt(instructionsContext, time.Now().Add(-time.Hour))
	memoryOld       = stampedAt(memoryContext, time.Now().Add(-time.Hour))
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

// The first main request after a fresh start carries the hooks' context;
// each hook whose heading it lacks is reported once.
func TestTheFirstMainRequestSettlesTheStart(t *testing.T) {
	s, rec := sessionStartTestSession(t, "")
	s.observeMainRequest(requestBody(hookBlock("user hook output", instructionsNow, memoryNow)))
	if events := rec.take(); len(events) > 0 {
		t.Fatalf("events after both delivered: %+v", events)
	}

	s, rec = sessionStartTestSession(t, "")
	s.observeMainRequest(requestBody(hookBlock(memoryNow)))
	onlyWarning(t, rec.take(), "instructions.md did not reach this session after its start: ")
	s.observeMainRequest(requestBody())
	if events := rec.take(); len(events) > 0 {
		t.Fatalf("a later request settled again: %+v", events)
	}

	// A resumed session runs no hook without Resume and expects none.
	s, rec = sessionStartTestSession(t, "sess-1")
	s.observeMainRequest(requestBody())
	if events := rec.take(); len(events) > 0 {
		t.Fatalf("a resumed session warned: %+v", events)
	}
}

// A resume runs the hooks whose Resume is set, and only this run's contexts
// count: the earlier runs' blocks a resumed transcript carries, under the
// same marker, are stamped before the session launched.
func TestTheFirstMainRequestSettlesAResume(t *testing.T) {
	s, rec := sessionStartTestSessionWith(t, "sess-1", resumingHooks(0, 1), nil)
	s.observeMainRequest(requestBody(
		hookBlock(instructionsOld, memoryOld),
		hookBlock(instructionsOld, memoryOld),
		hookBlock(memoryNow)))
	onlyWarning(t, rec.take(), "instructions.md did not reach this session after its resume: ")

	// This run's hook failed: an earlier run's block is not its delivery.
	s, rec = sessionStartTestSessionWith(t, "sess-1", resumingHooks(1), nil)
	s.observeMainRequest(requestBody(hookBlock(instructionsOld, memoryOld)))
	onlyWarning(t, rec.take(), "MEMORY.md did not reach this session after its resume: ")

	s, rec = sessionStartTestSessionWith(t, "sess-1", resumingHooks(1), nil)
	s.observeMainRequest(requestBody(hookBlock(memoryNow), "the user's prompt"))
	if events := rec.take(); len(events) > 0 {
		t.Fatalf("events after the resuming hook delivered: %+v", events)
	}
}

// Each hook's exact context, read out of a block that joins it with the
// user's own hook output, is kept under the session's id with the request
// that settles it, the latest of each hook's when two runs qualify.
func TestTheMainRequestKeepsTheReceipts(t *testing.T) {
	var kept []string
	s, rec := sessionStartTestSessionWith(t, "", testSessionStartHooks, &provider.SessionStartReceipts{
		Keep: func(sessionRef, label, context string) error {
			kept = append(kept, sessionRef+" "+label+" "+context)
			return nil
		},
	})
	later := stampedAt(strings.Replace(memoryContext, "- a", "- b", 1), time.Now().Add(2*time.Minute))
	s.observeMainRequest(requestBody(hookBlock(memoryNow), hookBlock("user hook output", instructionsNow, `a "quoted" user line`, later)))
	if events := rec.take(); len(events) > 0 {
		t.Fatalf("events: %+v", events)
	}
	if want := []string{"sess-1 instructions.md " + instructionsNow, "sess-1 MEMORY.md " + later}; strings.Join(kept, "\n") != strings.Join(want, "\n") {
		t.Fatalf("kept %q, want %q", kept, want)
	}
}

// PostCompact arms the compaction; the next main request settles it, and
// the start's contexts a compaction kept are not its delivery.
func TestTheMainRequestAfterACompactionSettlesIt(t *testing.T) {
	s, rec := sessionStartTestSession(t, "sess-1")
	s.finalizeCompaction("auto", "the summary")
	if s.sessionStart.Expecting() != provider.SessionStartAfterCompaction {
		t.Fatal("PostCompact did not arm the hooks")
	}
	s.observeMainRequest(requestBody("the summary", hookBlock(instructionsNow, memoryNow)))
	if events := rec.take(); len(events) > 0 {
		t.Fatalf("events after both delivered: %+v", events)
	}

	// The session launched before the start's run; the compaction came after
	// it.
	s, rec = sessionStartTestSession(t, "")
	s.sessionStartSince = time.Now().Add(-2 * time.Hour)
	s.observeMainRequest(requestBody(hookBlock(instructionsOld, memoryOld)))
	if events := rec.take(); len(events) > 0 {
		t.Fatalf("events after the start delivered: %+v", events)
	}
	s.finalizeCompaction("manual", "")
	s.observeMainRequest(requestBody("an older summary", hookBlock(instructionsOld, memoryOld)))
	onlyWarning(t, rec.take(), "instructions.md and MEMORY.md did not reach this session after its compaction: ")
}

// Only a hook block's context stamped after the expectation opened counts:
// a summary that quotes a context, an earlier run's block, a context without
// a stamp or with one that does not read as a time, a heading after the
// block's JSON string ends, or a block whose string does not decode is not a
// delivery.
func TestOnlyAStampedHookBlockDelivers(t *testing.T) {
	for name, tc := range map[string]struct {
		texts []string
		want  string
	}{
		"quoted in a summary":      {[]string{"Summary: the agent read " + instructionsNow + " and " + memoryNow}, "instructions.md and MEMORY.md "},
		"an earlier run's block":   {[]string{hookBlock(instructionsOld, memoryOld)}, "instructions.md and MEMORY.md "},
		"no stamp":                 {[]string{hookBlock(instructionsContext, memoryNow)}, "instructions.md did not"},
		"a stamp that is no time":  {[]string{hookBlock(strings.Replace(instructionsNow, "T", "x", 1), memoryNow)}, "instructions.md did not"},
		"after the block's string": {[]string{hookBlock(instructionsNow), memoryNow}, "MEMORY.md did not"},
		"past an escaped quote":    {[]string{hookBlock(`a "quoted" word`, instructionsNow, memoryNow)}, ""},
		"cut short":                {[]string{hookBlock(instructionsNow, strings.TrimSuffix(memoryNow, "[End of MEMORY.md]"))}, ""},
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
	s.observeMainRequest([]byte(`{"messages":[{"role":"user","content":"` + sessionStartMarker + `## Your memory index\x"}]}`))
	onlyWarning(t, rec.take(), "instructions.md and MEMORY.md did not reach")
	if !slices.ContainsFunc(logged, func(line string) bool {
		return strings.Contains(line, "SessionStart context block in a model request is not a JSON string")
	}) {
		t.Fatalf("logged %q", logged)
	}
}

// NewSession follows every hook from a fresh start and those whose Resume is
// set from a resume, and refuses hooks the delivery cannot tell apart before
// it launches anything.
func TestNewSessionFollowsTheHooks(t *testing.T) {
	binary := mockexec.Write(t, t.TempDir()+"/claude", "#!/bin/sh\nexec sleep 30\n")
	for name, tc := range map[string]struct {
		resume string
		hooks  []provider.SessionStartHook
		want   provider.SessionStartEvent
	}{
		"fresh":            {"", testSessionStartHooks, provider.SessionStartAfterStart},
		"resume":           {"sess-1", testSessionStartHooks, ""},
		"resume of a hook": {"sess-1", resumingHooks(1), provider.SessionStartAfterResume},
	} {
		rec := &eventRecorder{}
		s, err := NewSession(context.Background(), testThread, Config{
			Binary: binary, WorkDir: t.TempDir(), HookCmd: "/opt/ao", Resume: tc.resume, SessionStartHooks: tc.hooks,
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
		SessionStartHooks: []provider.SessionStartHook{{Label: "a", Command: "a"}},
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
