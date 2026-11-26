package trackerapp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// An agent's writes. A write an agent makes through the tracker tools
// carries who made it as a context value (ByAgent), and Service.write is
// its one reader: it records each part that was applied or may have been
// in tracker_agent_writes (store.AgentWrite), after the refresh stored
// what changed and before the open tickets settle, so the views they
// build carry the marks. A comment or a create adds something a second
// send would add again: it is recorded sent, with its digest, before it
// is sent, so a call repeated after a lost answer finds it (AgentRepeat).
// A person's writes carry no agent and record nothing.

// agentKey is the context key of the agent a write is made for.
type agentKey struct{}

// agentCall is who makes a write: the thread whose agent called, and the
// tool call it is part of.
type agentCall struct{ thread, call string }

// ByAgent is ctx for the writes of tool call call by thread's agent.
func ByAgent(ctx context.Context, thread, call string) context.Context {
	return context.WithValue(ctx, agentKey{}, agentCall{thread: thread, call: call})
}

// agentRepeatWindow is how long a recorded comment or create answers a
// repeat of it.
const agentRepeatWindow = time.Hour

// additive is a step whose second send adds it again: a comment or a
// create, with the issue it is on (none for a create), the project a
// create is made in, and its digest.
type additive struct {
	step    tracker.Step
	issue   tracker.IssueID
	project string
	digest  string
}

// CommentDigest is the digest of a comment of markdown on issue of
// connection, as a repeat of it is found by.
func CommentDigest(connection, issue, markdown string) string {
	return digestOf("comment", connection, issue, markdown)
}

// CreateDigest is the digest of a create of an issue of type issueType
// titled title in project of connection.
func CreateDigest(connection, project, issueType, title string) string {
	return digestOf("create", connection, project, issueType, title)
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
// connection's row as the write's hold found it, the row recorded sent
// before the send (0 for none) and the cards' update times before it.
type agentRecord struct {
	who    agentCall
	row    store.TrackerConnection
	add    *additive
	sent   int64
	before map[tracker.IssueID]int64
}

// beginAgentWrite is the record of op's write on run r when ctx carries
// an agent, nil otherwise. An additive step is recorded sent first; a
// record that cannot be written refuses the write, which sends nothing.
func (s *Service) beginAgentWrite(ctx context.Context, r *run, op writeOp) (*agentRecord, error) {
	who, ok := ctx.Value(agentKey{}).(agentCall)
	if !ok {
		return nil, nil
	}
	rec := &agentRecord{who: who, row: r.row, add: op.additive}
	if add := op.additive; add != nil {
		id, err := s.store.RecordAgentWriteSent(store.AgentWrite{
			Ticket:  store.TicketRef{Kind: r.row.Kind, Site: r.row.Site, IssueID: string(add.issue)},
			Project: add.project, Account: r.row.VerifiedAccountID, ThreadID: who.thread, CallID: who.call,
			Step: nameOf(agentStepNames[:], add.step), Digest: add.digest, At: millis(s.now()),
		})
		if err != nil {
			return nil, fmt.Errorf("trackerapp: record the agent's %s before it is sent: %w", nameOf(agentStepNames[:], add.step), err)
		}
		rec.sent = id
	}
	return rec, nil
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
	at := millis(s.now())
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
			State: store.AgentWriteApplied, Comment: string(p.Comment), Before: rec.before[p.Issue], After: after[p.Issue], At: at,
		}
		if state == tracker.StateUnknown {
			w.State = store.AgentWriteUnknown
		}
		if add := rec.add; add != nil && add.step == p.Step {
			w.Project, w.Digest = add.project, add.digest
		}
		rows = append(rows, w)
	}
	if err := s.store.RecordAgentWrites(rec.sent, rows); err != nil {
		s.logf("trackerapp: record the agent's write of thread %s: %v", rec.who.thread, err)
	}
}

// AgentRepeat is a comment or create an agent made earlier that a call
// repeats: the record's row, its state (applied, unknown, or sent, which
// is unknown), the issue it is on or made, the comment an applied comment
// made, and when this computer recorded it (Unix milliseconds).
type AgentRepeat struct {
	Row     int64
	State   string
	Issue   string
	Comment string
	At      int64
}

// AgentRepeatOf is thread's latest write with digest within the last
// hour, ok false when there is none.
func (s *Service) AgentRepeatOf(thread, digest string) (AgentRepeat, bool, error) {
	if thread == "" || digest == "" {
		return AgentRepeat{}, false, fmt.Errorf("%w: a repeat names its thread and digest", ErrInvalidRequest)
	}
	if err := s.enter(); err != nil {
		return AgentRepeat{}, false, err
	}
	defer s.leave()
	w, ok, err := s.store.AgentWriteRepeat(thread, digest, millis(s.now().Add(-agentRepeatWindow)))
	if err != nil || !ok {
		return AgentRepeat{}, false, err
	}
	return AgentRepeat{Row: w.ID, State: w.State, Issue: w.Ticket.IssueID, Comment: w.Comment, At: w.At}, true, nil
}

// SettleAgentRepeat records that the write of row, recorded sent or of
// unknown outcome, was found made: on issue, for a create, and as
// comment, for a comment.
func (s *Service) SettleAgentRepeat(row int64, issue, comment string) error {
	if err := s.enter(); err != nil {
		return err
	}
	defer s.leave()
	return s.store.SettleAgentWrite(row, issue, comment)
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

// markAgentWrites marks v, the view of issue on connection row, with the
// writes agents made there: a comment a write made names its thread, and
// a change in the history by the write's account at its time does too
// (from the card's update time before the write, or 2 minutes before its
// end, whichever is later, to its end). A failed read leaves the view
// unmarked and is logged.
func (s *Service) markAgentWrites(row store.TrackerConnection, v *DetailView) {
	marks, err := s.store.AgentWritesOf(store.TicketRef{Kind: row.Kind, Site: row.Site, IssueID: v.Card.ID})
	if err != nil {
		s.logf("trackerapp: read the agent writes on %s: %v", v.Card.Key, err)
		return
	}
	if len(marks) == 0 {
		return
	}
	for i := range v.Comments {
		for _, m := range marks {
			if m.Comment != "" && m.Comment == v.Comments[i].ID {
				v.Comments[i].Thread = &ThreadMarkView{ID: m.ThreadID, Title: m.ThreadTitle}
				break
			}
		}
	}
	for i := range v.History {
		e := &v.History[i]
		if e.Author == nil {
			continue
		}
		for _, m := range marks {
			if m.Step == "comment" || m.Account == "" || m.Account != e.Author.ID {
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
