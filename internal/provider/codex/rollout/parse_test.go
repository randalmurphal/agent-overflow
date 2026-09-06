package rollout

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/importir"
	"agent-overflow/internal/provider"
)

const testSessionID = "019fdd9f-c8fd-7093-8266-8af5e196ddea"

// writeRollout writes a fixture rollout with the real file-name shape, so
// SessionIDFromPath resolves the session id the same way it does on disk.
// Every fixture line here is hand-written: nothing is copied out of a real
// provider home.
func writeRollout(t *testing.T, sessionID string, lines ...string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout-2026-08-07T15-07-44-"+sessionID+".jsonl")
	body := ""
	for _, line := range lines {
		body += line + "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write rollout: %v", err)
	}
	return path
}

func parseFixture(t *testing.T, path string) ParseResult {
	t.Helper()
	res, err := Parse(context.Background(), ParseOptions{Path: path})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return res
}

func kinds(events []importir.Event) []provider.EventKind {
	out := make([]provider.EventKind, 0, len(events))
	for _, e := range events {
		out = append(out, e.Kind)
	}
	return out
}

func firstOfKind(t *testing.T, events []importir.Event, kind provider.EventKind) importir.Event {
	t.Helper()
	for _, e := range events {
		if e.Kind == kind {
			return e
		}
	}
	t.Fatalf("no %s event in %v", kind, kinds(events))
	return importir.Event{}
}

func countKind(events []importir.Event, kind provider.EventKind) int {
	n := 0
	for _, e := range events {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

const (
	metaLine = `{"timestamp":"2026-08-07T19:07:44.339Z","type":"session_meta","payload":{"id":"` + testSessionID +
		`","cwd":"/repo","originator":"codex_cli","cli_version":"0.146.0","git":{"branch":"main"}}}`
	turnContextLine = `{"timestamp":"2026-08-07T19:07:46.548Z","type":"turn_context","payload":{"turn_id":"turn-1","cwd":"/repo","model":"gpt-5.6-sol","effort":"high","approval_policy":"never","sandbox_policy":{"type":"read-only"},"summary":"auto"}}`
	taskStartedLine = `{"timestamp":"2026-08-07T19:07:46.600Z","type":"event_msg","payload":{"type":"task_started","turn_id":"turn-1","started_at":1786133866,"model_context_window":258400}}`
	userMsgLine     = `{"timestamp":"2026-08-07T19:07:47.000Z","type":"event_msg","payload":{"type":"user_message","message":"do the thing","images":[]}}`
	agentMsgLine    = `{"timestamp":"2026-08-07T19:07:59.000Z","type":"event_msg","payload":{"type":"agent_message","message":"done","phase":"final_answer"}}`
	taskCompleteLn  = `{"timestamp":"2026-08-07T19:08:00.000Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"turn-1","last_agent_message":"done","started_at":1786133866,"completed_at":1786133880}}`
)

func TestParseEnvelopeHappyPath(t *testing.T) {
	path := writeRollout(t, testSessionID,
		metaLine, turnContextLine, taskStartedLine, userMsgLine, agentMsgLine, taskCompleteLn)
	res := parseFixture(t, path)

	if res.Meta.SessionID != testSessionID {
		t.Fatalf("session meta not accepted: %+v", res.Meta)
	}
	if res.Meta.Cwd != "/repo" || res.Meta.GitBranch != "main" || res.Meta.CLIVersion != "0.146.0" {
		t.Fatalf("session meta fields missing: %+v", res.Meta)
	}
	want := []provider.EventKind{
		provider.EventTurnStart,
		provider.EventUserText,
		provider.EventContentBlockStop,
		provider.EventTurnComplete,
	}
	if got := kinds(res.Events); len(got) != len(want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	for i, k := range want {
		if res.Events[i].Kind != k {
			t.Fatalf("event %d = %s, want %s", i, res.Events[i].Kind, k)
		}
	}
	start := res.Events[0]
	if start.TurnID != "turn-1" || start.TurnIndex != 1 {
		t.Fatalf("turn identity = %q/%d, want turn-1/1", start.TurnID, start.TurnIndex)
	}
	var startMeta struct {
		Model         string `json:"model"`
		Effort        string `json:"effort"`
		ContextWindow int    `json:"contextWindow"`
	}
	if err := json.Unmarshal(start.Meta, &startMeta); err != nil {
		t.Fatalf("turn start meta: %v", err)
	}
	if startMeta.Model != "gpt-5.6-sol" || startMeta.Effort != "high" || startMeta.ContextWindow != 258400 {
		t.Fatalf("turn_context did not seed the turn: %+v", startMeta)
	}
	if res.Profile.Model != "gpt-5.6-sol" || res.Profile.ReasoningEffort != "high" || res.Profile.ContextWindow != 258400 {
		t.Fatalf("profile = %+v, want the latest turn settings", res.Profile)
	}
	if res.Events[1].Content != "do the thing" {
		t.Fatalf("user text = %q", res.Events[1].Content)
	}
	if res.Events[2].Content != "done" || !strings.Contains(string(res.Events[2].Meta), "text") {
		t.Fatalf("assistant block = %q meta %s", res.Events[2].Content, res.Events[2].Meta)
	}
	// Original provider timestamps, never now().
	if res.Events[1].Timestamp.UTC().Format("2006-01-02T15:04:05") != "2026-08-07T19:07:47" {
		t.Fatalf("user event lost its original timestamp: %v", res.Events[1].Timestamp)
	}
	if _, ok := res.Events[3].TurnComplete.(*provider.WireTurnCompleteMeta); !ok {
		t.Fatalf("turn complete meta = %T, want wire", res.Events[3].TurnComplete)
	}
}

// Current Codex writes task_started before turn_context. The profile and the
// already-emitted turn event must both learn the late context, even when the
// turn aborts without a token_count that could accidentally recover it.
func TestParseLateTurnContextProfilesAnAbortedTurn(t *testing.T) {
	aborted := `{"timestamp":"2026-08-07T19:08:00.000Z","type":"event_msg","payload":{"type":"turn_aborted","turn_id":"turn-1","reason":"interrupted","completed_at":1786133880}}`
	path := writeRollout(t, testSessionID,
		metaLine, taskStartedLine, turnContextLine, userMsgLine, aborted)
	res := parseFixture(t, path)

	if res.Profile.Model != "gpt-5.6-sol" || res.Profile.ReasoningEffort != "high" || res.Profile.ContextWindow != 258400 {
		t.Fatalf("late profile = %+v", res.Profile)
	}
	start := firstOfKind(t, res.Events, provider.EventTurnStart)
	var meta struct {
		Model         string `json:"model"`
		Effort        string `json:"effort"`
		ContextWindow int    `json:"contextWindow"`
	}
	if err := json.Unmarshal(start.Meta, &meta); err != nil {
		t.Fatalf("decode turn start meta: %v", err)
	}
	if meta.Model != res.Profile.Model || meta.Effort != res.Profile.ReasoningEffort || meta.ContextWindow != res.Profile.ContextWindow {
		t.Fatalf("turn start meta = %+v, profile = %+v", meta, res.Profile)
	}

	recovered, err := ReadLatestProfile(context.Background(), path)
	if err != nil {
		t.Fatalf("ReadLatestProfile: %v", err)
	}
	if recovered != res.Profile {
		t.Fatalf("recovered profile = %+v, parse profile = %+v", recovered, res.Profile)
	}
}

func TestParseProfileKeepsContextWindowWithoutUsageAccounting(t *testing.T) {
	tokenWindowOnly := `{"timestamp":"2026-08-07T19:07:58.000Z","type":"event_msg","payload":{"type":"token_count","info":{"model_context_window":300000}}}`
	aborted := `{"timestamp":"2026-08-07T19:08:00.000Z","type":"event_msg","payload":{"type":"turn_aborted","turn_id":"turn-1","reason":"interrupted","completed_at":1786133880}}`
	path := writeRollout(t, testSessionID,
		metaLine,
		`{"timestamp":"2026-08-07T19:07:45.000Z","type":"event_msg","payload":{"type":"task_started","turn_id":"turn-1"}}`,
		turnContextLine,
		tokenWindowOnly,
		aborted,
	)
	res := parseFixture(t, path)
	if res.Profile.ContextWindow != 300000 {
		t.Fatalf("profile context window = %d, want 300000", res.Profile.ContextWindow)
	}
	recovered, err := ReadLatestProfile(context.Background(), path)
	if err != nil {
		t.Fatalf("ReadLatestProfile: %v", err)
	}
	if recovered != res.Profile {
		t.Fatalf("recovered profile = %+v, parse profile = %+v", recovered, res.Profile)
	}
}

func TestParseSkipsUnknownTypesAndCountsThem(t *testing.T) {
	path := writeRollout(t, testSessionID,
		metaLine,
		taskStartedLine,
		`{"timestamp":"2026-08-07T19:07:46.548Z","type":"world_state","payload":{"full":true,"state":{}}}`,
		`{"timestamp":"2026-08-07T19:07:46.549Z","type":"future_record_codex_has_not_shipped_yet","payload":{}}`,
		`{"timestamp":"2026-08-07T19:07:46.550Z","type":"event_msg","payload":{"type":"brand_new_event"}}`,
		userMsgLine,
		taskCompleteLn,
	)
	res := parseFixture(t, path)

	// `world_state` is RECOGNISED and dropped, not unknown: Codex writes one
	// per turn on every modern thread, so counting it would put an unknown-
	// types warning on essentially every import. See converter.convert.
	if _, counted := res.UnknownTypes["world_state"]; counted {
		t.Fatalf("world_state should be recognised and dropped: %+v", res.UnknownTypes)
	}
	if res.UnknownTypes["future_record_codex_has_not_shipped_yet"] != 1 {
		t.Fatalf("unknown envelope not counted: %+v", res.UnknownTypes)
	}
	if res.UnknownTypes["event_msg/brand_new_event"] != 1 {
		t.Fatalf("unknown event_msg not counted: %+v", res.UnknownTypes)
	}
	if res.CorruptLines != 0 {
		t.Fatalf("unknown types must not count as corruption: %d", res.CorruptLines)
	}
	if countKind(res.Events, provider.EventUserText) != 1 {
		t.Fatal("parsing did not continue past the unknown records")
	}
}

func TestParseSkipsCorruptAndNULAndOversizedLines(t *testing.T) {
	path := writeRollout(t, testSessionID,
		metaLine,
		taskStartedLine,
		`{"timestamp":"2026-08-07T19:07:46.5`, // truncated JSON
		"{\"timestamp\":\"2026-08-07T19:07:46.548Z\",\"type\":\"event_msg\",\"payload\":{\"type\":\"user_message\",\"message\":\"nu\x00l\"}}",
		`{"timestamp":"2026-08-07T19:07:46.549Z","type":"event_msg","payload":{"type":"user_message","message":"`+strings.Repeat("x", 4096)+`"}}`,
		userMsgLine,
		taskCompleteLn,
	)
	res, err := Parse(context.Background(), ParseOptions{Path: path, MaxLineBytes: 1024})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if res.CorruptLines != 3 {
		t.Fatalf("CorruptLines = %d, want 3 (truncated + NUL + oversized)", res.CorruptLines)
	}
	if countKind(res.Events, provider.EventUserText) != 1 {
		t.Fatalf("only the good user message should survive: %v", kinds(res.Events))
	}
	var found bool
	for _, w := range res.Warnings {
		if w.Code == WarnCorruptLines {
			found = true
		}
	}
	if !found {
		t.Fatalf("corrupt lines should raise a warning: %+v", res.Warnings)
	}
}

// A forked rollout embeds the SOURCE session's meta as a second line. Only
// the line whose payload.id equals the file's own session id is accepted, and
// the ordering of the two lines must not matter.
func TestParseAcceptsOnlyTheMatchingSessionMeta(t *testing.T) {
	otherLine := `{"timestamp":"2026-08-07T19:07:44.000Z","type":"session_meta","payload":{"id":"11111111-2222-3333-4444-555555555555","cwd":"/somewhere/else","forked_from_id":"ignored"}}`
	ownLine := `{"timestamp":"2026-08-07T19:07:44.339Z","type":"session_meta","payload":{"id":"` + testSessionID +
		`","cwd":"/repo","forked_from_id":"019fdd9f-c7ef-7663-9d60-14ef9e8ce96b"}}`

	for name, lines := range map[string][]string{
		"own first":   {ownLine, otherLine, taskStartedLine, userMsgLine},
		"other first": {otherLine, ownLine, taskStartedLine, userMsgLine},
	} {
		t.Run(name, func(t *testing.T) {
			res := parseFixture(t, writeRollout(t, testSessionID, lines...))
			if res.Meta.SessionID != testSessionID {
				t.Fatalf("accepted the wrong meta: %+v", res.Meta)
			}
			if res.Meta.Cwd != "/repo" {
				t.Fatalf("cwd came from the wrong meta: %q", res.Meta.Cwd)
			}
			if res.Meta.ForkedFromID != "019fdd9f-c7ef-7663-9d60-14ef9e8ce96b" {
				t.Fatalf("forked_from_id came from the wrong meta: %q", res.Meta.ForkedFromID)
			}
		})
	}
}

func TestParseTailResumeRoundTripsExactly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout-2026-08-07T15-07-44-"+testSessionID+".jsonl")
	head := metaLine + "\n" + taskStartedLine + "\n" + userMsgLine + "\n" + taskCompleteLn + "\n"
	if err := os.WriteFile(path, []byte(head), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	first := parseFixture(t, path)
	if first.EndOffset != int64(len(head)) {
		t.Fatalf("EndOffset = %d, want %d", first.EndOffset, len(head))
	}

	// Append a second turn plus a HALF-WRITTEN line, exactly as a live
	// Codex leaves the file mid-append.
	second := `{"timestamp":"2026-08-07T19:10:00.000Z","type":"event_msg","payload":{"type":"task_started","turn_id":"turn-2","started_at":1786134000}}` + "\n" +
		`{"timestamp":"2026-08-07T19:10:01.000Z","type":"event_msg","payload":{"type":"user_message","message":"second"}}` + "\n"
	partial := `{"timestamp":"2026-08-07T19:10:02.000Z","type":"event_ms`
	appendFile(t, path, second+partial)

	res, err := Parse(context.Background(), ParseOptions{Path: path, FromOffset: first.EndOffset})
	if err != nil {
		t.Fatalf("tail Parse: %v", err)
	}
	if res.EndOffset != int64(len(head)+len(second)) {
		t.Fatalf("EndOffset = %d, want %d (the partial line must not be consumed)",
			res.EndOffset, len(head)+len(second))
	}
	if res.CorruptLines != 0 {
		t.Fatalf("a half-written trailing line is not corruption: %d", res.CorruptLines)
	}
	userEvents := 0
	for _, e := range res.Events {
		if e.Kind == provider.EventUserText {
			userEvents++
			if e.Content != "second" {
				t.Fatalf("tail replayed old content: %q", e.Content)
			}
		}
	}
	if userEvents != 1 {
		t.Fatalf("want exactly the appended user message, got %d", userEvents)
	}
	// The session meta still resolves on a tail read.
	if res.Meta.SessionID != testSessionID {
		t.Fatalf("tail read lost the session meta: %+v", res.Meta)
	}

	// Completing the partial line makes it readable from the same cursor.
	appendFile(t, path, `g","payload":{"type":"user_message","message":"third"}}`+"\n")
	third, err := Parse(context.Background(), ParseOptions{Path: path, FromOffset: res.EndOffset})
	if err != nil {
		t.Fatalf("third Parse: %v", err)
	}
	if countKind(third.Events, provider.EventUserText) != 1 {
		t.Fatalf("the completed line should now parse: %v", kinds(third.Events))
	}
}

func appendFile(t *testing.T, path, data string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open append: %v", err)
	}
	defer f.Close()
	if _, err := f.WriteString(data); err != nil {
		t.Fatalf("append: %v", err)
	}
}

func TestParseRejectsResumeOffsetPastEOF(t *testing.T) {
	path := writeRollout(t, testSessionID, metaLine)
	_, err := Parse(context.Background(), ParseOptions{Path: path, FromOffset: 1 << 20})
	if !errors.Is(err, ErrSourceShrank) {
		t.Fatalf("want ErrSourceShrank, got %v", err)
	}
}

// TestParseRejectsAResumeOffsetThatIsNoLongerARecordBoundary is the
// truncate-and-regrow guard. The size check alone only catches a file that
// SHRANK; a rollout truncated mid-line and then appended to past the old
// cursor is the same size or larger, and resuming inside a record splices a
// foreign session's tail onto the thread as if it continued this one.
func TestParseRejectsAResumeOffsetThatIsNoLongerARecordBoundary(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout-2026-08-07T15-07-44-"+testSessionID+".jsonl")
	head := metaLine + "\n" + taskStartedLine + "\n" + userMsgLine + "\n"
	if err := os.WriteFile(path, []byte(head), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	cursor := parseFixture(t, path).EndOffset

	// Truncate to the MIDDLE of the last record, then append fresh lines so
	// the file grows well past the cursor again.
	if err := os.Truncate(path, cursor-20); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	appendFile(t, path, taskCompleteLn+"\n"+taskStartedLine+"\n"+userMsgLine+"\n")

	stat, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if stat.Size() <= cursor {
		t.Fatalf("fixture did not regrow past the cursor (size %d, cursor %d)", stat.Size(), cursor)
	}
	if _, err := Parse(context.Background(), ParseOptions{Path: path, FromOffset: cursor}); !errors.Is(err, ErrSourceShrank) {
		t.Fatalf("want ErrSourceShrank on a regrown file, got %v", err)
	}
}

func TestParseSourceCoordinates(t *testing.T) {
	path := writeRollout(t, testSessionID, metaLine, taskStartedLine, userMsgLine, taskCompleteLn)
	res := parseFixture(t, path)

	user := firstOfKind(t, res.Events, provider.EventUserText)
	wantStart := int64(len(metaLine) + 1 + len(taskStartedLine) + 1)
	if user.SourceUUID != lineUUID(wantStart) {
		t.Fatalf("SourceUUID = %q, want %q", user.SourceUUID, lineUUID(wantStart))
	}
	wantNext := wantStart + int64(len(userMsgLine)+1)
	if user.SourceOffset != wantNext {
		t.Fatalf("SourceOffset = %d, want %d (the resume point past this line)", user.SourceOffset, wantNext)
	}
	// The last event's SourceOffset is itself a valid resume cursor.
	last := res.Events[len(res.Events)-1]
	if last.SourceOffset != res.EndOffset {
		t.Fatalf("last SourceOffset = %d, EndOffset = %d", last.SourceOffset, res.EndOffset)
	}
}

func TestReadSessionMetaIsBoundedAndIDMatched(t *testing.T) {
	other := `{"timestamp":"2026-08-07T19:07:44.000Z","type":"session_meta","payload":{"id":"11111111-2222-3333-4444-555555555555","cwd":"/elsewhere"}}`
	path := writeRollout(t, testSessionID, other, metaLine, userMsgLine)

	meta, err := ReadSessionMeta(path, "")
	if err != nil {
		t.Fatalf("ReadSessionMeta: %v", err)
	}
	if meta.SessionID != testSessionID || meta.Cwd != "/repo" {
		t.Fatalf("wrong meta accepted: %+v", meta)
	}

	missing := writeRollout(t, testSessionID, other, userMsgLine)
	if _, err := ReadSessionMeta(missing, testSessionID); !errors.Is(err, ErrSessionMetaNotFound) {
		t.Fatalf("want ErrSessionMetaNotFound, got %v", err)
	}
}

func TestSessionIDFromPath(t *testing.T) {
	cases := map[string]string{
		"/x/rollout-2026-08-07T15-07-44-" + testSessionID + ".jsonl":                                          testSessionID,
		"/x/rollout-2026-09-05T12-30-49-" + testSessionID + "_01a0729f-7b76-7001-af20-70bd4b717aff.jsonl":     testSessionID,
		"/x/rollout-2026-09-05T12-30-49-" + testSessionID + "_01a0729f-7b76-7001-af20-70bd4b717aff.jsonl.zst": testSessionID,
		"/x/rollout-2026-09-05T12-30-49-bad_01a0729f-7b76-7001-af20-70bd4b717aff.jsonl":                       "",
		"/x/rollout-2026-08-07T15-07-44-not-a-uuid.jsonl":                                                     "",
		"/x/something-else.jsonl": "",
	}
	for path, want := range cases {
		if got := SessionIDFromPath(path); got != want {
			t.Fatalf("SessionIDFromPath(%q) = %q, want %q", path, got, want)
		}
	}
}
