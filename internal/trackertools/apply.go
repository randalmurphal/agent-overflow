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

// Sending. A ticket's steps run in order and the first refused or
// unknown stops it (update.go). A step the ticket already holds is sent
// no request, and a comment an earlier call of the thread added is not
// added again (trackerapp's repeat record): both are said as already, not
// applied, so the answer and the timeline row name only what this call
// changed. The read marks follow the texts the tracker confirmed this
// call wrote (marks.wrote).

// repeatedComment is the note of a comment an earlier call added.
const repeatedComment = "Your earlier call added this comment; it was not added again."

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
	stopped, edited := false, false
	for _, st := range steps {
		switch {
		case stopped:
			out.NotSent = append(out.NotSent, st.names...)
			continue
		case st.same:
			out.Already = append(out.Already, st.names...)
			continue
		}
		v, err := st.send()
		switch {
		case errors.Is(err, trackerapp.ErrAgentWriteInFlight):
			out.Refused, stopped = st.names[0]+": the same "+st.what+" is still being sent by an earlier call, so it was not sent again; call again once that call has answered.", true
			continue
		case errors.Is(err, trackerapp.ErrAgentRepeatUnchecked):
			out.Refused, stopped = st.names[0]+": "+uncheckedSentence("an earlier call's "+st.what+" may have been added", "the ticket's comments could not be read to check", "sent", err), true
			continue
		case err == nil && v.Repeat != nil:
			out.Already, out.Note = append(out.Already, st.names...), repeatedComment
			continue
		}
		state, sentence := judge(p.src.Kind, strings.Join(st.names, ", "), st.what, v, err)
		switch {
		case state == tracker.StateApplied && unchanged(v):
			// The ticket held it already: nothing was sent.
			out.Already = append(out.Already, st.names...)
		case state == tracker.StateApplied:
			out.Applied = append(out.Applied, st.names...)
			written.Applied = append(written.Applied, st.names...)
			edited = edited || st.kind == stepEdit
		case state == tracker.StateUnknown:
			out.Unknown, stopped = sentence, true
		case state == tracker.StateRefused, state == tracker.StateNotSent:
			out.Refused, stopped = sentence, true
		}
	}
	if edited && len(p.texts) > 0 {
		// The edit's texts are the session's own at the version its write
		// left; every other text of the ticket is unread there.
		card, ok, err := s.t.MirrorCard(conn, p.d.Card.ID)
		switch {
		case err != nil:
			s.logf("tracker tools: read %s after its write: %v", p.key, err)
		case ok:
			s.marks.wrote(caller, markKey{conn, p.d.Card.ID}, p.d.Card.Updated, card.Updated, p.texts)
		}
	}
	return out, written
}

// unchanged reports a write whose every part is applied and sent
// nothing, the tracker holding every value already.
func unchanged(v trackerapp.WriteView) bool {
	return len(v.Parts) > 0 && !slices.ContainsFunc(v.Parts, func(p trackerapp.PartView) bool { return !p.Unchanged })
}

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
// Nothing was sent; done says so ("sent", "created").
func uncheckedSentence(maybe, failed, done string, err error) string {
	if errors.Is(err, trackerapp.ErrAgentAccountUnknown) {
		return maybe + ", and it cannot be checked: the connection's account was not known when it was sent. It was not " + done +
			" again; ask the user to check the connection in Settings, Issues, then call again."
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

// refusalText is a refusal as a model reads it: the provider's sentence
// without its package prefix, or for a refusal whose sentence would carry
// the tracker's names bare (a value matching several, a value matching
// none, a field changed since it was read) a sentence of the tool's own
// with each name quoted.
func refusalText(r *trackerapp.RefusalView) string {
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
	case r.Reason == "ambiguous" && len(r.Candidates) > 0:
		msg = "it matches more than one value: " + candidates(true) + "; give one by its ID"
	case r.Reason == "no_match" && len(r.Candidates) > 0:
		msg = "no value matches; it takes " + candidates(false)
	case r.Reason == "stale":
		msg = "it changed on the tracker after it was read; read the ticket again before you change it"
	}
	shown, _ := textClip(msg, 0, refusalCut)
	return strings.TrimSuffix(shown, ".") + "."
}
