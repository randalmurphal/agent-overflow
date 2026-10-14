package usermessage

import (
	"encoding/json"
	"os"
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

// The lengths split a joined text back into its members exactly, a member
// that holds the separator itself and a marker renumbered past nine
// included; lengths that do not describe the text split nothing.
func TestJoinedPartsSplitTheJoinedTextExactly(t *testing.T) {
	answer := "Reply:\nfirst" + JoinSeparator + "second"
	joined, lengths := JoinRenumberedTextLengths([]TextPart{
		{Text: "nine images", ImageCount: 9},
		{Text: answer},
		{Text: "and [Image #1]", ImageCount: 1},
	})
	parts := []JoinedPart{{SendID: "a", Bytes: lengths[0]}, {SendID: "thread-wake:t", Bytes: lengths[1]}, {Bytes: lengths[2]}}
	members, ok := SplitJoinedParts(joined, parts)
	nine := JoinRenumberedText([]TextPart{{Text: "nine images", ImageCount: 9}})
	if want := []string{nine, answer, "and [Image #10]"}; !ok || !slices.Equal(members, want) {
		t.Fatalf("split = %q (ok %v), want %q", members, ok, want)
	}
	if joined != JoinRenumberedText([]TextPart{{Text: "nine images", ImageCount: 9}, {Text: answer}, {Text: "and [Image #1]", ImageCount: 1}}) {
		t.Fatal("the lengths changed the joined text")
	}
	for _, bad := range [][]JoinedPart{
		nil,
		{{Bytes: len(joined)}, {Bytes: 0}},
		{{Bytes: lengths[0] + 1}, {Bytes: lengths[1]}, {Bytes: lengths[2]}},
		{{Bytes: lengths[0]}, {Bytes: lengths[1]}},
	} {
		if members, ok := SplitJoinedParts(joined, bad); ok {
			t.Errorf("parts %+v split the text into %q", bad, members)
		}
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

// The frontend's personTextOfUserMessage reads the same fixture, so the
// un-send's composer text agrees on both sides of the Stop.
func TestSplitPersonPartsFixture(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("testdata/person_parts.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name   string  `json:"name"`
		Text   string  `json:"text"`
		Meta   string  `json:"meta"`
		Person *string `json:"person"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		meta, err := FromItem(store.Item{Meta: tc.Meta})
		if err != nil {
			t.Fatalf("%s: meta: %v", tc.Name, err)
		}
		person, agents, ok := SplitPersonParts(tc.Text, meta)
		if (tc.Person != nil) != ok {
			t.Errorf("%s: ok = %v, want %v", tc.Name, ok, tc.Person != nil)
			continue
		}
		if !ok {
			continue
		}
		if person != *tc.Person {
			t.Errorf("%s: person = %q, want %q", tc.Name, person, *tc.Person)
		}
		// What is not the person's is the agents', whole.
		rebuilt := 0
		for _, agent := range agents {
			if !agent.IsAgent() || len(agent.Text) != agent.Bytes {
				t.Errorf("%s: agent part %+v", tc.Name, agent)
			}
			rebuilt += agent.Bytes
		}
		if len(meta.JoinedParts) > 0 && rebuilt+len(person) > len(tc.Text) {
			t.Errorf("%s: parts overlap", tc.Name)
		}
	}
}
