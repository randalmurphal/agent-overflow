package usermessage

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"agent-overflow/internal/store"
)

func TestMarshalReturnsEmptyForZeroInputs(t *testing.T) {
	got, err := Marshal(nil, nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if got != "" {
		t.Fatalf("Marshal(zero inputs) = %q, want empty string", got)
	}
}

func TestMarshalIncludesAttachments(t *testing.T) {
	attachments := []store.Attachment{
		{ID: "a1", ThreadID: "t1", Filename: "shot.png", MimeType: "image/png", Size: 12345},
	}
	got, err := Marshal(attachments, nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if got == "" {
		t.Fatal("Marshal returned empty string with one attachment")
	}
	var decoded Meta
	if err := json.Unmarshal([]byte(got), &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(decoded.Attachments) != 1 {
		t.Fatalf("len(attachments) = %d, want 1", len(decoded.Attachments))
	}
	if decoded.Attachments[0].ID != "a1" || decoded.Attachments[0].Filename != "shot.png" {
		t.Fatalf("attachment fields wrong: %+v", decoded.Attachments[0])
	}
}

func TestMarshalIncludesSourceAndRevisionContext(t *testing.T) {
	src := &store.ProposedPlanSourceRef{ThreadID: "t1", ItemID: "p1"}
	revPlan := &store.ProposedPlanSourceRef{ThreadID: "t1", ItemID: "p2"}
	revDiff := &store.DiffReviewSourceRef{ThreadID: "t1", Scope: "working-tree", SourceKey: "k"}
	got, err := Marshal(nil, src, revPlan, []string{"c1"}, revDiff, []string{"d1"})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var decoded Meta
	if err := json.Unmarshal([]byte(got), &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.SourceProposedPlan == nil || decoded.SourceProposedPlan.ItemID != "p1" {
		t.Fatalf("SourceProposedPlan wrong: %+v", decoded.SourceProposedPlan)
	}
	if decoded.RevisionSourceProposedPlan == nil || decoded.RevisionSourceProposedPlan.ItemID != "p2" {
		t.Fatalf("RevisionSourceProposedPlan wrong: %+v", decoded.RevisionSourceProposedPlan)
	}
	if len(decoded.RevisionSourceCommentIDs) != 1 || decoded.RevisionSourceCommentIDs[0] != "c1" {
		t.Fatalf("RevisionSourceCommentIDs wrong: %+v", decoded.RevisionSourceCommentIDs)
	}
	if decoded.RevisionSourceDiffReview == nil || decoded.RevisionSourceDiffReview.SourceKey != "k" {
		t.Fatalf("RevisionSourceDiffReview wrong: %+v", decoded.RevisionSourceDiffReview)
	}
	if len(decoded.RevisionSourceDiffCommentIDs) != 1 || decoded.RevisionSourceDiffCommentIDs[0] != "d1" {
		t.Fatalf("RevisionSourceDiffCommentIDs wrong: %+v", decoded.RevisionSourceDiffCommentIDs)
	}
}

func TestMarshalJSONShapeMatchesContract(t *testing.T) {
	// The JSON tags are the wire shape the frontend reads.
	// Field names must remain camelCase and `omitempty`.
	got, err := Marshal([]store.Attachment{
		{ID: "a", ThreadID: "t", Filename: "f.png", MimeType: "image/png", Size: 1},
	}, nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(got, `"attachments":`) {
		t.Fatalf("missing attachments key: %s", got)
	}
	if strings.Contains(got, `"sourceProposedPlan":`) ||
		strings.Contains(got, `"revisionSourceProposedPlan":`) ||
		strings.Contains(got, `"revisionSourceDiffReview":`) {
		t.Fatalf("omitempty fields leaked into output: %s", got)
	}
}

func TestFromItemEmptyMeta(t *testing.T) {
	meta, err := FromItem(store.Item{Meta: ""})
	if err != nil {
		t.Fatalf("FromItem: %v", err)
	}
	if !reflect.DeepEqual(meta, Meta{}) {
		t.Fatalf("empty meta should decode to zero Meta, got %+v", meta)
	}

	meta, err = FromItem(store.Item{Meta: "   "})
	if err != nil {
		t.Fatalf("FromItem whitespace: %v", err)
	}
	if !reflect.DeepEqual(meta, Meta{}) {
		t.Fatalf("whitespace meta should decode to zero Meta, got %+v", meta)
	}
}

func TestFromItemRoundTrip(t *testing.T) {
	encoded, err := Marshal([]store.Attachment{
		{ID: "a", ThreadID: "t", Filename: "f.png", MimeType: "image/png", Size: 12},
	}, nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got, err := FromItem(store.Item{Meta: encoded})
	if err != nil {
		t.Fatalf("FromItem: %v", err)
	}
	if len(got.Attachments) != 1 || got.Attachments[0].ID != "a" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}

func TestFromItemInvalidJSONReturnsError(t *testing.T) {
	_, err := FromItem(store.Item{Meta: "{this-is-not-json"})
	if err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
}

func TestEncodeDraftSourceReturnsEmptyForNil(t *testing.T) {
	got, err := EncodeDraftSource(nil)
	if err != nil {
		t.Fatalf("EncodeDraftSource: %v", err)
	}
	if got != "" {
		t.Fatalf("nil ref should encode to empty string, got %q", got)
	}
}

func TestEncodeDraftSourceReturnsEmptyForEmptyItemID(t *testing.T) {
	got, err := EncodeDraftSource(&store.ProposedPlanSourceRef{ThreadID: "t1", ItemID: ""})
	if err != nil {
		t.Fatalf("EncodeDraftSource: %v", err)
	}
	if got != "" {
		t.Fatalf("empty ItemID should encode to empty string, got %q", got)
	}
}

func TestEncodeDraftSourceEncodesValidRef(t *testing.T) {
	got, err := EncodeDraftSource(&store.ProposedPlanSourceRef{ThreadID: "t1", ItemID: "p1"})
	if err != nil {
		t.Fatalf("EncodeDraftSource: %v", err)
	}
	if !strings.Contains(got, `"itemId":"p1"`) {
		t.Fatalf("encoded ref missing itemId: %s", got)
	}
}
