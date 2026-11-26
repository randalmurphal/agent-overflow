package trackertools

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	"agent-overflow/internal/trackerapp"
)

// ticket_update changes tickets in one call. Every ticket is resolved,
// read as far as its steps need and checked first (plan.go), with nothing
// sent: a name or a value the ticket cannot take, or a long text the
// session was not given whole (marks.go), refuses the call; a status,
// sprint or estimate the ticket as it is now cannot take is said as that
// ticket's refusal, and nothing is sent for any ticket. A call naming
// several tickets then waits for the person's approval (Host.Approve). When
// set changes them, their cards are then read again (planCall.recheck): a
// ticket that changed during the wait, by its update time, status or
// sprints, is refused and sent nothing, and the others go on. A call that
// comments on Jira tickets looks them up again at the start of the
// comment phase, after its other writes, and each comment's audience is
// judged on the project that lookup finds (apply.go). What a person changes after that read, in the seconds the earlier
// tickets' sends take, is caught only where a write compares when it is
// sent: a text an edit replaces whole (planField). A move the ticket's
// workflow stopped offering, or a status its Linear team no longer has,
// is sent as the plan's read found it and refused by the tracker, which
// the provider says as stale. The thread's
// mode is read again before anything is sent. The steps run in order
// (apply.go), status, fields, estimate, sprint, comment, each for every
// ticket before the next, and the first refused or unknown stops that
// ticket alone: what came after it for that ticket is not sent. A step
// the ticket already holds, an edit the tracker answers
// as changing nothing, or a comment an earlier call of the thread added
// (trackerapp's repeat record), is already: it changed nothing.

type updateArgs struct {
	Keys    []string                   `json:"keys"`
	Set     map[string]json.RawMessage `json:"set"`
	Comment *string                    `json:"comment"`
}

// ticketAnswer is what became of one ticket: the steps this call applied,
// by the names the call gave, those the ticket already held or an earlier
// call made, the one refused or unknown with its sentence, the steps not
// sent after it, and a note.
type ticketAnswer struct {
	Key     string   `json:"key"`
	Applied []string `json:"applied,omitempty"`
	Already []string `json:"already,omitempty"`
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
	raw   json.RawMessage
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
		out = append(out, namedValue{name: n, value: v, raw: set[n]})
	}
	return out, nil
}

// maxApprovalBytes bounds what a call that waits for the person carries
// whole to every screen (Approval): its set's values and its comment.
const maxApprovalBytes = 256 << 10

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
	comment := ""
	if args.Comment != nil {
		comment = *args.Comment
	}
	if err := s.h.Writable(caller.Thread); err != nil {
		return nil, err
	}
	asks := false
	if len(args.Keys) > 1 {
		if asks, err = s.h.AsksBeforeSeveral(caller.Thread); err != nil {
			return nil, err
		}
	}
	if asks && approvalBytes(set, comment) > maxApprovalBytes {
		return nil, invalidf("set and comment together hold more than %d KiB, more than an approval shows the person; change fewer or shorter values in each call.", maxApprovalBytes>>10)
	}
	keys := args.Keys
	if len(keys) == 0 {
		keys = []string{""}
	}
	tickets, sources, err := s.resolveKeys(ctx, caller.Thread, keys, keysArg)
	if err != nil {
		return nil, err
	}
	for i, t := range tickets {
		if slices.ContainsFunc(tickets[:i], func(q ticket) bool { return q.connection == t.connection && q.issue == t.issue }) {
			return nil, invalidf("%s is named twice in keys.", qualifier(sources, t.site)(t.key))
		}
	}
	// The plan's reads and the writes share one session: the writes are
	// refreshed once, at its end, and the first write to a ticket takes
	// what its read found.
	ctx = trackerapp.WithWrites(ctx)
	pc := newPlanCall(s, caller, sources)
	plans, errs, err := pc.readTickets(ctx, tickets, set, comment)
	if err != nil {
		return nil, err
	}
	for i, p := range plans {
		if errs[i] != nil {
			return nil, errs[i]
		}
		if err := pc.planSteps(p, set); err != nil {
			return nil, err
		}
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
	approved := asks
	if approved {
		if err := s.h.Approve(ctx, caller.Thread, approvalOf(id, plans, set, comment)); err != nil {
			return nil, err
		}
	}
	// What set changes was judged on the tickets as they were before the
	// wait; a comment alone judges nothing, and is looked up just before
	// it is sent (apply.go).
	if approved && len(set) > 0 {
		if err := pc.recheck(ctx, plans); err != nil {
			return nil, err
		}
	}
	// The writes are admitted now: a thread made read-only while the call
	// planned or waited sends nothing.
	if err := s.h.Writable(caller.Thread); err != nil {
		return nil, err
	}
	// What was admitted is sent whole: the call's end no longer stops it,
	// and each write keeps its own bound.
	wctx := trackerapp.ByAgent(context.WithoutCancel(ctx), caller.Thread, id)
	answers, written := s.apply(wctx, caller, plans)
	out := updateAnswer{Tickets: answers}
	row := WriteRow{ID: id}
	for _, w := range written {
		if len(w.Applied) > 0 {
			row.Tickets = append(row.Tickets, w)
		}
	}
	if len(row.Tickets) > 0 {
		s.h.Wrote(caller.Thread, row)
	}
	return out, nil
}

// approvalBytes is what an approval of set and comment carries whole.
func approvalBytes(set []namedValue, comment string) int {
	n := len(comment)
	for _, nv := range set {
		n += len(nv.name) + len(nv.raw)
	}
	return n
}
