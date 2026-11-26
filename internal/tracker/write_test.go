package tracker

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

type checker interface{ Check() error }

func ptr[T any](v T) *T { return &v }

// TestInputChecks runs every input's Check on one input it accepts and
// on inputs that break each rule it states.
func TestInputChecks(t *testing.T) {
	start := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	end := start.Add(14 * 24 * time.Hour)
	day := mustDate(t, "2026-11-16")
	title := []FieldEdit{{Field: FieldPriority, Value: TypedValues("High")}}
	prev := IssueID("p")
	wl := Worklog{ID: "w1", Issue: "i1", Author: "acc-user1", Day: day, Spent: time.Hour}
	cases := []struct {
		good checker
		bad  map[string]checker
	}{
		{CreateInput{Project: "p", Title: "T", Fields: title}, map[string]checker{
			"no project":     CreateInput{Title: "T"},
			"no title":       CreateInput{Project: "p"},
			"title as field": CreateInput{Project: "p", Title: "T", Fields: []FieldEdit{{Field: FieldTitle, Value: TypedValues("T")}}},
			"rich text base": CreateInput{Project: "p", Title: "T", Fields: []FieldEdit{{Field: FieldDescription, Value: MarkdownEdit("x", "old")}}},
			"field twice":    CreateInput{Project: "p", Title: "T", Fields: append(title, title...)},
			"sprint alone":   CreateInput{Project: "p", Title: "T", Sprint: "s"},
			"after alone":    CreateInput{Project: "p", Title: "T", After: RowPlace{Issue: "i", Order: "o"}},
			"after no order": CreateInput{Project: "p", Title: "T", Board: "b", After: RowPlace{Issue: "i"}},
			"after no issue": CreateInput{Project: "p", Title: "T", Board: "b", After: RowPlace{Order: "o"}},
		}},
		{CreateInput{Project: "p", Title: "T", Board: "b", Sprint: "s"}, nil},
		{CreateInput{Project: "p", Title: "T", Board: "b", After: RowPlace{Issue: "i", Order: "o"}}, nil},
		{EditInput{Issue: "i", Fields: title}, map[string]checker{
			"no issue":             EditInput{Fields: title},
			"no fields":            EditInput{Issue: "i"},
			"no value":             EditInput{Issue: "i", Fields: []FieldEdit{{Field: FieldTitle}}},
			"replaces on Markdown": EditInput{Issue: "i", Fields: []FieldEdit{{Field: FieldDescription, Value: MarkdownEdit("x", "old").Replacing("old")}}},
			"replaces on picks":    EditInput{Issue: "i", Fields: []FieldEdit{{Field: FieldPriority, Value: PickValues("1").Replacing("2")}}},
		}},
		{MoveInput{Issue: "i", Move: "31"}, map[string]checker{
			"no issue":    MoveInput{Move: "31"},
			"no move":     MoveInput{Issue: "i"},
			"field twice": MoveInput{Issue: "i", Move: "31", Fields: append(title, title...)},
		}},
		{EstimateInput{Board: "b", Issue: "i", Value: ptr(3.0)}, map[string]checker{
			"no board": EstimateInput{Issue: "i"},
			"no issue": EstimateInput{Board: "b"},
			"negative": EstimateInput{Board: "b", Issue: "i", Value: ptr(-1.0)},
			"infinite": EstimateInput{Board: "b", Issue: "i", Value: ptr(math.Inf(1))},
			"NaN":      EstimateInput{Board: "b", Issue: "i", Value: ptr(math.NaN())},
		}},
		{RankInput{Board: "b", Issues: []IssueID{"1", "2"}, Prev: prev}, map[string]checker{
			"no board":         RankInput{Issues: []IssueID{"1"}, Prev: prev},
			"no issues":        RankInput{Board: "b", Prev: prev},
			"empty issue":      RankInput{Board: "b", Issues: []IssueID{""}, Prev: prev},
			"issue twice":      RankInput{Board: "b", Issues: []IssueID{"1", "1"}, Prev: prev},
			"no neighbor":      RankInput{Board: "b", Issues: []IssueID{"1"}},
			"same neighbor":    RankInput{Board: "b", Issues: []IssueID{"1"}, Prev: prev, Next: prev},
			"next to itself":   RankInput{Board: "b", Issues: []IssueID{"1", "p"}, Prev: prev},
			"next is a ranked": RankInput{Board: "b", Issues: []IssueID{"1"}, Prev: prev, Next: "1"},
		}},
		{SprintMoveInput{Board: "b", Issues: []IssueID{"1"}}, map[string]checker{
			"no board":    SprintMoveInput{Issues: []IssueID{"1"}},
			"no issues":   SprintMoveInput{Board: "b", Sprint: "s"},
			"issue twice": SprintMoveInput{Board: "b", Issues: []IssueID{"1", "1"}},
		}},
		{SprintMoveInput{Board: "b", Sprint: "s", Issues: []IssueID{"1"}, Next: "n"}, map[string]checker{
			"placed, issue twice":    SprintMoveInput{Board: "b", Issues: []IssueID{"1", "1"}, Prev: prev},
			"placed, same neighbor":  SprintMoveInput{Board: "b", Issues: []IssueID{"1"}, Prev: prev, Next: prev},
			"placed, next to itself": SprintMoveInput{Board: "b", Issues: []IssueID{"1"}, Next: "1"},
		}},
		{SprintInput{Board: "b", Name: "S", Start: start, End: end}, map[string]checker{
			"no board":     SprintInput{Name: "S"},
			"no name":      SprintInput{Board: "b"},
			"start alone":  SprintInput{Board: "b", Name: "S", Start: start},
			"end before":   SprintInput{Board: "b", Name: "S", Start: end, End: start},
			"no duration":  SprintInput{Board: "b", Name: "S", Start: start, End: start},
			"end no start": SprintInput{Board: "b", Name: "S", End: end},
		}},
		{SprintStart{Board: "b", Sprint: "s", Start: start, End: end}, map[string]checker{
			"no board":   SprintStart{Sprint: "s", Start: start, End: end},
			"no sprint":  SprintStart{Board: "b", Start: start, End: end},
			"no dates":   SprintStart{Board: "b", Sprint: "s"},
			"end before": SprintStart{Board: "b", Sprint: "s", Start: end, End: start},
			"empty name": SprintStart{Board: "b", Sprint: "s", Start: start, End: end, Name: ptr("")},
		}},
		{SprintStart{Board: "b", Sprint: "s", Start: start, End: end, Name: ptr("S"), Goal: ptr("")}, nil},
		{SprintCompletion{Board: "b", Sprint: "s"}, map[string]checker{
			"no board":  SprintCompletion{Sprint: "s"},
			"no sprint": SprintCompletion{Board: "b"},
			"to itself": SprintCompletion{Board: "b", Sprint: "s", MoveTo: "s"},
		}},
		{SprintEdit{Board: "b", Sprint: "s", Goal: ptr("")}, map[string]checker{
			"no board":     SprintEdit{Sprint: "s", Goal: ptr("g")},
			"no sprint":    SprintEdit{Board: "b", Goal: ptr("g")},
			"nothing":      SprintEdit{Board: "b", Sprint: "s"},
			"empty name":   SprintEdit{Board: "b", Sprint: "s", Name: ptr("")},
			"dates invert": SprintEdit{Board: "b", Sprint: "s", Start: &end, End: &start},
		}},
		{CommentInput{Issue: "i", Markdown: "hi", Audience: AudienceInternal}, map[string]checker{
			"no issue":    CommentInput{Markdown: "hi"},
			"no body":     CommentInput{Issue: "i"},
			"no audience": CommentInput{Issue: "i", Markdown: "hi", Audience: AudienceInternal + 1},
		}},
		{CommentEdit{Issue: "i", Comment: "c", Markdown: "new", From: "old"}, map[string]checker{
			"no issue":   CommentEdit{Comment: "c", Markdown: "new"},
			"no comment": CommentEdit{Issue: "i", Markdown: "new"},
			"no body":    CommentEdit{Issue: "i", Comment: "c"},
		}},
		{CommentDelete{Issue: "i", Comment: "c"}, map[string]checker{
			"no issue":   CommentDelete{Comment: "c"},
			"no comment": CommentDelete{Issue: "i"},
		}},
		{LinkInput{Type: "10000", Outward: "1", Inward: "2"}, map[string]checker{
			"no type":    LinkInput{Outward: "1", Inward: "2"},
			"no outward": LinkInput{Type: "t", Inward: "2"},
			"no inward":  LinkInput{Type: "t", Outward: "1"},
			"to itself":  LinkInput{Type: "t", Outward: "1", Inward: "1"},
		}},
		{AttachInput{Issue: "i", Files: []File{{Name: "a.png", Content: strings.NewReader("x"), Size: 1}}}, map[string]checker{
			"no issue":      AttachInput{Files: []File{{Name: "a.png", Content: strings.NewReader("x"), Size: 1}}},
			"no files":      AttachInput{Issue: "i"},
			"no name":       AttachInput{Issue: "i", Files: []File{{Content: strings.NewReader("x"), Size: 1}}},
			"no content":    AttachInput{Issue: "i", Files: []File{{Name: "a.png"}}},
			"negative size": AttachInput{Issue: "i", Files: []File{{Name: "a.png", Content: strings.NewReader("x"), Size: -1}}},
		}},
		{WorklogQuery{Author: "acc-user1", From: day, To: day}, map[string]checker{
			"no author": WorklogQuery{From: day, To: day},
			"no from":   WorklogQuery{Author: "a", To: day},
			"no to":     WorklogQuery{Author: "a", From: day},
			"backwards": WorklogQuery{Author: "a", From: mustDate(t, "2026-11-17"), To: day},
		}},
		{WorklogInput{Issue: "i", Day: day, Spent: 90 * time.Minute}, map[string]checker{
			"no issue":       WorklogInput{Day: day, Spent: time.Hour},
			"no day":         WorklogInput{Issue: "i", Spent: time.Hour},
			"no time":        WorklogInput{Issue: "i", Day: day},
			"negative time":  WorklogInput{Issue: "i", Day: day, Spent: -time.Hour},
			"part of second": WorklogInput{Issue: "i", Day: day, Spent: 1500 * time.Millisecond},
		}},
		{WorklogEdit{From: wl, Spent: ptr(2 * time.Hour)}, map[string]checker{
			"no worklog":  WorklogEdit{From: Worklog{Issue: "i"}, Spent: ptr(time.Hour)},
			"no issue":    WorklogEdit{From: Worklog{ID: "w"}, Spent: ptr(time.Hour)},
			"nothing":     WorklogEdit{From: wl},
			"clears day":  WorklogEdit{From: wl, Day: &Date{}},
			"zero spent":  WorklogEdit{From: wl, Spent: ptr(time.Duration(0))},
			"part second": WorklogEdit{From: wl, Spent: ptr(time.Millisecond)},
		}},
		{WorklogDelete{From: wl}, map[string]checker{
			"no worklog": WorklogDelete{From: Worklog{Issue: "i"}},
			"no issue":   WorklogDelete{From: Worklog{ID: "w"}},
		}},
	}
	for _, c := range cases {
		if err := c.good.Check(); err != nil {
			t.Errorf("%T refused a good input: %v", c.good, err)
		}
		for name, in := range c.bad {
			if err := in.Check(); !errors.Is(err, ErrInvalidInput) {
				t.Errorf("%T %s: Check = %v, want ErrInvalidInput", in, name, err)
			}
		}
	}
}

func TestRankAnchor(t *testing.T) {
	for _, tc := range []struct {
		name  string
		in    RankInput
		want  IssueID
		after bool
	}{
		{"both", RankInput{Prev: "p", Next: "n"}, "p", true},
		{"previous only", RankInput{Prev: "p"}, "p", true},
		{"next only", RankInput{Next: "n"}, "n", false},
		{"neither", RankInput{}, "", false},
	} {
		if got, after := tc.in.Anchor(); got != tc.want || after != tc.after {
			t.Errorf("%s: Anchor = %q, %v; want %q, %v", tc.name, got, after, tc.want, tc.after)
		}
	}
	for name, in := range map[string]RankInput{
		"previous only": {Board: "b", Issues: []IssueID{"1"}, Prev: "p"},
		"next only":     {Board: "b", Issues: []IssueID{"1"}, Next: "n"},
		"both":          {Board: "b", Issues: []IssueID{"1"}, Prev: "p", Next: "n"},
	} {
		if err := in.Check(); err != nil {
			t.Errorf("%s: Check = %v", name, err)
		}
	}
}

func TestFieldScope(t *testing.T) {
	if id, ok := OnIssue("i").Issue(); !ok || id != "i" {
		t.Fatal("OnIssue does not name its issue")
	}
	if _, _, ok := OnIssue("i").Create(); ok {
		t.Fatal("OnIssue reads as a create")
	}
	p, ty, ok := OnCreate("p", "10001").Create()
	if !ok || p != "p" || ty != "10001" {
		t.Fatal("OnCreate does not name its project and type")
	}
	var zero FieldScope
	if _, ok := zero.Issue(); ok {
		t.Fatal("the zero scope names an issue")
	}
	if _, _, ok := zero.Create(); ok {
		t.Fatal("the zero scope names a create")
	}
}
