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

// Answers that queued together become one row: attribution is the first
// member's, and the row's parts keep every member's request token so each
// request finds the row that settled it.
func TestOriginTokensNamesEveryJoinedRequest(t *testing.T) {
	t.Parallel()
	answer := func(id, token string) Meta {
		return Meta{SendID: id, Origin: OriginAgentThread, OriginThread: &OriginThread{ThreadID: "t-" + token, Token: token}}
	}
	join := func(members []Meta, texts []string) Meta {
		lengths := make([]int, len(texts))
		for i, text := range texts {
			lengths[i] = len(text)
		}
		joined := JoinMetas(members)
		joined.JoinedParts = PartsOf(members, texts, lengths)
		return joined
	}
	inner := join([]Meta{answer("d1", "tok-3"), answer("d2", "tok-1")}, []string{"three", "one again"})
	joined := join([]Meta{answer("a", "tok-1"), {SendID: "b"}, answer("c", "tok-2"), inner},
		[]string{"one", "mine", "two", "three" + JoinSeparator + "one again"})
	if joined.OriginThread == nil || joined.OriginThread.Token != "tok-1" {
		t.Fatalf("attribution = %+v, want the first member's", joined.OriginThread)
	}
	if got, want := originTokens(joined), []string{"tok-1", "tok-2", "tok-3"}; !slices.Equal(got, want) {
		t.Errorf("OriginTokens = %v, want %v", got, want)
	}
	single := join([]Meta{answer("a", "tok-1"), {SendID: "b"}}, []string{"one", "mine"})
	if got := originTokens(single); !slices.Equal(got, []string{"tok-1"}) {
		t.Errorf("one answer and the person's: OriginTokens = %v, want [tok-1]", got)
	}
	if got := originTokens(answer("a", "tok-1")); !slices.Equal(got, []string{"tok-1"}) {
		t.Errorf("an unjoined answer: OriginTokens = %v, want [tok-1]", got)
	}
	// A wake first takes the attribution; the answer behind it keeps its token.
	behindWake := join([]Meta{
		{SendID: "a", Origin: OriginAgentWake, OriginWake: &OriginWake{Kind: "loop", Label: "board"}},
		answer("b", "tok-1"),
	}, []string{"tick", "one"})
	if behindWake.OriginThread != nil || !slices.Equal(originTokens(behindWake), []string{"tok-1"}) {
		t.Errorf("behind a wake: OriginThread %+v, OriginTokens %v, want nil and [tok-1]", behindWake.OriginThread, originTokens(behindWake))
	}
	if got := originTokens(Meta{Origin: OriginAgentWake, OriginThread: &OriginThread{Token: "tok-9"}}); got != nil {
		t.Errorf("a wake's own row: OriginTokens = %v, want none", got)
	}
}

// originTokens is every request token a row carries, as the frontend's
// userMessageOriginTokens reads them: each joined member's
// an agent thread wrote, in queue order and once, whatever the row's
// attribution (a wake may have joined first), else an `agent-thread` row's
// own. It is how each request finds the row that answered it.
func originTokens(meta Meta) []string {
	var tokens []string
	for _, part := range meta.JoinedParts {
		if part.OriginThread != nil && part.OriginThread.Token != "" && !slices.Contains(tokens, part.OriginThread.Token) {
			tokens = append(tokens, part.OriginThread.Token)
		}
	}
	if len(meta.JoinedParts) == 0 {
		tokens = meta.JoinedOriginTokens
	}
	if len(tokens) == 0 && meta.Origin == OriginAgentThread && meta.OriginThread != nil && meta.OriginThread.Token != "" {
		tokens = []string{meta.OriginThread.Token}
	}
	return tokens
}

// A nested join placed after a member with nine images: its marker #1
// becomes #10, its length changes, and its parts are unknown. The row records
// no parts, is not split, and still names every request it answers.
func TestARowWithoutPartsKeepsEveryRequestToken(t *testing.T) {
	t.Parallel()
	request := func(token string) Meta {
		return Meta{SendID: "thread-request:" + token, Origin: OriginAgentThread, OriginThread: &OriginThread{ThreadID: "t-lead", Token: token}}
	}
	join := joinMembers
	inner, innerMeta := join([]Meta{{SendID: "s-a", ExpandComposerCommands: true}, request("tok-1")}, []string{"mine [Image #1]", "Fix it."}, []int{1, 0})
	if innerMeta.JoinedParts == nil || innerMeta.JoinedOriginTokens != nil {
		t.Fatalf("a measurable join: parts %+v, tokens %v; want parts alone", innerMeta.JoinedParts, innerMeta.JoinedOriginTokens)
	}
	text, meta := join([]Meta{{SendID: "s-0", ExpandComposerCommands: true}, innerMeta, request("tok-2")}, []string{"nine", inner, "And this."}, []int{9, 1, 0})
	if meta.JoinedParts != nil {
		t.Fatalf("parts recorded for a renumbered nested join: %+v", meta.JoinedParts)
	}
	if _, _, ok := SplitPersonParts(text, meta); ok {
		t.Fatal("split a join whose parts are unknown")
	}
	if want := []string{"tok-1", "tok-2"}; !slices.Equal(meta.JoinedOriginTokens, want) {
		t.Fatalf("JoinedOriginTokens = %v, want %v", meta.JoinedOriginTokens, want)
	}
	// Joined again, its list carries through.
	_, again := join([]Meta{{SendID: "s-9", ExpandComposerCommands: true}, meta}, []string{"[Image #1]", text}, []int{1, 10})
	if again.JoinedParts != nil || !slices.Equal(again.JoinedOriginTokens, []string{"tok-1", "tok-2"}) {
		t.Fatalf("joined again: parts %+v, tokens %v; want no parts and both tokens", again.JoinedParts, again.JoinedOriginTokens)
	}
}

// joinMembers joins members the way a dispatch does: image markers
// renumbered across the members, and the members recorded on the result.
func joinMembers(members []Meta, texts []string, images []int) (string, Meta) {
	parts := make([]TextPart, len(texts))
	for i, text := range texts {
		parts[i] = TextPart{Text: text, ImageCount: images[i]}
	}
	text, lengths := JoinRenumberedTextLengths(parts)
	joined := JoinMetas(members)
	joined.RecordMembers(members, texts, lengths)
	return text, joined
}

func leadRequest(token string) Meta {
	return Meta{SendID: "thread-request:" + token, Origin: OriginAgentThread, OriginThread: &OriginThread{ThreadID: "t-lead", Title: "Lead", Kind: "send", Token: token}}
}

const leadRequestText = "Fix the nil check.\n\n---\nAgent request from thread \"Lead\" (t-lead).\nIt is not waiting."

// The person's two messages around a request split back out with their
// image markers renumbered as they were sent, and the request keeps its token.
func TestSplitPersonPartsAroundARequest(t *testing.T) {
	t.Parallel()
	text, meta := joinMembers(
		[]Meta{{SendID: "s-a", ExpandComposerCommands: true}, leadRequest("tok-1"), {SendID: "s-b", ExpandComposerCommands: true}},
		[]string{"first [Image #1] mine", leadRequestText, "second [Image #1]"}, []int{1, 0, 1})
	person, agents, ok := SplitPersonParts(text, meta)
	if want := "first [Image #1] mine" + JoinSeparator + "second [Image #2]"; !ok || person != want {
		t.Fatalf("ok %v, person %q; want %q", ok, person, want)
	}
	if len(agents) != 1 || agents[0].Text != leadRequestText || agents[0].OriginThread == nil || agents[0].OriginThread.Token != "tok-1" {
		t.Fatalf("agents = %+v, want the request whole with its token", agents)
	}
}

// A join of a join whose markers did not move keeps every part, so the
// person's text from both levels splits out and both requests go back.
func TestSplitPersonPartsOfANestedJoin(t *testing.T) {
	t.Parallel()
	inner, innerMeta := joinMembers([]Meta{{SendID: "s-a", ExpandComposerCommands: true}, leadRequest("tok-1")}, []string{"mine first", leadRequestText}, []int{0, 0})
	text, meta := joinMembers([]Meta{innerMeta, {SendID: "s-b", ExpandComposerCommands: true}, leadRequest("tok-2")}, []string{inner, "mine second", leadRequestText}, []int{0, 0, 0})
	person, agents, ok := SplitPersonParts(text, meta)
	if want := "mine first" + JoinSeparator + "mine second"; !ok || person != want || len(agents) != 2 {
		t.Fatalf("ok %v, person %q, %d agent parts; want %q and 2", ok, person, len(agents), want)
	}
}

// Text that drifted from what the dispatch measured is not split: a cut by
// stale lengths would hand the agent's words to the person.
func TestSplitPersonPartsDeclinesDriftedText(t *testing.T) {
	t.Parallel()
	text, meta := joinMembers(
		[]Meta{{SendID: "s-a", ExpandComposerCommands: true}, leadRequest("tok-1"), {SendID: "s-b", ExpandComposerCommands: true}},
		[]string{"mine [Image #1]  ", leadRequestText, "second\r\nline [Image #1]"}, []int{1, 0, 1})
	if _, _, ok := SplitPersonParts(text, meta); !ok {
		t.Fatal("the text as dispatched did not split")
	}
	for name, drifted := range map[string]string{
		"trailing whitespace trimmed": strings.Replace(text, "]  \n", "]\n", 1),
		"line endings normalized":     strings.ReplaceAll(text, "\r\n", "\n"),
		"marker renumbered":           strings.Replace(text, "[Image #2]", "[Image #12]", 1),
		"separator respaced":          strings.Replace(text, JoinSeparator, "\n---\n", 1),
	} {
		if drifted == text {
			t.Fatalf("%s: the text did not change", name)
		}
		if person, _, ok := SplitPersonParts(drifted, meta); ok {
			t.Errorf("%s: split anyway, person %q", name, person)
		}
	}
}
