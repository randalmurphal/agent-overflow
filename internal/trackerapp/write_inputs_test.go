package trackerapp

import (
	"errors"
	"math"
	"slices"
	"testing"

	"agent-overflow/internal/tracker"
)

// A value as a ticket shows one becomes the edit that sets it: no kind
// clears, a choice is picked by ID, any other kind is typed in the form
// the tracker reads.
func TestAFieldValueBecomesItsEdit(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		value  FieldValueView
		form   tracker.EditForm
		values []string
	}{
		{"no kind", FieldValueView{}, tracker.EditClear, nil},
		{"text", FieldValueView{Kind: "text", Texts: []string{"a", "b"}}, tracker.EditTyped, []string{"a", "b"}},
		{"number", FieldValueView{Kind: "number", Numbers: []float64{2.5, 1e21}}, tracker.EditTyped, []string{"2.5", "1000000000000000000000"}},
		{"date", FieldValueView{Kind: "date", Dates: []string{"2026-11-02"}}, tracker.EditTyped, []string{"2026-11-02"}},
		{"date and time", FieldValueView{Kind: "date_time", Times: []int64{1_800_000_000_123}}, tracker.EditTyped, []string{"2027-01-15T08:00:00.123Z"}},
		{"user", FieldValueView{Kind: "user", Choices: []ChoiceView{{ID: "acc-user1", Name: "User 1"}}}, tracker.EditPick, []string{"acc-user1"}},
		{"options", FieldValueView{Kind: "option", Choices: []ChoiceView{{ID: "1"}, {ID: "2"}}}, tracker.EditPick, []string{"1", "2"}},
	}
	for _, c := range cases {
		e, err := FieldEditInput{Field: "f", Value: &c.value}.fieldEdit()
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if e.Field != "f" || e.Value.Form() != c.form || !slices.Equal(e.Value.Values(), c.values) {
			t.Errorf("%s: %v %q", c.name, e.Value.Form(), e.Value.Values())
		}
	}
	e, err := FieldEditInput{Field: "d", Markdown: &MarkdownEditInput{Markdown: "new", From: "old"}}.fieldEdit()
	if md, from := e.Value.Markdown(); err != nil || e.Value.Form() != tracker.EditMarkdown || md != "new" || from != "old" {
		t.Errorf("markdown: %v %q %q", err, md, from)
	}
}

// What a kind cannot hold, or the model cannot express, is refused before
// the connection is held or anything is sent.
func TestWriteInputRefusals(t *testing.T) {
	t.Parallel()
	value := func(v FieldValueView) []FieldEditInput { return []FieldEditInput{{Field: "f", Value: &v}} }
	cases := []struct {
		name   string
		fields []FieldEditInput
	}{
		{"no field", []FieldEditInput{{Typed: []string{"a"}}}},
		{"nothing set", []FieldEditInput{{Field: "f"}}},
		{"two set", []FieldEditInput{{Field: "f", Typed: []string{"a"}, Markdown: &MarkdownEditInput{Markdown: "a"}}}},
		{"no kind with values", value(FieldValueView{Texts: []string{"a"}})},
		{"text with no values", value(FieldValueView{Kind: "text"})},
		{"text holding a number", value(FieldValueView{Kind: "text", Texts: []string{"a"}, Numbers: []float64{1}})},
		{"number holding text", value(FieldValueView{Kind: "number", Texts: []string{"1"}})},
		{"no number", value(FieldValueView{Kind: "number", Numbers: []float64{math.NaN()}})},
		{"infinite number", value(FieldValueView{Kind: "number", Numbers: []float64{math.Inf(1)}})},
		{"not a date", value(FieldValueView{Kind: "date", Dates: []string{"2026-13-01"}})},
		{"date holding a time", value(FieldValueView{Kind: "date", Times: []int64{1}})},
		{"choice with no ID", value(FieldValueView{Kind: "option", Choices: []ChoiceView{{Name: "High"}}})},
		{"user holding text", value(FieldValueView{Kind: "user", Choices: []ChoiceView{{ID: "acc-user1"}}, Texts: []string{"User 1"}})},
		{"rich text", value(FieldValueView{Kind: "rich_text", Texts: []string{"a"}})},
		{"read only", value(FieldValueView{Kind: "read_only", Texts: []string{"a"}})},
		{"unknown kind", value(FieldValueView{Kind: "color", Texts: []string{"red"}})},
	}
	f := newWriteFixture(t)
	before := len(f.p.callsInOrder())
	for _, c := range cases {
		if _, err := f.svc.Edit(t.Context(), f.conn.ID, EditRequest{Issue: "10001", Fields: c.fields}); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("edit with %s: %v", c.name, err)
		}
	}
	day := "2026-11-02"
	spent := int64(math.MaxInt64)
	others := map[string]func() error{
		"a create from no board": func() error {
			_, err := f.svc.Create(t.Context(), f.conn.ID, CreateRequest{Project: "10000", Title: "New"})
			return err
		},
		"an unknown audience": func() error {
			_, err := f.svc.AddComment(t.Context(), f.conn.ID, CommentRequest{Issue: "10001", Markdown: "a", Audience: "everyone"})
			return err
		},
		"a worklog of no day": func() error {
			_, err := f.svc.AddWorklog(t.Context(), f.conn.ID, WorklogRequest{Issue: "10001", Day: "2 Nov", SpentSeconds: 60})
			return err
		},
		"a worklog of more seconds than a duration holds": func() error {
			_, err := f.svc.AddWorklog(t.Context(), f.conn.ID, WorklogRequest{Issue: "10001", Day: day, SpentSeconds: spent})
			return err
		},
		"a worklog edit to no day": func() error {
			bad := "tomorrow"
			_, err := f.svc.EditWorklog(t.Context(), f.conn.ID, WorklogEditRequest{From: WorklogView{ID: "w1", Issue: "10001", Day: day}, Day: &bad})
			return err
		},
		"a worklog edit of one read with no day": func() error {
			_, err := f.svc.EditWorklog(t.Context(), f.conn.ID, WorklogEditRequest{From: WorklogView{ID: "w1", Issue: "10001"}})
			return err
		},
		"a worklog edit to too many seconds": func() error {
			_, err := f.svc.EditWorklog(t.Context(), f.conn.ID, WorklogEditRequest{From: WorklogView{ID: "w1", Issue: "10001", Day: day}, SpentSeconds: &spent})
			return err
		},
		"a worklog delete of one read with no day": func() error {
			_, err := f.svc.DeleteWorklog(t.Context(), f.conn.ID, WorklogDeleteRequest{From: WorklogView{ID: "w1", Issue: "10001", Day: "x"}})
			return err
		},
		"a write on no connection": func() error {
			_, err := f.svc.Rank(t.Context(), "", RankRequest{Board: "1", Issues: []string{"10001"}, Next: "10002"})
			return err
		},
	}
	for name, do := range others {
		if err := do(); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if got := f.callsSince(before); len(got) > 0 {
		t.Fatalf("refused inputs called the tracker: %v", got)
	}
}
