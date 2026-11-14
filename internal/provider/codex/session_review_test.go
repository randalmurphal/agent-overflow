package codex

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// The expected JSON below is hand-written from the Rust types at
// codex-rs/app-server-protocol/src/protocol/v2/review.rs
// (rust-v0.146.0-alpha.4). ReviewTarget carries
// `#[serde(tag = "type", rename_all = "camelCase")]` on the enum — so the
// tag values are camelCased variant names — plus
// `#[serde(rename_all = "camelCase")]` on each struct variant for the
// payload keys. `Commit.title` is `Option<String>` with NO
// skip_serializing_if, so the key is always present and null when absent.
func TestReviewTargetMarshalsExactlyTheWireShape(t *testing.T) {
	t.Parallel()
	baseBranch, err := ReviewBaseBranch("main")
	if err != nil {
		t.Fatalf("ReviewBaseBranch: %v", err)
	}
	commit, err := ReviewCommit("abc123", "fix: thing")
	if err != nil {
		t.Fatalf("ReviewCommit: %v", err)
	}
	untitled, err := ReviewCommit("abc123", "")
	if err != nil {
		t.Fatalf("ReviewCommit untitled: %v", err)
	}
	custom, err := ReviewCustom("look for races")
	if err != nil {
		t.Fatalf("ReviewCustom: %v", err)
	}

	cases := []struct {
		name   string
		target ReviewTarget
		want   string
	}{
		{"uncommitted", ReviewUncommittedChanges(), `{"type":"uncommittedChanges"}`},
		{"baseBranch", baseBranch, `{"branch":"main","type":"baseBranch"}`},
		{"commit", commit, `{"sha":"abc123","title":"fix: thing","type":"commit"}`},
		{"commit untitled", untitled, `{"sha":"abc123","title":null,"type":"commit"}`},
		{"custom", custom, `{"instructions":"look for races","type":"custom"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			encoded, err := json.Marshal(tc.target)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(encoded) != tc.want {
				t.Fatalf("marshal = %s, want %s", encoded, tc.want)
			}
			var back ReviewTarget
			if err := json.Unmarshal(encoded, &back); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if back != tc.target {
				t.Fatalf("round trip = %+v, want %+v", back, tc.target)
			}
		})
	}
}

func TestReviewTargetRejectsIllegalStates(t *testing.T) {
	t.Parallel()
	// The zero value is not a variant. Serialising it as anything at all
	// would silently review something the caller never asked for.
	if _, err := json.Marshal(ReviewTarget{}); err == nil {
		t.Fatal("zero-value ReviewTarget must not marshal")
	}
	if _, err := ReviewBaseBranch("   "); err == nil {
		t.Fatal("blank base branch must be rejected")
	}
	if _, err := ReviewCommit("", "title"); err == nil {
		t.Fatal("blank sha must be rejected")
	}
	if _, err := ReviewCustom(" \n "); err == nil {
		t.Fatal("blank instructions must be rejected")
	}
}

func TestReviewTargetUnmarshalValidatesPayloads(t *testing.T) {
	t.Parallel()
	// Decoding is the other door into the type; it has to enforce the same
	// invariants the constructors do or the unexported fields buy nothing.
	for _, body := range []string{
		`{"type":"commit"}`,
		`{"type":"baseBranch"}`,
		`{"type":"custom","instructions":""}`,
		`{"type":"somethingNew"}`,
		`{"type":"uncommittedChanges"`,
	} {
		var target ReviewTarget
		if err := json.Unmarshal([]byte(body), &target); err == nil {
			t.Fatalf("unmarshal %s must fail, got %+v", body, target)
		}
	}
}

// startReviewForTurnAsync runs StartReviewForTurn on a goroutine and answers
// its config/read with an empty config, leaving review/start for the caller.
func startReviewForTurnAsync(t *testing.T, s *Session, target ReviewTarget) (*pendingAnswerer, <-chan reviewStartResult) {
	t.Helper()
	done := make(chan reviewStartResult, 1)
	go func() {
		started, err := s.StartReviewForTurn(context.Background(), target, ReviewRunOptions{TurnIndex: 3})
		done <- reviewStartResult{started: started, err: err}
	}()
	answerer := newPendingAnswerer(s)
	answerer.answer(t, `{"config":{}}`)
	return answerer, done
}

type reviewStartResult struct {
	started ReviewStarted
	err     error
}

func decodeReviewStartFrame(t *testing.T, capturePath string) (string, map[string]json.RawMessage) {
	t.Helper()
	frames := waitForCapturedRawFrames(t, capturePath, 2, backgroundTerminalTestTimeout)
	var frame struct {
		Method string                     `json:"method"`
		Params map[string]json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(frames[1], &frame); err != nil {
		t.Fatalf("decode captured frame: %v", err)
	}
	return frame.Method, frame.Params
}

// The request carries only threadId and target. Omitting delivery is the
// protocol's inline default on every supported version; codex 0.166.0
// removed every other value.
func TestStartReviewForTurnSendsTheWireFrameWithoutDelivery(t *testing.T) {
	t.Parallel()
	s, capturePath := newCapturingSession(t, "codex-thread-review")
	target, err := ReviewBaseBranch("main")
	if err != nil {
		t.Fatalf("ReviewBaseBranch: %v", err)
	}

	answerer, done := startReviewForTurnAsync(t, s, target)
	answerer.answer(t, `{"turn":{"id":"turn-1","items":[],"status":"inProgress"},
		"reviewThreadId":"codex-thread-review"}`)

	got := <-done
	if got.err != nil {
		t.Fatalf("StartReviewForTurn: %v", got.err)
	}
	want := ReviewStarted{ReviewThreadID: "codex-thread-review", TurnID: "turn-1", TurnStatus: "inProgress"}
	if got.started != want {
		t.Errorf("started = %+v, want %+v", got.started, want)
	}

	method, params := decodeReviewStartFrame(t, capturePath)
	if method != reviewStartMethod {
		t.Errorf("method = %q, want %q", method, reviewStartMethod)
	}
	if len(params) != 2 || string(params["threadId"]) != `"codex-thread-review"` ||
		string(params["target"]) != `{"branch":"main","type":"baseBranch"}` {
		t.Errorf("params = %s, want exactly threadId and target", params)
	}
}

// A server that answers with a different thread ran the review somewhere this
// Session cannot scope or interrupt. The call must name that thread and close
// the process so the billed review does not continue invisibly.
func TestStartReviewForTurnRefusesAReviewOnAnotherThread(t *testing.T) {
	t.Parallel()
	s, _ := newCapturingSession(t, "codex-thread-own")

	answerer, done := startReviewForTurnAsync(t, s, ReviewUncommittedChanges())
	answerer.answer(t, `{"turn":{"id":"turn-2","items":[],"status":"inProgress"},
		"reviewThreadId":"review-thread-9"}`)

	got := <-done
	if got.err == nil || !strings.Contains(got.err.Error(), "review-thread-9") {
		t.Fatalf("StartReviewForTurn on another thread = %+v, %v; want a refusal naming it", got.started, got.err)
	}
	if !s.closing.Load() {
		t.Error("session left open after the review landed on another thread")
	}
}

func TestStartReviewForTurnRefusesMissingReviewThreadID(t *testing.T) {
	t.Parallel()
	s, _ := newCapturingSession(t, "codex-thread-noid")

	answerer, done := startReviewForTurnAsync(t, s, ReviewUncommittedChanges())
	answerer.answer(t, `{"turn":{"id":"turn-3","items":[],"status":"inProgress"}}`)

	got := <-done
	// Without the returned id AO cannot verify where the review runs.
	if got.err == nil || !strings.Contains(got.err.Error(), "reviewThreadId") {
		t.Fatalf("StartReviewForTurn without a reviewThreadId = %v, want a refusal", got.err)
	}
	if !s.closing.Load() {
		t.Error("session left open after an unverifiable review start")
	}
}

func TestStartReviewForTurnValidatesBeforeWriting(t *testing.T) {
	t.Parallel()
	s, capturePath := newCapturingSession(t, "codex-thread-guard")

	if _, err := s.StartReviewForTurn(context.Background(), ReviewTarget{}, ReviewRunOptions{}); err == nil {
		t.Fatal("StartReviewForTurn with an unset target must fail")
	}
	if frames := readCapturedRawFrames(t, capturePath); len(frames) != 0 {
		t.Fatalf("rejected request still wrote %d frames", len(frames))
	}
	s.mu.Lock()
	review := s.review
	s.mu.Unlock()
	if review != nil {
		t.Fatalf("rejected request left a review reservation: %+v", review)
	}

	// A session that never completed its handshake has no thread id to
	// name; the RPC must say so rather than sending an empty threadId.
	bare := &Session{threadID: testThread, pending: map[int64]chan json.RawMessage{}}
	if _, err := bare.StartReviewForTurn(context.Background(), ReviewUncommittedChanges(), ReviewRunOptions{}); err == nil {
		t.Fatal("StartReviewForTurn without a thread id must fail")
	}
	if err := bare.CompactThread(context.Background()); err == nil {
		t.Fatal("CompactThread without a thread id must fail")
	}
}

func TestCompactThreadSendsTheWireFrame(t *testing.T) {
	t.Parallel()
	s, capturePath := newCapturingSession(t, "codex-thread-compact")

	errCh := make(chan error, 1)
	go func() { errCh <- s.CompactThread(context.Background()) }()
	newPendingAnswerer(s).answer(t, `{}`)
	if err := <-errCh; err != nil {
		t.Fatalf("CompactThread: %v", err)
	}

	frames := waitForCapturedRawFrames(t, capturePath, 1, backgroundTerminalTestTimeout)
	var frame struct {
		Method string `json:"method"`
		Params struct {
			ThreadID string `json:"threadId"`
		} `json:"params"`
	}
	if err := json.Unmarshal(frames[0], &frame); err != nil {
		t.Fatalf("decode captured frame: %v", err)
	}
	if frame.Method != threadCompactStartMethod {
		t.Errorf("method = %q, want %q", frame.Method, threadCompactStartMethod)
	}
	if frame.Params.ThreadID != "codex-thread-compact" {
		t.Errorf("threadId = %q", frame.Params.ThreadID)
	}
}
