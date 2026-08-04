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

func TestReviewDeliveryWireValues(t *testing.T) {
	// `v2_enum_from_core!` applies `#[serde(rename_all = "camelCase")]`;
	// both variants are single words, so the wire values are lowercase.
	if string(ReviewDeliveryInline) != "inline" || string(ReviewDeliveryDetached) != "detached" {
		t.Fatalf("delivery wire values drifted: %q / %q", ReviewDeliveryInline, ReviewDeliveryDetached)
	}
	if !ReviewDeliveryDefault.valid() || !ReviewDeliveryInline.valid() || !ReviewDeliveryDetached.valid() {
		t.Fatal("every declared delivery must validate")
	}
	if ReviewDelivery("Inline").valid() {
		t.Fatal("an unknown delivery must not validate")
	}
}

func TestStartReviewSendsTheWireFrameAndRoutesOnTheReturnedID(t *testing.T) {
	s, capturePath := newCapturingSession(t, "codex-thread-review")

	type result struct {
		started ReviewStarted
		err     error
	}
	done := make(chan result, 1)
	go func() {
		target, err := ReviewBaseBranch("main")
		if err != nil {
			done <- result{err: err}
			return
		}
		started, err := s.StartReview(context.Background(), target, ReviewDeliveryDetached)
		done <- result{started: started, err: err}
	}()

	newPendingAnswerer(s).answer(t, `{"turn":{"id":"turn-1","items":[],"status":"inProgress"},
		"reviewThreadId":"review-thread-9"}`)

	got := <-done
	if got.err != nil {
		t.Fatalf("StartReview: %v", got.err)
	}
	if got.started.ReviewThreadID != "review-thread-9" {
		t.Errorf("ReviewThreadID = %q, want the returned id", got.started.ReviewThreadID)
	}
	if got.started.TurnID != "turn-1" || got.started.TurnStatus != "inProgress" {
		t.Errorf("turn fields = %+v", got.started)
	}
	// Detached is derived from the RETURNED id versus this session's own
	// thread, not from the requested delivery.
	if !got.started.Detached {
		t.Error("Detached = false for a review that came back on another thread")
	}

	frames := waitForCapturedRawFrames(t, capturePath, 1, backgroundTerminalTestTimeout)
	var frame struct {
		Method string `json:"method"`
		Params struct {
			ThreadID string          `json:"threadId"`
			Target   json.RawMessage `json:"target"`
			Delivery string          `json:"delivery"`
		} `json:"params"`
	}
	if err := json.Unmarshal(frames[0], &frame); err != nil {
		t.Fatalf("decode captured frame: %v", err)
	}
	if frame.Method != reviewStartMethod {
		t.Errorf("method = %q, want %q", frame.Method, reviewStartMethod)
	}
	if frame.Params.ThreadID != "codex-thread-review" {
		t.Errorf("threadId = %q", frame.Params.ThreadID)
	}
	if string(frame.Params.Target) != `{"branch":"main","type":"baseBranch"}` {
		t.Errorf("target = %s", frame.Params.Target)
	}
	if frame.Params.Delivery != "detached" {
		t.Errorf("delivery = %q, want detached", frame.Params.Delivery)
	}
}

func TestStartReviewInlineReportsNotDetachedAndOmitsTheDefaultDelivery(t *testing.T) {
	s, capturePath := newCapturingSession(t, "codex-thread-inline")

	done := make(chan ReviewStarted, 1)
	errCh := make(chan error, 1)
	go func() {
		started, err := s.StartReview(context.Background(), ReviewUncommittedChanges(), ReviewDeliveryDefault)
		if err != nil {
			errCh <- err
			return
		}
		done <- started
	}()

	newPendingAnswerer(s).answer(t, `{"turn":{"id":"turn-2","items":[],"status":"inProgress"},
		"reviewThreadId":"codex-thread-inline"}`)

	select {
	case err := <-errCh:
		t.Fatalf("StartReview: %v", err)
	case started := <-done:
		if started.Detached {
			t.Error("Detached = true for a review returned on this session's own thread")
		}
	}

	frames := waitForCapturedRawFrames(t, capturePath, 1, backgroundTerminalTestTimeout)
	var frame struct {
		Params map[string]json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(frames[0], &frame); err != nil {
		t.Fatalf("decode captured frame: %v", err)
	}
	// Omitted rather than sent as "inline": the wire field is
	// `Option<ReviewDelivery>` and absence is already the inline default,
	// so naming it would assert a default we do not own.
	if _, present := frame.Params["delivery"]; present {
		t.Errorf("delivery key present for the default delivery: %v", frame.Params)
	}
}

func TestStartReviewRefusesMissingReviewThreadID(t *testing.T) {
	s, _ := newCapturingSession(t, "codex-thread-noid")

	errCh := make(chan error, 1)
	go func() {
		_, err := s.StartReview(context.Background(), ReviewUncommittedChanges(), ReviewDeliveryInline)
		errCh <- err
	}()
	newPendingAnswerer(s).answer(t, `{"turn":{"id":"turn-3","items":[],"status":"inProgress"}}`)

	err := <-errCh
	// Falling back to "assume inline" here would reintroduce exactly the
	// assumption the returned id exists to replace.
	if err == nil || !strings.Contains(err.Error(), "reviewThreadId") {
		t.Fatalf("StartReview without a reviewThreadId = %v, want a refusal", err)
	}
}

func TestStartReviewValidatesBeforeWriting(t *testing.T) {
	s, capturePath := newCapturingSession(t, "codex-thread-guard")

	if _, err := s.StartReview(context.Background(), ReviewTarget{}, ReviewDeliveryInline); err == nil {
		t.Fatal("StartReview with an unset target must fail")
	}
	if _, err := s.StartReview(context.Background(), ReviewUncommittedChanges(), ReviewDelivery("sideways")); err == nil {
		t.Fatal("StartReview with an unknown delivery must fail")
	}
	if frames := readCapturedRawFrames(t, capturePath); len(frames) != 0 {
		t.Fatalf("rejected requests still wrote %d frames", len(frames))
	}

	// A session that never completed its handshake has no thread id to
	// name; the RPC must say so rather than sending an empty threadId.
	bare := &Session{threadID: testThread, pending: map[int64]chan json.RawMessage{}}
	if _, err := bare.StartReview(context.Background(), ReviewUncommittedChanges(), ReviewDeliveryInline); err == nil {
		t.Fatal("StartReview without a thread id must fail")
	}
	if err := bare.CompactThread(context.Background()); err == nil {
		t.Fatal("CompactThread without a thread id must fail")
	}
}

func TestCompactThreadSendsTheWireFrame(t *testing.T) {
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
