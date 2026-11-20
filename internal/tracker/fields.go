package tracker

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"
)

// The fields of a card and the description, named by these IDs on every
// tracker. A provider reports and edits these by these IDs and gives no
// other field one of them; every other field keeps the tracker's own ID.
// Status, sprint and estimate are not fields: Move, MoveToSprint and
// SetEstimate change them.
const (
	FieldTitle       FieldID = "title"
	FieldType        FieldID = "type"
	FieldAssignee    FieldID = "assignee"
	FieldPriority    FieldID = "priority"
	FieldLabels      FieldID = "labels"
	FieldParent      FieldID = "parent"
	FieldDescription FieldID = "description"
)

// Kind is the type of a field's values.
type Kind uint8

const (
	KindText Kind = iota + 1
	// KindRichText is Markdown.
	KindRichText
	KindNumber
	KindDate
	KindDateTime
	KindUser
	KindOption
	// KindReadOnly is a value the app shows as text and cannot edit.
	KindReadOnly
)

// kindNames are the kinds' names by kind: an array, so linking the
// package builds nothing at start.
var kindNames = [...]string{
	KindText: "text", KindRichText: "rich_text", KindNumber: "number", KindDate: "date",
	KindDateTime: "date_time", KindUser: "user", KindOption: "option", KindReadOnly: "read_only",
}

// String is the kind's name, as Value's JSON writes it.
func (k Kind) String() string {
	if int(k) < len(kindNames) && kindNames[k] != "" {
		return kindNames[k]
	}
	return fmt.Sprintf("Kind(%d)", uint8(k))
}

// Field is one field of an issue with its metadata and value. List
// reports a field that holds several values of its kind. Editable is
// false for a field the screen offers for reading only; a KindReadOnly
// field is never editable.
type Field struct {
	ID       FieldID
	Name     string
	Kind     Kind
	List     bool
	Required bool
	Editable bool
	Value    Value
}

// Value is a field's value as read: empty, or one or more values of one
// kind. Its constructors are the only way to make one, so a Value never
// mixes kinds. The zero Value is empty.
type Value struct {
	kind    Kind
	texts   []string
	numbers []float64
	dates   []Date
	times   []time.Time
	choices []Choice
}

// The constructors below copy their arguments; given no values they
// return the empty Value.

// TextValue is plain text, one value per string.
func TextValue(s ...string) Value {
	if len(s) == 0 {
		return Value{}
	}
	return Value{kind: KindText, texts: slices.Clone(s)}
}

// RichTextValue is one rich text value as Markdown.
func RichTextValue(markdown string) Value {
	return Value{kind: KindRichText, texts: []string{markdown}}
}

// NumberValue is one or more numbers.
func NumberValue(n ...float64) Value {
	if len(n) == 0 {
		return Value{}
	}
	return Value{kind: KindNumber, numbers: slices.Clone(n)}
}

// DateValue is one or more calendar days.
func DateValue(d ...Date) Value {
	if len(d) == 0 {
		return Value{}
	}
	return Value{kind: KindDate, dates: slices.Clone(d)}
}

// DateTimeValue is one or more instants.
func DateTimeValue(t ...time.Time) Value {
	if len(t) == 0 {
		return Value{}
	}
	return Value{kind: KindDateTime, times: slices.Clone(t)}
}

// UserValue is one or more users by ID with their display names.
func UserValue(u ...Choice) Value {
	if len(u) == 0 {
		return Value{}
	}
	return Value{kind: KindUser, choices: slices.Clone(u)}
}

// OptionValue is one or more options by ID with their names.
func OptionValue(o ...Choice) Value {
	if len(o) == 0 {
		return Value{}
	}
	return Value{kind: KindOption, choices: slices.Clone(o)}
}

// ReadOnlyValue is a value the app shows as text and cannot edit.
func ReadOnlyValue(display string) Value {
	return Value{kind: KindReadOnly, texts: []string{display}}
}

// Kind is the kind of the value; zero for the empty Value.
func (v Value) Kind() Kind { return v.kind }

// Empty reports no value.
func (v Value) Empty() bool { return v.kind == 0 }

// Texts are the values of a text, rich text or read-only Value; nil for
// another kind. The slice is the caller's own.
func (v Value) Texts() []string { return slices.Clone(v.texts) }

// Numbers are the values of a number Value; nil for another kind.
func (v Value) Numbers() []float64 { return slices.Clone(v.numbers) }

// Dates are the values of a date Value; nil for another kind.
func (v Value) Dates() []Date { return slices.Clone(v.dates) }

// Times are the values of a date-time Value; nil for another kind.
func (v Value) Times() []time.Time { return slices.Clone(v.times) }

// Choices are the values of a user or option Value; nil for another kind.
func (v Value) Choices() []Choice { return slices.Clone(v.choices) }

type valueJSON struct {
	Kind    string      `json:"kind,omitempty"`
	Texts   []string    `json:"texts,omitempty"`
	Numbers []float64   `json:"numbers,omitempty"`
	Dates   []Date      `json:"dates,omitempty"`
	Times   []time.Time `json:"times,omitempty"`
	Choices []Choice    `json:"choices,omitempty"`
}

// MarshalJSON writes the kind's name and its values; {} for the empty
// Value.
func (v Value) MarshalJSON() ([]byte, error) {
	j := valueJSON{Texts: v.texts, Numbers: v.numbers, Dates: v.dates, Times: v.times, Choices: v.choices}
	if !v.Empty() {
		j.Kind = v.kind.String()
	}
	return json.Marshal(j)
}

// UnmarshalJSON reads what MarshalJSON writes. An unknown kind, values of
// another kind than the one named, or a kind with no values is an error.
func (v *Value) UnmarshalJSON(b []byte) error {
	var j valueJSON
	if err := json.Unmarshal(b, &j); err != nil {
		return fmt.Errorf("tracker: field value: %w", err)
	}
	var out Value
	switch j.Kind {
	case "":
	case KindText.String():
		out = TextValue(j.Texts...)
	case KindRichText.String():
		if len(j.Texts) == 1 {
			out = RichTextValue(j.Texts[0])
		}
	case KindReadOnly.String():
		if len(j.Texts) == 1 {
			out = ReadOnlyValue(j.Texts[0])
		}
	case KindNumber.String():
		out = NumberValue(j.Numbers...)
	case KindDate.String():
		out = DateValue(j.Dates...)
	case KindDateTime.String():
		out = DateTimeValue(j.Times...)
	case KindUser.String():
		out = UserValue(j.Choices...)
	case KindOption.String():
		out = OptionValue(j.Choices...)
	default:
		return fmt.Errorf("tracker: field value: unknown kind %q", j.Kind)
	}
	n := len(j.Texts) + len(j.Numbers) + len(j.Dates) + len(j.Times) + len(j.Choices)
	if n != out.count() || (j.Kind != "" && n == 0) {
		return fmt.Errorf("tracker: field value: %q does not hold one or more values of its own kind", j.Kind)
	}
	*v = out
	return nil
}

func (v Value) count() int {
	return len(v.texts) + len(v.numbers) + len(v.dates) + len(v.times) + len(v.choices)
}

// EditForm is how an Edit gives a field's new value.
type EditForm uint8

const (
	// EditClear empties the field.
	EditClear EditForm = iota + 1
	// EditPick names options or users by ID, as picked from a list.
	EditPick
	// EditTyped gives text a person or an agent typed, which the provider
	// resolves by the field's kind: an option or user by name, a number,
	// an ISO 8601 date, an RFC 3339 date-time, plain text. Text it cannot
	// match is refused with ReasonNoMatch or ReasonAmbiguous.
	EditTyped
	// EditMarkdown is a rich text field's new Markdown and the Markdown it
	// was written from.
	EditMarkdown
)

// Edit is a field's new value. Its constructors are the only way to make
// one; the zero Edit is no edit and is refused.
type Edit struct {
	form     EditForm
	values   []string
	markdown string
	from     string
}

// ClearField empties the field: no value, an empty list, no parent.
func ClearField() Edit { return Edit{form: EditClear} }

// PickValues names one or more options or users by ID. For a list field
// the IDs replace the list.
func PickValues(id string, more ...string) Edit {
	return Edit{form: EditPick, values: append([]string{id}, more...)}
}

// TypedValues is text a person or an agent typed, one string per value.
// For a list field the values replace the list.
func TypedValues(text string, more ...string) Edit {
	return Edit{form: EditTyped, values: append([]string{text}, more...)}
}

// MarkdownEdit is a rich text field's new Markdown and from, the Markdown
// the field held when the edit was written ("" for an empty field or a
// create). A provider refuses the edit with ReasonStale when the field
// holds other text by the time it writes, and with ReasonLoss when the
// edit would lose content Markdown cannot show and the write did not
// accept that.
func MarkdownEdit(markdown, from string) Edit {
	return Edit{form: EditMarkdown, markdown: markdown, from: from}
}

// Form is how e gives the value; zero for the zero Edit.
func (e Edit) Form() EditForm { return e.form }

// Values are the IDs of EditPick or the texts of EditTyped; nil for
// another form. The slice is the caller's own.
func (e Edit) Values() []string { return slices.Clone(e.values) }

// Markdown is the new Markdown of EditMarkdown and the Markdown it was
// written from.
func (e Edit) Markdown() (markdown, from string) { return e.markdown, e.from }

// FieldEdit sets one field.
type FieldEdit struct {
	Field FieldID
	Value Edit
}

// checkEdits refuses edits that name no field, give no value, or name one
// field twice.
func checkEdits(edits []FieldEdit) error {
	seen := make(map[FieldID]bool, len(edits))
	for _, e := range edits {
		if e.Field == "" {
			return invalidf("an edit names no field")
		}
		if e.Value.form == 0 {
			return invalidf("the edit of %s gives no value", e.Field)
		}
		if seen[e.Field] {
			return invalidf("%s is edited twice", e.Field)
		}
		seen[e.Field] = true
	}
	return nil
}
