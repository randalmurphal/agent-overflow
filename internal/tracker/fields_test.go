package tracker

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestValueHoldsOneKind(t *testing.T) {
	d, _ := ParseDate("2026-11-16")
	at := time.Date(2026, 11, 16, 9, 30, 0, 0, time.UTC)
	user := Choice{ID: "acc-user1", Name: "User 1"}
	cases := []struct {
		v     Value
		kind  Kind
		texts []string
		nums  []float64
		dates []Date
		times []time.Time
		refs  []Choice
	}{
		{v: TextValue("a", "b"), kind: KindText, texts: []string{"a", "b"}},
		{v: RichTextValue("**x**"), kind: KindRichText, texts: []string{"**x**"}},
		{v: ReadOnlyValue("shown"), kind: KindReadOnly, texts: []string{"shown"}},
		{v: NumberValue(3), kind: KindNumber, nums: []float64{3}},
		{v: DateValue(d), kind: KindDate, dates: []Date{d}},
		{v: DateTimeValue(at), kind: KindDateTime, times: []time.Time{at}},
		{v: UserValue(user), kind: KindUser, refs: []Choice{user}},
		{v: OptionValue(Choice{ID: "10001", Name: "High"}), kind: KindOption, refs: []Choice{{ID: "10001", Name: "High"}}},
	}
	for _, c := range cases {
		if c.v.Kind() != c.kind || c.v.Empty() {
			t.Fatalf("%v: kind %v, empty %v", c.kind, c.v.Kind(), c.v.Empty())
		}
		if !reflect.DeepEqual(c.v.Texts(), c.texts) || !reflect.DeepEqual(c.v.Numbers(), c.nums) ||
			!reflect.DeepEqual(c.v.Dates(), c.dates) || !reflect.DeepEqual(c.v.Times(), c.times) ||
			!reflect.DeepEqual(c.v.Choices(), c.refs) {
			t.Fatalf("%v holds members of another kind: %#v", c.kind, c.v)
		}
	}
}

func TestValueWithNoValuesIsEmpty(t *testing.T) {
	for _, v := range []Value{TextValue(), NumberValue(), DateValue(), DateTimeValue(), UserValue(), OptionValue(), {}} {
		if !v.Empty() || v.Kind() != 0 {
			t.Fatalf("%#v is not the empty Value", v)
		}
	}
}

func TestValueIsTheCallersOwn(t *testing.T) {
	in := []string{"a"}
	v := TextValue(in...)
	in[0] = "changed"
	out := v.Texts()
	out[0] = "changed too"
	if got := v.Texts(); got[0] != "a" {
		t.Fatalf("Value shares its slices: %q", got)
	}
}

func TestValueJSONRoundTrips(t *testing.T) {
	d, _ := ParseDate("2026-11-16")
	at := time.Date(2026, 11, 16, 9, 30, 0, 0, time.FixedZone("", 3600))
	for _, v := range []Value{
		{}, TextValue("a", "b"), RichTextValue("# T"), ReadOnlyValue("x"), NumberValue(1.5, -2),
		DateValue(d), DateTimeValue(at), UserValue(Choice{ID: "acc-user1", Name: "User 1"}),
		OptionValue(Choice{ID: "1", Name: "One"}, Choice{ID: "2", Name: "Two"}),
	} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("Marshal %#v: %v", v, err)
		}
		var back Value
		if err := json.Unmarshal(b, &back); err != nil {
			t.Fatalf("Unmarshal %s: %v", b, err)
		}
		if back.Kind() != v.Kind() || !reflect.DeepEqual(back.Texts(), v.Texts()) ||
			!reflect.DeepEqual(back.Numbers(), v.Numbers()) || !reflect.DeepEqual(back.Dates(), v.Dates()) ||
			!reflect.DeepEqual(back.Choices(), v.Choices()) || len(back.Times()) != len(v.Times()) {
			t.Fatalf("%s read back as %#v", b, back)
		}
		for i, tm := range v.Times() {
			if !back.Times()[i].Equal(tm) {
				t.Fatalf("time %v read back as %v", tm, back.Times()[i])
			}
		}
	}
}

func TestValueJSONRefusesMixedOrUnknown(t *testing.T) {
	for _, s := range []string{
		`{"kind":"color","texts":["red"]}`,
		`{"kind":"text","numbers":[1]}`,
		`{"kind":"text","texts":["a"],"numbers":[1]}`,
		`{"kind":"rich_text","texts":["a","b"]}`,
		`{"kind":"read_only"}`,
		`{"kind":"number"}`,
		`{"texts":["no kind"]}`,
		`{"kind":"date","dates":["2026-02-30"]}`,
	} {
		var v Value
		if err := json.Unmarshal([]byte(s), &v); err == nil {
			t.Errorf("Unmarshal(%s) = %#v, want an error", s, v)
		}
	}
}

func TestEditForms(t *testing.T) {
	if e := ClearField(); e.Form() != EditClear || e.Values() != nil {
		t.Fatalf("ClearField = %#v", e)
	}
	if e := PickValues("1", "2"); e.Form() != EditPick || !reflect.DeepEqual(e.Values(), []string{"1", "2"}) {
		t.Fatalf("PickValues = %#v", e)
	}
	if e := TypedValues("High"); e.Form() != EditTyped || !reflect.DeepEqual(e.Values(), []string{"High"}) {
		t.Fatalf("TypedValues = %#v", e)
	}
	e := MarkdownEdit("new", "old")
	if md, from := e.Markdown(); e.Form() != EditMarkdown || md != "new" || from != "old" || e.Values() != nil {
		t.Fatalf("MarkdownEdit = %#v", e)
	}
	if (Edit{}).Form() != 0 {
		t.Fatal("the zero Edit has a form")
	}
}

func TestCheckEditsRefusesBadEdits(t *testing.T) {
	cases := map[string][]FieldEdit{
		"no field":    {{Value: ClearField()}},
		"no value":    {{Field: FieldTitle}},
		"field twice": {{Field: FieldLabels, Value: ClearField()}, {Field: FieldLabels, Value: TypedValues("x")}},
	}
	for name, edits := range cases {
		if err := checkEdits(edits); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%s: checkEdits = %v, want ErrInvalidInput", name, err)
		}
	}
	if err := checkEdits([]FieldEdit{{Field: FieldTitle, Value: TypedValues("T")}, {Field: "customfield_10001", Value: PickValues("1")}}); err != nil {
		t.Fatalf("good edits refused: %v", err)
	}
}
