package usermessage

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"agent-overflow/internal/agentmeta"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
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

func TestIsAgentMessage(t *testing.T) {
	for _, tc := range []struct {
		meta Meta
		want bool
	}{
		{Meta{}, false},
		{Meta{Origin: "external-queue"}, false},
		{Meta{Origin: "peer-session"}, false},
		{Meta{Origin: agentmeta.OriginAgentThread}, true},
		{Meta{Origin: agentmeta.OriginAgentWake}, true},
		// The person's composer message Claude joined after an agent's.
		{Meta{Origin: agentmeta.OriginAgentWake, ExpandComposerCommands: true}, false},
		{Meta{Origin: agentmeta.OriginAgentThread, ExpandComposerCommands: true}, false},
	} {
		if got := agentmeta.IsAgentMessage(tc.meta.Agent()); got != tc.want {
			t.Errorf("IsAgentMessage(%+v) = %v, want %v", tc.meta, got, tc.want)
		}
	}
}

// The attribution part of a row's meta (agentmeta.Meta) is the fields of
// Meta of the same names, types and JSON keys, and Agent copies each: a
// reader of either decodes the same attribution from the same row.
func TestAgentIsTheAttributionPartOfMeta(t *testing.T) {
	agentType, metaType := reflect.TypeFor[agentmeta.Meta](), reflect.TypeFor[Meta]()
	var meta Meta
	for i := range agentType.NumField() {
		field := agentType.Field(i)
		own, ok := metaType.FieldByName(field.Name)
		if !ok || own.Type != field.Type || own.Tag.Get("json") != field.Tag.Get("json") {
			t.Fatalf("agentmeta.Meta.%s %s %q has no twin in Meta (%+v)", field.Name, field.Type, field.Tag.Get("json"), own)
		}
		reflect.ValueOf(&meta).Elem().FieldByIndex(own.Index).Set(nonZeroValue(t, field.Type))
	}
	agent := meta.Agent()
	for i := range agentType.NumField() {
		if reflect.ValueOf(agent).Field(i).IsZero() {
			t.Errorf("Agent does not copy %s", agentType.Field(i).Name)
		}
	}
	encoded, err := MarshalMeta(meta)
	if err != nil {
		t.Fatal(err)
	}
	var decoded agentmeta.Meta
	if err := json.Unmarshal([]byte(encoded), &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, agent) {
		t.Fatalf("a row's meta decodes as %+v, Agent reads %+v", decoded, agent)
	}
}

func TestProjectionCarriesBoundedShellMetadata(t *testing.T) {
	t.Parallel()
	shell := store.AttachmentShell{
		Command: "git status", ExitCode: 3, DurationMs: 42, Truncated: true, TimedOut: true, Preview: "one\n",
	}
	meta := Input{Attachments: []store.Attachment{
		{ID: "a1", ThreadID: "t1", Filename: "output.txt", Kind: store.AttachmentKindFile, Size: 9, Shell: shell},
		{ID: "a2", ThreadID: "t1", Filename: "notes.txt", Kind: store.AttachmentKindFile, Size: 4},
	}}.Projection()
	got := meta.Attachments[0].Shell
	want := ShellMeta{Command: "git status", ExitCode: 3, DurationMs: 42, Truncated: true, TimedOut: true, Preview: "one\n", More: true}
	if got == nil || *got != want {
		t.Fatalf("shell meta: %+v", got)
	}
	if meta.Attachments[1].Shell != nil {
		t.Fatal("an ordinary file carries shell metadata")
	}

	whole := Input{Attachments: []store.Attachment{{ID: "a", Kind: store.AttachmentKindFile, Size: 4, Shell: shell}}}.Projection()
	if whole.Attachments[0].Shell.More {
		t.Fatal("a preview holding the whole output reports more")
	}

	long := strings.Repeat("é", ShellCommandMetaBytes)
	shell.Command = "x" + long
	bounded := Input{Attachments: []store.Attachment{{ID: "a", Kind: store.AttachmentKindFile, Shell: shell}}}.Projection().Attachments[0].Shell
	if bounded.CommandLength != 1+ShellCommandMetaBytes || len(bounded.Command) > ShellCommandMetaBytes || !utf8.ValidString(bounded.Command) ||
		!strings.HasPrefix(shell.Command, bounded.Command) {
		t.Fatalf("bounded command: %d bytes, whole length %d", len(bounded.Command), bounded.CommandLength)
	}

	encoded, err := Marshal(Input{Attachments: []store.Attachment{{ID: "a1", ThreadID: "t1", Kind: store.AttachmentKindFile, Size: 9, Shell: store.AttachmentShell{Command: "ls", Preview: "one\n"}}}})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := FromItem(store.Item{Meta: encoded})
	if err != nil || decoded.Attachments[0].Shell == nil || decoded.Attachments[0].Shell.Command != "ls" {
		t.Fatalf("round trip: %+v %v", decoded.Attachments, err)
	}
}

// A row is a tick only when every message in it is a wake: a person's
// message joined to one, which only the recorded parts name, makes it not.
func TestIsTickNeedsEveryMemberToBeAWake(t *testing.T) {
	for _, tc := range []struct {
		name string
		meta Meta
		want bool
	}{
		{"a wake", Meta{SendID: "agent-wake:w1:1"}, true},
		{"a person's", Meta{}, false},
		{"joined wakes", Meta{JoinedSendIDs: []string{"agent-wake:w1:1", "agent-wake:w2:1"}}, true},
		{"a person joined to a wake", Meta{
			JoinedSendIDs: []string{"agent-wake:w1:1"},
			JoinedParts:   []agentmeta.JoinedPart{{Bytes: 5}, {SendID: "agent-wake:w1:1", Bytes: 5}},
		}, false},
		{"a request joined to a wake", Meta{JoinedSendIDs: []string{"agent-wake:w1:1", "thread-request:t"}}, false},
	} {
		if got := IsTick(tc.meta); got != tc.want {
			t.Errorf("%s: IsTick = %v, want %v", tc.name, got, tc.want)
		}
	}
	if AllTicks(nil) {
		t.Error("AllTicks(nil) = true, want false: no message is no tick")
	}
}

// A client receives a row's meta with each message's facts as
// agentmeta.Describe read them, a report's descriptor, and every other key
// as stored; a row that is the person's is sent as it is.
func TestProjectRowMetaKeepsWhatItDoesNotDescribe(t *testing.T) {
	message := built(t)(Report(agentmeta.ReportLoop, Reporter{Label: "l"}, "Loop fired.", "note: \"x\"", "x"))
	attribution := message.Attribution()
	stored, err := MarshalMeta(Meta{Origin: attribution.Origin, OriginWake: attribution.Wake, SendID: "agent-wake:1"})
	if err != nil {
		t.Fatal(err)
	}
	stored, err = MergeProviderIDs(stored, "provider-1", "parent-1")
	if err != nil {
		t.Fatal(err)
	}
	served := ProjectRowMeta(message.Text(), stored)
	decoded, err := FromItem(store.Item{Meta: served})
	if err != nil || ReadProviderItemID(served) != "provider-1" || ReadProviderParentUUID(served) != "parent-1" ||
		!reflect.DeepEqual(decoded.OriginWake.Facts, attribution.Wake.Facts) || decoded.OriginWake.Report == nil ||
		decoded.OriginWake.Report.Sender != "l" || decoded.SendID != "agent-wake:1" {
		t.Fatalf("a report whose facts hold served as %s", served)
	}
	if got := ProjectRowMeta("mine", `{"sendId":"s","provider_item_id":"p"}`); got != `{"sendId":"s","provider_item_id":"p"}` {
		t.Fatalf("the person's row changed: %s", got)
	}
	edited := ProjectRowMeta(message.Text()+"!", stored)
	decoded, err = FromItem(store.Item{Meta: edited})
	if err != nil || ReadProviderItemID(edited) != "provider-1" || ReadProviderParentUUID(edited) != "parent-1" ||
		decoded.OriginWake.Facts.Form != threadtools.FormWhole || decoded.SendID != "agent-wake:1" {
		t.Fatalf("projected %s", edited)
	}
	// main's request gets the facts its text holds.
	main := "Do it.\n\n---\nAgent request from thread \"L\" (t-l).\nIt is not waiting for an answer. If it should hear something back, call thread_reply with token a, once: the reply reaches it as a message, and it sees only your reply text."
	projected := ProjectRowMeta(main, `{"origin":"agent-thread","originThread":{"threadId":"t-l","title":"L","token":"a"},"sendId":"thread-request:a"}`)
	decoded, err = FromItem(store.Item{Meta: projected})
	if err != nil || decoded.OriginThread.Facts == nil || decoded.OriginThread.Facts.Form != threadtools.FormRequest || decoded.OriginThread.Facts.Headline != "Do it." {
		t.Fatalf("main's request projected %s", projected)
	}
}
