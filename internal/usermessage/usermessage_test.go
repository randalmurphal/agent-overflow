package usermessage

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"agent-overflow/internal/store"
)

func TestMarshalReturnsEmptyForZeroInputs(t *testing.T) {
	got, err := Marshal(Input{})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if got != "" {
		t.Fatalf("Marshal(zero inputs) = %q, want empty string", got)
	}
}

func TestMarshalRoundTripsComposerCommandProvenance(t *testing.T) {
	encoded, err := Marshal(Input{ExpandComposerCommands: true})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	meta, err := FromItem(store.Item{Meta: encoded})
	if err != nil {
		t.Fatalf("FromItem: %v", err)
	}
	if !meta.ExpandComposerCommands {
		t.Fatalf("ExpandComposerCommands = false, want true in %s", encoded)
	}
}

func TestMarshalIncludesAttachments(t *testing.T) {
	attachments := []store.Attachment{
		{ID: "a1", ThreadID: "t1", Filename: "shot.png", MimeType: "image/png", Size: 12345},
	}
	got, err := Marshal(Input{Attachments: attachments})
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
	got, err := Marshal(Input{
		SourcePlan:             src,
		RevisionSourcePlan:     revPlan,
		RevisionCommentIDs:     []string{"c1"},
		RevisionSourceDiff:     revDiff,
		RevisionDiffCommentIDs: []string{"d1"},
	})
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
	got, err := Marshal(Input{Attachments: []store.Attachment{
		{ID: "a", ThreadID: "t", Filename: "f.png", MimeType: "image/png", Size: 1},
	}})
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
	encoded, err := Marshal(Input{Attachments: []store.Attachment{
		{ID: "a", ThreadID: "t", Filename: "f.png", MimeType: "image/png", Size: 12},
	}})
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

func TestReadProviderItemID(t *testing.T) {
	cases := []struct {
		name string
		meta string
		want string
	}{
		{"empty", "", ""},
		{"whitespace", "   \n\t", ""},
		{"missing key", `{"foo":"bar"}`, ""},
		{"present", `{"provider_item_id":"u-abc"}`, "u-abc"},
		{"present with other keys", `{"foo":"bar","provider_item_id":"u-abc"}`, "u-abc"},
		{"non-string value", `{"provider_item_id":42}`, ""},
		{"null value", `{"provider_item_id":null}`, ""},
		{"malformed json", `{not-json`, ""},
		{"json null literal", `null`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ReadProviderItemID(tc.meta); got != tc.want {
				t.Errorf("ReadProviderItemID(%q) = %q, want %q", tc.meta, got, tc.want)
			}
		})
	}
}

func TestMergeProviderItemIDEmptyProviderIDPassesThrough(t *testing.T) {
	cases := []string{"", `{"foo":"bar"}`, `{"provider_item_id":"u-old"}`}
	for _, existing := range cases {
		got, err := MergeProviderItemID(existing, "")
		if err != nil {
			t.Fatalf("MergeProviderItemID(%q, \"\"): %v", existing, err)
		}
		if got != existing {
			t.Errorf("MergeProviderItemID(%q, \"\") = %q, want unchanged %q", existing, got, existing)
		}
	}
}

func TestMergeProviderItemIDPreservesOtherKeys(t *testing.T) {
	got, err := MergeProviderItemID(`{"foo":"bar","baz":42}`, "u-new")
	if err != nil {
		t.Fatalf("MergeProviderItemID: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(got), &decoded); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if decoded["provider_item_id"] != "u-new" {
		t.Errorf("provider_item_id = %v, want u-new", decoded["provider_item_id"])
	}
	if decoded["foo"] != "bar" || decoded["baz"].(float64) != 42 {
		t.Errorf("other keys lost: %+v", decoded)
	}
}

func TestMergeProviderItemIDEmptyMetaProducesValidJSON(t *testing.T) {
	got, err := MergeProviderItemID("", "u-new")
	if err != nil {
		t.Fatalf("MergeProviderItemID(\"\", \"u-new\"): %v", err)
	}
	if got == "" {
		t.Fatal("expected JSON output, got empty")
	}
	if ReadProviderItemID(got) != "u-new" {
		t.Errorf("round-trip mismatch: %q reads back as %q, want u-new", got, ReadProviderItemID(got))
	}
}

func TestMergeProviderItemIDDuplicateIsNoOp(t *testing.T) {
	existing := `{"provider_item_id":"u-same","foo":"bar"}`
	got, err := MergeProviderItemID(existing, "u-same")
	if err != nil {
		t.Fatalf("MergeProviderItemID: %v", err)
	}
	if got != existing {
		t.Errorf("duplicate merge should be no-op; got %q, want unchanged %q", got, existing)
	}
}

func TestMergeProviderItemIDReplacesExisting(t *testing.T) {
	got, err := MergeProviderItemID(`{"provider_item_id":"u-old","foo":"bar"}`, "u-new")
	if err != nil {
		t.Fatalf("MergeProviderItemID: %v", err)
	}
	if ReadProviderItemID(got) != "u-new" {
		t.Errorf("provider_item_id not replaced; got %q", got)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(got), &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded["foo"] != "bar" {
		t.Errorf("sibling key lost: %+v", decoded)
	}
}

func TestMergeProviderItemIDRejectsMalformedExisting(t *testing.T) {
	_, err := MergeProviderItemID(`{not-json`, "u-new")
	if err == nil {
		t.Fatal("expected error for malformed existing meta, got nil")
	}
}

func TestMergeProviderItemIDHandlesJSONNullExisting(t *testing.T) {
	// "null" decodes to a nil map; the merge must still produce a valid
	// JSON object with the stamped id so the round-trip through
	// ReadProviderItemID works.
	got, err := MergeProviderItemID(`null`, "u-new")
	if err != nil {
		t.Fatalf("MergeProviderItemID(\"null\", ...): %v", err)
	}
	if ReadProviderItemID(got) != "u-new" {
		t.Errorf("json-null existing should be replaced with valid object; got %q", got)
	}
}

// R5-8 (round 5): the parent uuid is stamped into item meta in the SAME
// merge as the item id so the two can never diverge across a failed
// follow-up write. These pin the combined helper's contract.

func TestMergeProviderIDsStampsBothKeys(t *testing.T) {
	got, err := MergeProviderIDs(`{"attachments":[{"id":"att-1"}]}`, "u-new", "p-new")
	if err != nil {
		t.Fatalf("MergeProviderIDs: %v", err)
	}
	if ReadProviderItemID(got) != "u-new" {
		t.Errorf("item id not stamped: %q", got)
	}
	if ReadProviderParentUUID(got) != "p-new" {
		t.Errorf("parent uuid not stamped: %q", got)
	}
	if !strings.Contains(got, "att-1") {
		t.Errorf("existing keys lost: %q", got)
	}
}

func TestMergeProviderIDsEmptyValuePreservesStoredKey(t *testing.T) {
	existing, err := MergeProviderIDs("", "u-old", "p-old")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	got, err := MergeProviderIDs(existing, "u-new", "")
	if err != nil {
		t.Fatalf("merge with empty parent: %v", err)
	}
	if ReadProviderItemID(got) != "u-new" || ReadProviderParentUUID(got) != "p-old" {
		t.Errorf("empty parent must not blank the stored one: %q", got)
	}
	got, err = MergeProviderIDs(existing, "", "p-new")
	if err != nil {
		t.Fatalf("merge with empty id: %v", err)
	}
	if ReadProviderItemID(got) != "u-old" || ReadProviderParentUUID(got) != "p-new" {
		t.Errorf("empty id must not blank the stored one: %q", got)
	}
}

func TestMergeProviderIDsNoChangeReturnsOriginal(t *testing.T) {
	existing, err := MergeProviderIDs("", "u-1", "p-1")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	got, err := MergeProviderIDs(existing, "u-1", "p-1")
	if err != nil {
		t.Fatalf("no-op merge: %v", err)
	}
	if got != existing {
		t.Errorf("identical values must return the original string for no-op detection: %q vs %q", got, existing)
	}
	if got, err := MergeProviderIDs(existing, "", ""); err != nil || got != existing {
		t.Errorf("both-empty merge must pass through: %q err=%v", got, err)
	}
}

func TestReadProviderParentUUID(t *testing.T) {
	if got := ReadProviderParentUUID(""); got != "" {
		t.Errorf("empty meta = %q", got)
	}
	if got := ReadProviderParentUUID(`{not-json`); got != "" {
		t.Errorf("malformed meta = %q", got)
	}
	if got := ReadProviderParentUUID(`{"provider_parent_uuid":42}`); got != "" {
		t.Errorf("non-string value = %q", got)
	}
	if got := ReadProviderParentUUID(`{"provider_parent_uuid":"p-1"}`); got != "p-1" {
		t.Errorf("stored parent = %q, want p-1", got)
	}
}

// The kind rides the meta because the timeline renders from it: a tile
// for an image, a chip for a file. It is omitempty and empty means image,
// so a row written before the column existed still renders as what it is.
func TestMarshalProjectsAttachmentKind(t *testing.T) {
	got, err := Marshal(Input{Attachments: []store.Attachment{
		{ID: "a1", ThreadID: "t1", Filename: "shot.png", MimeType: "image/png", Kind: store.AttachmentKindImage},
		{ID: "a2", ThreadID: "t1", Filename: "report.pdf", MimeType: "application/pdf", Kind: store.AttachmentKindFile},
	}})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var decoded Meta
	if err := json.Unmarshal([]byte(got), &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.Attachments[0].Kind != store.AttachmentKindImage {
		t.Errorf("image kind: got %q", decoded.Attachments[0].Kind)
	}
	if decoded.Attachments[1].Kind != store.AttachmentKindFile {
		t.Errorf("file kind: got %q", decoded.Attachments[1].Kind)
	}

	// A pre-kind row decodes to the empty string, which readers treat as
	// an image rather than as a value to repair.
	var legacy Meta
	if err := json.Unmarshal([]byte(`{"attachments":[{"id":"a3","filename":"old.png"}]}`), &legacy); err != nil {
		t.Fatalf("decode legacy: %v", err)
	}
	if legacy.Attachments[0].Kind != "" {
		t.Errorf("legacy kind: got %q want empty", legacy.Attachments[0].Kind)
	}
}
