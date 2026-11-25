package tracker

import (
	"errors"
	"fmt"

	"agent-overflow/internal/tracker/trackerapi"
)

// ErrInvalidInput is matched, through errors.Is, by the error of an input
// refused before any request: by the checks in this package, by a
// provider's own, or by a client. It is the transport's sentinel, so
// there is one, and a provider that classifies a failure cannot match one
// spelling and miss the other.
var ErrInvalidInput = trackerapi.ErrInvalidInput

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrInvalidInput}, args...)...)
}

// State is what became of one part of a write.
type State uint8

const (
	// StateUnknown is a part that was sent and not answered in full: it
	// may have happened. The caller reads the tracker before it offers
	// the write again. It is the zero State, so an outcome nobody set
	// reads as unknown, never as done or as safe to send again.
	StateUnknown State = iota
	// StateApplied is a part the tracker applied.
	StateApplied
	// StateRefused is a part the tracker, or a check made before sending
	// it, refused: it did not happen and would not as asked.
	StateRefused
	// StateNotSent is a part nobody judged: an earlier part failed, or
	// the call ended, before it was sent.
	StateNotSent
)

func (s State) String() string {
	switch s {
	case StateUnknown:
		return "unknown"
	case StateApplied:
		return "applied"
	case StateRefused:
		return "refused"
	case StateNotSent:
		return "not sent"
	}
	return fmt.Sprintf("State(%d)", uint8(s))
}

// Outcome is what became of one part of a write. Its constructors are the
// only way to make one, so an outcome is exactly one state and carries a
// refusal only when refused. The zero Outcome is unknown.
type Outcome struct {
	state   State
	refusal Refusal
	err     error
}

// Applied is the outcome of a part the tracker applied.
func Applied() Outcome { return Outcome{state: StateApplied} }

// Refused is the outcome of a part that was refused, and why.
func Refused(r Refusal) Outcome { return Outcome{state: StateRefused, refusal: r, err: r.Err} }

// Unknown is the outcome of a part that may have happened; err is the
// failure that left it unknown.
func Unknown(err error) Outcome { return Outcome{state: StateUnknown, err: err} }

// NotSent is the outcome of a part nobody judged; err says why it was not
// sent.
func NotSent(err error) Outcome { return Outcome{state: StateNotSent, err: err} }

// State is the outcome's state.
func (o Outcome) State() State { return o.state }

// Refusal is why a refused part was refused; ok is false for another
// state.
func (o Outcome) Refusal() (r Refusal, ok bool) { return o.refusal, o.state == StateRefused }

// Err is the failure behind an outcome that is not applied, with the
// client's typed errors reachable through errors.As; nil when applied.
func (o Outcome) Err() error { return o.err }

// Reason is the kind of a refusal, which decides what the caller can do
// about it.
type Reason uint8

const (
	// ReasonTracker is a refusal the tracker answered. Message is its
	// reason and Fields its messages by field.
	ReasonTracker Reason = iota + 1
	// ReasonInvalid is input the provider's checks refused before sending
	// it.
	ReasonInvalid
	// ReasonUnsupported is a write the connection's tracker cannot make
	// (see Capabilities).
	ReasonUnsupported
	// ReasonStale is a write whose starting point is no longer what the
	// tracker holds: the Markdown an edit was written from, a worklog as
	// read, the board an issue was on, where a rank's neighbors stood, a
	// sprint's state, a comment or link that is gone. The caller reads it
	// again. A provider's check before a write refuses with it whenever
	// reading again would show the caller what changed, which includes
	// every case where the tracker cannot tell "moved since" from "never
	// was"; ReasonInvalid is for input no reading could make right.
	ReasonStale
	// ReasonLoss is a rich text edit that would lose content Markdown
	// cannot show. Lost names it; the same write with AcceptLoss set
	// makes the edit.
	ReasonLoss
	// ReasonNoMatch is typed text that matches no value. Candidates are
	// the values allowed or suggested, when the tracker offered some.
	ReasonNoMatch
	// ReasonAmbiguous is typed text that matches several values, which
	// Candidates lists; picking one by ID resolves it.
	ReasonAmbiguous
	// ReasonMissing is a create or move that leaves required fields unset;
	// Fields names them.
	ReasonMissing
)

// Refusal is why a part was refused. Message is a caller-safe sentence;
// the members after it are set where the Reason says.
type Refusal struct {
	Reason     Reason
	Message    string
	Fields     []FieldMessage
	Candidates []Choice
	// Lost names, one sentence each, the content a rich text edit would
	// lose, with how to keep it.
	Lost []string
	// Err is the failure the refusal was read from; Outcome.Err returns
	// it.
	Err error
}

// FieldMessage is a message about one field.
type FieldMessage struct {
	Field   FieldID
	Message string
}

// Step is what one part of a write did.
type Step uint8

const (
	StepCreate Step = iota + 1
	// StepUpload attaches a file: a file of Attach, or an image a Markdown
	// body embeds.
	StepUpload
	StepEdit
	StepMove
	StepEstimate
	StepRank
	// StepSprintMove moves an issue into a sprint or to the backlog.
	StepSprintMove
	// StepSprint creates, starts, edits or closes a sprint.
	StepSprint
	StepComment
	StepLink
	StepWorklog
	// StepPoker casts, reveals or clears planning poker votes, which the
	// add-on holds outside the tracker's own data (Poker).
	StepPoker
)

func (s Step) String() string {
	switch s {
	case StepCreate:
		return "create"
	case StepUpload:
		return "upload"
	case StepEdit:
		return "edit"
	case StepMove:
		return "move"
	case StepEstimate:
		return "estimate"
	case StepRank:
		return "rank"
	case StepSprintMove:
		return "sprint move"
	case StepSprint:
		return "sprint"
	case StepComment:
		return "comment"
	case StepLink:
		return "link"
	case StepWorklog:
		return "worklog"
	case StepPoker:
		return "poker"
	}
	return fmt.Sprintf("Step(%d)", uint8(s))
}

// Part is one thing a write touched: one issue of a rank, one step of a
// create. Issue is the issue it is about, empty for a part about no issue
// (a sprint) and for a create the tracker did not confirm. Changed are the
// resources the part may have changed whatever its outcome; the caller
// reads again those of a part that is applied or unknown.
type Part struct {
	Issue   IssueID
	Step    Step
	Outcome Outcome
	Changed []Resource
}

// Result is every part of one write, in the order they were sent or
// would have been.
type Result struct {
	Parts []Part
}

// Refuse is the result of a write refused as a whole before anything was
// sent: one refused part for each issue named, or one about no issue when
// none is. A provider uses it for input its checks refuse
// (ReasonInvalid) and a write it cannot make (ReasonUnsupported).
func Refuse(step Step, r Refusal, issues ...IssueID) Result {
	if len(issues) == 0 {
		return Result{Parts: []Part{{Step: step, Outcome: Refused(r)}}}
	}
	parts := make([]Part, len(issues))
	for i, id := range issues {
		parts[i] = Part{Issue: id, Step: step, Outcome: Refused(r)}
	}
	return Result{Parts: parts}
}

// Invalid is Refuse with ReasonInvalid, for an input check's error.
func Invalid(step Step, err error, issues ...IssueID) Result {
	return Refuse(step, Refusal{Reason: ReasonInvalid, Message: err.Error(), Err: err}, issues...)
}

// Tally counts a result's parts by state.
type Tally struct {
	Applied, Refused, Unknown, NotSent int
}

// Tally counts r's parts by state.
func (r Result) Tally() Tally {
	var t Tally
	for _, p := range r.Parts {
		switch p.Outcome.State() {
		case StateApplied:
			t.Applied++
		case StateRefused:
			t.Refused++
		case StateNotSent:
			t.NotSent++
		case StateUnknown:
			t.Unknown++
		}
	}
	return t
}

// AllApplied reports a result whose every part was applied; false for a
// result with no parts.
func (r Result) AllApplied() bool {
	t := r.Tally()
	return t.Applied > 0 && t.Applied == len(r.Parts)
}

// Refresh is every resource a part that is applied or unknown may have
// changed, each once, in the order first named.
func (r Result) Refresh() []Resource {
	var out []Resource
	seen := map[Resource]bool{}
	for _, p := range r.Parts {
		if s := p.Outcome.State(); s != StateApplied && s != StateUnknown {
			continue
		}
		for _, res := range p.Changed {
			if !seen[res] {
				seen[res] = true
				out = append(out, res)
			}
		}
	}
	return out
}

// Created is the issue a create made, when the tracker confirmed it.
func (r Result) Created() (IssueID, bool) {
	for _, p := range r.Parts {
		if p.Step == StepCreate && p.Outcome.State() == StateApplied {
			return p.Issue, true
		}
	}
	return "", false
}

// CreateUnknown reports a create whose answer was lost: the tracker may
// hold a new issue and nobody knows its ID. A second attempt may make a
// second issue; nothing retries it.
func (r Result) CreateUnknown() bool {
	for _, p := range r.Parts {
		if p.Step == StepCreate && p.Outcome.State() == StateUnknown {
			return true
		}
	}
	return false
}

// stepChanges are the kinds of resource a part of each step may change.
// A part that is applied or unknown names each of them: for its own issue
// when the kind is an issue's, and for the board or the author and day the
// write named otherwise. It is the one statement of what a write of each
// kind leaves stale, so a provider cannot name less.
//
// A rank and an estimate change the issue on every board that shares the
// board's rank or estimate field, which is what the card names: a card is
// read through every board the mirror has the issue on.
//
// An array by step, so linking the package builds nothing at start; a
// part's check refuses a step outside it before indexing.
var stepChanges = [...][]ResourceKind{
	StepCreate:     {ResourceCard, ResourceDetail},
	StepUpload:     {ResourceDetail},
	StepEdit:       {ResourceCard, ResourceDetail},
	StepMove:       {ResourceCard, ResourceDetail},
	StepEstimate:   {ResourceRow, ResourceCard, ResourceDetail},
	StepRank:       {ResourceRow, ResourceCard},
	StepSprintMove: {ResourceRow, ResourceCard, ResourceDetail},
	StepSprint:     {ResourceStructure},
	StepComment:    {ResourceDetail},
	StepLink:       {ResourceDetail},
	StepWorklog:    {ResourceWorklogs},
	// The add-on's votes are no mirrored resource: an open ticket that
	// follows them reads them again (trackerapp).
	StepPoker: {},
}

// Check reports a result no provider may return: no parts; a part with
// no step, or with a resource no constructor made; a refusal with no
// reason; a part that is applied or unknown and does not name every kind
// of resource its step may change (stepChanges), other than a create of
// unknown outcome; a create part that names an issue and is not applied,
// or is applied and names none; more than one create part. A provider's
// tests run it on every result; the caller runs it before acting on one.
func (r Result) Check() error {
	if len(r.Parts) == 0 {
		return errors.New("tracker: result has no parts")
	}
	creates := 0
	for i, p := range r.Parts {
		if err := p.check(); err != nil {
			return fmt.Errorf("tracker: result part %d (%s): %w", i, p.Step, err)
		}
		if p.Step == StepCreate {
			creates++
		}
	}
	if creates > 1 {
		return fmt.Errorf("tracker: result has %d create parts", creates)
	}
	return nil
}

func (p Part) check() error {
	if p.Step < StepCreate || p.Step > StepPoker {
		return errors.New("no step")
	}
	for _, res := range p.Changed {
		if err := res.check(); err != nil {
			return err
		}
	}
	state := p.Outcome.State()
	if r, ok := p.Outcome.Refusal(); ok && (r.Reason < ReasonTracker || r.Reason > ReasonMissing) {
		return errors.New("refused with no reason")
	}
	if p.Step == StepCreate && (state == StateApplied) != (p.Issue != "") {
		return fmt.Errorf("a create that is %s names issue %q", state, p.Issue)
	}
	lostCreate := p.Step == StepCreate && state == StateUnknown
	if (state != StateApplied && state != StateUnknown) || lostCreate {
		return nil
	}
	for _, kind := range stepChanges[p.Step] {
		if !p.names(kind) {
			return fmt.Errorf("%s and does not name the %s it may have changed", state, kind)
		}
	}
	return nil
}

// names reports whether p.Changed holds a resource of kind: p's own
// issue's for a card, row or detail, any for a structure or worklogs.
func (p Part) names(kind ResourceKind) bool {
	for _, res := range p.Changed {
		if res.kind != kind {
			continue
		}
		switch kind {
		case ResourceCard, ResourceRow, ResourceDetail:
			if res.issue == p.Issue {
				return true
			}
		case ResourceStructure, ResourceWorklogs:
			return true
		}
	}
	return false
}

// ResourceKind is a kind of mirrored resource, each with one read that
// sync and writes share.
type ResourceKind uint8

const (
	// ResourceCard is an issue's card, read through every board the
	// mirror has it on, or alone when it is on none.
	ResourceCard ResourceKind = iota + 1
	// ResourceRow is an issue's row on one board.
	ResourceRow
	// ResourceDetail is an issue's detail, read when it is cached or open.
	ResourceDetail
	// ResourceStructure is a board's structure.
	ResourceStructure
	// ResourceWorklogs is one author's worklogs of one day.
	ResourceWorklogs
)

func (k ResourceKind) String() string {
	switch k {
	case ResourceCard:
		return "card"
	case ResourceRow:
		return "board row"
	case ResourceDetail:
		return "detail"
	case ResourceStructure:
		return "board structure"
	case ResourceWorklogs:
		return "worklogs"
	}
	return fmt.Sprintf("ResourceKind(%d)", uint8(k))
}

// Resource names something a write may have changed. Its constructors are
// the only way to make one, so each names what its kind needs. Resources
// are comparable: equal values name the same resource.
type Resource struct {
	kind   ResourceKind
	issue  IssueID
	board  BoardID
	author UserID
	day    Date
}

// CardOf is issue's card.
func CardOf(issue IssueID) Resource { return Resource{kind: ResourceCard, issue: issue} }

// RowOf is issue's row on board.
func RowOf(board BoardID, issue IssueID) Resource {
	return Resource{kind: ResourceRow, board: board, issue: issue}
}

// DetailOf is issue's detail.
func DetailOf(issue IssueID) Resource { return Resource{kind: ResourceDetail, issue: issue} }

// StructureOf is board's structure.
func StructureOf(board BoardID) Resource { return Resource{kind: ResourceStructure, board: board} }

// WorklogsOf is author's worklogs of day.
func WorklogsOf(author UserID, day Date) Resource {
	return Resource{kind: ResourceWorklogs, author: author, day: day}
}

// Kind is the resource's kind; zero for the zero Resource.
func (r Resource) Kind() ResourceKind { return r.kind }

// Issue is the issue of a card, row or detail.
func (r Resource) Issue() IssueID { return r.issue }

// Board is the board of a row or structure.
func (r Resource) Board() BoardID { return r.board }

// Worklogs are the author and day of a worklogs resource.
func (r Resource) Worklogs() (UserID, Date) { return r.author, r.day }

func (r Resource) check() error {
	ok := false
	switch r.kind {
	case ResourceCard, ResourceDetail:
		ok = r.issue != ""
	case ResourceRow:
		ok = r.issue != "" && r.board != ""
	case ResourceStructure:
		ok = r.board != ""
	case ResourceWorklogs:
		ok = r.author != "" && !r.day.IsZero()
	}
	if !ok {
		return fmt.Errorf("resource %+v names less than its kind needs", r)
	}
	return nil
}
