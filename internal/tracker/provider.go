package tracker

import (
	"context"
	"time"
)

// Capabilities say what a connection's tracker supports beyond what every
// tracker does. A read a tracker cannot serve returns nothing for it; a
// write it cannot make is refused with ReasonUnsupported before anything
// is sent. Time tracking is Provider.Time, not a flag.
type Capabilities struct {
	// IssueTypes: issues have a type, picked on create (Jira).
	IssueTypes bool
	// CustomFields: issues have fields beyond the card's and the
	// description, which project setup can pin (Jira).
	CustomFields bool
	// MoveFields: a move can take and require fields (Jira transition
	// screens).
	MoveFields bool
	// SprintWrites: sprints are created, started, edited and completed
	// through the API (Jira; Linear generates cycles).
	SprintWrites bool
	// MaxAttachmentBytes is the largest file the tracker stores, zero for
	// a tracker whose issues hold no uploaded files. Above zero, Markdown
	// bodies may embed local images too (Jira).
	MaxAttachmentBytes int64
	// DetailMovesUpdated: every change to the issue that its Detail shows
	// (a field, the description, a comment, a link, an attachment, a
	// status change) moves the issue's update time, and Detail reads
	// nothing it holds of the issue before the read that gives
	// Card.Updated has returned, so a stored Detail whose card has the
	// update time of the issue's card read now holds every such change.
	// It does not cover what belongs to another issue or to the
	// configuration: a linked issue's key, title and status, and the moves
	// and field metadata a workflow or screen edit changes. trackerapp
	// shows a linked issue from the mirror's card of it when the mirror
	// keeps one, and reads a stored Detail again once it is older than the
	// board structure's cadence, so a linked issue the mirror does not keep
	// and the moves and field metadata are at most that old. Jira,
	// measured: a comment, a link, an attachment and a worklog each move
	// `updated`. Linear has none: a comment leaves `updatedAt` as it was.
	// See docs/references/tracker-api-measurements.md.
	DetailMovesUpdated bool
	// HistorySettle is how long after an update the tracker may still be
	// recording it in an issue's history: a history read is final for the
	// update when its IssueHistory.CompleteTo is at or after the update,
	// or when it began at least this long after an earlier answer for the
	// same update arrived. The one copy of each provider's margin.
	HistorySettle time.Duration
	// Evidence reports that Provider.Evidence reads who commented on and
	// logged time on an issue, the time from the connection's time source
	// (Provider.Time): Jira does; Linear, whose work credit does not
	// count, does not.
	Evidence bool
}

// FieldScope is where a field's values are asked for: an existing issue,
// one of its moves, or a create in a project with an issue type. The zero
// FieldScope names neither and is refused.
type FieldScope struct {
	issue     IssueID
	move      MoveID
	project   ProjectID
	issueType IssueTypeID
}

// OnIssue is the scope of an existing issue's fields.
func OnIssue(issue IssueID) FieldScope { return FieldScope{issue: issue} }

// OnMove is the scope of the fields move's screen takes on issue
// (Move.Fields), whose values can differ from the issue's own.
func OnMove(issue IssueID, move MoveID) FieldScope { return FieldScope{issue: issue, move: move} }

// OnCreate is the scope of a create's fields in project with issueType,
// which is empty without Capabilities.IssueTypes.
func OnCreate(project ProjectID, issueType IssueTypeID) FieldScope {
	return FieldScope{project: project, issueType: issueType}
}

// Issue is the issue of an OnIssue or an OnMove scope.
func (s FieldScope) Issue() (IssueID, bool) { return s.issue, s.issue != "" }

// Move is the move of an OnMove scope.
func (s FieldScope) Move() (MoveID, bool) { return s.move, s.move != "" }

// Create is the project and issue type of an OnCreate scope.
func (s FieldScope) Create() (ProjectID, IssueTypeID, bool) {
	return s.project, s.issueType, s.issue == "" && s.project != ""
}

// Provider is one connection's tracker. Its calls take neutral inputs and
// return neutral values; a read fails with the transport's typed errors
// unchanged (errors.As finds them), and a write reports every part it
// touched in its Result instead of returning one error. A Provider is
// safe for concurrent use and holds no state the caller must release:
// the caller owns the clients under it.
//
// The window arguments are the start of the mirror's history window.
type Provider interface {
	// Capabilities are what the connection's tracker supports.
	Capabilities() Capabilities
	// Identity is the user the credential belongs to.
	Identity(ctx context.Context) (User, error)

	// Projects are the projects the user can see.
	Projects(ctx context.Context) ([]Project, error)
	// BoardProjects are the projects board shows: those a ticket made
	// from it can be made in.
	BoardProjects(ctx context.Context, board BoardID) ([]Project, error)
	// IssueTypes are the types an issue can be created with in project;
	// none without Capabilities.IssueTypes.
	IssueTypes(ctx context.Context, project ProjectID) ([]IssueType, error)
	// ProjectIssueTypes are the issue types project has, which a user
	// who can see the project reads whatever it may create; none without
	// Capabilities.IssueTypes.
	ProjectIssueTypes(ctx context.Context, project ProjectID) ([]IssueType, error)
	// Boards are project's boards.
	Boards(ctx context.Context, project ProjectID) ([]Board, error)
	// Structure is board's structure, read from the tracker, with its
	// sprints that are open or closed at or after window.
	Structure(ctx context.Context, board BoardID, window time.Time) (Structure, error)
	// LinkTypes are the kinds of link the tracker offers.
	LinkTypes(ctx context.Context) ([]LinkType, error)
	// Fields are the fields a project's setup can pin for the site's
	// tickets: every field of the site that an issue's Detail lists, by
	// the rule Detail lists them by, with its name and kind (no value);
	// one of KindRichText is a section of the ticket. Refused wrapping
	// ErrUnsupported without Capabilities.CustomFields.
	Fields(ctx context.Context) ([]Field, error)

	// ChangedRows is the next page of a board's changed rows.
	ChangedRows(ctx context.Context, q DeltaQuery) (RowPage, error)
	// BoardIssues lists the IDs on board within the window.
	BoardIssues(ctx context.Context, board BoardID, window time.Time) (Listing, error)
	// BoardRows are board's rows of issues that rule lists (Listing.Rule):
	// an issue the board holds that rule leaves out is Missing, as the
	// listing left it out. A rule the tracker refuses fails it with
	// ErrRuleRefused.
	BoardRows(ctx context.Context, board BoardID, issues []IssueID, rule Eligibility) (Rows, error)
	// NewRows places issue, which a create just made, on boards, after
	// CheckNewRows: its row on each board that holds it, as that board's
	// own list will deliver it, read without waiting for a list that
	// shows a new issue late. A board the tracker cannot judge now is
	// Unknown, with why; a failure of the whole read fails the call.
	NewRows(ctx context.Context, issue IssueID, boards []BoardID) (Placement, error)
	// Cards are the cards of issues, read with no board.
	Cards(ctx context.Context, issues []IssueID) (Cards, error)
	// History is the status, sprint, estimate and assignee changes of the
	// issues q names. A failure returns no history.
	History(ctx context.Context, q HistoryQuery) (History, error)
	// Evidence is who commented on and logged time on the issues q names,
	// after q.Check: each source asked of each issue is read whole or, when
	// it failed for that issue alone (the tracker did not return the issue
	// or said it failed, an answer too large even alone, an author not
	// confirmed a person's), in Evidence.Failed with why. A failure every
	// issue shares (the tracker unreachable, the credential refused, a
	// rate limit) fails the call; the Evidence it returns holds the sources
	// read whole before it. Without Capabilities.Evidence it fails wrapping
	// ErrUnsupported before any request.
	Evidence(ctx context.Context, q EvidenceQuery) (Evidence, error)

	// Detail is one issue's detail.
	Detail(ctx context.Context, issue IssueID) (Detail, error)
	// Details are the details of issues, each read as Detail reads it and
	// all read together, in as few requests as the tracker allows: an
	// issue the tracker does not return is in Details.Missing rather than
	// failing the read, and any other failure fails the whole read. Each
	// issue is named once.
	Details(ctx context.Context, issues []IssueID) (Details, error)
	// PullRequests are the pull requests the tracker's forge
	// integrations link to issue, from Jira's development panel, whose
	// endpoints are undocumented. It reads each integration apart: the
	// rows read are returned beside errors.Join of each failed read's
	// error, so one integration's failure leaves the others' rows. A site
	// that keeps no such data fails wrapping ErrNoDevelopmentData; Linear
	// fails wrapping ErrUnsupported before any request.
	PullRequests(ctx context.Context, issue IssueID) ([]LinkedPullRequest, error)
	// Comments is a page of issue's comments, newest first.
	Comments(ctx context.Context, issue IssueID, page PageToken) (CommentPage, error)
	// Moves are the moves issue's workflow allows now.
	Moves(ctx context.Context, issue IssueID) ([]Move, error)
	// CreateFields are the fields a create in project with issueType takes
	// besides the title, those it requires marked Required, each with an
	// empty Value: a field left unset takes the tracker's default.
	CreateFields(ctx context.Context, project ProjectID, issueType IssueTypeID) ([]ScreenField, error)
	// AssignableUsers are the users who can be assigned in scope whose
	// name or email starts with text. Empty text returns those the
	// tracker offers unasked, which may be none.
	AssignableUsers(ctx context.Context, scope FieldScope, text string) (UserList, error)
	// Choices are the values field takes in scope whose name starts with
	// text: options, or users for a user field. Empty text returns those
	// the tracker offers unasked, which may be none.
	Choices(ctx context.Context, scope FieldScope, field FieldID, text string) (ChoiceList, error)
	// Lookup finds the issues keys name, after CheckLookupKeys; it
	// splits the keys into as many requests as its tracker needs.
	Lookup(ctx context.Context, keys []string) (KeyLookup, error)
	// Search is one page of the cards q's text matches, after q.Check.
	Search(ctx context.Context, q SearchQuery) (SearchPage, error)
	// Download streams one of an issue's attachments, after in.Check. An
	// attachment that is not the issue's fails wrapping ErrInvalidInput,
	// a tracker without Capabilities.MaxAttachmentBytes fails wrapping
	// ErrUnsupported before any request, and a file over that size, which
	// the tracker does not store, fails with the transport's error once
	// that much has been written.
	Download(ctx context.Context, in DownloadInput) error

	// Create makes an issue. Its StepCreate part names the new issue when
	// the tracker confirmed it, and no issue when the answer was lost.
	Create(ctx context.Context, in CreateInput) Result
	// Edit changes fields of an issue.
	Edit(ctx context.Context, in EditInput) Result
	// Move moves an issue to the status of one of its moves.
	Move(ctx context.Context, in MoveInput) Result
	// SetEstimate sets an issue's estimate on a board.
	SetEstimate(ctx context.Context, in EstimateInput) Result
	// Rank places issues between two neighbors on a board, one part per
	// issue.
	Rank(ctx context.Context, in RankInput) Result
	// MoveToSprint moves issues into a sprint or to a board's backlog, one
	// part per issue.
	MoveToSprint(ctx context.Context, in SprintMoveInput) Result
	// CreateSprint, StartSprint, CompleteSprint and EditSprint change a
	// board's sprints; refused without Capabilities.SprintWrites.
	// CompleteSprint has a part for each unfinished issue it moves and one
	// for the close.
	CreateSprint(ctx context.Context, in SprintInput) Result
	StartSprint(ctx context.Context, in SprintStart) Result
	CompleteSprint(ctx context.Context, in SprintCompletion) Result
	EditSprint(ctx context.Context, in SprintEdit) Result
	// AddComment, EditComment and DeleteComment change an issue's
	// comments.
	AddComment(ctx context.Context, in CommentInput) Result
	EditComment(ctx context.Context, in CommentEdit) Result
	DeleteComment(ctx context.Context, in CommentDelete) Result
	// Link links two issues.
	Link(ctx context.Context, in LinkInput) Result
	// Unlink removes one link as one StepLink part about in.Issue that
	// names both issues' detail.
	Unlink(ctx context.Context, in UnlinkInput) Result
	// Attach uploads files to an issue as one StepUpload part, reading
	// each file's content as it is sent; refused without
	// Capabilities.MaxAttachmentBytes.
	Attach(ctx context.Context, in AttachInput) Result

	// Time is the connection's logged time; nil for a tracker with no time
	// tracking.
	Time() TimeTracker
	// Poker is the connection's planning poker; nil for a tracker that
	// cannot have it.
	Poker() Poker
}
