package trackertools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/tracker"
	"agent-overflow/internal/trackerapp"
)

// ticket_update changes tickets in one call. Every ticket is resolved,
// read and checked first (plan), with nothing sent: a name or a value
// the ticket cannot take, or a long text the session was not given whole
// (marks.go), refuses the call; a status, sprint or estimate the ticket
// as it is now cannot take is said as that ticket's refusal, and nothing
// is sent for any ticket. A call naming several tickets then waits for
// the person's approval (Host.Approve). Each ticket's steps run in order,
// status, fields, estimate, sprint, comment, and the first refused or
// unknown stops that ticket: what came after it is not sent. A step the
// ticket already holds is applied with no request, so the same call
// again makes only what was not; a comment, which a second send adds
// again, is checked against the record of an earlier one first
// (repeatComment).

type updateArgs struct {
	Keys    []string                   `json:"keys"`
	Set     map[string]json.RawMessage `json:"set"`
	Comment *string                    `json:"comment"`
}

// ticketAnswer is what became of one ticket: the steps applied, by the
// names the call gave, the one refused or unknown with its sentence, the
// steps not sent after it, and a note.
type ticketAnswer struct {
	Key     string   `json:"key"`
	Applied []string `json:"applied,omitempty"`
	Refused string   `json:"refused,omitempty"`
	Unknown string   `json:"unknown,omitempty"`
	NotSent []string `json:"not_sent,omitempty"`
	Note    string   `json:"note,omitempty"`
}

type updateAnswer struct {
	Tickets []ticketAnswer `json:"tickets"`
}

// namedValue is one entry of set, in the order the call's names sort.
type namedValue struct {
	name  string
	value setValue
}

// parseSet is set's entries, sorted by name so a call's plan is the same
// however its client ordered them.
func parseSet(set map[string]json.RawMessage) ([]namedValue, error) {
	names := make([]string, 0, len(set))
	for n := range set {
		names = append(names, n)
	}
	slices.Sort(names)
	out := make([]namedValue, 0, len(names))
	for _, n := range names {
		if strings.TrimSpace(n) == "" {
			return nil, invalidf("set has an empty name.")
		}
		v, err := parseSetValue(n, set[n])
		if err != nil {
			return nil, err
		}
		out = append(out, namedValue{name: n, value: v})
	}
	return out, nil
}

func (s *Server) update(ctx context.Context, caller Caller, raw json.RawMessage) (any, error) {
	var args updateArgs
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	if len(args.Set) == 0 && args.Comment == nil {
		return nil, invalidf("Give set or comment.")
	}
	if args.Comment != nil && strings.TrimSpace(*args.Comment) == "" {
		return nil, invalidf("comment is empty; write one or leave it out.")
	}
	if len(args.Keys) > maxUpdateKeys {
		return nil, invalidf("keys holds at most %d tickets; change the rest in another call.", maxUpdateKeys)
	}
	set, err := parseSet(args.Set)
	if err != nil {
		return nil, err
	}
	if err := s.h.Writable(caller.Thread); err != nil {
		return nil, err
	}
	comment := ""
	if args.Comment != nil {
		comment = *args.Comment
	}
	keys := args.Keys
	if len(keys) == 0 {
		keys = []string{""}
	}
	var plans []*planned
	for _, key := range keys {
		p, err := s.planTicket(ctx, caller, key, set, comment)
		if err != nil {
			return nil, err
		}
		if slices.ContainsFunc(plans, func(q *planned) bool { return q.t.connection == p.t.connection && q.t.issue == p.t.issue }) {
			return nil, invalidf("%s is named twice in keys.", p.key)
		}
		plans = append(plans, p)
	}
	if slices.ContainsFunc(plans, func(p *planned) bool { return p.refused != "" }) {
		// Nothing is sent: the call is the person's request as a whole.
		out := updateAnswer{}
		for _, p := range plans {
			out.Tickets = append(out.Tickets, ticketAnswer{Key: p.key, Refused: p.refused, NotSent: p.stepNames(p.refused != "", p.refusedStep)})
		}
		return out, nil
	}
	id := s.newID()
	if len(plans) > 1 {
		if err := s.h.Approve(ctx, caller.Thread, approvalOf(id, plans, comment)); err != nil {
			return nil, err
		}
	}
	// What was approved is sent whole: the call's end no longer stops it,
	// and each write keeps its own bound.
	wctx := trackerapp.ByAgent(context.WithoutCancel(ctx), caller.Thread, id)
	out := updateAnswer{}
	row := WriteRow{ID: id}
	for _, p := range plans {
		answer, written := s.apply(wctx, caller, p)
		out.Tickets = append(out.Tickets, answer)
		if len(written.Applied) > 0 {
			row.Tickets = append(row.Tickets, written)
		}
	}
	if len(row.Tickets) > 0 {
		s.h.Wrote(caller.Thread, row)
	}
	return out, nil
}

// planTicket resolves key for caller, reads the ticket as it is now and
// plans set and comment on it.
func (s *Server) planTicket(ctx context.Context, caller Caller, key string, set []namedValue, comment string) (*planned, error) {
	t, sources, err := s.resolve(ctx, caller.Thread, key)
	if err != nil {
		return nil, err
	}
	d, err := s.detailOf(ctx, t)
	if err != nil {
		return nil, err
	}
	src := sources[slices.IndexFunc(sources, func(src trackerapp.TicketSource) bool { return src.ID == t.connection })]
	p := &planned{t: t, key: qualifier(sources, t.site)(d.Card.Key), src: src, d: d, comment: comment}
	for _, nv := range set {
		if err := s.planStep(caller, p, nv); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// detailOf is t's detail as it is now.
func (s *Server) detailOf(ctx context.Context, t ticket) (trackerapp.DetailView, error) {
	d, err := s.t.IssueDetail(ctx, t.connection, t.issue)
	switch {
	case notFound(err) && t.linked:
		return d, errorsx.Public(CodeNotFound, fmt.Sprintf(
			"%s, the ticket this thread is linked to, is not on %s any more, or the user's account cannot see it.", t.key, t.site), err)
	case notFound(err):
		return d, errorsx.Public(CodeNotFound, fmt.Sprintf("No ticket %s on %s.", t.key, t.site), err)
	}
	return d, err
}

// The steps of a ticket's change, in the order they are sent.
type stepKind int

const (
	stepMove stepKind = iota
	stepEdit
	stepEstimate
	stepSprint
	stepComment
)

// planned is one ticket's change as the call sends it, checked against
// the ticket as it is now (d).
type planned struct {
	t   ticket
	key string
	src trackerapp.TicketSource
	d   trackerapp.DetailView

	move     *trackerapp.MoveRequest
	moveName string
	moveSame bool

	edits     []trackerapp.FieldEditInput
	editNames []string
	// texts are the texts the edits replace whole, as the marks name them.
	texts []string

	estimate     *trackerapp.EstimateRequest
	estimateName string
	estimateSame bool

	sprint     *trackerapp.SprintMoveRequest
	sprintName string
	sprintSame bool

	comment string

	// refused is a check against the ticket as it is now that the call
	// fails, refusedStep the step it refuses.
	refused     string
	refusedStep stepKind

	// changes are the lines an approval shows.
	changes []string
}

// stepNames are the names of p's steps, in order, but the one of kind
// when except is set: what a call that sends nothing did not send.
func (p *planned) stepNames(except bool, kind stepKind) []string {
	var out []string
	for _, k := range []stepKind{stepMove, stepEdit, stepEstimate, stepSprint, stepComment} {
		if except && k == kind {
			continue
		}
		switch k {
		case stepMove:
			if p.move != nil || p.moveSame || p.moveName != "" {
				out = append(out, p.moveName)
			}
		case stepEdit:
			out = append(out, p.editNames...)
		case stepEstimate:
			if p.estimateName != "" {
				out = append(out, p.estimateName)
			}
		case stepSprint:
			if p.sprintName != "" {
				out = append(out, p.sprintName)
			}
		case stepComment:
			if p.comment != "" {
				out = append(out, setComment)
			}
		}
	}
	return out
}

// refuse records the first check the ticket fails.
func (p *planned) refuse(k stepKind, sentence string) {
	if p.refused == "" {
		p.refused, p.refusedStep = sentence, k
	}
}

// topNames are the names a refusal of an unknown name lists first.
var topNames = []string{setStatus, setSprint, setEstimate, setTitle, setAssignee, setPriority, setLabels, setParent, setDescription}

// planStep plans one entry of set on p.
func (s *Server) planStep(caller Caller, p *planned, nv namedValue) error {
	given := strings.TrimSpace(nv.name)
	lower := strings.ToLower(given)
	switch {
	case lower == setStatus:
		return p.planStatus(given, nv.value)
	case lower == setSprint:
		return s.planSprint(p, given, nv.value)
	case lower == setEstimate:
		return s.planEstimate(p, given, nv.value)
	case lower == setType:
		return invalidf("%s's type cannot be changed here; leave out type.", p.key)
	case lower == setComment:
		return invalidf("comment is its own argument; leave it out of set.")
	case readOnlyName(lower):
		return invalidf("%s is the tracker's own; no write sets it.", quoteName(given))
	}
	if id, ok := standardField(lower); ok {
		return s.planField(caller, p, given, trackerapp.FieldView{ID: string(id), Name: given, Editable: true, Kind: standardKind(id)}, nv.value)
	}
	f, ok := fieldMatch(p.d.Fields, given)
	if !ok {
		return invalidf("%s has no field %s; it has %s.", p.key, quoteName(given), fieldNames(p.d.Fields, topNames))
	}
	if s.estimatesBy(p, f.ID) {
		return s.planEstimate(p, given, nv.value)
	}
	if !f.Editable {
		return invalidf("%s's %s cannot be changed here.", p.key, name(f.Name))
	}
	return s.planField(caller, p, given, f, nv.value)
}

// standardKind is the kind of a field a read writes at its top.
func standardKind(id tracker.FieldID) string {
	switch id {
	case tracker.FieldAssignee:
		return tracker.KindUser.String()
	case tracker.FieldPriority, tracker.FieldLabels:
		return tracker.KindOption.String()
	case tracker.FieldDescription:
		return tracker.KindRichText.String()
	}
	return tracker.KindText.String()
}

// planField plans an edit of field f, which set names given, to v.
func (s *Server) planField(caller Caller, p *planned, given string, f trackerapp.FieldView, v setValue) error {
	edit := trackerapp.FieldEditInput{Field: f.ID}
	id := tracker.FieldID(f.ID)
	current := ""
	if len(f.Value.Texts) > 0 {
		current = f.Value.Texts[0]
	}
	text := fieldText(f.ID, 0)
	if id == tracker.FieldDescription {
		current, text = p.d.Description, textDescription
	}
	switch {
	case v.null && id == tracker.FieldTitle:
		return invalidf("The title cannot be cleared; give one.")
	case v.null:
		edit.Value = &trackerapp.FieldValueView{}
	case f.Kind == tracker.KindRichText.String():
		md, err := v.one(given)
		if err != nil {
			return err
		}
		edit.Markdown = &trackerapp.MarkdownEditInput{Markdown: md, From: current}
	case f.Kind == tracker.KindUser.String():
		edit = userValue(v, p.src.Account)
		edit.Field = f.ID
	default:
		edit.Typed = typedOf(f.Kind, id, v)
	}
	// A long text is written back whole: the session must have it whole,
	// as it is now. A text the read shows whole is short, and set whole.
	if f.Kind == tracker.KindRichText.String() || f.Kind == tracker.KindText.String() && jsonLen(current) > fieldHead {
		what := name(f.Name)
		if id == tracker.FieldDescription {
			what = setDescription
		}
		if err := s.marks.textCheck(caller.Session, markKey{p.t.connection, p.d.Card.ID}, p.key, p.d.Card.Updated, text, what, len(current)); err != nil {
			return err
		}
		p.texts = append(p.texts, text)
	}
	p.edits = append(p.edits, edit)
	p.editNames = append(p.editNames, given)
	switch {
	case edit.Markdown != nil:
		p.changes = append(p.changes, fmt.Sprintf("%s → new text (%d characters)", given, len([]rune(edit.Markdown.Markdown))))
	default:
		p.changes = append(p.changes, given+" → "+v.shown())
	}
	return nil
}

// planStatus plans a move to the status v names, matched with the
// ticket's moves by the status each leads to, or by the move's name.
func (p *planned) planStatus(given string, v setValue) error {
	want, err := v.one(given)
	if err != nil {
		return invalidf("status takes the name of a status.")
	}
	want = strings.TrimSpace(want)
	p.moveName = given
	if strings.EqualFold(p.d.Card.Status.Name, want) {
		p.moveSame = true
		return nil
	}
	var to []string
	for _, m := range p.d.Moves {
		if strings.EqualFold(m.To.Name, want) || strings.EqualFold(m.Name, want) {
			p.move = &trackerapp.MoveRequest{Issue: p.d.Card.ID, Move: m.ID}
			p.changes = append(p.changes, "Status → "+name(m.To.Name))
			return nil
		}
		if m.To.Name != "" && !slices.Contains(to, name(m.To.Name)) {
			to = append(to, name(m.To.Name))
		}
	}
	if len(to) == 0 {
		p.refuse(stepMove, fmt.Sprintf("%s: %s cannot move to %s; its workflow allows no move now.", given, p.key, quoteName(want)))
		return nil
	}
	p.refuse(stepMove, fmt.Sprintf("%s: %s cannot move to %s; it can move to %s.", given, p.key, quoteName(want), sayList(to, "or")))
	return nil
}

// boardOf is the first of the ticket's boards here that ok takes, with
// its summary.
func (s *Server) boardOf(p *planned, ok func(trackerapp.TicketRowView, trackerapp.BoardSummary) bool) (trackerapp.TicketRowView, trackerapp.BoardSummary, bool, error) {
	for _, row := range p.d.Rows {
		b, err := s.t.BoardSummary(p.t.connection, row.Board)
		if errors.Is(err, trackerapp.ErrNotMirrored) {
			continue
		}
		if err != nil {
			return row, b, false, err
		}
		if ok(row, b) {
			return row, b, true, nil
		}
	}
	return trackerapp.TicketRowView{}, trackerapp.BoardSummary{}, false, nil
}

// estimatesBy reports whether field is the one a board of the ticket
// estimates by, which set's estimate answers to.
func (s *Server) estimatesBy(p *planned, field string) bool {
	_, _, ok, err := s.boardOf(p, func(_ trackerapp.TicketRowView, b trackerapp.BoardSummary) bool { return b.EstimateField == field })
	return err == nil && ok
}

// planSprint plans a move into the open sprint v names, of the first of
// the ticket's boards here that plans sprints, or to its backlog for
// backlog or null.
func (s *Server) planSprint(p *planned, given string, v setValue) error {
	p.sprintName = given
	want := "backlog"
	if !v.null {
		w, err := v.one(given)
		if err != nil {
			return invalidf("sprint takes a sprint's name, backlog or null.")
		}
		want = strings.TrimSpace(w)
	}
	row, b, ok, err := s.boardOf(p, func(_ trackerapp.TicketRowView, b trackerapp.BoardSummary) bool { return b.PlansSprints })
	if err != nil {
		return err
	}
	if !ok {
		p.refuse(stepSprint, fmt.Sprintf("%s: %s is on no board read on this computer that plans sprints; ask the user to add its board in Settings, Issues, or open it there.", given, p.key))
		return nil
	}
	var open []trackerapp.SprintView
	for _, sp := range b.Sprints {
		if sp.State == "active" || sp.State == "future" {
			open = append(open, sp)
		}
	}
	if strings.EqualFold(want, "backlog") {
		p.sprintSame = !slices.ContainsFunc(open, func(sp trackerapp.SprintView) bool { return slices.Contains(p.d.Card.Sprints, sp.ID) })
		p.sprint = &trackerapp.SprintMoveRequest{Board: row.Board, Issues: []string{p.d.Card.ID}}
		p.changes = append(p.changes, "Sprint → backlog")
		return nil
	}
	i := slices.IndexFunc(open, func(sp trackerapp.SprintView) bool { return strings.EqualFold(sp.Name, want) })
	if i < 0 {
		names := make([]string, len(open))
		for j, sp := range open {
			names[j] = name(sp.Name)
		}
		if len(names) == 0 {
			p.refuse(stepSprint, fmt.Sprintf("%s: board %s has no open sprint.", given, name(b.Name)))
		} else {
			p.refuse(stepSprint, fmt.Sprintf("%s: board %s has no open sprint called %s; its open sprints are %s.", given, name(b.Name), quoteName(want), sayList(names, "and")))
		}
		return nil
	}
	p.sprintSame = slices.Contains(p.d.Card.Sprints, open[i].ID)
	p.sprint = &trackerapp.SprintMoveRequest{Board: row.Board, Sprint: open[i].ID, Issues: []string{p.d.Card.ID}}
	p.changes = append(p.changes, "Sprint → "+name(open[i].Name))
	return nil
}

// planEstimate plans the estimate v gives, on the first of the ticket's
// boards here that has estimates, in its unit.
func (s *Server) planEstimate(p *planned, given string, v setValue) error {
	p.estimateName = given
	row, b, ok, err := s.boardOf(p, func(r trackerapp.TicketRowView, _ trackerapp.BoardSummary) bool {
		return r.Unit == "points" || r.Unit == "time"
	})
	if err != nil {
		return err
	}
	if !ok {
		p.refuse(stepEstimate, fmt.Sprintf("%s: %s is on no board read on this computer that has estimates; ask the user to add its board in Settings, Issues, or open it there.", given, p.key))
		return nil
	}
	in := trackerapp.EstimateRequest{Board: row.Board, Issue: p.d.Card.ID}
	if v.null {
		p.estimateSame = row.Estimate == nil
		p.changes = append(p.changes, "Estimate → nothing")
	} else {
		text, err := v.one(given)
		if err != nil {
			return err
		}
		n, err := estimateValue(text, row.Unit, b.Name)
		if err != nil {
			return err
		}
		in.Value = &n
		p.estimateSame = row.Estimate != nil && *row.Estimate == n
		p.changes = append(p.changes, "Estimate → "+estimateText(n, row.Unit))
	}
	p.estimate = &in
	return nil
}

// approvalOf is the approval of plans, the call id's.
func approvalOf(id string, plans []*planned, comment string) Approval {
	a := Approval{ID: id, Comment: comment}
	for _, p := range plans {
		title, _ := textClip(p.d.Card.Title, 0, refCut)
		a.Tickets = append(a.Tickets, ApprovalTicket{Key: p.key, Title: title, Changes: p.changes})
	}
	return a
}

// sendStep is one step of a ticket's change as apply runs it.
type sendStep struct {
	kind  stepKind
	names []string
	same  bool
	send  func() (trackerapp.WriteView, error)
	// what names the change in an unknown outcome's sentence.
	what string
}

// apply sends p's steps in order on ctx, which carries the agent, and
// answers what became of each, and the ticket as the timeline shows it.
func (s *Server) apply(ctx context.Context, caller Caller, p *planned) (ticketAnswer, WrittenTicket) {
	conn := p.t.connection
	var steps []sendStep
	if p.move != nil || p.moveSame {
		steps = append(steps, sendStep{kind: stepMove, names: []string{p.moveName}, same: p.moveSame, what: "move",
			send: func() (trackerapp.WriteView, error) { return s.t.Move(ctx, conn, *p.move) }})
	}
	if len(p.edits) > 0 {
		steps = append(steps, sendStep{kind: stepEdit, names: p.editNames, what: "change",
			send: func() (trackerapp.WriteView, error) {
				return s.t.Edit(ctx, conn, trackerapp.EditRequest{Issue: p.d.Card.ID, Fields: p.edits})
			}})
	}
	if p.estimate != nil {
		steps = append(steps, sendStep{kind: stepEstimate, names: []string{p.estimateName}, same: p.estimateSame, what: "estimate",
			send: func() (trackerapp.WriteView, error) { return s.t.SetEstimate(ctx, conn, *p.estimate) }})
	}
	if p.sprint != nil {
		steps = append(steps, sendStep{kind: stepSprint, names: []string{p.sprintName}, same: p.sprintSame, what: "sprint move",
			send: func() (trackerapp.WriteView, error) { return s.t.MoveToSprint(ctx, conn, *p.sprint) }})
	}
	if p.comment != "" {
		steps = append(steps, sendStep{kind: stepComment, names: []string{setComment}, what: "comment",
			send: func() (trackerapp.WriteView, error) {
				return s.t.AddComment(ctx, conn, trackerapp.CommentRequest{Issue: p.d.Card.ID, Markdown: p.comment})
			}})
	}
	out := ticketAnswer{Key: p.key}
	written := WrittenTicket{Connection: conn, Issue: p.d.Card.ID, Key: p.key, Title: p.d.Card.Title}
	stopped := false
	for _, st := range steps {
		switch {
		case stopped:
			out.NotSent = append(out.NotSent, st.names...)
			continue
		case st.same:
			out.Applied = append(out.Applied, st.names...)
			continue
		}
		if st.kind == stepComment {
			made, note, refusal := s.repeatComment(ctx, caller, p)
			switch {
			case refusal != "":
				out.Refused, stopped = refusal, true
				continue
			case made:
				out.Applied, out.Note = append(out.Applied, setComment), note
				written.Applied = append(written.Applied, setComment)
				continue
			}
		}
		v, err := st.send()
		state, sentence := judge(p.src.Kind, strings.Join(st.names, ", "), st.what, v, err)
		switch state {
		case tracker.StateApplied:
			out.Applied = append(out.Applied, st.names...)
			written.Applied = append(written.Applied, st.names...)
		case tracker.StateUnknown:
			out.Unknown, stopped = sentence, true
		default:
			out.Refused, stopped = sentence, true
		}
	}
	if len(written.Applied) > 0 {
		// The call's own writes moved the ticket's update time: a mark of
		// the version it was made from follows it.
		if card, ok, err := s.t.MirrorCard(conn, p.d.Card.ID); err == nil && ok {
			s.marks.wrote(caller.Session, markKey{conn, p.d.Card.ID}, p.d.Card.Updated, card.Updated, p.texts)
		}
	}
	return out, written
}

// repeatComment checks p's comment against an earlier call's that added
// the same comment in the last hour: one applied is made, and one whose
// answer never came is looked for among the ticket's latest comments, by
// the connection's account, near the time it was sent. made reports a
// comment that is there, with the note that says so; a refusal is a check
// that could not be made, so the comment is not sent.
func (s *Server) repeatComment(ctx context.Context, caller Caller, p *planned) (made bool, note, refusal string) {
	const repeated = "Your earlier call added this comment; it was not added again."
	rep, ok, err := s.t.AgentRepeat(caller.Thread, trackerapp.CommentDigest(p.t.connection, p.d.Card.ID, p.comment))
	switch {
	case err != nil:
		return false, "", "comment: the earlier calls could not be checked for this comment, so it was not sent; the same call again tries again."
	case !ok:
		return false, "", ""
	case rep.State == "applied":
		return true, repeated, ""
	}
	page, err := s.t.Comments(ctx, p.t.connection, p.d.Card.ID, "")
	if err != nil {
		return false, "", "comment: an earlier call's comment may have been added, and the ticket's comments could not be read to see, so it was not sent; the same call again tries again."
	}
	for _, c := range page.Comments {
		if p.src.Account != "" && c.Author.ID == p.src.Account && c.Created >= rep.At-repeatBefore && c.Created <= rep.At+repeatAfter {
			if err := s.t.SettleAgentRepeat(rep.Row, "", c.ID); err != nil {
				// The comment is there; a later repeat looks for it again.
				s.logf("tracker tools: record the comment %s found on %s: %v", c.ID, p.key, err)
			}
			return true, repeated, ""
		}
	}
	return false, "", ""
}

// A comment or create whose answer never came is looked for from
// repeatBefore before the time it was sent, for the clocks' difference,
// to repeatAfter after.
const (
	repeatBefore = 60_000
	repeatAfter  = 120_000
)

// judge is what became of one step, names (the call's names for it) on a
// tracker of kind: its write's view v or its error.
func judge(kind, names, what string, v trackerapp.WriteView, err error) (tracker.State, string) {
	if err != nil {
		if _, msg, ok := errorsx.PublicDetails(err); ok {
			if strings.Contains(msg, "may or may not") {
				return tracker.StateUnknown, names + ": " + msg
			}
			return tracker.StateRefused, names + ": " + msg
		}
		return tracker.StateUnknown, unknownSentence(kind, names, what)
	}
	state := tracker.StateApplied
	var refusal *trackerapp.RefusalView
	var failure *trackerapp.FailureView
	for _, part := range v.Parts {
		switch part.State {
		case "refused":
			if refusal == nil {
				refusal = part.Refusal
			}
			state = tracker.StateRefused
		case "unknown":
			if state != tracker.StateRefused {
				state = tracker.StateUnknown
			}
		case "not_sent":
			if state == tracker.StateApplied {
				state, failure = tracker.StateNotSent, part.Failure
			}
		}
	}
	switch state {
	case tracker.StateApplied:
		return state, ""
	case tracker.StateUnknown:
		return state, unknownSentence(kind, names, what)
	case tracker.StateRefused:
		if refusal != nil {
			return state, names + ": " + refusalText(refusal)
		}
	}
	if failure != nil {
		return tracker.StateRefused, names + ": " + failure.Message
	}
	return tracker.StateRefused, names + ": the tracker did not make it."
}

// unknownSentence is a step whose answer never came on a tracker of kind.
func unknownSentence(kind, names, what string) string {
	if what == "comment" {
		return fmt.Sprintf("%s: %s's answer never came, so the comment may or may not have been added; the same call again adds it only if it was not.", names, trackerNameOf(kind))
	}
	return fmt.Sprintf("%s: %s's answer never came, so the %s may or may not have been made; the same call again makes only what was not.", names, trackerNameOf(kind), what)
}

// trackerNameOf is a tracker kind's name.
func trackerNameOf(kind string) string {
	switch kind {
	case "jira":
		return "Jira"
	case "linear":
		return "Linear"
	}
	return "The tracker"
}

// refusalText is a refusal as a model reads it: the provider's sentence
// without its package prefix, and the values a text could match.
func refusalText(r *trackerapp.RefusalView) string {
	msg := strings.TrimSpace(r.Message)
	for _, prefix := range []string{"jira: ", "linear: ", "linearprovider: ", "jiraprovider: ", "tracker: "} {
		msg = strings.TrimPrefix(msg, prefix)
	}
	if i := strings.Index(msg, "; candidates: "); i >= 0 {
		msg = msg[:i]
	}
	if r.Reason == "no_match" && len(r.Candidates) > 0 {
		names := make([]string, 0, 10)
		for i, c := range r.Candidates {
			if i == 10 {
				names = append(names, fmt.Sprintf("%d more", len(r.Candidates)-10))
				break
			}
			names = append(names, name(cmpOr(c.Name, c.ID)))
		}
		msg += "; it takes " + sayList(names, "or")
	}
	shown, _ := textClip(msg, 0, refusalCut)
	return strings.TrimSuffix(shown, ".") + "."
}
