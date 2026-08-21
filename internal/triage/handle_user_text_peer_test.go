package triage

import (
	"encoding/json"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
)

// A message another Claude session sent to this one reaches the same
// top-level, no-pending-send branch as provider-injected context — no
// pending entry CAN match, because the CLI mints the uuid — but it is not
// the same thing and must not persist as "Injected provider context". The
// parser proved its provenance (a structured `origin.kind == "peer"` or a
// balanced wrapper), so it persists as a message with a named author.
func TestHandleUserText_PeerMessage_PersistsAMessageWithItsAuthor(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 2)

	if err := router.Handle(provider.ProviderEvent{
		Kind:     provider.EventUserText,
		ThreadID: "t1",
		Content:  "the build is red on main",
		Meta: json.RawMessage(`{"provider_item_id":"peer-uuid-1","cross_session_message":true,` +
			`"cross_session_from":"uds:/tmp/cc-socks/3896836.sock","cross_session_from_name":"BETA","origin":"peer-session"}`),
		Timestamp: time.UnixMilli(1_700_000_000_000),
	}); err != nil {
		t.Fatalf("Handle EventUserText: %v", err)
	}

	if _, found, _ := st.GetThreadItem("t1", "injected:wire:peer-uuid-1"); found {
		t.Fatal("a peer message persisted as injected provider context")
	}

	persisted, found, err := st.GetThreadItem("t1", "user:peer:peer-uuid-1")
	if err != nil || !found {
		t.Fatalf("expected user:peer row to exist: found=%v err=%v", found, err)
	}
	if persisted.Kind != string(provider.ItemUserText) {
		t.Fatalf("Kind = %q, want user_text", persisted.Kind)
	}
	// User-role because that is what the model saw: the CLI injects the
	// delivery as a user-role turn, and a system notice would not explain
	// why the assistant answered.
	if persisted.Role != "user" {
		t.Fatalf("Role = %q, want user", persisted.Role)
	}
	if persisted.TurnIndex != 2 {
		t.Fatalf("TurnIndex = %d, want 2 (the open turn the peer started)", persisted.TurnIndex)
	}
	if persisted.Summary != "the build is red on main" {
		t.Fatalf("Summary = %q, want the peer's own text unlabelled", persisted.Summary)
	}

	var meta map[string]any
	if err := json.Unmarshal([]byte(persisted.Meta), &meta); err != nil {
		t.Fatalf("decode persisted meta: %v", err)
	}
	// The attribution is what keeps a user-role row from being a lie about
	// WHO — same field and vocabulary as Codex's external-queue rows, so
	// the frontend has one branch for both.
	if got, _ := meta["origin"].(string); got != crossSessionMessageOrigin {
		t.Fatalf("meta.origin = %v, want %q", meta["origin"], crossSessionMessageOrigin)
	}
	if got, _ := meta["cross_session_from_name"].(string); got != "BETA" {
		t.Fatalf("meta.cross_session_from_name = %v, want BETA", meta["cross_session_from_name"])
	}
	if flagged, _ := meta["wire_only"].(bool); !flagged {
		t.Fatalf("meta.wire_only = %v, want true (no draft, no local send, no edit pencil)", meta["wire_only"])
	}
}

// An older CLI shape carries no name, only a socket address — and a
// delivery whose author cannot be named is still not something the user
// typed. The absent key must be ABSENT rather than empty, so the renderer
// can fall back instead of labelling the message as being from nobody.
func TestHandleUserText_PeerMessage_WithoutAName(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 1)

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventUserText,
		ThreadID:  "t1",
		Content:   "hello",
		Meta:      json.RawMessage(`{"provider_item_id":"peer-uuid-2","cross_session_message":true}`),
		Timestamp: time.UnixMilli(1_700_000_000_000),
	}); err != nil {
		t.Fatalf("Handle EventUserText: %v", err)
	}
	persisted, found, err := st.GetThreadItem("t1", "user:peer:peer-uuid-2")
	if err != nil || !found {
		t.Fatalf("expected user:peer row: found=%v err=%v", found, err)
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(persisted.Meta), &meta); err != nil {
		t.Fatalf("decode persisted meta: %v", err)
	}
	for _, key := range []string{"cross_session_from", "cross_session_from_name"} {
		if _, present := meta[key]; present {
			t.Fatalf("%s present with no value on the wire: %v", key, meta)
		}
	}
	if got, _ := meta["origin"].(string); got != crossSessionMessageOrigin {
		t.Fatalf("meta.origin = %v, want %q", meta["origin"], crossSessionMessageOrigin)
	}
}

// Resume replay re-delivers the same envelope. The id is deterministic
// from the wire id precisely so the second arrival upserts rather than
// duplicating the message.
func TestHandleUserText_PeerMessage_IsIdempotentAcrossReplay(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 1)

	evt := provider.ProviderEvent{
		Kind:      provider.EventUserText,
		ThreadID:  "t1",
		Content:   "hello",
		Meta:      json.RawMessage(`{"provider_item_id":"peer-uuid-3","cross_session_message":true,"cross_session_from_name":"BETA"}`),
		Timestamp: time.UnixMilli(1_700_000_000_000),
	}
	for i := 0; i < 2; i++ {
		if err := router.Handle(evt); err != nil {
			t.Fatalf("Handle #%d: %v", i+1, err)
		}
	}

	rows, err := st.ListItemsForTurn("t1", 1)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	peerRows := 0
	for _, r := range rows {
		if r.ID == "user:peer:peer-uuid-3" {
			peerRows++
		}
	}
	if peerRows != 1 {
		t.Fatalf("got %d peer rows across two Handle calls, want 1", peerRows)
	}
}

// The origin vocabulary is duplicated across the provider boundary on
// purpose — triage is provider-agnostic by contract and may not reach
// into a provider package for a term — so the two definitions are pinned
// against each other here rather than left to drift. The frontend
// branches on this exact string.
func TestPeerMessageOriginMatchesTheProviderConstant(t *testing.T) {
	if crossSessionMessageOrigin != claude.PeerTurnOrigin {
		t.Fatalf("triage origin %q != claude.PeerTurnOrigin %q", crossSessionMessageOrigin, claude.PeerTurnOrigin)
	}
}
