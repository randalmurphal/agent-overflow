package composerdraft

import (
	"encoding/json"
	"testing"

	"agent-overflow/internal/store"
	"agent-overflow/internal/usermessage"
)

func TestFromPartsEmptyAttachments(t *testing.T) {
	draft, err := FromParts("t-1", "hello", nil, nil, 42)
	if err != nil {
		t.Fatalf("FromParts: %v", err)
	}
	if draft.ThreadID != "t-1" {
		t.Errorf("ThreadID = %q, want t-1", draft.ThreadID)
	}
	if draft.Content != "hello" {
		t.Errorf("Content = %q, want hello", draft.Content)
	}
	if draft.Attachments != "null" && draft.Attachments != "[]" {
		t.Errorf("Attachments = %q, want null/[]", draft.Attachments)
	}
	if draft.TerminalChips != "[]" {
		t.Errorf("TerminalChips = %q, want []", draft.TerminalChips)
	}
	if draft.PendingPlanImplementation != "" {
		t.Errorf("PendingPlanImplementation = %q, want empty", draft.PendingPlanImplementation)
	}
	if draft.UpdatedAt != 42 {
		t.Errorf("UpdatedAt = %d, want 42", draft.UpdatedAt)
	}
}

func TestFromPartsWithAttachmentsAndPlan(t *testing.T) {
	ref := &store.ProposedPlanSourceRef{ThreadID: "src", ItemID: "p1"}
	draft, err := FromParts("t-2", "body", []string{"a1", "a2"}, ref, 99)
	if err != nil {
		t.Fatalf("FromParts: %v", err)
	}
	var ids []string
	if err := json.Unmarshal([]byte(draft.Attachments), &ids); err != nil {
		t.Fatalf("decode attachments: %v", err)
	}
	if len(ids) != 2 || ids[0] != "a1" || ids[1] != "a2" {
		t.Errorf("attachments = %v, want [a1 a2]", ids)
	}
	if draft.PendingPlanImplementation == "" {
		t.Error("PendingPlanImplementation must be populated for a non-empty ref")
	}
}

func TestFromUserItemRoundTripsAttachmentsAndSourcePlan(t *testing.T) {
	src := &store.ProposedPlanSourceRef{ThreadID: "src", ItemID: "p1"}
	meta, err := usermessage.Marshal(
		[]store.Attachment{
			{ID: "att-1", ThreadID: "t-src", Filename: "shot.png", MimeType: "image/png", Size: 12},
		},
		src, nil, nil, nil, nil,
	)
	if err != nil {
		t.Fatalf("usermessage.Marshal: %v", err)
	}
	item := store.Item{
		ID:       "u-1",
		ThreadID: "t-src",
		Kind:     "user_text",
		Role:     "user",
		Summary:  "hello there",
		Meta:     meta,
	}

	draft, err := FromUserItem("t-target", item, 100)
	if err != nil {
		t.Fatalf("FromUserItem: %v", err)
	}
	if draft.ThreadID != "t-target" {
		t.Errorf("ThreadID = %q, want t-target", draft.ThreadID)
	}
	if draft.Content != "hello there" {
		t.Errorf("Content = %q, want %q", draft.Content, "hello there")
	}
	var ids []string
	if err := json.Unmarshal([]byte(draft.Attachments), &ids); err != nil {
		t.Fatalf("decode attachments: %v", err)
	}
	if len(ids) != 1 || ids[0] != "att-1" {
		t.Errorf("attachments = %v, want [att-1]", ids)
	}
	if draft.PendingPlanImplementation == "" {
		t.Error("PendingPlanImplementation must round-trip the source plan ref")
	}
}

func TestFromUserItemSkipsBlankAttachmentIDs(t *testing.T) {
	// Marshal directly using usermessage so the JSON shape stays
	// authoritative; then patch in a blank-ID attachment to mimic an
	// older / corrupt row.
	meta, err := usermessage.Marshal(
		[]store.Attachment{
			{ID: "  ", ThreadID: "t-src", Filename: "x.png", MimeType: "image/png", Size: 1},
			{ID: "att-2", ThreadID: "t-src", Filename: "y.png", MimeType: "image/png", Size: 1},
		},
		nil, nil, nil, nil, nil,
	)
	if err != nil {
		t.Fatalf("usermessage.Marshal: %v", err)
	}
	item := store.Item{ID: "u-1", ThreadID: "t-src", Kind: "user_text", Role: "user", Summary: "x", Meta: meta}

	draft, err := FromUserItem("t-target", item, 1)
	if err != nil {
		t.Fatalf("FromUserItem: %v", err)
	}
	var ids []string
	if err := json.Unmarshal([]byte(draft.Attachments), &ids); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, id := range ids {
		if id == "  " || id == "" {
			t.Errorf("blank attachment id leaked through: %v", ids)
		}
	}
}
