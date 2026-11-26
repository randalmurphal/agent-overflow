package trackerapp

import (
	"fmt"
	"math"
	"strconv"
	"time"

	"agent-overflow/internal/tracker"
)

// The wire inputs of the writes: JSON with plain values, each converted to
// the model's input in one place (its model method). A conversion refuses,
// wrapping ErrInvalidRequest, only what it cannot express in the model: a
// field value a kind cannot hold, a day that is not one, an audience it
// does not know. What the model refuses (a create with no title, a rank
// with no neighbor) is the provider's to refuse, as a refused part, so
// every caller reads one refusal for one input.

// FieldEditInput sets one field: with Value, to a value given as a ticket
// shows one (FieldValueView), with Typed, to text a person or an agent
// typed, which the tracker resolves by the field's kind, or with Markdown,
// a rich text field's new Markdown and the Markdown it held when the edit
// was written. Exactly one is set. Replaces, on a clear or typed text of
// a plain text field, are the texts it held when the edit was written: the
// tracker refuses the edit as stale when it holds others by then.
type FieldEditInput struct {
	Field    string             `json:"field"`
	Value    *FieldValueView    `json:"value,omitempty"`
	Typed    []string           `json:"typed,omitempty"`
	Markdown *MarkdownEditInput `json:"markdown,omitempty"`
	Replaces *[]string          `json:"replaces,omitempty"`
}

// MarkdownEditInput is a rich text field's new Markdown and From, the
// Markdown the field held when the edit was written ("" for an empty
// field or a create).
type MarkdownEditInput struct {
	Markdown string `json:"markdown"`
	From     string `json:"from"`
}

// fieldEdit is in as the model's edit. A Value with no kind clears the
// field; a user or option value names its choices by ID, as picked from a
// list; a text, number, date or date-time value is given as typed text in
// the form the tracker reads (a number as written, a date YYYY-MM-DD, a
// time RFC 3339 in UTC). A rich text value is refused: its edit needs the
// Markdown it was written from (Markdown). A read-only value, an unknown
// kind, values of another kind than the one named, or a kind with no
// values is refused.
func (in FieldEditInput) fieldEdit() (tracker.FieldEdit, error) {
	set := 0
	for _, given := range []bool{in.Value != nil, len(in.Typed) > 0, in.Markdown != nil} {
		if given {
			set++
		}
	}
	if in.Field == "" || set != 1 {
		return tracker.FieldEdit{}, fmt.Errorf("%w: a field edit names its field and exactly one of a value, typed text and Markdown", ErrInvalidRequest)
	}
	out := tracker.FieldEdit{Field: tracker.FieldID(in.Field)}
	switch {
	case in.Markdown != nil:
		out.Value = tracker.MarkdownEdit(in.Markdown.Markdown, in.Markdown.From)
	case len(in.Typed) > 0:
		out.Value = tracker.TypedValues(in.Typed[0], in.Typed[1:]...)
	default:
		edit, err := valueEdit(*in.Value)
		if err != nil {
			return tracker.FieldEdit{}, fmt.Errorf("%w: field %s: %w", ErrInvalidRequest, in.Field, err)
		}
		out.Value = edit
	}
	if in.Replaces != nil {
		if in.Markdown != nil {
			return tracker.FieldEdit{}, fmt.Errorf("%w: field %s: a Markdown edit gives what it replaces as from", ErrInvalidRequest, in.Field)
		}
		out.Value = out.Value.Replacing(*in.Replaces...)
	}
	return out, nil
}

// valueEdit is v, a value as a ticket shows one, as the edit that sets
// it (FieldEditInput.fieldEdit).
func valueEdit(v FieldValueView) (tracker.Edit, error) {
	n := len(v.Texts) + len(v.Numbers) + len(v.Dates) + len(v.Times) + len(v.Choices)
	own := 0
	var typed []string
	switch v.Kind {
	case "":
		if n > 0 {
			return tracker.Edit{}, fmt.Errorf("a value with no kind holds %d values", n)
		}
		return tracker.ClearField(), nil
	case tracker.KindText.String():
		own, typed = len(v.Texts), v.Texts
	case tracker.KindNumber.String():
		own = len(v.Numbers)
		for _, x := range v.Numbers {
			if math.IsInf(x, 0) || math.IsNaN(x) {
				return tracker.Edit{}, fmt.Errorf("%v is not a number", x)
			}
			typed = append(typed, strconv.FormatFloat(x, 'f', -1, 64))
		}
	case tracker.KindDate.String():
		own = len(v.Dates)
		for _, d := range v.Dates {
			day, err := tracker.ParseDate(d)
			if err != nil {
				return tracker.Edit{}, err
			}
			typed = append(typed, day.String())
		}
	case tracker.KindDateTime.String():
		own = len(v.Times)
		for _, ms := range v.Times {
			typed = append(typed, time.UnixMilli(ms).UTC().Format(time.RFC3339Nano))
		}
	case tracker.KindUser.String(), tracker.KindOption.String():
		own = len(v.Choices)
		for _, c := range v.Choices {
			if c.ID == "" {
				return tracker.Edit{}, fmt.Errorf("a %s value names a choice with no ID", v.Kind)
			}
			typed = append(typed, c.ID)
		}
		if own == n && own > 0 {
			return tracker.PickValues(typed[0], typed[1:]...), nil
		}
	case tracker.KindRichText.String():
		return tracker.Edit{}, fmt.Errorf("a rich text value is set with its Markdown and the Markdown it was written from")
	case tracker.KindReadOnly.String():
		return tracker.Edit{}, fmt.Errorf("a read-only value cannot be set")
	default:
		return tracker.Edit{}, fmt.Errorf("unknown kind %q", v.Kind)
	}
	if own != n || own == 0 {
		return tracker.Edit{}, fmt.Errorf("a %s value holds %d values, %d of its own kind", v.Kind, n, own)
	}
	return tracker.TypedValues(typed[0], typed[1:]...), nil
}

// fieldEdits are ins as the model's edits.
func fieldEdits(ins []FieldEditInput) ([]tracker.FieldEdit, error) {
	if len(ins) == 0 {
		return nil, nil
	}
	out := make([]tracker.FieldEdit, len(ins))
	for i, in := range ins {
		e, err := in.fieldEdit()
		if err != nil {
			return nil, err
		}
		out[i] = e
	}
	return out, nil
}

// CreateRequest makes an issue from a section of Board: an issue made is
// read through that board, and a create of unknown outcome runs the
// board's tick at once. Type is the issue type, empty on a tracker
// without issue types.
type CreateRequest struct {
	Board   string           `json:"board"`
	Project string           `json:"project"`
	Type    string           `json:"type,omitempty"`
	Title   string           `json:"title"`
	Fields  []FieldEditInput `json:"fields,omitempty"`
}

func (in CreateRequest) model() (tracker.CreateInput, error) {
	if in.Board == "" {
		return tracker.CreateInput{}, fmt.Errorf("%w: a create names the board it is made from", ErrInvalidRequest)
	}
	fields, err := fieldEdits(in.Fields)
	return tracker.CreateInput{Project: tracker.ProjectID(in.Project), Type: tracker.IssueTypeID(in.Type), Title: in.Title, Fields: fields}, err
}

// EditRequest changes fields of an issue. AcceptLoss is the person's
// acceptance that a rich text edit loses what a refusal of reason loss
// named.
type EditRequest struct {
	Issue      string           `json:"issue"`
	Fields     []FieldEditInput `json:"fields"`
	AcceptLoss bool             `json:"accept_loss,omitempty"`
}

func (in EditRequest) model() (tracker.EditInput, error) {
	fields, err := fieldEdits(in.Fields)
	return tracker.EditInput{Issue: tracker.IssueID(in.Issue), Fields: fields, AcceptLoss: in.AcceptLoss}, err
}

// MoveRequest moves an issue by one of the moves its ticket lists, with
// the fields the move takes.
type MoveRequest struct {
	Issue      string           `json:"issue"`
	Move       string           `json:"move"`
	Fields     []FieldEditInput `json:"fields,omitempty"`
	AcceptLoss bool             `json:"accept_loss,omitempty"`
}

func (in MoveRequest) model() (tracker.MoveInput, error) {
	fields, err := fieldEdits(in.Fields)
	return tracker.MoveInput{Issue: tracker.IssueID(in.Issue), Move: tracker.MoveID(in.Move), Fields: fields, AcceptLoss: in.AcceptLoss}, err
}

// EstimateRequest sets an issue's estimate on a board, in the board's
// unit; no Value clears it.
type EstimateRequest struct {
	Board string   `json:"board"`
	Issue string   `json:"issue"`
	Value *float64 `json:"value,omitempty"`
}

func (in EstimateRequest) model() tracker.EstimateInput {
	return tracker.EstimateInput{Board: tracker.BoardID(in.Board), Issue: tracker.IssueID(in.Issue), Value: copyNumber(in.Value)}
}

// RankRequest places Issues, in order, on Board between Prev, the issue
// they go just after, and Next, the issue they go just before; one of the
// two may be empty for an end of the list.
type RankRequest struct {
	Board  string   `json:"board"`
	Issues []string `json:"issues"`
	Prev   string   `json:"prev,omitempty"`
	Next   string   `json:"next,omitempty"`
}

func (in RankRequest) model() tracker.RankInput {
	return tracker.RankInput{Board: tracker.BoardID(in.Board), Issues: issueIDsInOrder(in.Issues),
		Prev: tracker.IssueID(in.Prev), Next: tracker.IssueID(in.Next)}
}

// SprintMoveRequest moves Issues into Sprint, an open sprint of Board, or
// to the board's backlog when Sprint is empty.
type SprintMoveRequest struct {
	Board  string   `json:"board"`
	Sprint string   `json:"sprint,omitempty"`
	Issues []string `json:"issues"`
}

func (in SprintMoveRequest) model() tracker.SprintMoveInput {
	return tracker.SprintMoveInput{Board: tracker.BoardID(in.Board), Sprint: tracker.SprintID(in.Sprint), Issues: issueIDsInOrder(in.Issues)}
}

// SprintRequest is a new future sprint of Board. Start and End are Unix
// milliseconds, both set or both 0.
type SprintRequest struct {
	Board string `json:"board"`
	Name  string `json:"name"`
	Goal  string `json:"goal,omitempty"`
	Start int64  `json:"start,omitempty"`
	End   int64  `json:"end,omitempty"`
}

func (in SprintRequest) model() tracker.SprintInput {
	return tracker.SprintInput{Board: tracker.BoardID(in.Board), Name: in.Name, Goal: in.Goal, Start: timeOf(in.Start), End: timeOf(in.End)}
}

// SprintStartRequest starts a future sprint of Board, Start to End in Unix
// milliseconds.
type SprintStartRequest struct {
	Board  string `json:"board"`
	Sprint string `json:"sprint"`
	Start  int64  `json:"start"`
	End    int64  `json:"end"`
}

func (in SprintStartRequest) model() tracker.SprintStart {
	return tracker.SprintStart{Board: tracker.BoardID(in.Board), Sprint: tracker.SprintID(in.Sprint), Start: timeOf(in.Start), End: timeOf(in.End)}
}

// SprintCompleteRequest closes an active sprint of Board once its
// unfinished issues moved to MoveTo, an open sprint, or to the backlog
// when MoveTo is empty.
type SprintCompleteRequest struct {
	Board  string `json:"board"`
	Sprint string `json:"sprint"`
	MoveTo string `json:"move_to,omitempty"`
}

func (in SprintCompleteRequest) model() tracker.SprintCompletion {
	return tracker.SprintCompletion{Board: tracker.BoardID(in.Board), Sprint: tracker.SprintID(in.Sprint), MoveTo: tracker.SprintID(in.MoveTo)}
}

// SprintEditRequest changes the members of a sprint of Board that are
// set; Start and End are Unix milliseconds.
type SprintEditRequest struct {
	Board  string  `json:"board"`
	Sprint string  `json:"sprint"`
	Name   *string `json:"name,omitempty"`
	Goal   *string `json:"goal,omitempty"`
	Start  *int64  `json:"start,omitempty"`
	End    *int64  `json:"end,omitempty"`
}

func (in SprintEditRequest) model() tracker.SprintEdit {
	out := tracker.SprintEdit{Board: tracker.BoardID(in.Board), Sprint: tracker.SprintID(in.Sprint), Name: in.Name, Goal: in.Goal}
	if in.Start != nil {
		t := timeOf(*in.Start)
		out.Start = &t
	}
	if in.End != nil {
		t := timeOf(*in.End)
		out.End = &t
	}
	return out
}

// CommentRequest is a new comment, its body as Markdown. Audience is
// public or internal on a project that chooses who a comment reaches,
// and empty elsewhere.
type CommentRequest struct {
	Issue    string `json:"issue"`
	Markdown string `json:"markdown"`
	Audience string `json:"audience,omitempty"`
}

func (in CommentRequest) model() (tracker.CommentInput, error) {
	out := tracker.CommentInput{Issue: tracker.IssueID(in.Issue), Markdown: in.Markdown}
	switch in.Audience {
	case "":
	case "public":
		out.Audience = tracker.AudiencePublic
	case "internal":
		out.Audience = tracker.AudienceInternal
	default:
		return out, fmt.Errorf("%w: comment audience %q is not public or internal", ErrInvalidRequest, in.Audience)
	}
	return out, nil
}

// CommentEditRequest replaces a comment's body; From is the body as read
// when the edit was written.
type CommentEditRequest struct {
	Issue      string `json:"issue"`
	Comment    string `json:"comment"`
	Markdown   string `json:"markdown"`
	From       string `json:"from"`
	AcceptLoss bool   `json:"accept_loss,omitempty"`
}

func (in CommentEditRequest) model() tracker.CommentEdit {
	return tracker.CommentEdit{Issue: tracker.IssueID(in.Issue), Comment: tracker.CommentID(in.Comment), Markdown: in.Markdown,
		From: in.From, AcceptLoss: in.AcceptLoss}
}

// CommentDeleteRequest deletes a comment of an issue.
type CommentDeleteRequest struct {
	Issue   string `json:"issue"`
	Comment string `json:"comment"`
}

func (in CommentDeleteRequest) model() tracker.CommentDelete {
	return tracker.CommentDelete{Issue: tracker.IssueID(in.Issue), Comment: tracker.CommentID(in.Comment)}
}

// LinkRequest links Outward to Inward by the link type's outward phrase
// ("WEB-1 blocks WEB-2").
type LinkRequest struct {
	Type    string `json:"type"`
	Outward string `json:"outward"`
	Inward  string `json:"inward"`
}

func (in LinkRequest) model() tracker.LinkInput {
	return tracker.LinkInput{Type: tracker.LinkTypeID(in.Type), Outward: tracker.IssueID(in.Outward), Inward: tracker.IssueID(in.Inward)}
}

// UnlinkRequest removes Link, as Issue's ticket shows it, whose other side
// is Other.
type UnlinkRequest struct {
	Issue string `json:"issue"`
	Link  string `json:"link"`
	Other string `json:"other"`
}

func (in UnlinkRequest) model() tracker.UnlinkInput {
	return tracker.UnlinkInput{Issue: tracker.IssueID(in.Issue), Link: tracker.LinkID(in.Link), Other: tracker.IssueID(in.Other)}
}

// WorklogView is time logged on an issue: Day is YYYY-MM-DD in the
// author's own zone, Updated the tracker's revision in Unix milliseconds.
// Key and Title name the issue as the read found it, both empty when the
// tracker did not return it; a write ignores them. A worklog edit or
// delete names the worklog as read, Updated included: a worklog whose
// revision moved since is refused as stale.
type WorklogView struct {
	ID           string `json:"id"`
	Issue        string `json:"issue"`
	Key          string `json:"key,omitempty"`
	Title        string `json:"title,omitempty"`
	Author       string `json:"author"`
	Day          string `json:"day"`
	SpentSeconds int64  `json:"spent_seconds"`
	Description  string `json:"description,omitempty"`
	Updated      int64  `json:"updated,omitempty"`
}

func worklogView(w tracker.Worklog) WorklogView {
	return WorklogView{ID: string(w.ID), Issue: string(w.Issue), Key: w.IssueKey, Title: w.IssueTitle, Author: string(w.Author), Day: w.Day.String(),
		SpentSeconds: int64(w.Spent / time.Second), Description: w.Description, Updated: millis(w.Updated)}
}

func (v WorklogView) model() (tracker.Worklog, error) {
	day, err := parseDay(v.Day)
	if err != nil {
		return tracker.Worklog{}, err
	}
	spent, err := seconds(v.SpentSeconds)
	return tracker.Worklog{ID: tracker.WorklogID(v.ID), Issue: tracker.IssueID(v.Issue), Author: tracker.UserID(v.Author), Day: day,
		Spent: spent, Description: v.Description, Updated: timeOf(v.Updated)}, err
}

// WorklogRequest is a new worklog of the credential's own user.
type WorklogRequest struct {
	Issue        string `json:"issue"`
	Day          string `json:"day"`
	SpentSeconds int64  `json:"spent_seconds"`
	Description  string `json:"description,omitempty"`
}

func (in WorklogRequest) model() (tracker.WorklogInput, error) {
	day, err := parseDay(in.Day)
	if err != nil {
		return tracker.WorklogInput{}, err
	}
	spent, err := seconds(in.SpentSeconds)
	return tracker.WorklogInput{Issue: tracker.IssueID(in.Issue), Day: day, Spent: spent, Description: in.Description}, err
}

// WorklogEditRequest changes the members of From, the worklog as read,
// that are set.
type WorklogEditRequest struct {
	From         WorklogView `json:"from"`
	Day          *string     `json:"day,omitempty"`
	SpentSeconds *int64      `json:"spent_seconds,omitempty"`
	Description  *string     `json:"description,omitempty"`
	AcceptLoss   bool        `json:"accept_loss,omitempty"`
}

func (in WorklogEditRequest) model() (tracker.WorklogEdit, error) {
	from, err := in.From.model()
	if err != nil {
		return tracker.WorklogEdit{}, err
	}
	out := tracker.WorklogEdit{From: from, Description: in.Description, AcceptLoss: in.AcceptLoss}
	if in.Day != nil {
		day, err := parseDay(*in.Day)
		if err != nil {
			return tracker.WorklogEdit{}, err
		}
		out.Day = &day
	}
	if in.SpentSeconds != nil {
		spent, err := seconds(*in.SpentSeconds)
		if err != nil {
			return tracker.WorklogEdit{}, err
		}
		out.Spent = &spent
	}
	return out, nil
}

// WorklogDeleteRequest deletes From, the worklog as read.
type WorklogDeleteRequest struct {
	From WorklogView `json:"from"`
}

func (in WorklogDeleteRequest) model() (tracker.WorklogDelete, error) {
	from, err := in.From.model()
	return tracker.WorklogDelete{From: from}, err
}

// parseDay reads a day YYYY-MM-DD, refusing anything else with
// ErrInvalidRequest.
func parseDay(s string) (tracker.Date, error) {
	d, err := tracker.ParseDate(s)
	if err != nil {
		return tracker.Date{}, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	return d, nil
}

// seconds is n seconds, refusing with ErrInvalidRequest a count no
// duration holds. Whether a time spent is positive is the model's check.
func seconds(n int64) (time.Duration, error) {
	if n > math.MaxInt64/int64(time.Second) || n < math.MinInt64/int64(time.Second) {
		return 0, fmt.Errorf("%w: %d seconds is not a time spent", ErrInvalidRequest, n)
	}
	return time.Duration(n) * time.Second, nil
}
