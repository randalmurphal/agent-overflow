package trackerapp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// An agent's writes. A write an agent makes through the tracker tools
// carries who made it as a context value (ByAgent), and Service.write is
// its one reader: it records each part that was applied or may have been
// in tracker_agent_writes (store.AgentWrite), after the refresh stored
// what changed and before the open tickets settle, so the views they
// build carry the marks. A person's writes carry no agent and record
// nothing.
//
// A comment or a create adds something a second send would add again.
// What this keeps true: a comment or a create an agent sends is one
// logical write per thread and digest within the hour, and exactly one
// caller holds the right to send it at a time. The check for an earlier
// write and the claim of the send are one step, under agentSends' lock and
// one store transaction (claimAgentWrite). A second caller finds one of
// three things: the write applied, and is answered with the earlier
// result with nothing sent (WriteView.Repeat); the write in flight in this
// process, and is refused (ErrAgentWriteInFlight); or the write
// interrupted (sent with no sender alive, a row an earlier run of the app
// left included, or unknown), and takes the claim and looks for it on the
// tracker (additive.find). A found comment is the agent's only when the
// connection's account wrote it, inside the window around the time the
// write was sent, and its body equals the text sent once both are reduced
// to letters and digits; a found create matches by title and creation
// time in that window. A lookup that finds nothing sends under the same
// claim; one that cannot be made sends nothing and leaves the outcome
// unknown (ErrAgentRepeatUnchecked). The digest names the ticket by
// tracker kind and site, never by the local connection ID, and a create's
// digest covers what the create request sends, not the steps made after
// it, so a call again after one of those was refused finds the ticket.

// agentKey is the context key of the agent a write is made for.
type agentKey struct{}

// agentCall is who makes a write: the thread whose agent called, and the
// tool call it is part of.
type agentCall struct{ thread, call string }

// ByAgent is ctx for the writes of tool call call by thread's agent.
func ByAgent(ctx context.Context, thread, call string) context.Context {
	return context.WithValue(ctx, agentKey{}, agentCall{thread: thread, call: call})
}

// ErrAgentWriteInFlight is an agent's comment or create that an earlier
// call of the same thread is sending now: nothing was sent.
var ErrAgentWriteInFlight = errors.New("trackerapp: an earlier call is sending the same write")

// ErrAgentRepeatUnchecked is an agent's comment or create whose earlier
// attempt may have been made and could not be looked for: nothing was
// sent.
var ErrAgentRepeatUnchecked = errors.New("trackerapp: an earlier attempt of the write could not be looked for")

// agentRepeatWindow is how long a recorded comment or create answers a
// repeat of it.
const agentRepeatWindow = time.Hour

// A comment or create whose answer never came is looked for from
// repeatBefore before the time it was sent, for the clocks' difference,
// to repeatAfter after.
const (
	repeatBefore = time.Minute
	repeatAfter  = 2 * time.Minute
)

// additive is a step whose second send adds it again: a comment or a
// create, with the issue it is on (none for a create), the project a
// create is made in, the digest of what it adds on a tracker of kind at
// site, and the lookup of an earlier attempt sent at sent by account:
// what it made (a comment's ID, a create's issue), ok false when the
// tracker shows none.
type additive struct {
	step    tracker.Step
	issue   tracker.IssueID
	project string
	digest  func(kind, site string) (string, error)
	find    func(ctx context.Context, p tracker.Provider, sent time.Time, account string) (made string, ok bool, err error)
}

// agentSends are the rows of the comments and creates this process is
// sending now. Its lock is held across a claim's store transaction, so a
// claim and the sends in flight agree (claimAgentWrite).
type agentSends struct {
	mu      sync.Mutex
	sending map[int64]bool
}

// commentOf is the additive step of a comment of markdown on issue.
func commentOf(issue tracker.IssueID, markdown string) *additive {
	return &additive{
		step: tracker.StepComment, issue: issue,
		digest: func(kind, site string) (string, error) {
			return digestOf("comment", kind, site, string(issue), markdown), nil
		},
		find: func(ctx context.Context, p tracker.Provider, sent time.Time, account string) (string, bool, error) {
			from, to := sent.Add(-repeatBefore), sent.Add(repeatAfter)
			page := tracker.PageToken("")
			for {
				got, err := p.Comments(ctx, issue, page)
				if err != nil {
					return "", false, err
				}
				for _, c := range got.Comments {
					if c.Created.Before(from) {
						// Newest first: every later comment is older still.
						return "", false, nil
					}
					if account != "" && string(c.Author) == account && !c.Created.After(to) && sameText(c.Body, markdown) {
						return string(c.ID), true, nil
					}
				}
				if got.Next == "" {
					return "", false, nil
				}
				page = got.Next
			}
		},
	}
}

// createOf is the additive step of in, a create.
func createOf(in CreateRequest) *additive {
	return &additive{
		step: tracker.StepCreate, project: in.Project,
		digest: func(kind, site string) (string, error) {
			fields, err := json.Marshal(in.Fields)
			if err != nil {
				return "", fmt.Errorf("trackerapp: the digest of a create: %w", err)
			}
			return digestOf("create", kind, site, in.Project, in.Type, in.Title, string(fields)), nil
		},
		find: func(ctx context.Context, p tracker.Provider, sent time.Time, _ string) (string, bool, error) {
			from, to := sent.Add(-repeatBefore), sent.Add(repeatAfter)
			page, err := p.Search(ctx, tracker.SearchQuery{Text: in.Title, Project: tracker.ProjectID(in.Project), Limit: repeatSearch})
			if err != nil {
				return "", false, err
			}
			for _, c := range page.Cards {
				if c.Title == in.Title && !c.Created.Before(from) && !c.Created.After(to) {
					return string(c.ID), true, nil
				}
			}
			return "", false, nil
		},
	}
}

// repeatSearch is the most cards a create's lookup reads: one page.
const repeatSearch = 20

// sameText reports whether two bodies hold the same letters and digits:
// the tracker rewrites a comment's Markdown as it stores it.
func sameText(a, b string) bool {
	return textKey(a) == textKey(b)
}

func textKey(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func digestOf(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// agentStepNames are the steps tracker_agent_writes records, by step:
// those the tracker tools make; any other is "". An upload is part of the
// comment or create that embeds it. An array, which costs nothing to
// link, read through nameOf.
var agentStepNames = [...]string{
	tracker.StepCreate: "create", tracker.StepEdit: "edit", tracker.StepMove: "move",
	tracker.StepEstimate: "estimate", tracker.StepSprintMove: "sprint_move", tracker.StepComment: "comment",
}

// agentRecord is the record of one write an agent makes: whose, the
// connection's row as the write's hold found it, the row claimed for an
// additive step (0 for none), when it was sent and the cards' update
// times before it.
type agentRecord struct {
	who    agentCall
	row    store.TrackerConnection
	add    *additive
	digest string
	claim  int64
	sent   int64
	before map[tracker.IssueID]int64
}

// beginAgentWrite is the record of op's write on run r when ctx carries
// an agent, nil otherwise. An additive step is claimed first
// (claimAgentWrite): an earlier attempt found made answers repeat and
// nothing is sent. A claim that cannot be made refuses the write, which
// sends nothing. A record with a claim is released by endAgentWrite.
func (s *Service) beginAgentWrite(ctx context.Context, r *run, op writeOp) (*agentRecord, *RepeatView, error) {
	who, ok := ctx.Value(agentKey{}).(agentCall)
	if !ok {
		return nil, nil, nil
	}
	rec := &agentRecord{who: who, row: r.row, add: op.additive, sent: millis(s.now())}
	if rec.add == nil {
		return rec, nil, nil
	}
	var err error
	if rec.digest, err = rec.add.digest(r.row.Kind, r.row.Site); err != nil {
		return nil, nil, err
	}
	repeat, err := s.claimAgentWrite(r, rec)
	if err != nil || repeat != nil {
		return nil, repeat, err
	}
	return rec, nil, nil
}

// claimAgentWrite claims rec's additive step: rec.claim is the row its
// send is recorded under, or repeat what an earlier attempt made.
func (s *Service) claimAgentWrite(r *run, rec *agentRecord) (*RepeatView, error) {
	step := nameOf(agentStepNames[:], rec.add.step)
	w := store.AgentWrite{
		Ticket:  store.TicketRef{Kind: r.row.Kind, Site: r.row.Site, IssueID: string(rec.add.issue)},
		Project: rec.add.project, Account: r.row.VerifiedAccountID, ThreadID: rec.who.thread, CallID: rec.who.call,
		Step: step, Digest: rec.digest, Sent: rec.sent, At: rec.sent,
	}
	s.agentSends.mu.Lock()
	got, claimed, err := s.store.ClaimAgentWrite(w, rec.sent-agentRepeatWindow.Milliseconds())
	sending := err == nil && !claimed && got.State != store.AgentWriteApplied && s.agentSends.sending[got.ID]
	if err == nil && !sending && got.State != store.AgentWriteApplied {
		if s.agentSends.sending == nil {
			s.agentSends.sending = map[int64]bool{}
		}
		s.agentSends.sending[got.ID] = true
	}
	s.agentSends.mu.Unlock()
	switch {
	case err != nil:
		return nil, fmt.Errorf("trackerapp: record the agent's %s before it is sent: %w", step, err)
	case claimed:
		rec.claim = got.ID
		return nil, nil
	case got.State == store.AgentWriteApplied:
		return &RepeatView{Issue: got.Ticket.IssueID, Comment: got.Comment}, nil
	case sending:
		return nil, ErrAgentWriteInFlight
	}
	// Interrupted: the claim is this caller's, which looks for it.
	rec.claim = got.ID
	made, found, err := rec.add.find(r.ctx, r.p, time.UnixMilli(got.Sent), got.Account)
	if err != nil {
		s.endAgentWrite(rec)
		return nil, fmt.Errorf("%w: %w", ErrAgentRepeatUnchecked, err)
	}
	if found {
		issue, comment := "", made
		if rec.add.step == tracker.StepCreate {
			issue, comment = made, ""
		}
		err := s.store.SettleAgentWrite(got.ID, issue, comment)
		s.endAgentWrite(rec)
		if err != nil {
			// What it made is there; a later repeat looks for it again.
			s.logf("trackerapp: record the agent's %s found made: %v", step, err)
		}
		if issue == "" {
			issue = got.Ticket.IssueID
		}
		return &RepeatView{Issue: issue, Comment: comment}, nil
	}
	if err := s.store.ResendAgentWrite(got.ID, rec.sent); err != nil {
		s.endAgentWrite(rec)
		return nil, fmt.Errorf("trackerapp: record the agent's %s before it is sent again: %w", step, err)
	}
	return nil, nil
}

// endAgentWrite releases rec's claim: its row is no longer being sent by
// this process. It runs after the outcome replaced the row, so a claim
// never finds a row with no sender that this process is still sending.
func (s *Service) endAgentWrite(rec *agentRecord) {
	if rec == nil || rec.claim == 0 {
		return
	}
	s.agentSends.mu.Lock()
	delete(s.agentSends.sending, rec.claim)
	s.agentSends.mu.Unlock()
}

// cardTimes are the update times of ids' cards as the mirror of root
// holds them; an issue it does not keep is left out. A failed read is
// logged and answers none: the record then places the write by its own
// time.
func (s *Service) cardTimes(root int64, ids []tracker.IssueID) map[tracker.IssueID]int64 {
	out := map[tracker.IssueID]int64{}
	if root == 0 || len(ids) == 0 {
		return out
	}
	held := make([]string, len(ids))
	for i, id := range ids {
		held[i] = string(id)
	}
	for batch := range slices.Chunk(held, readBatch) {
		cards, err := s.store.ListMirrorCards(root, batch)
		if err != nil {
			s.logf("trackerapp: read the cards an agent's write is on: %v", err)
			return map[tracker.IssueID]int64{}
		}
		for _, c := range cards {
			out[tracker.IssueID(c.IssueID)] = c.UpdatedAt
		}
	}
	return out
}

// partIssues are the issues res's parts name.
func partIssues(res tracker.Result) []tracker.IssueID {
	var ids []tracker.IssueID
	for _, p := range res.Parts {
		if p.Issue != "" && !slices.Contains(ids, p.Issue) {
			ids = append(ids, p.Issue)
		}
	}
	return ids
}

// finishAgentWrite records res, the write's outcome, replacing the sent row:
// each part of a recorded step that was applied or may have been, with
// the cards' update times before the write and after (0 when the mirror
// does not keep the card). A record that fails is logged: the write
// stands, and only its mark and a repeat's check of it are lost.
func (s *Service) finishAgentWrite(rec *agentRecord, res tracker.Result, after map[tracker.IssueID]int64) {
	if rec == nil {
		return
	}
	at := max(millis(s.now()), rec.sent)
	var rows []store.AgentWrite
	for _, p := range res.Parts {
		step := nameOf(agentStepNames[:], p.Step)
		state := p.Outcome.State()
		if step == "" || state != tracker.StateApplied && state != tracker.StateUnknown {
			continue
		}
		w := store.AgentWrite{
			Ticket:  store.TicketRef{Kind: rec.row.Kind, Site: rec.row.Site, IssueID: string(p.Issue)},
			Account: rec.row.VerifiedAccountID, ThreadID: rec.who.thread, CallID: rec.who.call, Step: step,
			State: store.AgentWriteApplied, Comment: string(p.Comment), Before: rec.before[p.Issue], After: after[p.Issue], Sent: rec.sent, At: at,
		}
		if state == tracker.StateUnknown {
			w.State = store.AgentWriteUnknown
		}
		if add := rec.add; add != nil && add.step == p.Step {
			w.Project, w.Digest = add.project, rec.digest
		}
		rows = append(rows, w)
	}
	if err := s.store.RecordAgentWrites(rec.claim, rows); err != nil {
		s.logf("trackerapp: record the agent's write of thread %s: %v", rec.who.thread, err)
	}
}

// RepeatView is what an earlier attempt of an agent's comment or create
// made, which its repeat answered with nothing sent: the issue it is on or
// made, and the comment, for a comment found by its ID (empty when the
// tracker did not name it).
type RepeatView struct {
	Issue   string
	Comment string
}

// ThreadMarkView is the thread an agent's write came from, by its ID and
// title (empty for a thread deleted since).
type ThreadMarkView struct {
	ID    string `json:"id"`
	Title string `json:"title,omitempty"`
}

// agentWriteWindow is how long before a write's refreshed update time a
// history event by its account counts as the write's own.
const agentWriteWindow = 2 * time.Minute

// markAgentWrites marks v, the view of issue on connection row of mirror
// root, with the writes agents made there: a comment a write made names
// its thread (markComments), and a change in the history does too when a
// write recorded applied made a change of its kind, by the write's
// account, at its time (from the card's update time before the write, or
// 2 minutes before its end, whichever is later, to its end). The kinds
// (historyKind): a move changes the status, a sprint move the sprint, an
// estimate the estimate, an edit any other field. A write of unknown
// outcome marks nothing in the history. A failed read leaves the view
// unmarked and is logged.
func (s *Service) markAgentWrites(root int64, row store.TrackerConnection, v *DetailView) {
	marks, err := s.store.AgentWritesOf(store.TicketRef{Kind: row.Kind, Site: row.Site, IssueID: v.Card.ID})
	if err != nil {
		s.logf("trackerapp: read the agent writes on %s: %v", v.Card.Key, err)
		return
	}
	if len(marks) == 0 {
		return
	}
	markComments(marks, v.Comments)
	var estimates []string
	if slices.ContainsFunc(marks, func(m store.AgentWriteMark) bool { return m.Step == "estimate" }) {
		estimates = s.estimateNames(root, v)
	}
	for i := range v.History {
		e := &v.History[i]
		if e.Author == nil {
			continue
		}
		kind := historyKind(e.Field, estimates)
		for _, m := range marks {
			if m.State != store.AgentWriteApplied || m.Step != kind || m.Account == "" || m.Account != e.Author.ID {
				continue
			}
			end := m.After
			if end == 0 {
				end = m.At
			}
			start := max(m.Before, end-agentWriteWindow.Milliseconds())
			if e.At > start && e.At <= end {
				e.Agent = &ThreadMarkView{ID: m.ThreadID, Title: m.ThreadTitle}
				break
			}
		}
	}
}

// markComments marks each of comments an agent's write made with its
// thread, by the comment's ID.
func markComments(marks []store.AgentWriteMark, comments []CommentView) {
	for i := range comments {
		for _, m := range marks {
			if m.Comment != "" && m.Comment == comments[i].ID {
				comments[i].Thread = &ThreadMarkView{ID: m.ThreadID, Title: m.ThreadTitle}
				break
			}
		}
	}
}

// markCommentPage marks page, comments of issue on connection row, as
// markComments does; a failed read leaves it unmarked and is logged.
func (s *Service) markCommentPage(row store.TrackerConnection, issue string, page []CommentView) {
	marks, err := s.store.AgentWritesOf(store.TicketRef{Kind: row.Kind, Site: row.Site, IssueID: issue})
	if err != nil {
		s.logf("trackerapp: read the agent writes on %s: %v", issue, err)
		return
	}
	markComments(marks, page)
}

// historyKind is the step whose change a history event of field is: the
// status and resolution a move's, the sprint (a Linear cycle) a sprint
// move's, the estimate an estimate's (Linear's, and on Jira the fields
// estimates names, those v's boards estimate by), any other an edit's.
func historyKind(field string, estimates []string) string {
	switch strings.ToLower(field) {
	case "status", "resolution":
		return "move"
	case "sprint", "cycle":
		return "sprint_move"
	case "estimate":
		return "estimate"
	}
	if slices.ContainsFunc(estimates, func(n string) bool { return strings.EqualFold(n, field) }) {
		return "estimate"
	}
	return "edit"
}

// estimateNames are the fields v's boards in mirror root estimate by, by
// their ID and by the name v shows; a board the mirror cannot read adds
// none, and the failure is logged.
func (s *Service) estimateNames(root int64, v *DetailView) []string {
	var out []string
	for _, r := range v.Rows {
		m, ok, err := s.store.GetMirrorBoard(root, r.Board)
		if err != nil {
			s.logf("trackerapp: read board %s for the estimate marks of %s: %v", r.Board, v.Card.Key, err)
			continue
		}
		if !ok {
			continue
		}
		field := structureOf(m, tracker.Catalog{}).Estimate.Field
		if field == "" || slices.Contains(out, field) {
			continue
		}
		out = append(out, field)
		if i := slices.IndexFunc(v.Fields, func(f FieldView) bool { return f.ID == field }); i >= 0 {
			out = append(out, v.Fields[i].Name)
		}
	}
	return out
}
