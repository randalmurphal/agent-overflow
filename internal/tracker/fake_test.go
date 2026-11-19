package tracker

import (
	"context"
	"time"
)

// fakeProvider and fakeTime implement the interfaces in full, so the
// package does not compile when a method's signature names a type that
// cannot be built or returned. They are not run.
var (
	_ Provider    = fakeProvider{}
	_ TimeTracker = fakeTime{}
)

type fakeProvider struct{}

func (fakeProvider) Capabilities() Capabilities                         { return Capabilities{} }
func (fakeProvider) Identity(context.Context) (User, error)             { return User{}, nil }
func (fakeProvider) Projects(context.Context) ([]Project, error)        { return nil, nil }
func (fakeProvider) LinkTypes(context.Context) ([]LinkType, error)      { return nil, nil }
func (fakeProvider) Boards(context.Context, ProjectID) ([]Board, error) { return nil, nil }
func (fakeProvider) IssueTypes(context.Context, ProjectID) ([]IssueType, error) {
	return nil, nil
}
func (fakeProvider) Structure(context.Context, BoardID, time.Time) (Structure, error) {
	return Structure{}, nil
}
func (fakeProvider) ChangedRows(context.Context, DeltaQuery) (RowPage, error) {
	return RowPage{}, nil
}
func (fakeProvider) BoardIssues(context.Context, BoardID, time.Time) (Listing, error) {
	return Listing{}, nil
}
func (fakeProvider) BoardRows(context.Context, BoardID, []IssueID) (Rows, error) {
	return Rows{}, nil
}
func (fakeProvider) Cards(context.Context, []IssueID) (Cards, error)        { return Cards{}, nil }
func (fakeProvider) History(context.Context, HistoryQuery) (History, error) { return History{}, nil }
func (fakeProvider) Detail(context.Context, IssueID) (Detail, error)        { return Detail{}, nil }
func (fakeProvider) Moves(context.Context, IssueID) ([]Move, error)         { return nil, nil }
func (fakeProvider) Comments(context.Context, IssueID, PageToken) (CommentPage, error) {
	return CommentPage{}, nil
}
func (fakeProvider) CreateFields(context.Context, ProjectID, IssueTypeID) ([]Field, error) {
	return nil, nil
}
func (fakeProvider) AssignableUsers(context.Context, FieldScope, string) ([]User, error) {
	return nil, nil
}
func (fakeProvider) Choices(context.Context, FieldScope, FieldID, string) ([]Choice, error) {
	return nil, nil
}
func (fakeProvider) Create(context.Context, CreateInput) Result              { return Result{} }
func (fakeProvider) Edit(context.Context, EditInput) Result                  { return Result{} }
func (fakeProvider) Move(context.Context, MoveInput) Result                  { return Result{} }
func (fakeProvider) SetEstimate(context.Context, EstimateInput) Result       { return Result{} }
func (fakeProvider) Rank(context.Context, RankInput) Result                  { return Result{} }
func (fakeProvider) MoveToSprint(context.Context, SprintMoveInput) Result    { return Result{} }
func (fakeProvider) CreateSprint(context.Context, SprintInput) Result        { return Result{} }
func (fakeProvider) StartSprint(context.Context, SprintStart) Result         { return Result{} }
func (fakeProvider) CompleteSprint(context.Context, SprintCompletion) Result { return Result{} }
func (fakeProvider) EditSprint(context.Context, SprintEdit) Result           { return Result{} }
func (fakeProvider) AddComment(context.Context, CommentInput) Result         { return Result{} }
func (fakeProvider) EditComment(context.Context, CommentEdit) Result         { return Result{} }
func (fakeProvider) DeleteComment(context.Context, CommentDelete) Result     { return Result{} }
func (fakeProvider) Link(context.Context, LinkInput) Result                  { return Result{} }
func (fakeProvider) Attach(context.Context, AttachInput) Result              { return Result{} }
func (fakeProvider) Lookup(context.Context, []string) (KeyLookup, error)     { return KeyLookup{}, nil }
func (fakeProvider) Search(context.Context, SearchQuery) (SearchPage, error) {
	return SearchPage{}, nil
}
func (fakeProvider) Download(context.Context, DownloadInput) error { return nil }
func (fakeProvider) Unlink(context.Context, UnlinkInput) Result    { return Result{} }
func (fakeProvider) Time() TimeTracker                             { return fakeTime{} }

type fakeTime struct{}

func (fakeTime) Worklogs(context.Context, WorklogQuery) ([]Worklog, error) { return nil, nil }
func (fakeTime) AddWorklog(context.Context, WorklogInput) Result           { return Result{} }
func (fakeTime) EditWorklog(context.Context, WorklogEdit) Result           { return Result{} }
func (fakeTime) DeleteWorklog(context.Context, WorklogDelete) Result       { return Result{} }
