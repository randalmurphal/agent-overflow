package trackerapp

import (
	"context"
	"fmt"
	"slices"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// The writes, one method per provider write, each on connection id
// through Service.write: the outcomes as the tracker answered them, with
// what the mirror could not be refreshed with beside them. A request the
// conversion refuses fails with ErrInvalidRequest before anything is read
// or sent; a connection that is off, removed or a Tempo connection fails
// with ErrDisabled, ErrNotFound or ErrNoProvider; a provider that answers
// with a result it may not fails with ErrBadResult. Every other outcome,
// a refusal included, is in the view.

// call1 is a writeOp of the provider write w with in.
func call1[T any](in T, w func(tracker.Provider, context.Context, T) tracker.Result) writeOp {
	return writeOp{call: func(r *run) tracker.Result { return w(r.p, r.ctx, in) }}
}

// Create makes an issue from a section of in.Board, its sprint's or the
// backlog's, landing after the section's last row as the mirror holds it
// (sectionFoot). The issue made is read through that board; a create of unknown outcome is reported as that and
// not retried, and the board's tick runs at once (through its pump when
// one is live), since an issue it brings in is the only evidence.
func (s *Service) Create(ctx context.Context, id string, in CreateRequest) (WriteView, error) {
	m, err := in.model()
	if err != nil {
		return WriteView{}, err
	}
	op := writeOp{board: m.Board, additive: createOf(in), call: func(r *run) tracker.Result {
		after, err := s.sectionFoot(r.root, m.Board, m.Sprint)
		if err != nil {
			return tracker.Result{Parts: []tracker.Part{{Step: tracker.StepCreate, Outcome: tracker.NotSent(err)}}}
		}
		m.After = after
		return r.p.Create(r.ctx, m)
	}}
	return s.write(ctx, id, op)
}

// sectionFoot is the row a ticket made in sprint of board (the backlog
// for none) lands after: the section's last row as the mirror holds it,
// with no tracker request; zero when the section holds none.
func (s *Service) sectionFoot(root int64, board tracker.BoardID, sprint tracker.SprintID) (tracker.RowPlace, error) {
	issue, order, ok, err := s.store.LastMirrorPlanRow(root, string(board), string(sprint))
	if err != nil || !ok {
		return tracker.RowPlace{}, err
	}
	return tracker.RowPlace{Issue: tracker.IssueID(issue), Order: tracker.OrderKey(order)}, nil
}

// Edit changes fields of an issue; the first write to it in ctx's
// session takes the session's read of it (write_session.go).
func (s *Service) Edit(ctx context.Context, id string, in EditRequest) (WriteView, error) {
	m, err := in.model()
	if err != nil {
		return WriteView{}, err
	}
	m.Read = sessionOf(ctx).read(id, m.Issue)
	return s.write(ctx, id, call1(m, tracker.Provider.Edit))
}

// Move moves an issue to the status of one of its moves; the first write
// to it in ctx's session takes the session's read of it.
func (s *Service) Move(ctx context.Context, id string, in MoveRequest) (WriteView, error) {
	m, err := in.model()
	if err != nil {
		return WriteView{}, err
	}
	m.Read = sessionOf(ctx).read(id, m.Issue)
	return s.write(ctx, id, call1(m, tracker.Provider.Move))
}

// SetEstimate sets an issue's estimate on a board; the first write to the
// issue in ctx's session takes the session's read of its row there.
func (s *Service) SetEstimate(ctx context.Context, id string, in EstimateRequest) (WriteView, error) {
	m := in.model()
	m.Read = sessionOf(ctx).rowRead(id, m.Board, m.Issue)
	return s.write(ctx, id, call1(m, tracker.Provider.SetEstimate))
}

// Rank places issues between two neighbors on a board, a part per issue.
func (s *Service) Rank(ctx context.Context, id string, in RankRequest) (WriteView, error) {
	return s.write(ctx, id, call1(in.model(), tracker.Provider.Rank))
}

// MoveToSprint moves issues into a sprint or to a board's backlog, a part
// per issue.
func (s *Service) MoveToSprint(ctx context.Context, id string, in SprintMoveRequest) (WriteView, error) {
	return s.write(ctx, id, call1(in.model(), tracker.Provider.MoveToSprint))
}

// CreateSprint makes a future sprint on a board.
func (s *Service) CreateSprint(ctx context.Context, id string, in SprintRequest) (WriteView, error) {
	return s.write(ctx, id, call1(in.model(), tracker.Provider.CreateSprint))
}

// StartSprint starts a future sprint.
func (s *Service) StartSprint(ctx context.Context, id string, in SprintStartRequest) (WriteView, error) {
	return s.write(ctx, id, call1(in.model(), tracker.Provider.StartSprint))
}

// CompleteSprint closes an active sprint once its unfinished issues
// moved, a part per issue moved and one for the close.
func (s *Service) CompleteSprint(ctx context.Context, id string, in SprintCompleteRequest) (WriteView, error) {
	return s.write(ctx, id, call1(in.model(), tracker.Provider.CompleteSprint))
}

// EditSprint changes a sprint's name, goal or dates.
func (s *Service) EditSprint(ctx context.Context, id string, in SprintEditRequest) (WriteView, error) {
	return s.write(ctx, id, call1(in.model(), tracker.Provider.EditSprint))
}

// AddComment adds a comment to an issue; in ctx's session it is judged on
// the project the session's last key lookup found the issue in while no
// write but a comment was sent on the connection since (projectFor), and
// otherwise on the issue's project the provider reads with it.
func (s *Service) AddComment(ctx context.Context, id string, in CommentRequest) (WriteView, error) {
	m, err := in.model()
	if err != nil {
		return WriteView{}, err
	}
	m.Project = sessionOf(ctx).projectFor(id, m.Issue)
	op := call1(m, tracker.Provider.AddComment)
	op.additive = commentOf(m.Issue, in.Markdown)
	return s.write(ctx, id, op)
}

// EditComment replaces a comment's body.
func (s *Service) EditComment(ctx context.Context, id string, in CommentEditRequest) (WriteView, error) {
	return s.write(ctx, id, call1(in.model(), tracker.Provider.EditComment))
}

// DeleteComment deletes a comment.
func (s *Service) DeleteComment(ctx context.Context, id string, in CommentDeleteRequest) (WriteView, error) {
	return s.write(ctx, id, call1(in.model(), tracker.Provider.DeleteComment))
}

// Link links two issues.
func (s *Service) Link(ctx context.Context, id string, in LinkRequest) (WriteView, error) {
	return s.write(ctx, id, call1(in.model(), tracker.Provider.Link))
}

// Unlink removes one link between two issues.
func (s *Service) Unlink(ctx context.Context, id string, in UnlinkRequest) (WriteView, error) {
	return s.write(ctx, id, call1(in.model(), tracker.Provider.Unlink))
}

// Attach uploads files to an issue as one part. Each file's Content is
// read once, during the call; the caller closes it after.
func (s *Service) Attach(ctx context.Context, id string, issue string, files []tracker.File) (WriteView, error) {
	return s.write(ctx, id, call1(tracker.AttachInput{Issue: tracker.IssueID(issue), Files: files}, tracker.Provider.Attach))
}

// timeCall is a writeOp of the time tracker's write w about issue, refused
// with ReasonUnsupported on a tracker with no time tracking.
func timeCall[T any](issue tracker.IssueID, in T, w func(tracker.TimeTracker, context.Context, T) tracker.Result) writeOp {
	return writeOp{call: func(r *run) tracker.Result {
		tt := r.p.Time()
		if tt == nil {
			return tracker.Refuse(tracker.StepWorklog, tracker.Refusal{Reason: tracker.ReasonUnsupported,
				Message: "this tracker has no time tracking", Err: tracker.ErrUnsupported}, issue)
		}
		return w(tt, r.ctx, in)
	}}
}

// AddWorklog logs time on an issue as the credential's own user.
func (s *Service) AddWorklog(ctx context.Context, id string, in WorklogRequest) (WriteView, error) {
	m, err := in.model()
	if err != nil {
		return WriteView{}, err
	}
	return s.write(ctx, id, timeCall(m.Issue, []tracker.WorklogInput{m}, tracker.TimeTracker.AddWorklogs))
}

// EditWorklog changes a worklog, as read, on its issue.
func (s *Service) EditWorklog(ctx context.Context, id string, in WorklogEditRequest) (WriteView, error) {
	m, err := in.model()
	if err != nil {
		return WriteView{}, err
	}
	return s.write(ctx, id, timeCall(m.From.Issue, m, tracker.TimeTracker.EditWorklog))
}

// DeleteWorklog deletes a worklog, as read.
func (s *Service) DeleteWorklog(ctx context.Context, id string, in WorklogDeleteRequest) (WriteView, error) {
	m, err := in.model()
	if err != nil {
		return WriteView{}, err
	}
	return s.write(ctx, id, timeCall(m.From.Issue, m, tracker.TimeTracker.DeleteWorklog))
}

// WorklogDays is an author's worklogs of a range of days, by day and ID,
// as the mirror holds them, at most 1,000. Unread are the days of the
// range, sorted, whose worklogs of the author this computer has never
// read: a screen shows no total for them. Without More every other day
// was read, so one with no worklogs is 0h. More reports that the range
// holds more worklogs than the answer: only the days before the last
// worklog's day are then whole, and the last worklog's day and every
// later read day are read again from that day before they are totaled.
// Failure is why a refresh could not read them from the tracker: they
// are then as the mirror held them before.
// Drafted are the author's time draft entries of the range (time_draft.go),
// every state, read in the same snapshot; a computer from before the time
// draft answers none.
type WorklogDays struct {
	Worklogs []WorklogView   `json:"worklogs"`
	More     bool            `json:"more,omitempty"`
	Unread   []string        `json:"unread,omitempty"`
	Failure  *FailureView    `json:"failure,omitempty"`
	Drafted  []TimeEntryView `json:"drafted,omitempty"`
	// readAt is when each read day was last read, Unix milliseconds.
	readAt map[string]int64
}

// worklogReadSpan is the most days one mirror read of worklogs covers: a
// year, so its unread days stay a bounded list.
const worklogReadSpan = 366

// Worklogs is author's worklogs on connection id whose day is from to to
// (YYYY-MM-DD, both included, at most worklogReadSpan days) as the mirror
// holds them, with no tracker call, and which of those days it has read;
// RefreshWorklogs reads them from the tracker first. The connection and
// the mirror are read in one snapshot: one that is removed, turned off or
// a Tempo connection is refused as a refresh refuses it (ErrNotFound,
// ErrDisabled, ErrNoProvider), and one with no time tracking with an
// error wrapping tracker.ErrUnsupported. A connection whose mirror holds
// nothing yet answers every day unread.
func (s *Service) Worklogs(id, author, from, to string) (WorklogDays, error) {
	first, last, err := worklogRange(author, from, to)
	if err != nil {
		return WorklogDays{}, err
	}
	n := daysBetween(first, last) + 1
	if n > worklogReadSpan {
		return WorklogDays{}, fmt.Errorf("%w: worklogs are read at most %d days at a time", ErrInvalidRequest, worklogReadSpan)
	}
	if err := s.enter(); err != nil {
		return WorklogDays{}, err
	}
	defer s.leave()
	snap, err := s.store.ReadMirrorWorklogDays(id, author, from, to)
	switch {
	case err != nil:
		return WorklogDays{}, fmt.Errorf("trackerapp: read the worklogs of %s: %w", author, err)
	case !snap.Found:
		return WorklogDays{}, fmt.Errorf("%w %s", ErrNotFound, id)
	}
	if err := usable(snap.Connection); err != nil {
		return WorklogDays{}, err
	}
	if !hasTimeTracking(snap.Connection.Kind) {
		return WorklogDays{}, fmt.Errorf("trackerapp: connection %s has no time tracking: %w", id, tracker.ErrUnsupported)
	}
	out := WorklogDays{Worklogs: make([]WorklogView, 0, len(snap.Logs)), More: snap.More, Drafted: timeEntryViews(snap.Entries),
		readAt: make(map[string]int64, len(snap.ReadDays))}
	for i, d := range snap.ReadDays {
		out.readAt[d] = snap.ReadAt[i]
	}
	for _, m := range snap.Logs {
		w, err := worklogOf(m)
		if err != nil {
			return WorklogDays{}, err
		}
		out.Worklogs = append(out.Worklogs, worklogView(w))
	}
	for i := range n {
		d := dayTime(first).AddDate(0, 0, i).Format(time.DateOnly)
		if _, read := slices.BinarySearch(snap.ReadDays, d); !read {
			out.Unread = append(out.Unread, d)
		}
	}
	return out, nil
}

// hasTimeTracking reports whether a connection of kind has a time
// tracker, as its provider's Time answers: Jira's own worklogs or its
// Tempo connection's, and none on Linear. The mirror read decides by kind
// because it makes no provider, which would read the key file.
func hasTimeTracking(kind string) bool { return kind == store.TrackerKindJira }

// RefreshWorklogs reads author's worklogs on connection id whose day is
// from to to, at most worklogSpan days, from the connection's time
// tracker through the worklogs resource (Service.write with no write),
// makes them the mirror's, and returns them as the mirror then holds
// them. A read that failed is settled as the connection's problem and
// returned as the answer's Failure. A connection with no time tracking
// is refused with an error wrapping tracker.ErrUnsupported.
func (s *Service) RefreshWorklogs(ctx context.Context, id, author, from, to string) (WorklogDays, error) {
	first, last, err := worklogRange(author, from, to)
	if err != nil {
		return WorklogDays{}, err
	}
	if daysBetween(first, last) >= worklogSpan {
		return WorklogDays{}, fmt.Errorf("%w: a worklog refresh reads at most %d days", ErrInvalidRequest, worklogSpan)
	}
	n := daysBetween(first, last)
	days := make([]tracker.Resource, 0, n+1)
	for i := range n + 1 {
		d, err := tracker.DateOf(dayTime(first).AddDate(0, 0, i))
		if err != nil {
			return WorklogDays{}, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
		}
		days = append(days, tracker.WorklogsOf(tracker.UserID(author), d))
	}
	view, err := s.write(ctx, id, writeOp{also: days, unable: func(p tracker.Provider) error {
		if p.Time() == nil {
			return fmt.Errorf("trackerapp: connection %s has no time tracking: %w", id, tracker.ErrUnsupported)
		}
		return nil
	}})
	switch {
	case err != nil:
		return WorklogDays{}, err
	case view.Ended:
		return WorklogDays{}, fmt.Errorf("%w: %s was turned off while its worklogs were read", ErrDisabled, id)
	}
	out, err := s.Worklogs(id, author, from, to)
	if err == nil && len(view.Stale) > 0 {
		f := view.Stale[0].Failure
		out.Failure = &f
	}
	return out, err
}

// worklogRange checks an author's days from to to.
func worklogRange(author, from, to string) (tracker.Date, tracker.Date, error) {
	first, err := parseDay(from)
	if err != nil {
		return tracker.Date{}, tracker.Date{}, err
	}
	last, err := parseDay(to)
	if err != nil {
		return tracker.Date{}, tracker.Date{}, err
	}
	if author == "" || last.Compare(first) < 0 {
		return tracker.Date{}, tracker.Date{}, fmt.Errorf("%w: worklogs are read for an author, from a day to one no earlier", ErrInvalidRequest)
	}
	return first, last, nil
}
