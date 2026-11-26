package tracker

import (
	"io"
	"math"
	"strings"
	"time"
)

// The inputs of Provider's writes. Each has a Check that refuses what no
// tracker takes; a provider runs it first and returns Invalid with its
// error, so every provider refuses the same input the same way. A
// Markdown body may embed local images only when ImageDir names the
// absolute folder they are read from, and only on a tracker with
// Capabilities.MaxAttachmentBytes; a provider without it refuses a write
// that names an ImageDir.

// CreateInput is an issue to create. Type is required with
// Capabilities.IssueTypes and must be empty without it. Fields set every
// field but the title; a rich text edit there gives "" as its from.
// Sprint, when set, is an open sprint of Board the issue is made in; with
// none it lands in the backlog.
type CreateInput struct {
	Project  ProjectID
	Type     IssueTypeID
	Title    string
	Fields   []FieldEdit
	ImageDir string
	Board    BoardID
	Sprint   SprintID
	// After is the row of Board the issue lands just after: its section's
	// last, as the caller holds it; zero for the tracker's default. A
	// tracker whose new issues land last already (Jira) needs none.
	After RowPlace
}

// RowPlace is a row of a board: its issue, and its place in the board's
// order as last read.
type RowPlace struct {
	Issue IssueID
	Order OrderKey
}

// Check refuses a create with no project or title, a sprint with no
// board, a field edited twice or the title among its fields, or rich text
// written from anything but an empty field.
func (in CreateInput) Check() error {
	if in.Project == "" || in.Title == "" {
		return invalidf("a create needs a project and a title")
	}
	if in.Sprint != "" && in.Board == "" {
		return invalidf("a create in a sprint names the sprint's board")
	}
	if (in.After.Issue == "") != (in.After.Order == "") || in.After.Issue != "" && in.Board == "" {
		return invalidf("a create after a row names the row's issue, its order and its board")
	}
	if err := checkEdits(in.Fields); err != nil {
		return err
	}
	for _, e := range in.Fields {
		if e.Field == FieldTitle {
			return invalidf("a create sets its title in Title, not as a field")
		}
		if _, from := e.Value.Markdown(); from != "" {
			return invalidf("a create's %s is written from text no new issue holds", e.Field)
		}
	}
	return nil
}

// IssueRead is an issue as a read of its provider found it (Detail.Read),
// which a write of the same provider to the issue takes as current in
// place of reading the issue again. A caller passes it only to the first
// write it sends to the issue after the read. What it holds is the
// provider's own; a provider given none, or another provider's, reads the
// issue as it would.
type IssueRead interface {
	// ReadIssue is the issue read.
	ReadIssue() IssueID
}

// EditInput changes fields of one issue. AcceptLoss is the user's
// explicit acceptance that a rich text edit loses content Markdown cannot
// show; without it such an edit is refused with ReasonLoss. Read is the
// issue as the caller read it (IssueRead), or nil.
type EditInput struct {
	Issue      IssueID
	Fields     []FieldEdit
	AcceptLoss bool
	ImageDir   string
	Read       IssueRead
}

// Check refuses an edit that names no issue or no field, or a field twice.
func (in EditInput) Check() error {
	if in.Issue == "" || len(in.Fields) == 0 {
		return invalidf("an edit needs an issue and at least one field")
	}
	return checkEdits(in.Fields)
}

// MoveInput moves an issue by one of the moves Moves lists for it, with
// the fields the move takes. Read is the issue as the caller read it
// (IssueRead), or nil.
type MoveInput struct {
	Issue      IssueID
	Move       MoveID
	Fields     []FieldEdit
	AcceptLoss bool
	ImageDir   string
	Read       IssueRead
}

// Check refuses a move that names no issue or move, or a field twice.
func (in MoveInput) Check() error {
	if in.Issue == "" || in.Move == "" {
		return invalidf("a move needs an issue and a move")
	}
	return checkEdits(in.Fields)
}

// EstimateInput sets an issue's estimate on a board, in the board's unit;
// a nil Value clears it.
type EstimateInput struct {
	Board BoardID
	Issue IssueID
	Value *float64
}

// Check refuses an estimate that names no board or issue, or whose value
// is negative or not finite.
func (in EstimateInput) Check() error {
	if in.Board == "" || in.Issue == "" {
		return invalidf("an estimate needs a board and an issue")
	}
	if v := in.Value; v != nil && (*v < 0 || math.IsInf(*v, 0) || math.IsNaN(*v)) {
		return invalidf("estimate %v is not a number of zero or more", *v)
	}
	return nil
}

// RankInput places Issues, in the order given, on Board between Prev, the
// issue they go just after, and Next, the issue they go just before. One
// of the two may be empty for an end of the list; not both.
//
// A neighbor is named by its issue alone. Where it stands is the
// provider's to read when it ranks: a position the caller read earlier
// may have moved, and a rank computed from it lands somewhere else. A
// provider refuses a rank whose neighbors no longer stand in the order
// the input names.
type RankInput struct {
	Board  BoardID
	Issues []IssueID
	Prev   IssueID
	Next   IssueID
}

// Anchor is the neighbor a tracker that ranks relative to one issue
// ranks against, for an input Check passed: Prev, with after set, or Next
// when there is no Prev.
func (in RankInput) Anchor() (issue IssueID, after bool) {
	if in.Prev != "" {
		return in.Prev, true
	}
	return in.Next, false
}

// Check refuses a rank that names no board or issue, an issue twice, no
// neighbor, the same neighbor on both sides, or a neighbor among the
// issues ranked.
func (in RankInput) Check() error {
	if in.Board == "" {
		return invalidf("a rank needs a board")
	}
	if err := checkIssues(in.Issues); err != nil {
		return err
	}
	if in.Prev == "" && in.Next == "" {
		return invalidf("a rank needs a neighbor")
	}
	if in.Prev == in.Next {
		return invalidf("issue %s is named as both neighbors of a rank", in.Prev)
	}
	for _, id := range in.Issues {
		if id == in.Prev || id == in.Next {
			return invalidf("issue %s is ranked next to itself", id)
		}
	}
	return nil
}

// SprintMoveInput moves Issues into Sprint, or to the backlog of Board
// when Sprint is empty. The sprint must be open. Prev and Next, when
// either is set, place the issues where they land on Board as a
// RankInput's do (Rank); with neither they land where the tracker puts
// them.
type SprintMoveInput struct {
	Board  BoardID
	Sprint SprintID
	Issues []IssueID
	Prev   IssueID
	Next   IssueID
}

// Rank is the rank that places the moved issues, and whether the move
// names a place.
func (in SprintMoveInput) Rank() (RankInput, bool) {
	return RankInput{Board: in.Board, Issues: in.Issues, Prev: in.Prev, Next: in.Next}, in.Prev != "" || in.Next != ""
}

// Check refuses a move that names no board or issue, an issue twice, or
// a place a rank refuses (RankInput.Check).
func (in SprintMoveInput) Check() error {
	if in.Board == "" {
		return invalidf("a sprint move needs a board")
	}
	if rank, placed := in.Rank(); placed {
		return rank.Check()
	}
	return checkIssues(in.Issues)
}

// SprintInput is a new future sprint on a board. Start and End are
// optional and set together.
type SprintInput struct {
	Board      BoardID
	Name       string
	Goal       string
	Start, End time.Time
}

// Check refuses a sprint with no board or name, or dates that are not a
// start before an end.
func (in SprintInput) Check() error {
	if in.Board == "" || in.Name == "" {
		return invalidf("a sprint needs a board and a name")
	}
	if in.Start.IsZero() != in.End.IsZero() {
		return invalidf("a sprint's start and end are set together")
	}
	return checkSpan(in.Start, in.End)
}

// SprintStart starts a future sprint of Board with its dates, and sets
// its Name and Goal in the same write when they are set.
type SprintStart struct {
	Board      BoardID
	Sprint     SprintID
	Start, End time.Time
	Name, Goal *string
}

// Check refuses a start that names no board or sprint, empties the name,
// or has dates that are not a start before an end.
func (in SprintStart) Check() error {
	if in.Board == "" || in.Sprint == "" || in.Start.IsZero() || in.End.IsZero() {
		return invalidf("a sprint start needs a board, a sprint, a start and an end")
	}
	if in.Name != nil && *in.Name == "" {
		return invalidf("a sprint's name cannot be emptied")
	}
	return checkSpan(in.Start, in.End)
}

// SprintCompletion closes an active sprint of Board once its unfinished
// issues have moved to MoveTo, an open sprint, or to the backlog when
// MoveTo is empty. A sprint that still holds unfinished issues stays open
// and its close is not sent.
type SprintCompletion struct {
	Board  BoardID
	Sprint SprintID
	MoveTo SprintID
}

// Check refuses a completion that names no board or sprint, or that moves
// the sprint's issues to itself.
func (in SprintCompletion) Check() error {
	if in.Board == "" || in.Sprint == "" {
		return invalidf("a sprint completion needs a board and a sprint")
	}
	if in.MoveTo == in.Sprint {
		return invalidf("sprint %s cannot take its own unfinished issues", in.Sprint)
	}
	return nil
}

// SprintEdit changes the members of a sprint of Board that are set.
type SprintEdit struct {
	Board      BoardID
	Sprint     SprintID
	Name, Goal *string
	Start, End *time.Time
}

// Check refuses an edit that names no board or sprint, changes nothing,
// empties the name, or sets both dates other than a start before an end.
func (in SprintEdit) Check() error {
	if in.Board == "" || in.Sprint == "" {
		return invalidf("a sprint edit needs a board and a sprint")
	}
	if in.Name == nil && in.Goal == nil && in.Start == nil && in.End == nil {
		return invalidf("a sprint edit changes nothing")
	}
	if in.Name != nil && *in.Name == "" {
		return invalidf("a sprint's name cannot be emptied")
	}
	if in.Start != nil && in.End != nil {
		return checkSpan(*in.Start, *in.End)
	}
	return nil
}

// Audience is who a new comment reaches on a project that
// ChoosesAudience. Elsewhere it must be AudienceUnset.
type Audience uint8

const (
	AudienceUnset Audience = iota
	// AudiencePublic reaches everyone who can see the issue, a service
	// desk's customer included.
	AudiencePublic
	// AudienceInternal is a note only the tracker's agents see.
	AudienceInternal
)

// CommentInput is a new comment, its body as Markdown. Project is the
// issue's project as a read made after the last wait for a person, just
// before the comment, found it: who the comment reaches is judged on it.
// Empty, the provider reads the project with the comment. A tracker whose
// comments have no audience takes no notice of it.
type CommentInput struct {
	Issue    IssueID
	Markdown string
	Audience Audience
	ImageDir string
	Project  ProjectID
}

// Check refuses a comment that names no issue or has no body.
func (in CommentInput) Check() error {
	if in.Issue == "" || in.Markdown == "" {
		return invalidf("a comment needs an issue and a body")
	}
	if in.Audience > AudienceInternal {
		return invalidf("comment audience %d is not an audience", in.Audience)
	}
	return nil
}

// CommentEdit replaces a comment's body. From is the body as read when
// the edit was written: a comment that holds another body by the time the
// provider writes is refused with ReasonStale. AcceptLoss is as on
// EditInput.
type CommentEdit struct {
	Issue      IssueID
	Comment    CommentID
	Markdown   string
	From       string
	AcceptLoss bool
	ImageDir   string
}

// Check refuses an edit that names no issue or comment, or empties the
// body.
func (in CommentEdit) Check() error {
	if in.Issue == "" || in.Comment == "" || in.Markdown == "" {
		return invalidf("a comment edit needs an issue, a comment and a body")
	}
	return nil
}

// CommentDelete deletes a comment.
type CommentDelete struct {
	Issue   IssueID
	Comment CommentID
}

// Check refuses a delete that names no issue or comment.
func (in CommentDelete) Check() error {
	if in.Issue == "" || in.Comment == "" {
		return invalidf("a comment delete needs an issue and a comment")
	}
	return nil
}

// LinkInput links two issues: Outward relates to Inward by the type's
// Outward phrase ("WEB-1 blocks WEB-2").
type LinkInput struct {
	Type    LinkTypeID
	Outward IssueID
	Inward  IssueID
}

// Check refuses a link that names no type or issue, or links an issue to
// itself.
func (in LinkInput) Check() error {
	if in.Type == "" || in.Outward == "" || in.Inward == "" {
		return invalidf("a link needs a type and two issues")
	}
	if in.Outward == in.Inward {
		return invalidf("issue %s cannot be linked to itself", in.Outward)
	}
	return nil
}

// UnlinkInput removes one link: Link as read from Issue's detail, whose
// other side is Other.
type UnlinkInput struct {
	Issue IssueID
	Link  LinkID
	Other IssueID
}

// Check refuses an unlink that names no link or issue, or the same issue
// on both sides.
func (in UnlinkInput) Check() error {
	if in.Issue == "" || in.Link == "" || in.Other == "" {
		return invalidf("an unlink needs the link and both its issues")
	}
	if in.Issue == in.Other {
		return invalidf("issue %s cannot be linked to itself", in.Issue)
	}
	return nil
}

// File is one file to attach: Content holds exactly Size bytes. The
// provider reads Content once, as it sends it; a Content that is an
// io.Closer and not an io.Seeker is closed if the write ends before
// reading it to its end (trackerapi.Part), which must end a read of it
// in progress.
type File struct {
	Name    string
	Content io.Reader
	Size    int64
}

// AttachInput attaches files to an issue.
type AttachInput struct {
	Issue IssueID
	Files []File
}

// Check refuses an attach that names no issue or file, an issue ID or a
// file name CheckIssueID or CheckFileName refuses, or a file with no
// content or a negative size.
func (in AttachInput) Check() error {
	if in.Issue == "" || len(in.Files) == 0 {
		return invalidf("an attach needs an issue and a file")
	}
	if err := CheckIssueID(in.Issue); err != nil {
		return err
	}
	for _, f := range in.Files {
		if err := CheckFileName(f.Name); err != nil {
			return err
		}
		if f.Content == nil || f.Size < 0 {
			return invalidf("an attached file needs content and a size")
		}
	}
	return nil
}

// MaxIssueIDBytes is the longest issue ID a client may name. A tracker's
// issue IDs are far shorter: Jira's are numbers (at most 19 digits) and
// Linear's are UUIDs (36 bytes).
const MaxIssueIDBytes = 64

// MaxFileNameBytes is the longest file name an attachment may have, in
// UTF-8 bytes. A platform bounds a name by its own units (255 UTF-16
// units on Windows, 255 bytes on most Linux file systems), so a name
// another platform holds, 255 CJK characters say, can take up to 765
// bytes here; 1,024 holds every one, as the Windows download name cap
// does (webview2host's maxDownloadNameBytes).
const MaxFileNameBytes = 1024

// CheckIssueID refuses an issue ID a client sent that is empty or longer
// than MaxIssueIDBytes.
func CheckIssueID(id IssueID) error {
	switch {
	case id == "":
		return invalidf("no issue is named")
	case len(id) > MaxIssueIDBytes:
		return invalidf("an issue ID is at most %d bytes, and this one is %d", MaxIssueIDBytes, len(id))
	}
	return nil
}

// CheckFileName refuses an attachment's file name that is empty, holds a
// NUL character or is longer than MaxFileNameBytes.
func CheckFileName(name string) error {
	switch {
	case name == "":
		return invalidf("the file has no name")
	case strings.ContainsRune(name, 0):
		return invalidf("the file name holds a NUL character")
	case len(name) > MaxFileNameBytes:
		return invalidf("a file name is at most %d bytes, and this one is %d; rename the file", MaxFileNameBytes, len(name))
	}
	return nil
}

func checkIssues(ids []IssueID) error {
	if len(ids) == 0 {
		return invalidf("the write names no issue")
	}
	seen := make(map[IssueID]bool, len(ids))
	for _, id := range ids {
		if id == "" {
			return invalidf("the write names an empty issue ID")
		}
		if seen[id] {
			return invalidf("issue %s is named twice", id)
		}
		seen[id] = true
	}
	return nil
}

func checkSpan(start, end time.Time) error {
	if !start.IsZero() && !end.After(start) {
		return invalidf("the end %s is not after the start %s", end.Format(time.RFC3339), start.Format(time.RFC3339))
	}
	return nil
}
