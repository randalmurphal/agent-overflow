package trackerapp

import (
	"errors"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/jira"
)

// WriteView is what became of a write, as the tracker answered it: every
// part with its outcome, the issue a create made, and what the mirror
// could not be refreshed with afterwards.
type WriteView struct {
	// Parts are the write's parts in the order they were sent or would
	// have been: one per issue of a rank or a sprint move, one per step of
	// a create.
	Parts []PartView `json:"parts"`
	// Created is the issue a create made, when the tracker confirmed it,
	// even when a later step of the create failed.
	Created string `json:"created,omitempty"`
	// CreatedKey is the key of the issue Created names when a later step
	// of the create was not applied and the mirror holds its card after
	// the write's reads; empty otherwise.
	CreatedKey string `json:"created_key,omitempty"`
	// CreateUnknown reports a create whose answer was lost: the tracker
	// may hold a new issue nobody can name, and a second attempt may make
	// a second one. Nothing retried it; the board's tick ran at once.
	CreateUnknown bool `json:"create_unknown,omitempty"`
	// NotOnBoard is the board the create was made from when the tracker
	// says that board does not hold the new issue (its filter leaves it
	// out, its status is in no column, or it is an epic on a Jira scrum
	// board): the issue was created, no row was stored, and the board
	// will not show it.
	NotOnBoard string `json:"not_on_board,omitempty"`
	// Stale names what the write may have changed that the mirror could
	// not be refreshed with, and why. The write stands: the views of it
	// are stale until the next sync reads it.
	Stale []StaleView `json:"stale,omitempty"`
	// Ended reports a connection turned off or removed while the write was
	// made: its mirror was cleared, so nothing was refreshed.
	Ended bool `json:"ended,omitempty"`
}

// PartView is one part of a write. Step is create, upload, edit, move,
// estimate, rank, sprint_move, sprint, comment, link or worklog; State is
// applied, refused, unknown (sent and not answered in full: it may have
// happened) or not_sent. Refusal is why a refused part was refused, and
// Failure why an unknown or unsent part is so, as a person reads it.
type PartView struct {
	Issue   string       `json:"issue,omitempty"`
	Step    string       `json:"step"`
	State   string       `json:"state"`
	Refusal *RefusalView `json:"refusal,omitempty"`
	Failure *FailureView `json:"failure,omitempty"`
}

// RefusalView is why a part was refused. Reason is tracker (the tracker
// answered Message, with Fields its messages by field), invalid,
// unsupported, stale (read again and retry), loss (Lost names what a rich
// text edit would lose; the same write accepting the loss makes it),
// no_match or ambiguous (Candidates are the values offered), or missing
// (Fields names the required fields left unset).
type RefusalView struct {
	Reason     string             `json:"reason"`
	Message    string             `json:"message"`
	Fields     []FieldMessageView `json:"fields,omitempty"`
	Candidates []ChoiceView       `json:"candidates,omitempty"`
	Lost       []string           `json:"lost,omitempty"`
}

// FieldMessageView is a message about one field.
type FieldMessageView struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// StaleView is resources a write may have changed that were not read
// again, and the failure that stopped their read.
type StaleView struct {
	Resources []ResourceView `json:"resources"`
	Failure   FailureView    `json:"failure"`
}

// ResourceView names something the mirror holds. Kind is card (an issue's
// card on every board), row (Issue on Board), detail (an open or cached
// ticket), structure (Board's columns and sprints), worklogs (Author's
// worklogs of Day, YYYY-MM-DD) or delta (Board's changed rows, read after
// a create of unknown outcome).
type ResourceView struct {
	Kind   string `json:"kind"`
	Issue  string `json:"issue,omitempty"`
	Board  string `json:"board,omitempty"`
	Author string `json:"author,omitempty"`
	Day    string `json:"day,omitempty"`
}

// The wire names of the write model's enums, indexed by value: arrays,
// which cost nothing to link, read through nameOf.
var (
	stepNames = [...]string{
		tracker.StepCreate: "create", tracker.StepUpload: "upload", tracker.StepEdit: "edit", tracker.StepMove: "move",
		tracker.StepEstimate: "estimate", tracker.StepRank: "rank", tracker.StepSprintMove: "sprint_move",
		tracker.StepSprint: "sprint", tracker.StepComment: "comment", tracker.StepLink: "link", tracker.StepWorklog: "worklog",
		tracker.StepPoker: "poker",
	}
	stateNames = [...]string{
		tracker.StateUnknown: "unknown", tracker.StateApplied: "applied", tracker.StateRefused: "refused", tracker.StateNotSent: "not_sent",
	}
	reasonNames = [...]string{
		tracker.ReasonTracker: "tracker", tracker.ReasonInvalid: "invalid", tracker.ReasonUnsupported: "unsupported",
		tracker.ReasonStale: "stale", tracker.ReasonLoss: "loss", tracker.ReasonNoMatch: "no_match",
		tracker.ReasonAmbiguous: "ambiguous", tracker.ReasonMissing: "missing",
	}
	resourceNames = [...]string{
		tracker.ResourceCard: "card", tracker.ResourceRow: "row", tracker.ResourceDetail: "detail",
		tracker.ResourceStructure: "structure", tracker.ResourceWorklogs: "worklogs",
	}
)

// resourceDelta is the kind of a board's delta in a ResourceView: no
// resource of the model, the read a create of unknown outcome runs.
const resourceDelta = "delta"

// partViews are res's parts as views; kind is the connection's kind, which
// names the tracker in a failure's sentence, and at when the write ended.
func partViews(kind string, res tracker.Result, at int64) []PartView {
	out := make([]PartView, len(res.Parts))
	for i, p := range res.Parts {
		state := p.Outcome.State()
		v := PartView{Issue: string(p.Issue), Step: nameOf(stepNames[:], p.Step), State: nameOf(stateNames[:], state)}
		r, refused := p.Outcome.Refusal()
		err := p.Outcome.Err()
		switch {
		case refused:
			v.Refusal = refusalView(r)
			// A credential refused is the connection's setup, and says what
			// the connection's problem says.
			if r.Err != nil {
				if f := ClassifyFailure(kind, r.Err); f.Kind == FailureSetup {
					v.Refusal.Message = f.Message
				}
			}
		case state == tracker.StateUnknown:
			// What failed matters less than that it may have happened: a
			// second try could make the change twice.
			f := Failure{Kind: FailureTracker}
			if err != nil {
				f = ClassifyFailure(kind, err)
			}
			msg := unknownOutcome(responderOf(kind, err), p.Step)
			if pe, ok := errors.AsType[*jira.PokerError](err); ok {
				// The answer arrived and was not one that says: say which.
				msg = pe.Step + " " + pe.Detail + ", so this change may or may not have been made; " + LostWriteCheck(p.Step)
			}
			v.Failure = &FailureView{Kind: string(f.Kind), Message: msg, At: at}
		case err != nil && state != tracker.StateApplied:
			f := ClassifyFailure(kind, err)
			v.Failure = &FailureView{Kind: string(f.Kind), Message: f.Message, At: at}
		}
		out[i] = v
	}
	return out
}

// unknownOutcome is the sentence of a step that was sent and not answered
// in full, on a connection of kind: what to look at before trying again.
func unknownOutcome(kind string, step tracker.Step) string {
	lost := trackerName(kind) + "'s answer did not arrive in full, so "
	if step == tracker.StepCreate {
		return lost + "the issue may or may not have been created; " + LostWriteCheck(step)
	}
	return lost + "this change may or may not have been made; " + LostWriteCheck(step)
}

// LostWriteCheck is what to look at, after a write of step whose outcome
// is not known, before trying it again: where the change shows if it was
// made. A created issue may not match its board's filter, so it is
// searched for; a sprint shows among the board's sprints, a worklog in
// the day's logged time, a rank in the board's order, and every other
// change on its ticket.
func LostWriteCheck(step tracker.Step) string {
	switch step {
	case tracker.StepCreate:
		return "search the tracker for the issue before you try again"
	case tracker.StepSprint:
		return "look at the board's sprints before you try again"
	case tracker.StepWorklog:
		return "look at the time you logged that day before you try again"
	case tracker.StepRank:
		return "look at the issue's place on the board before you try again"
	case tracker.StepPoker:
		return "look at the ticket's poker votes before you try again"
	case tracker.StepUpload, tracker.StepEdit, tracker.StepMove, tracker.StepEstimate,
		tracker.StepSprintMove, tracker.StepComment, tracker.StepLink:
	}
	return "look at the ticket before you try again"
}

func refusalView(r tracker.Refusal) *RefusalView {
	v := &RefusalView{Reason: nameOf(reasonNames[:], r.Reason), Message: r.Message, Lost: r.Lost}
	for _, f := range r.Fields {
		v.Fields = append(v.Fields, FieldMessageView{Field: string(f.Field), Message: f.Message})
	}
	for _, c := range r.Candidates {
		v.Candidates = append(v.Candidates, ChoiceView(c))
	}
	return v
}

// resourceView is res as a view.
func resourceView(res tracker.Resource) ResourceView {
	v := ResourceView{Kind: nameOf(resourceNames[:], res.Kind()), Issue: string(res.Issue()), Board: string(res.Board())}
	if author, day := res.Worklogs(); author != "" {
		v.Author, v.Day = string(author), day.String()
	}
	return v
}
