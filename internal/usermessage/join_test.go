package usermessage

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"agent-overflow/internal/store"
)

// TestMetaTypedKeysCoverEveryField pins metaTypedKeys against the struct's own
// JSON tags. A field added to Meta without its key here would survive
// MergeJoinedMeta's delete pass and leave the folded row carrying one member's
// value beside the union of the rest.
func TestMetaTypedKeysCoverEveryField(t *testing.T) {
	var want []string
	metaType := reflect.TypeOf(Meta{})
	for i := 0; i < metaType.NumField(); i++ {
		tag := metaType.Field(i).Tag.Get("json")
		name, _, _ := strings.Cut(tag, ",")
		if name == "" || name == "-" {
			t.Fatalf("Meta.%s has no JSON name: tag %q", metaType.Field(i).Name, tag)
		}
		want = append(want, name)
	}
	got := slices.Clone(metaTypedKeys)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("metaTypedKeys:\n got %v\nwant %v", got, want)
	}
}

// TestMetaIsZeroCoversEveryField pins isZero the same way: a field it does not
// read would make a Meta carrying only that field encode to "" and vanish.
func TestMetaIsZeroCoversEveryField(t *testing.T) {
	metaType := reflect.TypeOf(Meta{})
	for i := 0; i < metaType.NumField(); i++ {
		field := metaType.Field(i)
		value := reflect.New(metaType).Elem()
		value.Field(i).Set(nonZeroValue(t, field.Type))
		meta, _ := value.Interface().(Meta)
		if meta.isZero() {
			t.Errorf("isZero() ignores Meta.%s, so a row carrying only that field would be dropped", field.Name)
		}
	}
}

func nonZeroValue(t *testing.T, typ reflect.Type) reflect.Value {
	t.Helper()
	switch typ.Kind() {
	case reflect.String:
		return reflect.ValueOf("x").Convert(typ)
	case reflect.Bool:
		return reflect.ValueOf(true).Convert(typ)
	case reflect.Pointer:
		return reflect.New(typ.Elem())
	case reflect.Slice:
		return reflect.Append(reflect.MakeSlice(typ, 0, 1), reflect.New(typ.Elem()).Elem())
	default:
		t.Fatalf("no non-zero sample for %s; extend this helper with the new kind", typ)
		return reflect.Value{}
	}
}

// TestMergeJoinedMetaKeepsWireCorrelation is the reason the fold rewrites the
// blob key by key instead of re-marshalling a decoded Meta: the survivor's
// provider ids are the only anchor a revert can slice at.
func TestMergeJoinedMetaKeepsWireCorrelation(t *testing.T) {
	existing := `{"sendId":"send-b","command":"workflow","provider_item_id":"uuid-b","provider_parent_uuid":"uuid-a","promoted_from_queue":true}`
	merged, err := MergeJoinedMeta(existing, Meta{
		SendID:        "send-a",
		JoinedSendIDs: []string{"send-a", "send-b"},
	})
	if err != nil {
		t.Fatalf("MergeJoinedMeta: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(merged), &fields); err != nil {
		t.Fatalf("decode merged meta: %v", err)
	}
	for key, want := range map[string]any{
		"provider_item_id":     "uuid-b",
		"provider_parent_uuid": "uuid-a",
		"promoted_from_queue":  true,
		"sendId":               "send-a",
	} {
		if fields[key] != want {
			t.Errorf("%s = %v, want %v", key, fields[key], want)
		}
	}
	// A typed key the join does not set is REPLACED, not kept: the folded
	// row's command marker must describe the joined text, not whichever
	// member happened to own the survivor row.
	if _, present := fields["command"]; present {
		t.Errorf("stale typed key survived the rewrite: %v", fields["command"])
	}
	ids, _ := fields["joinedSendIds"].([]any)
	if len(ids) != 2 || ids[0] != "send-a" || ids[1] != "send-b" {
		t.Errorf("joinedSendIds = %v, want [send-a send-b]", fields["joinedSendIds"])
	}
}

// TestJoinRenumberedTextRenumbersAcrossMembers covers the marker walk the two
// joiners share: each member numbers its images from 1, and the joined row
// indexes into the concatenated attachment list.
func TestJoinRenumberedTextRenumbersAcrossMembers(t *testing.T) {
	joined := JoinRenumberedText([]TextPart{
		{Text: "look at [Image #1] please", ImageCount: 1},
		{Text: "and [Image #2] next to [Image #1]", ImageCount: 2},
	})
	want := "look at [Image #1] please" + JoinSeparator + "and [Image #3] next to [Image #2]"
	if joined != want {
		t.Errorf("joined text:\n got %q\nwant %q", joined, want)
	}
}

func TestImageAttachmentCountTreatsEmptyKindAsImage(t *testing.T) {
	got := ImageAttachmentCount([]AttachmentMeta{
		{ID: "a"},
		{ID: "b", Kind: store.AttachmentKindImage},
		{ID: "c", Kind: store.AttachmentKindFile},
	})
	if got != 2 {
		t.Errorf("ImageAttachmentCount = %d, want 2 (empty kind is an image, a file is not)", got)
	}
}
