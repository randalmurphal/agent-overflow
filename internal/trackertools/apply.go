package trackertools

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/tracker"
	"agent-overflow/internal/trackerapp"
)

// Sending. The steps run in order, status, fields, estimate, sprint,
// comment, each for every ticket before the next (update.go), and the
// first refused or unknown stops its ticket alone. The sprint moves of
// tickets bound for the same sprint of the same board are one write, whose
// part for each ticket is that ticket's outcome. A step the ticket already
// holds is sent no request, and a comment an earlier call of the thread
// added is not added again (trackerapp's repeat record): both are said as
// already, not applied, so the answer and the timeline row name only what
// this call changed. Once every step is sent, the call's write session is
// ended, which refreshes what the writes changed once
// (trackerapp.Service.EndWrites), and the read marks follow the texts the
// tracker confirmed this call wrote (marks.wrote).

// repeatedComment is the note of a comment an earlier call added.
const repeatedComment = "Your earlier call added this comment; it was not added again."

// stepOrder is the order the steps are sent in.
var stepOrder = [...]stepKind{stepMove, stepEdit, stepEstimate, stepSprint, stepComment}

// sendStep is one step of a ticket's change as apply runs it: send makes
// its write; a sprint move's is made with the others bound for its sprint
// (sprint).
type sendStep struct {
	kind   stepKind
	names  []string
	same   bool
	send   func() (trackerapp.WriteView, error)
	sprint *trackerapp.SprintMoveRequest
	// what names the change in an unknown outcome's sentence.
	what string
}

// stepsOf are p's steps, on ctx, which carries the agent and the call's
// write session.
func (s *Server) stepsOf(ctx context.Context, p *planned) []sendStep {
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
		steps = append(steps, sendStep{kind: stepSprint, names: []string{p.sprintName}, same: p.sprintSame, what: "sprint move", sprint: p.sprint})
	}
	if p.comment != "" {
		steps = append(steps, sendStep{kind: stepComment, names: []string{setComment}, what: "comment",
			send: func() (trackerapp.WriteView, error) {
				return s.t.AddComment(ctx, conn, trackerapp.CommentRequest{Issue: p.d.Card.ID, Markdown: p.comment})
			}})
	}
	return steps
}

// sending is one ticket's change as apply sends it: its plan and steps,
// what became of it, and whether a step stopped it.
type sending struct {
	p       *planned
	steps   []sendStep
	out     ticketAnswer
	written WrittenTicket
	stopped bool
	edited  bool
}

// step is t's step of kind, and whether it has one.
func (t *sending) step(kind stepKind) (sendStep, bool) {
	i := slices.IndexFunc(t.steps, func(st sendStep) bool { return st.kind == kind })
	if i < 0 {
		return sendStep{}, false
	}
	return t.steps[i], true
}

// apply sends plans' steps on ctx, which carries the agent and the call's
// write session, ends the session, and answers what became of each
// ticket, and each ticket as the timeline shows it, in plans' order.
func (s *Server) apply(ctx context.Context, caller Caller, plans []*planned) ([]ticketAnswer, []WrittenTicket) {
	all := make([]*sending, len(plans))
	for i, p := range plans {
		// A ticket the read after the approval found changed is sent
		// nothing: each of its steps is not sent.
		all[i] = &sending{p: p, steps: s.stepsOf(ctx, p), out: ticketAnswer{Key: p.key, Refused: p.changed}, stopped: p.changed != "",
			written: WrittenTicket{Connection: p.t.connection, Issue: p.d.Card.ID, Key: p.key, Title: p.d.Card.Title}}
	}
	for _, kind := range stepOrder {
		if kind == stepComment {
			s.readAudience(ctx, all)
		}
		var moves []*sprintGroup
		for _, t := range all {
			st, ok := t.step(kind)
			switch {
			case !ok:
			case t.stopped:
				t.out.NotSent = append(t.out.NotSent, st.names...)
			case st.same:
				t.out.Already = append(t.out.Already, st.names...)
			case st.sprint != nil:
				moves = joinSprint(moves, t, *st.sprint)
			default:
				v, err := st.send()
				t.answer(st, v, err)
			}
		}
		for _, g := range moves {
			v, err := s.t.MoveToSprint(ctx, g.conn, g.req)
			for _, t := range g.tickets {
				st, _ := t.step(stepSprint)
				t.answer(st, partsOf(v, t.p.d.Card.ID), err)
			}
		}
	}
	if err := s.t.EndWrites(ctx); err != nil {
		s.logf("tracker tools: refresh after the writes: %v", err)
	}
	answers := make([]ticketAnswer, len(all))
	written := make([]WrittenTicket, len(all))
	for i, t := range all {
		if t.edited && len(t.p.texts) > 0 {
			s.wroteTexts(caller, t.p)
		}
		answers[i], written[i] = t.out, t.written
	}
	return answers, written
}

// wroteTexts marks p's edited texts the session's own at the version its
// writes left; every other text of the ticket is unread there.
func (s *Server) wroteTexts(caller Caller, p *planned) {
	card, ok, err := s.t.MirrorCard(p.t.connection, p.d.Card.ID)
	switch {
	case err != nil:
		s.logf("tracker tools: read %s after its write: %v", p.key, err)
	case ok:
		s.marks.wrote(caller, markKey{p.t.connection, p.d.Card.ID}, p.d.Card.Updated, card.Updated, p.texts)
	}
}

// readAudience looks up, once per Jira connection, the tickets of all
// that still have a comment to send, at the start of the comment phase:
// after every other write of the call, so the project the call's write
// session keeps from it, which each comment's audience is judged on, is
// the ticket's after those writes (trackerapp's projectFor). A ticket the
// lookup does not find, or whose lookup fails, is sent no comment, and
// its answer says why; its other steps keep theirs.
func (s *Server) readAudience(ctx context.Context, all []*sending) {
	owed := func(t *sending) bool {
		st, ok := t.step(stepComment)
		return ok && !st.same && !t.stopped && t.p.src.Kind == "jira"
	}
	var conns []string
	for _, t := range all {
		if owed(t) && !slices.Contains(conns, t.p.t.connection) {
			conns = append(conns, t.p.t.connection)
		}
	}
	for _, conn := range conns {
		var on []*sending
		var keys []string
		for _, t := range all {
			if owed(t) && t.p.t.connection == conn {
				on, keys = append(on, t), append(keys, t.p.d.Card.Key)
			}
		}
		found, err := s.t.Lookup(ctx, conn, keys)
		if err != nil {
			s.logf("tracker tools: look up %d tickets on %s before their comments: %v", len(keys), conn, err)
		}
		for _, t := range on {
			st, _ := t.step(stepComment)
			switch {
			case err != nil:
				why := "it could not be read"
				if _, msg, ok := errorsx.PublicDetails(err); ok {
					why = t.p.t.site + ": " + msg
				}
				t.refuse(st, fmt.Sprintf("%s could not be read again just before its comment, so the comment was not added. %s", t.p.key, why))
			case !slices.ContainsFunc(found.Found, func(m trackerapp.KeyMatchView) bool { return m.Card.ID == t.p.d.Card.ID }):
				t.refuse(st, goneSentence(t.p, "the comment was not added"))
			}
		}
	}
}

// refuse answers t's step st as refused before it was sent, for
// sentence, which stops t: the step is answered, and no longer sent.
func (t *sending) refuse(st sendStep, sentence string) {
	t.out.Refused, t.stopped = st.names[0]+": "+sentence, true
	t.steps = slices.DeleteFunc(t.steps, func(s sendStep) bool { return s.kind == st.kind })
}

// answer records what became of t's step st: its write's view v or its
// error. A step refused or of unknown outcome stops t.
func (t *sending) answer(st sendStep, v trackerapp.WriteView, err error) {
	out := &t.out
	switch {
	case errors.Is(err, trackerapp.ErrAgentWriteInFlight):
		out.Refused, t.stopped = st.names[0]+": the same "+st.what+" is still being sent by an earlier call, so it was not sent again; call again once that call has answered.", true
		return
	case errors.Is(err, trackerapp.ErrAgentRepeatUnchecked):
		out.Refused, t.stopped = st.names[0]+": "+uncheckedSentence("an earlier call's "+st.what+" may have been added", "the ticket's comments could not be read to check", "sent", "read the ticket to see whether the comment is there", err), true
		return
	case err == nil && v.Repeat != nil:
		out.Already, out.Note = append(out.Already, st.names...), repeatedComment
		return
	}
	state, sentence := judge(t.p.src.Kind, strings.Join(st.names, ", "), st.what, v, err, func(id string) string { return fieldNameIn(t.p.d.Fields, id) })
	switch {
	case state == tracker.StateApplied && unchanged(v):
		// The ticket held it already: nothing was sent.
		out.Already = append(out.Already, st.names...)
	case state == tracker.StateApplied:
		out.Applied = append(out.Applied, st.names...)
		t.written.Applied = append(t.written.Applied, st.names...)
		t.edited = t.edited || st.kind == stepEdit
	case state == tracker.StateUnknown:
		out.Unknown, t.stopped = sentence, true
	case state == tracker.StateRefused, state == tracker.StateNotSent:
		out.Refused, t.stopped = sentence, true
	}
}

// sprintGroup is one sprint move of several tickets: those bound for the
// same sprint, or the backlog, of the same board of one connection.
type sprintGroup struct {
	conn    string
	req     trackerapp.SprintMoveRequest
	tickets []*sending
}

// joinSprint adds t's sprint move req to the group bound where it is, or
// to a new one.
func joinSprint(groups []*sprintGroup, t *sending, req trackerapp.SprintMoveRequest) []*sprintGroup {
	conn := t.p.t.connection
	for _, g := range groups {
		if g.conn == conn && g.req.Board == req.Board && g.req.Sprint == req.Sprint {
			g.req.Issues = append(g.req.Issues, req.Issues...)
			g.tickets = append(g.tickets, t)
			return groups
		}
	}
	req.Issues = slices.Clone(req.Issues)
	return append(groups, &sprintGroup{conn: conn, req: req, tickets: []*sending{t}})
}

// partsOf is v as issue's own: its parts about issue, and those about no
// issue, which are every issue's.
func partsOf(v trackerapp.WriteView, issue string) trackerapp.WriteView {
	out := v
	out.Parts = slices.DeleteFunc(slices.Clone(v.Parts), func(p trackerapp.PartView) bool { return p.Issue != "" && p.Issue != issue })
	return out
}

// unchanged reports a write whose every part is applied and sent
// nothing, the tracker holding every value already.
func unchanged(v trackerapp.WriteView) bool {
	return len(v.Parts) > 0 && !slices.ContainsFunc(v.Parts, func(p trackerapp.PartView) bool { return !p.Unchanged })
}

// judge is what became of one step, names (the call's names for it) on a
// tracker of kind: its write's view v or its error. named is a field's
// name by its ID, as a refusal's sentence names it (refusalText).
func judge(kind, names, what string, v trackerapp.WriteView, err error, named func(string) string) (tracker.State, string) {
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
			return state, names + ": " + refusalText(kind, refusal, false, named)
		}
	case tracker.StateNotSent:
		// Its failure says why, below.
	}
	if failure != nil {
		return tracker.StateRefused, names + ": " + failure.Message
	}
	return tracker.StateRefused, names + ": the tracker did not make it."
}

// unknownSentence is a step whose answer never came on a tracker of kind.
func unknownSentence(kind, names, what string) string {
	if what == "comment" {
		return fmt.Sprintf("%s: %s's answer never came, so the comment may or may not have been added; the same call again looks for it first.", names, trackerNameOf(kind))
	}
	return fmt.Sprintf("%s: %s's answer never came, so the %s may or may not have been made; read the ticket to see before you call again.", names, trackerNameOf(kind), what)
}

// uncheckedSentence is a comment or create whose earlier attempt may have
// been made (maybe) and could not be looked for (err, a
// trackerapp.ErrAgentRepeatUnchecked): the lookup failed (failed says
// how), or the connection's account was not known when it was sent.
// Nothing was sent; done says so ("sent", "created"). A call again
// cannot check an attempt sent with no account, and is refused the same
// way while that attempt answers repeats, so its sentence says where to
// look (look) instead.
func uncheckedSentence(maybe, failed, done, look string, err error) string {
	if errors.Is(err, trackerapp.ErrAgentAccountUnknown) {
		return maybe + ", and it cannot be checked: the connection's account was not known when it was sent. It was not " + done +
			" again, and a call again within an hour of that attempt is refused the same way; " + look + "."
	}
	return maybe + ", and " + failed + ", so it was not " + done + " again; call again to check again."
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

// refusalText is a refusal on a tracker of kind as a model reads it: the
// provider's sentence without its package prefix, or for a refusal whose
// sentence would carry the tracker's names bare (a value matching
// several, a value matching none, a field changed since it was read) a
// sentence of the tool's own with each name quoted. A value matching
// several says what tells them apart in the text this tool sends: Jira
// takes a value by its ID, Linear only by name, and a person also by
// email (linearprovider's matchOne), so one Linear values share may be
// left for the person to pick on the ticket, or, for a create (creating),
// set after it.
func refusalText(kind string, r *trackerapp.RefusalView, creating bool, named func(string) string) string {
	msg := strings.TrimSpace(r.Message)
	for _, prefix := range []string{"jira: ", "linear: ", "linearprovider: ", "jiraprovider: ", "tracker: "} {
		msg = strings.TrimPrefix(msg, prefix)
	}
	if i := strings.Index(msg, "; candidates: "); i >= 0 {
		msg = msg[:i]
	}
	candidates := func(withID bool) string {
		names := make([]string, 0, 10)
		for i, c := range r.Candidates {
			if i == 10 {
				names = append(names, fmt.Sprintf("%d more", len(r.Candidates)-10))
				break
			}
			n := quoteName(cmpOr(c.Name, c.ID))
			if withID && c.Name != "" {
				n += " (" + strconv.Quote(c.ID) + ")"
			}
			names = append(names, n)
		}
		return sayList(names, "or")
	}
	switch {
	case r.Reason == "ambiguous" && len(r.Candidates) > 0 && kind == "linear":
		other := "ask the user to pick one on the ticket"
		if creating {
			other = "leave it out of the create, if the ticket can be made without it, and ask the user to set it on the new ticket"
		}
		msg = "it matches more than one value: " + candidates(false) + "; Linear matches it by name, and a person also by email: give a name or an email only one of them has, or " + other
	case r.Reason == "ambiguous" && len(r.Candidates) > 0:
		msg = "it matches more than one value: " + candidates(true) + "; give one by its ID"
	case r.Reason == "no_match" && len(r.Candidates) > 0:
		msg = "no value matches; it takes " + candidates(false)
	case r.Reason == "stale":
		msg = "it changed on the tracker after it was read; read the ticket again before you change it"
	case r.Reason == "tracker" && len(r.Fields) > 0:
		// The tracker's sentence of each field it refused, by the name a
		// read shows the field by.
		said := make([]string, len(r.Fields))
		for i, f := range r.Fields {
			name := f.Field
			if named != nil {
				name = named(f.Field)
			}
			said[i] = name + ": " + strings.TrimSuffix(strings.TrimSpace(f.Message), ".")
		}
		msg = strings.TrimSuffix(msg, ".") + "; " + strings.Join(said, "; ")
	}
	shown, _ := textClip(msg, 0, refusalCut)
	return strings.TrimSuffix(shown, ".") + "."
}
