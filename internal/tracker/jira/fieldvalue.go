package jira

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"agent-overflow/internal/tracker/adf"
)

// ErrNoValue is a field read as empty: null, or absent from the answer.
var ErrNoValue = errors.New("jira: the field has no value")

// FieldValue is one field's raw value, read through the reader for the
// shape the field's schema promises. A reader fails with ErrNoValue on
// null and with a decode error on any other shape than its own.
type FieldValue json.RawMessage

// Null reports a null or absent value.
func (v FieldValue) Null() bool { return isNull(json.RawMessage(v)) }

// JSON is the value as Jira sent it.
func (v FieldValue) JSON() json.RawMessage { return json.RawMessage(v) }

func (v FieldValue) decode(dst any) error {
	if v.Null() {
		return ErrNoValue
	}
	return json.Unmarshal(v, dst)
}

// Number reads a number field.
func (v FieldValue) Number() (float64, error) {
	var n float64
	err := v.decode(&n)
	return n, err
}

// Text reads a single-line text field. A rich text value is an ADF
// document; read it with RichText.
func (v FieldValue) Text() (string, error) {
	var s string
	err := v.decode(&s)
	return s, err
}

// Option is a select list choice; Child is the child choice of a
// cascading select.
type Option struct {
	ID    string
	Value string
	Child *Option
}

type optionJSON struct {
	ID    json.RawMessage `json:"id"`
	Value string          `json:"value"`
	Name  string          `json:"name"`
	Child *optionJSON     `json:"child"`
}

func (o optionJSON) option() Option {
	out := Option{ID: idText(o.ID), Value: firstNonEmpty(o.Value, o.Name)}
	if o.Child != nil {
		child := o.Child.option()
		out.Child = &child
	}
	return out
}

// Option reads a single select, radio or cascading select field.
func (v FieldValue) Option() (Option, error) {
	var o optionJSON
	if err := v.decode(&o); err != nil {
		return Option{}, err
	}
	return o.option(), nil
}

// Options reads a multi-select or checkbox field.
func (v FieldValue) Options() ([]Option, error) {
	var os []optionJSON
	if err := v.decode(&os); err != nil {
		return nil, err
	}
	out := make([]Option, len(os))
	for i, o := range os {
		out[i] = o.option()
	}
	return out, nil
}

// User reads a user picker field.
func (v FieldValue) User() (User, error) {
	var u userJSON
	if err := v.decode(&u); err != nil {
		return User{}, err
	}
	return u.user(), nil
}

// Users reads a multi-user picker field.
func (v FieldValue) Users() ([]User, error) {
	var us []userJSON
	if err := v.decode(&us); err != nil {
		return nil, err
	}
	return users(us), nil
}

// Date reads a date picker field.
func (v FieldValue) Date() (Date, error) {
	var d Date
	err := v.decode(&d)
	return d, err
}

// DateTime reads a date-time picker field.
func (v FieldValue) DateTime() (time.Time, error) {
	var s string
	if err := v.decode(&s); err != nil {
		return time.Time{}, err
	}
	return parseTime(s)
}

// Sprint is a sprint as an issue's sprint field names it. Dates are zero
// when the sprint has not reached them.
type Sprint struct {
	ID      int64
	Name    string
	State   string // future, active or closed
	BoardID int64
	Goal    string
	Start   time.Time
	End     time.Time
	Done    time.Time
}

// Sprints reads a sprint field: every sprint the issue was in.
func (v FieldValue) Sprints() ([]Sprint, error) {
	var ss []struct {
		ID       int64  `json:"id"`
		Name     string `json:"name"`
		State    string `json:"state"`
		BoardID  int64  `json:"boardId"`
		Goal     string `json:"goal"`
		Start    string `json:"startDate"`
		End      string `json:"endDate"`
		Complete string `json:"completeDate"`
	}
	if err := v.decode(&ss); err != nil {
		return nil, err
	}
	out := make([]Sprint, len(ss))
	for i, s := range ss {
		out[i] = Sprint{ID: s.ID, Name: s.Name, State: s.State, BoardID: s.BoardID, Goal: s.Goal}
		for _, t := range []struct {
			text string
			dst  *time.Time
		}{{s.Start, &out[i].Start}, {s.End, &out[i].End}, {s.Complete, &out[i].Done}} {
			if t.text == "" {
				continue
			}
			parsed, err := parseTime(t.text)
			if err != nil {
				return nil, fmt.Errorf("sprint %d: %w", s.ID, err)
			}
			*t.dst = parsed
		}
	}
	return out, nil
}

// RichText reads a multi-line text field: an ADF document.
func (v FieldValue) RichText() (*adf.Node, error) {
	if v.Null() {
		return nil, ErrNoValue
	}
	var doc *adf.Node
	if err := decodeDoc(json.RawMessage(v), &doc); err != nil {
		return nil, err
	}
	return doc, nil
}

// Date is a calendar day without a time or zone, as Jira's date fields
// hold it. The zero Date is no date.
type Date struct {
	Year  int
	Month time.Month
	Day   int
}

// dateLayout is the ISO 8601 day Jira reads and writes.
const dateLayout = "2006-01-02"

// ParseDate reads an ISO 8601 day, such as 2026-11-16.
func ParseDate(s string) (Date, error) {
	t, err := time.Parse(dateLayout, strings.TrimSpace(s))
	if err != nil {
		return Date{}, fmt.Errorf("jira: %q is not a date like 2026-11-16", s)
	}
	return Date{Year: t.Year(), Month: t.Month(), Day: t.Day()}, nil
}

// IsZero reports no date.
func (d Date) IsZero() bool { return d == Date{} }

// String is the ISO 8601 day, or "" for no date.
func (d Date) String() string {
	if d.IsZero() {
		return ""
	}
	return time.Date(d.Year, d.Month, d.Day, 0, 0, 0, 0, time.UTC).Format(dateLayout)
}

// UnmarshalJSON reads an ISO 8601 day or null.
func (d *Date) UnmarshalJSON(b []byte) error {
	if isNull(b) {
		*d = Date{}
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	if s == "" {
		*d = Date{}
		return nil
	}
	parsed, err := ParseDate(s)
	if err != nil {
		return err
	}
	*d = parsed
	return nil
}

// MarshalJSON writes the ISO 8601 day, or null for no date.
func (d Date) MarshalJSON() ([]byte, error) {
	if d.IsZero() {
		return []byte("null"), nil
	}
	return []byte(strconv.Quote(d.String())), nil
}

// jiraTimeLayout is how Jira writes a timestamp: milliseconds and an
// offset without a colon.
const jiraTimeLayout = "2006-01-02T15:04:05.000-0700"

// parseTime reads a Jira timestamp, or RFC 3339.
func parseTime(s string) (time.Time, error) {
	for _, layout := range []string{jiraTimeLayout, "2006-01-02T15:04:05-0700", time.RFC3339Nano} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("jira: %q is not a timestamp", s)
}
