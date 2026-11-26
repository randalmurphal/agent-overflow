package trackertools

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/tracker"
	"agent-overflow/internal/trackerapp"
)

// Planning. A call's tickets are read as far as its steps need, each
// connection's together (planCall.readTickets): a comment alone needs each
// ticket's ID, key and title, which the key's lookup or the mirror gives
// with no request; an entry judged on the ticket's card and rows alone
// (needsDetail) needs a fresh card, all of them in one cards read; any
// other needs the ticket's detail as it is now, all of them in one read.
// Each entry is checked against that read, and the boards a status,
// sprint or estimate is judged on are read once per call
// (planCall.board). The reads share the call's write session, so the
// first write to a ticket takes what its detail read found
// (trackerapp.WithWrites).

// planCall is the planning of one call: who calls, the sources on, and
// the boards read so far, by connection and board.
type planCall struct {
	s       *Server
	caller  Caller
	sources []trackerapp.TicketSource
	read    map[[2]string]boardRead
}

// boardRead is a board as the mirror holds it; ok is false for one the
// mirror does not hold.
type boardRead struct {
	b  trackerapp.BoardSummary
	ok bool
}

// boardOn is one of a ticket's boards the mirror holds: the ticket's row
// on it and the board.
type boardOn struct {
	row trackerapp.TicketRowView
	b   trackerapp.BoardSummary
}

func newPlanCall(s *Server, caller Caller, sources []trackerapp.TicketSource) *planCall {
	return &planCall{s: s, caller: caller, sources: sources, read: map[[2]string]boardRead{}}
}

// needsDetail reports whether planStep judges the set entry name on the
// ticket's detail: a status (its moves), the description (its text), or
// a field of the ticket's own (its fields, and the board's estimate
// field). The sprint, the estimate, the fields every ticket has and the
// names refused whatever the ticket holds are judged on its card and
// rows.
func needsDetail(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	switch {
	case lower == setStatus, lower == setDescription:
		return true
	case lower == setSprint, lower == setEstimate, lower == setType, lower == setComment, readOnlyName(lower):
		return false
	}
	_, standard := standardField(lower)
	return !standard
}

// readTickets reads tickets as far as set needs (a detail, else the card,
// through its estimate board's rows when set names the estimate), comment
// their ticket alone, each connection's together, and answers each
// ticket's plan with its card, or its read's failure, in tickets' order. A
// ticket its estimate board no longer lists is refused its estimate. A
// read of several that fails as a whole fails the call.
func (pc *planCall) readTickets(ctx context.Context, tickets []ticket, set []namedValue, comment string) ([]*planned, []error, error) {
	detail, card, estimate := false, len(set) > 0, ""
	for _, nv := range set {
		detail = detail || needsDetail(nv.name)
		if given := strings.TrimSpace(nv.name); strings.EqualFold(given, setEstimate) {
			estimate = given
		}
	}
	plans := make([]*planned, len(tickets))
	errs := make([]error, len(tickets))
	var details, cards, onRows []int
	target := map[int]estimateOn{}
	for i, t := range tickets {
		j := slices.IndexFunc(pc.sources, func(src trackerapp.TicketSource) bool { return src.ID == t.connection })
		if j < 0 {
			errs[i] = errorsx.Public(CodeNotFound, fmt.Sprintf("The connection that reads %s is no longer on.", t.key), nil)
			continue
		}
		plans[i] = &planned{t: t, src: pc.sources[j], comment: comment}
		switch {
		case detail:
			details = append(details, i)
		case card && estimate != "":
			switch on, ok, err := pc.heldTarget(plans[i]); {
			case err != nil:
				errs[i] = err
			case ok:
				target[i], onRows = on, append(onRows, i)
			default:
				cards = append(cards, i)
			}
		case card:
			cards = append(cards, i)
		default:
			switch c, held, err := pc.s.heldCard(t); {
			case err != nil:
				errs[i] = err
			case held:
				plans[i].d.Card = c
			default:
				details = append(details, i)
			}
		}
	}
	unkept, _, err := pc.readCards(ctx, plans, cards, errs, func(ctx context.Context, conn string, group []int) ([]trackerapp.TicketCardRead, error) {
		return pc.s.t.IssueCards(ctx, conn, issuesOf(plans, group))
	})
	if err != nil {
		return nil, nil, err
	}
	unkeptRows, off, err := pc.readCards(ctx, plans, onRows, errs, func(ctx context.Context, conn string, group []int) ([]trackerapp.TicketCardRead, error) {
		rows := make([]trackerapp.IssueRow, len(group))
		for k, i := range group {
			rows[k] = trackerapp.IssueRow{Issue: tracker.IssueID(plans[i].t.issue), Board: tracker.BoardID(target[i].board)}
		}
		return pc.s.t.IssueRows(ctx, conn, rows)
	})
	if err != nil {
		return nil, nil, err
	}
	if err := pc.readDetails(ctx, plans, slices.Sorted(slices.Values(slices.Concat(details, unkept, unkeptRows))), errs); err != nil {
		return nil, nil, err
	}
	for i, p := range plans {
		if p != nil && errs[i] == nil {
			p.key = qualifier(pc.sources, p.t.site)(p.d.Card.Key)
		}
		if slices.Contains(off, i) {
			p.estimateName = estimate
			p.refuse(stepEstimate, fmt.Sprintf("%s: %s is not on board %s any more, or the user's account cannot see it; read it again and call again for it.", estimate, p.key, quoteName(target[i].b.Name)))
		}
	}
	return plans, errs, nil
}

// heldTarget is the board p's ticket's estimate is planned on, of its
// boards as the mirror holds them (estimateTarget), with p's card set to
// the mirror's; ok is false when the mirror keeps no such board or no
// card of the ticket.
func (pc *planCall) heldTarget(p *planned) (estimateOn, bool, error) {
	v, held, err := pc.s.t.MirrorIssue(p.t.connection, p.t.issue)
	if err != nil || !held {
		return estimateOn{}, false, err
	}
	boards := make([]string, len(v.Boards))
	for i, b := range v.Boards {
		boards[i] = b.Board
	}
	p.d.Card = v.Card
	return pc.estimateTarget(p.t.connection, boards, "")
}

// recheck reads the cards of plans again, once per connection, after the
// person approved what set changes: those that set an estimate through
// their row on its board (IssueRows), whose reads the estimates then
// take, and the others by their keys (Lookup). A ticket the read no
// longer finds, whose card moved since its plan read it, or whose
// estimate on the board its plan judged moved, or that board no longer
// lists, is sent nothing (planned.changed): a write sent now would be
// judged on a ticket the person did not see. A card moved when its update
// time did, or the status or sprints the plan judged on, which a tracker
// might change without moving the update time. A read that fails is the
// call's error, nothing sent. A comment's audience is not judged on this
// read: the comment phase looks its tickets up after the call's other
// writes (Server.readAudience).
func (pc *planCall) recheck(ctx context.Context, plans []*planned) error {
	var conns []string
	for _, p := range plans {
		if !slices.Contains(conns, p.t.connection) {
			conns = append(conns, p.t.connection)
		}
	}
	for _, conn := range conns {
		var on, est []*planned
		var keys []string
		for _, p := range plans {
			switch {
			case p.t.connection != conn:
			case p.estimate != nil:
				est = append(est, p)
			default:
				on, keys = append(on, p), append(keys, p.d.Card.Key)
			}
		}
		if err := pc.recheckEstimates(ctx, conn, est); err != nil {
			return err
		}
		if len(on) == 0 {
			continue
		}
		found, err := pc.s.t.Lookup(ctx, conn, keys)
		if err != nil {
			site := on[0].t.site
			if code, msg, ok := errorsx.PublicDetails(err); ok {
				return errorsx.Public(code, "Nothing was sent: the tickets could not be read again after the user approved. "+site+": "+msg, err)
			}
			return fmt.Errorf("nothing was sent: read the tickets again after the user approved: %s: %w", site, err)
		}
		for _, p := range on {
			i := slices.IndexFunc(found.Found, func(m trackerapp.KeyMatchView) bool { return m.Card.ID == p.d.Card.ID })
			switch {
			case i < 0:
				p.changed = goneSentence(p, "nothing was sent to it")
			case cardMoved(p.d.Card, found.Found[i].Card):
				p.changed = changedSentence(p)
			}
		}
	}
	return nil
}

// changedSentence is why p, changed during the wait, was sent nothing.
func changedSentence(p *planned) string {
	return fmt.Sprintf("%s changed on %s while the user was deciding, so nothing was sent to it; read it again and call again for it.", p.key, trackerNameOf(p.src.Kind))
}

// recheckEstimates reads est's plans, of connection conn, each setting an
// estimate, through their rows on their estimates' boards, in one read
// (IssueRows): a plan whose board no longer lists the ticket, whose card
// moved, or whose estimate there moved, is changed.
func (pc *planCall) recheckEstimates(ctx context.Context, conn string, est []*planned) error {
	if len(est) == 0 {
		return nil
	}
	rows := make([]trackerapp.IssueRow, len(est))
	for k, p := range est {
		rows[k] = trackerapp.IssueRow{Issue: tracker.IssueID(p.d.Card.ID), Board: tracker.BoardID(p.estimate.Board)}
	}
	got, err := pc.s.t.IssueRows(ctx, conn, rows)
	if err != nil {
		site := est[0].t.site
		if code, msg, ok := errorsx.PublicDetails(err); ok {
			return errorsx.Public(code, "Nothing was sent: the tickets' estimates could not be read again after the user approved. "+site+": "+msg, err)
		}
		return fmt.Errorf("nothing was sent: read the tickets' estimates again after the user approved: %s: %w", site, err)
	}
	for k, p := range est {
		board := p.estimate.Board
		was := slices.IndexFunc(p.d.Rows, func(r trackerapp.TicketRowView) bool { return r.Board == board })
		switch r := got[k]; {
		case errors.Is(r.Err, trackerapp.ErrNotOnBoard):
			name := board
			if b, held, err := pc.board(conn, board); err == nil && held {
				name = b.Name
			}
			p.changed = fmt.Sprintf("%s is not on board %s any more, or the user's account cannot see it, so nothing was sent to it; read it again and call again for it.", p.key, quoteName(name))
		case r.Err != nil:
			// Not kept by the mirror any more.
			p.changed = fmt.Sprintf("%s could not be read again on its board after the user approved, so nothing was sent to it; read it again and call again for it.", p.key)
		case cardMoved(p.d.Card, r.View.Card):
			p.changed = changedSentence(p)
		default:
			now := slices.IndexFunc(r.View.Rows, func(r trackerapp.TicketRowView) bool { return r.Board == board })
			if !sameNumber(p.d.Rows[was].Estimate, r.View.Rows[now].Estimate) {
				p.changed = changedSentence(p)
			}
		}
	}
	return nil
}

// sameNumber reports whether a and b are both none or equal.
func sameNumber(a, b *float64) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

// goneSentence is the sentence of p's ticket a read made after its plan
// no longer finds: so it was sent what (so).
func goneSentence(p *planned, so string) string {
	return fmt.Sprintf("%s is not on %s any more, or the user's account cannot see it, so %s.", p.key, trackerNameOf(p.src.Kind), so)
}

// cardMoved reports whether now is not the card was: its update time,
// status or sprints differ.
func cardMoved(was, now trackerapp.CardView) bool {
	sorted := func(s []string) []string {
		s = slices.Clone(s)
		slices.Sort(s)
		return s
	}
	return was.Updated != now.Updated || was.Status.ID != now.Status.ID || !slices.Equal(sorted(was.Sprints), sorted(now.Sprints))
}

// heldCard is t's ID, key and title with no request: from the lookup that
// found it, else the mirror; held is false when neither has it.
func (s *Server) heldCard(t ticket) (card trackerapp.CardView, held bool, _ error) {
	if t.title != "" {
		return trackerapp.CardView{ID: t.issue, Key: t.key, Title: t.title}, true, nil
	}
	v, held, err := s.t.MirrorIssue(t.connection, t.issue)
	return v.Card, held, err
}

// byConnection is at, indexes of plans, grouped by connection in the
// order each first appears.
func byConnection(plans []*planned, at []int) [][]int {
	var out [][]int
	for _, i := range at {
		k := slices.IndexFunc(out, func(g []int) bool { return plans[g[0]].t.connection == plans[i].t.connection })
		if k < 0 {
			out = append(out, []int{i})
		} else {
			out[k] = append(out[k], i)
		}
	}
	return out
}

// issuesOf are the issues of plans at.
func issuesOf(plans []*planned, at []int) []string {
	out := make([]string, len(at))
	for k, i := range at {
		out[k] = plans[i].t.issue
	}
	return out
}

// readCards reads the cards of plans at with read (IssueCards, or
// IssueRows for a call that sets an estimate, which judges it on its
// board's rows as they are now), a connection's group in one read, into
// each plan's detail (its card and rows), or its failure into errs. It
// returns those the mirror does not keep, whose detail is read instead,
// and those the rows read found off their board (trackerapp
// ErrNotOnBoard), which keep the card they had.
func (pc *planCall) readCards(ctx context.Context, plans []*planned, at []int, errs []error, read func(ctx context.Context, conn string, group []int) ([]trackerapp.TicketCardRead, error)) (unkept, off []int, _ error) {
	for _, group := range byConnection(plans, at) {
		got, err := read(ctx, plans[group[0]].t.connection, group)
		if err != nil {
			return nil, nil, err
		}
		for k, i := range group {
			switch r := got[k]; {
			case errors.Is(r.Err, trackerapp.ErrNotMirrored):
				unkept = append(unkept, i)
			case errors.Is(r.Err, trackerapp.ErrNotOnBoard):
				off = append(off, i)
			case r.Err != nil:
				errs[i] = readFailure(plans[i].t, r.Err)
			default:
				plans[i].d = trackerapp.DetailView{Card: r.View.Card, Rows: r.View.Rows}
			}
		}
	}
	return unkept, off, nil
}

// readDetails reads the details of plans at as they are now, a
// connection's in one read, into each plan, or its failure into errs.
func (pc *planCall) readDetails(ctx context.Context, plans []*planned, at []int, errs []error) error {
	for _, group := range byConnection(plans, at) {
		got, err := pc.s.t.IssueDetails(ctx, plans[group[0]].t.connection, issuesOf(plans, group))
		if err != nil {
			return err
		}
		for k, i := range group {
			if got[k].Err != nil {
				errs[i] = readFailure(plans[i].t, got[k].Err)
				continue
			}
			plans[i].d = got[k].View
		}
	}
	return nil
}

// readFailure is err, the failure of t's own read, as the call's: a
// ticket the tracker did not return named as missing.
func readFailure(t ticket, err error) error {
	switch {
	case notFound(err) && t.linked:
		return errorsx.Public(CodeNotFound, fmt.Sprintf(
			"%s, the ticket this thread is linked to, is not on %s any more, or the user's account cannot see it.", t.key, t.site), err)
	case notFound(err):
		return errorsx.Public(CodeNotFound, fmt.Sprintf("No ticket %s on %s.", t.key, t.site), err)
	}
	return err
}

// planSteps plans each entry of set on p.
func (pc *planCall) planSteps(p *planned, set []namedValue) error {
	for _, nv := range set {
		if err := pc.planStep(p, nv); err != nil {
			return err
		}
	}
	return nil
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

// stepOrder is the order the steps are sent in.
var stepOrder = [...]stepKind{stepMove, stepEdit, stepEstimate, stepSprint, stepComment}

// step is one step of a ticket's change as the call named it: its kind,
// the names the call gave it, whether the ticket holds it already, and
// what an unknown outcome's sentence calls it.
type step struct {
	kind  stepKind
	names []string
	same  bool
	what  string
}

// planned is one ticket's change as the call sends it, checked against
// the ticket as it is now (d; only its card for a comment alone).
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
	// changed is why the ticket is sent nothing though the person approved
	// it: the read after the approval found it moved or gone (recheck).
	changed string

	// changes are the lines an approval shows.
	changes []ApprovalChange
}

// steps are p's steps the call named, in the order they are sent, a
// step the plan refused included: the one account of them, which the
// sends (Server.stepsOf) and every answer of what was not sent
// (stepNames) read.
func (p *planned) steps() []step {
	var out []step
	for _, k := range stepOrder {
		switch {
		case k == stepMove && p.moveName != "":
			out = append(out, step{kind: k, names: []string{p.moveName}, same: p.moveSame, what: "move"})
		case k == stepEdit && len(p.editNames) > 0:
			out = append(out, step{kind: k, names: p.editNames, what: "change"})
		case k == stepEstimate && p.estimateName != "":
			out = append(out, step{kind: k, names: []string{p.estimateName}, same: p.estimateSame, what: "estimate"})
		case k == stepSprint && p.sprintName != "":
			out = append(out, step{kind: k, names: []string{p.sprintName}, same: p.sprintSame, what: "sprint move"})
		case k == stepComment && p.comment != "":
			out = append(out, step{kind: k, names: []string{setComment}, what: "comment"})
		}
	}
	return out
}

// stepNames are the names of p's steps, in order, but the one of kind
// when except is set: what a call that sends nothing did not send.
func (p *planned) stepNames(except bool, kind stepKind) []string {
	var out []string
	for _, st := range p.steps() {
		if !except || st.kind != kind {
			out = append(out, st.names...)
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
func (pc *planCall) planStep(p *planned, nv namedValue) error {
	given := strings.TrimSpace(nv.name)
	lower := strings.ToLower(given)
	switch {
	case lower == setStatus:
		return p.planStatus(given, nv.value)
	case lower == setSprint:
		return pc.planSprint(p, given, nv.value)
	case lower == setEstimate:
		on, ok, err := pc.estimateTarget(p.t.connection, rowBoards(p.d.Rows), "")
		switch {
		case err != nil:
			return err
		case !ok:
			p.estimateName = given
			p.refuse(stepEstimate, fmt.Sprintf("%s: %s is on no board read on this computer that has estimates; ask the user to add its board in Settings, Issues, or open it there.", given, p.key))
			return nil
		}
		return pc.planEstimate(p, given, nv.value, on)
	case lower == setType:
		return invalidf("%s's type cannot be changed here; leave out type.", p.key)
	case lower == setComment:
		return invalidf("comment is its own argument; leave it out of set.")
	case readOnlyName(lower):
		return invalidf("%s is the tracker's own; no write sets it.", quoteName(given))
	}
	if id, ok := standardField(lower); ok {
		return pc.planField(p, given, trackerapp.FieldView{ID: string(id), Name: given, Editable: true, Kind: standardKind(id)}, nv)
	}
	f, ok := fieldMatch(p.d.Fields, given)
	if !ok {
		return invalidf("%s has no field %s; it has %s.", p.key, quoteName(given), fieldNames(p.d.Fields, topNames))
	}
	if on, by, err := pc.estimateTarget(p.t.connection, rowBoards(p.d.Rows), f.ID); err != nil {
		return err
	} else if by {
		return pc.planEstimate(p, given, nv.value, on)
	}
	if !f.Editable {
		return invalidf("%s's %s cannot be changed here.", p.key, quoteName(f.Name))
	}
	return pc.planField(p, given, f, nv)
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
	case tracker.FieldTitle, tracker.FieldType, tracker.FieldParent:
	}
	return tracker.KindText.String()
}

// planField plans an edit of field f, which set names given, to nv's
// value. An edit that replaces a text whole, a rich text or a plain text
// the session must hold whole (marks.go), carries what it replaces, which
// the tracker compares when the edit is sent: a text a person changed
// after the plan read it is refused as stale, not overwritten. Every other
// value is checked only as the whole ticket is, by the read after an
// approval (recheck).
func (pc *planCall) planField(p *planned, given string, f trackerapp.FieldView, nv namedValue) error {
	v := nv.value
	edit := trackerapp.FieldEditInput{Field: f.ID}
	id := tracker.FieldID(f.ID)
	rich := f.Kind == tracker.KindRichText.String()
	switch {
	case v.null && id == tracker.FieldTitle:
		return invalidf("The title cannot be cleared; give one.")
	case rich:
		current, text := "", fieldText(f.ID, 0)
		if len(f.Value.Texts) > 0 {
			current = f.Value.Texts[0]
		}
		if id == tracker.FieldDescription {
			current, text = p.d.Description, textDescription
		}
		md := ""
		if !v.null {
			var err error
			if md, err = v.one(given); err != nil {
				return err
			}
		}
		// A clear is the empty Markdown, which carries what it replaces as
		// every Markdown edit does.
		edit.Markdown = &trackerapp.MarkdownEditInput{Markdown: md, From: current}
		if err := pc.s.marks.textCheck(pc.caller.Session, markKey{p.t.connection, p.d.Card.ID}, p.key, p.d.Card.Updated, text, richName(id, f.Name), len(current)); err != nil {
			return err
		}
		p.texts = append(p.texts, text)
	case v.null:
		edit.Value = &trackerapp.FieldValueView{}
	case f.Kind == tracker.KindUser.String():
		edit = userValue(v, p.src.Account)
		edit.Field = f.ID
	case id == tracker.FieldParent:
		typed, err := parentKeys(v, p.src.Kind, p.t.site, p.key)
		if err != nil {
			return err
		}
		edit.Typed = typed
	default:
		edit.Typed = typedOf(f.Kind, v)
	}
	if f.Kind == tracker.KindText.String() {
		replaced, err := pc.longTexts(p, f)
		if err != nil {
			return err
		}
		if len(replaced) > 0 {
			current := slices.Clone(f.Value.Texts)
			edit.Replaces = &current
			p.texts = append(p.texts, replaced...)
		}
	}
	p.edits = append(p.edits, edit)
	p.editNames = append(p.editNames, given)
	line := ApprovalChange{Line: given + " → " + v.shown()}
	switch {
	case edit.Markdown != nil && v.null:
		line.Line = given + " → nothing"
	case edit.Markdown != nil:
		line = ApprovalChange{Line: fmt.Sprintf("%s → new text (%d characters)", given, len([]rune(edit.Markdown.Markdown))), Value: nv.name}
	case v.cut():
		line.Value = nv.name
	}
	p.changes = append(p.changes, line)
	return nil
}

// richName is a rich text field as a refusal names it.
func richName(id tracker.FieldID, fieldName string) string {
	if id == tracker.FieldDescription {
		return setDescription
	}
	return quoteName(fieldName)
}

// longTexts are the texts of plain text field f the session must hold
// whole before an edit replaces them, checked: each value longer than a
// first answer shows, and every value when a read of the ticket at its
// current version gave some of them and not all whole.
func (pc *planCall) longTexts(p *planned, f trackerapp.FieldView) ([]string, error) {
	k := markKey{p.t.connection, p.d.Card.ID}
	part := pc.s.marks.partOf(pc.caller.Session, k, p.d.Card.Updated, f.ID, f.Value.Texts)
	var out []string
	for j, current := range f.Value.Texts {
		if !part && jsonLen(current) <= fieldHead {
			continue
		}
		text := fieldText(f.ID, j)
		if err := pc.s.marks.textCheck(pc.caller.Session, k, p.key, p.d.Card.Updated, text, quoteName(f.Name), len(current)); err != nil {
			return nil, err
		}
		out = append(out, text)
	}
	return out, nil
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
			p.changes = append(p.changes, ApprovalChange{Line: "Status → " + name(m.To.Name)})
			return nil
		}
		if m.To.Name != "" && !slices.Contains(to, quoteName(m.To.Name)) {
			to = append(to, quoteName(m.To.Name))
		}
	}
	if len(to) == 0 {
		p.refuse(stepMove, fmt.Sprintf("%s: %s cannot move to %s; its workflow allows no move now.", given, p.key, quoteName(want)))
		return nil
	}
	p.refuse(stepMove, fmt.Sprintf("%s: %s cannot move to %s; it can move to %s.", given, p.key, quoteName(want), sayList(to, "or")))
	return nil
}

// board is board id of connection as the mirror holds it, read once per
// call.
func (pc *planCall) board(connection, id string) (trackerapp.BoardSummary, bool, error) {
	k := [2]string{connection, id}
	if r, ok := pc.read[k]; ok {
		return r.b, r.ok, nil
	}
	b, err := pc.s.t.BoardSummary(connection, id)
	switch {
	case errors.Is(err, trackerapp.ErrNotMirrored):
		pc.read[k] = boardRead{}
		return b, false, nil
	case err != nil:
		return b, false, err
	}
	pc.read[k] = boardRead{b: b, ok: true}
	return b, true, nil
}

// boardsOf are p's ticket's boards the mirror holds, in the ticket's
// order, that ok takes.
func (pc *planCall) boardsOf(p *planned, ok func(boardOn) bool) ([]boardOn, error) {
	var out []boardOn
	for _, row := range p.d.Rows {
		b, held, err := pc.board(p.t.connection, row.Board)
		if err != nil {
			return nil, err
		}
		if held && ok(boardOn{row: row, b: b}) {
			out = append(out, boardOn{row: row, b: b})
		}
	}
	return out, nil
}

// estimateTarget is the board of connection an estimate set by field is
// planned, checked and sent on: of boards, a ticket's boards in board
// order, the first the mirror holds that estimates by field, or for field
// "" (set's estimate) the first that has estimates, in points or time.
// ok is false when none does. It is the one rule of an estimate's board:
// the plan judges the estimate on that board's row, the approval words it
// in that board's unit, the read after an approval and the send name that
// board. ticket_read prints that board's estimate from the ticket's rows,
// each carrying its board's unit (sheet.estimate), and the ticket screen
// shows and sets the same board's points when it was opened from no
// board with estimates (ticketRow in
// frontend/src/lib/stores/trackerTicketModel.ts).
func (pc *planCall) estimateTarget(connection string, boards []string, field string) (_ estimateOn, ok bool, _ error) {
	for _, id := range boards {
		b, held, err := pc.board(connection, id)
		if err != nil {
			return estimateOn{}, false, err
		}
		if !held || (field == "" && !hasEstimates(b.Unit)) || (field != "" && b.EstimateField != field) {
			continue
		}
		return estimateOn{board: id, b: b}, true, nil
	}
	return estimateOn{}, false, nil
}

// hasEstimates reports whether a board of unit, or a ticket's row on it,
// has estimates: in points or in time.
func hasEstimates(unit string) bool {
	return unit == "points" || unit == "time"
}

// estimateOn is the board an estimate is planned on (estimateTarget).
type estimateOn struct {
	board string
	b     trackerapp.BoardSummary
}

// rowBoards are the boards of rows, in their order.
func rowBoards(rows []trackerapp.TicketRowView) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Board
	}
	return out
}

// openSprints are b's active and future sprints.
func openSprints(b trackerapp.BoardSummary) []trackerapp.SprintView {
	var open []trackerapp.SprintView
	for _, sp := range b.Sprints {
		if sp.State == "active" || sp.State == "future" {
			open = append(open, sp)
		}
	}
	return open
}

// planSprint plans a move into the open sprint v names, of the ticket's
// boards here that plan sprints, or to the backlog for backlog or null:
// judged against every open sprint of those boards, so the ticket is
// already there only when it is in the named sprint, or, for the
// backlog, in none of them. A move to the backlog is made on the board
// whose open sprint holds the ticket.
func (pc *planCall) planSprint(p *planned, given string, v setValue) error {
	p.sprintName = given
	want := "backlog"
	if !v.null {
		w, err := v.one(given)
		if err != nil {
			return invalidf("sprint takes a sprint's name, backlog or null.")
		}
		want = strings.TrimSpace(w)
	}
	on, err := pc.boardsOf(p, func(o boardOn) bool { return o.b.PlansSprints })
	if err != nil {
		return err
	}
	if len(on) == 0 {
		p.refuse(stepSprint, fmt.Sprintf("%s: %s is on no board read on this computer that plans sprints; ask the user to add its board in Settings, Issues, or open it there.", given, p.key))
		return nil
	}
	issues := []string{p.d.Card.ID}
	if strings.EqualFold(want, "backlog") {
		p.sprint = &trackerapp.SprintMoveRequest{Board: on[0].row.Board, Issues: issues}
		p.sprintSame = true
		for _, o := range on {
			if slices.ContainsFunc(openSprints(o.b), func(sp trackerapp.SprintView) bool { return slices.Contains(p.d.Card.Sprints, sp.ID) }) {
				p.sprint.Board, p.sprintSame = o.row.Board, false
				break
			}
		}
		p.changes = append(p.changes, ApprovalChange{Line: "Sprint → backlog"})
		return nil
	}
	var names []string
	for _, o := range on {
		for _, sp := range openSprints(o.b) {
			if strings.EqualFold(sp.Name, want) {
				p.sprintSame = slices.Contains(p.d.Card.Sprints, sp.ID)
				p.sprint = &trackerapp.SprintMoveRequest{Board: o.row.Board, Sprint: sp.ID, Issues: issues}
				p.changes = append(p.changes, ApprovalChange{Line: "Sprint → " + name(sp.Name)})
				return nil
			}
			if n := quoteName(sp.Name); !slices.Contains(names, n) {
				names = append(names, n)
			}
		}
	}
	where := "board " + quoteName(on[0].b.Name) + " has"
	its := "its"
	if len(on) > 1 {
		where, its = p.key+"'s boards have", "their"
	}
	if len(names) == 0 {
		p.refuse(stepSprint, fmt.Sprintf("%s: %s no open sprint.", given, where))
	} else {
		p.refuse(stepSprint, fmt.Sprintf("%s: %s no open sprint called %s; %s open sprints are %s.", given, where, quoteName(want), its, sayList(names, "and")))
	}
	return nil
}

// planEstimate plans the estimate v gives on board on, one of the
// ticket's rows (estimateTarget), in its unit, and refuses a second entry
// of set that names the ticket's estimate.
func (pc *planCall) planEstimate(p *planned, given string, v setValue, on estimateOn) error {
	if p.estimateName != "" {
		// One estimate a ticket: a second entry naming it is not dropped.
		return invalidf("%s and %s both set %s's estimate; set one estimate per call.", quoteName(p.estimateName), quoteName(given), p.key)
	}
	p.estimateName = given
	row := p.d.Rows[slices.IndexFunc(p.d.Rows, func(r trackerapp.TicketRowView) bool { return r.Board == on.board })]
	b := on.b
	in := trackerapp.EstimateRequest{Board: row.Board, Issue: p.d.Card.ID}
	if v.null {
		p.estimateSame = row.Estimate == nil
		p.changes = append(p.changes, ApprovalChange{Line: "Estimate → nothing"})
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
		p.changes = append(p.changes, ApprovalChange{Line: "Estimate → " + estimateText(n, row.Unit)})
	}
	p.estimate = &in
	return nil
}

// approvalOf is the approval of plans, the call id's, with set's values
// and the comment whole.
func approvalOf(id string, plans []*planned, set []namedValue, comment string) Approval {
	a := Approval{ID: id, Comment: comment}
	for _, nv := range set {
		a.Values = append(a.Values, ApprovalValue{Name: nv.name, Value: nv.value.whole()})
	}
	for _, p := range plans {
		title, _ := textClip(p.d.Card.Title, 0, refCut)
		a.Tickets = append(a.Tickets, ApprovalTicket{Key: p.key, Title: title, Changes: p.changes})
	}
	return a
}
