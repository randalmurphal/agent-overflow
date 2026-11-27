package tracker

import (
	"context"
	"time"

	"agent-overflow/internal/tracker/trackerapi"
)

// OneCredential is p with every read one trackerapi.Walk, so one answer
// is one credential's view however many client requests the provider
// composes it from: a read whose answers were drawn under two reads of
// a client's credential (a 401 made the client read the key file again,
// and it now holds another account's token) starts again from its first
// request under the credential the client holds now, once, and a second
// change fails it with an error matching trackerapi.ErrCredentialChanged,
// a retryable failure. Download streams to its writer, so it fails
// instead of starting again (trackerapi.WalkOnce). Writes pass through:
// a write is never part of a walk, and a write's result reports each of
// its steps. The answers of separate calls are separate views; what the
// service does with a mirror whose credential now names another account
// is the identity verification's.
//
// A method added to Provider or TimeTracker does not compile here until
// it is given its walk or passed through.
func OneCredential(p Provider) Provider {
	if p == nil {
		return nil
	}
	return oneCredential{p}
}

type oneCredential struct{ p Provider }

func (o oneCredential) Capabilities() Capabilities { return o.p.Capabilities() }

func (o oneCredential) Identity(ctx context.Context) (User, error) {
	return trackerapi.Walk(ctx, o.p.Identity)
}

func (o oneCredential) Projects(ctx context.Context) ([]Project, error) {
	return trackerapi.Walk(ctx, o.p.Projects)
}

func (o oneCredential) BoardProjects(ctx context.Context, board BoardID) ([]Project, error) {
	return trackerapi.Walk(ctx, func(ctx context.Context) ([]Project, error) { return o.p.BoardProjects(ctx, board) })
}

func (o oneCredential) IssueTypes(ctx context.Context, project ProjectID) ([]IssueType, error) {
	return trackerapi.Walk(ctx, func(ctx context.Context) ([]IssueType, error) { return o.p.IssueTypes(ctx, project) })
}

func (o oneCredential) ProjectIssueTypes(ctx context.Context, project ProjectID) ([]IssueType, error) {
	return trackerapi.Walk(ctx, func(ctx context.Context) ([]IssueType, error) { return o.p.ProjectIssueTypes(ctx, project) })
}

func (o oneCredential) Boards(ctx context.Context, project ProjectID) ([]Board, error) {
	return trackerapi.Walk(ctx, func(ctx context.Context) ([]Board, error) { return o.p.Boards(ctx, project) })
}

func (o oneCredential) Structure(ctx context.Context, board BoardID, window time.Time) (Structure, error) {
	return trackerapi.Walk(ctx, func(ctx context.Context) (Structure, error) { return o.p.Structure(ctx, board, window) })
}

func (o oneCredential) LinkTypes(ctx context.Context) ([]LinkType, error) {
	return trackerapi.Walk(ctx, o.p.LinkTypes)
}

func (o oneCredential) Fields(ctx context.Context) ([]Field, error) {
	return trackerapi.Walk(ctx, o.p.Fields)
}

func (o oneCredential) ChangedRows(ctx context.Context, q DeltaQuery) (RowPage, error) {
	return trackerapi.Walk(ctx, func(ctx context.Context) (RowPage, error) { return o.p.ChangedRows(ctx, q) })
}

func (o oneCredential) BoardIssues(ctx context.Context, board BoardID, window time.Time) (Listing, error) {
	return trackerapi.Walk(ctx, func(ctx context.Context) (Listing, error) { return o.p.BoardIssues(ctx, board, window) })
}

func (o oneCredential) BoardRows(ctx context.Context, board BoardID, issues []IssueID, rule Eligibility) (Rows, error) {
	return trackerapi.Walk(ctx, func(ctx context.Context) (Rows, error) { return o.p.BoardRows(ctx, board, issues, rule) })
}

func (o oneCredential) NewRows(ctx context.Context, issue IssueID, boards []BoardID) (Placement, error) {
	return trackerapi.Walk(ctx, func(ctx context.Context) (Placement, error) { return o.p.NewRows(ctx, issue, boards) })
}

func (o oneCredential) Cards(ctx context.Context, issues []IssueID) (Cards, error) {
	return trackerapi.Walk(ctx, func(ctx context.Context) (Cards, error) { return o.p.Cards(ctx, issues) })
}

func (o oneCredential) History(ctx context.Context, q HistoryQuery) (History, error) {
	return trackerapi.Walk(ctx, func(ctx context.Context) (History, error) { return o.p.History(ctx, q) })
}

func (o oneCredential) Evidence(ctx context.Context, q EvidenceQuery) (Evidence, error) {
	return trackerapi.Walk(ctx, func(ctx context.Context) (Evidence, error) { return o.p.Evidence(ctx, q) })
}

func (o oneCredential) Detail(ctx context.Context, issue IssueID) (Detail, error) {
	return trackerapi.Walk(ctx, func(ctx context.Context) (Detail, error) { return o.p.Detail(ctx, issue) })
}

func (o oneCredential) Details(ctx context.Context, issues []IssueID) (Details, error) {
	return trackerapi.Walk(ctx, func(ctx context.Context) (Details, error) { return o.p.Details(ctx, issues) })
}

func (o oneCredential) Comments(ctx context.Context, issue IssueID, page PageToken) (CommentPage, error) {
	return trackerapi.Walk(ctx, func(ctx context.Context) (CommentPage, error) { return o.p.Comments(ctx, issue, page) })
}

func (o oneCredential) Moves(ctx context.Context, issue IssueID) ([]Move, error) {
	return trackerapi.Walk(ctx, func(ctx context.Context) ([]Move, error) { return o.p.Moves(ctx, issue) })
}

func (o oneCredential) CreateFields(ctx context.Context, project ProjectID, issueType IssueTypeID) ([]ScreenField, error) {
	return trackerapi.Walk(ctx, func(ctx context.Context) ([]ScreenField, error) { return o.p.CreateFields(ctx, project, issueType) })
}

func (o oneCredential) AssignableUsers(ctx context.Context, scope FieldScope, text string) (UserList, error) {
	return trackerapi.Walk(ctx, func(ctx context.Context) (UserList, error) { return o.p.AssignableUsers(ctx, scope, text) })
}

func (o oneCredential) Choices(ctx context.Context, scope FieldScope, field FieldID, text string) (ChoiceList, error) {
	return trackerapi.Walk(ctx, func(ctx context.Context) (ChoiceList, error) { return o.p.Choices(ctx, scope, field, text) })
}

func (o oneCredential) Lookup(ctx context.Context, keys []string) (KeyLookup, error) {
	return trackerapi.Walk(ctx, func(ctx context.Context) (KeyLookup, error) { return o.p.Lookup(ctx, keys) })
}

func (o oneCredential) Search(ctx context.Context, q SearchQuery) (SearchPage, error) {
	return trackerapi.Walk(ctx, func(ctx context.Context) (SearchPage, error) { return o.p.Search(ctx, q) })
}

func (o oneCredential) Download(ctx context.Context, in DownloadInput) error {
	return trackerapi.WalkOnce(ctx, func(ctx context.Context) error { return o.p.Download(ctx, in) })
}

func (o oneCredential) Create(ctx context.Context, in CreateInput) Result { return o.p.Create(ctx, in) }
func (o oneCredential) Edit(ctx context.Context, in EditInput) Result     { return o.p.Edit(ctx, in) }
func (o oneCredential) Move(ctx context.Context, in MoveInput) Result     { return o.p.Move(ctx, in) }
func (o oneCredential) SetEstimate(ctx context.Context, in EstimateInput) Result {
	return o.p.SetEstimate(ctx, in)
}
func (o oneCredential) Rank(ctx context.Context, in RankInput) Result { return o.p.Rank(ctx, in) }
func (o oneCredential) MoveToSprint(ctx context.Context, in SprintMoveInput) Result {
	return o.p.MoveToSprint(ctx, in)
}
func (o oneCredential) CreateSprint(ctx context.Context, in SprintInput) Result {
	return o.p.CreateSprint(ctx, in)
}
func (o oneCredential) StartSprint(ctx context.Context, in SprintStart) Result {
	return o.p.StartSprint(ctx, in)
}
func (o oneCredential) CompleteSprint(ctx context.Context, in SprintCompletion) Result {
	return o.p.CompleteSprint(ctx, in)
}
func (o oneCredential) EditSprint(ctx context.Context, in SprintEdit) Result {
	return o.p.EditSprint(ctx, in)
}
func (o oneCredential) AddComment(ctx context.Context, in CommentInput) Result {
	return o.p.AddComment(ctx, in)
}
func (o oneCredential) EditComment(ctx context.Context, in CommentEdit) Result {
	return o.p.EditComment(ctx, in)
}
func (o oneCredential) DeleteComment(ctx context.Context, in CommentDelete) Result {
	return o.p.DeleteComment(ctx, in)
}
func (o oneCredential) Link(ctx context.Context, in LinkInput) Result     { return o.p.Link(ctx, in) }
func (o oneCredential) Unlink(ctx context.Context, in UnlinkInput) Result { return o.p.Unlink(ctx, in) }
func (o oneCredential) Attach(ctx context.Context, in AttachInput) Result { return o.p.Attach(ctx, in) }

func (o oneCredential) Time() TimeTracker {
	t := o.p.Time()
	if t == nil {
		return nil
	}
	return oneCredentialTime{t}
}

type oneCredentialTime struct{ t TimeTracker }

func (o oneCredentialTime) Worklogs(ctx context.Context, q WorklogQuery) ([]Worklog, error) {
	return trackerapi.Walk(ctx, func(ctx context.Context) ([]Worklog, error) { return o.t.Worklogs(ctx, q) })
}

func (o oneCredentialTime) AddWorklogs(ctx context.Context, ins []WorklogInput) Result {
	return o.t.AddWorklogs(ctx, ins)
}

func (o oneCredentialTime) EditWorklog(ctx context.Context, in WorklogEdit) Result {
	return o.t.EditWorklog(ctx, in)
}

func (o oneCredentialTime) DeleteWorklog(ctx context.Context, in WorklogDelete) Result {
	return o.t.DeleteWorklog(ctx, in)
}

func (o oneCredential) Poker() Poker {
	p := o.p.Poker()
	if p == nil {
		return nil
	}
	return oneCredentialPoker{p}
}

type oneCredentialPoker struct{ p Poker }

func (o oneCredentialPoker) Support(ctx context.Context) (bool, error) {
	return trackerapi.Walk(ctx, o.p.Support)
}

func (o oneCredentialPoker) Read(ctx context.Context, issue IssueID) (PokerRound, error) {
	return trackerapi.Walk(ctx, func(ctx context.Context) (PokerRound, error) { return o.p.Read(ctx, issue) })
}

func (o oneCredentialPoker) Vote(ctx context.Context, in PokerVoteInput) Result {
	return o.p.Vote(ctx, in)
}
func (o oneCredentialPoker) Reveal(ctx context.Context, issue IssueID) Result {
	return o.p.Reveal(ctx, issue)
}
func (o oneCredentialPoker) Clear(ctx context.Context, issue IssueID) Result {
	return o.p.Clear(ctx, issue)
}
