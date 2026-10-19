package claudetui

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/testutil/mockexec"
)

var testSessionStartHooks = []provider.SessionStartHook{
	{Label: "instructions.md", Command: `'/opt/ao' __agent-home-context 'instructions.md' '/h'`, Heading: "## Your standing instructions"},
	{Label: "MEMORY.md", Command: `'/opt/ao' __agent-home-context 'MEMORY.md' '/h'`, Heading: "## Your memory index"},
}

// The hooks join the relay's SessionStart entry as a matcher group of their
// own, and nothing else in the block changes.
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
	with := decode(Config{HookCmd: "/opt/ao", SessionStartHooks: testSessionStartHooks})
	if len(without["SessionStart"]) != 1 {
		t.Fatalf("relay-only SessionStart = %v", without["SessionStart"])
	}
	got, _ := json.Marshal(with["SessionStart"])
	want, _ := json.Marshal(append(append([]any{}, without["SessionStart"]...), claude.SessionStartHookGroup(testSessionStartHooks)))
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

// sessionStartTestSession is newWiredSession with the hooks and without its
// feed loop, so the recorder holds only what the SessionStart check emits.
func sessionStartTestSession(t *testing.T, resume string) (*Session, *eventRecorder) {
	t.Helper()
	delivery, err := claude.NewSessionStartDelivery(testSessionStartHooks, resume)
	if err != nil {
		t.Fatalf("NewSessionStartDelivery: %v", err)
	}
	rec := &eventRecorder{}
	s := &Session{
		threadID:     testThread,
		parser:       newSessionParser(),
		feed:         make(chan json.RawMessage, 16),
		done:         make(chan struct{}),
		logf:         func(string, ...any) {},
		onEvent:      rec.record,
		sessionStart: delivery,
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
// contexts in.
func hookBlock(source string, contexts ...string) string {
	return "<system-reminder>\nSessionStart:" + source + " hook additional context: " + strings.Join(contexts, "\n") + "\n</system-reminder>"
}

const (
	instructionsContext = "## Your standing instructions\n\nRole"
	memoryContext       = "## Your memory index\n\n- a"
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
	s.observeMainRequest(requestBody(hookBlock("startup", "user hook output", instructionsContext, memoryContext)))
	if events := rec.take(); len(events) > 0 {
		t.Fatalf("events after both delivered: %+v", events)
	}

	s, rec = sessionStartTestSession(t, "")
	s.observeMainRequest(requestBody(hookBlock("startup", memoryContext)))
	onlyWarning(t, rec.take(), "instructions.md did not reach this session after its start: ")
	s.observeMainRequest(requestBody())
	if events := rec.take(); len(events) > 0 {
		t.Fatalf("a later request settled again: %+v", events)
	}

	// A resumed session runs no hooks and expects none.
	s, rec = sessionStartTestSession(t, "sess-1")
	s.observeMainRequest(requestBody())
	if events := rec.take(); len(events) > 0 {
		t.Fatalf("a resumed session warned: %+v", events)
	}
}

// PostCompact arms the compaction; the next main request settles it.
func TestTheMainRequestAfterACompactionSettlesIt(t *testing.T) {
	s, rec := sessionStartTestSession(t, "sess-1")
	s.finalizeCompaction("auto", "the summary")
	if s.sessionStart.Expecting() != provider.SessionStartAfterCompaction {
		t.Fatal("PostCompact did not arm the hooks")
	}
	s.observeMainRequest(requestBody("the summary", hookBlock("compact", instructionsContext, memoryContext)))
	if events := rec.take(); len(events) > 0 {
		t.Fatalf("events after both delivered: %+v", events)
	}

	s.finalizeCompaction("manual", "")
	s.observeMainRequest(requestBody("an older summary"))
	onlyWarning(t, rec.take(), "instructions.md and MEMORY.md did not reach this session after its compaction: ")
}

// Only the hook blocks of the expected source count: a summary that quotes
// a heading, the start's block a compaction kept, or a heading after the
// block's JSON string ends is not a delivery.
func TestOnlyTheExpectedHookBlockDelivers(t *testing.T) {
	for name, tc := range map[string]struct {
		texts []string
		want  string
	}{
		"quoted in a summary":      {[]string{"Summary: the agent read " + instructionsContext + " and " + memoryContext}, "instructions.md and MEMORY.md "},
		"the start's kept block":   {[]string{hookBlock("startup", instructionsContext, memoryContext)}, "instructions.md and MEMORY.md "},
		"after the block's string": {[]string{hookBlock("compact", instructionsContext), memoryContext}, "MEMORY.md did not"},
		"past an escaped quote":    {[]string{hookBlock("compact", `a "quoted" word`, instructionsContext, memoryContext)}, ""},
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
}

// NewSession follows the hooks from a fresh start, not from a resume, and
// refuses hooks the delivery cannot tell apart before it launches anything.
func TestNewSessionFollowsTheHooks(t *testing.T) {
	binary := mockexec.Write(t, t.TempDir()+"/claude", "#!/bin/sh\nexec sleep 30\n")
	for name, tc := range map[string]struct {
		resume string
		want   provider.SessionStartEvent
	}{"fresh": {"", provider.SessionStartAfterStart}, "resume": {"sess-1", ""}} {
		rec := &eventRecorder{}
		s, err := NewSession(context.Background(), testThread, Config{
			Binary: binary, WorkDir: t.TempDir(), HookCmd: "/opt/ao", Resume: tc.resume, SessionStartHooks: testSessionStartHooks,
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
	if err == nil || !strings.Contains(err.Error(), "heading are all required") {
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
