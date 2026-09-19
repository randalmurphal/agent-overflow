package threadtools

import (
	"context"
	"strings"
	"sync"

	"agent-overflow/internal/entityid"
)

// Id resolution. A thread id is a complete address: ids are canonical v4
// UUIDs, unique across computers, and a Move keeps the id, so the only
// question a reference raises is which computer holds it.
//
// Rules, in order:
//
//   - A reference shorter than MinPrefixLen is refused. A guess that
//     matched would be worse than a refusal.
//   - computer_id skips resolution entirely and asks that computer alone,
//     which also turns an unreachable computer into an immediate error
//     rather than an incomplete answer.
//   - A full id matches this computer first and fans out only on a miss.
//   - A prefix ALWAYS fans out, so an ambiguity across computers is
//     detected rather than masked by a local match.
//   - The fan-out runs concurrently under ResolutionTimeout. A computer
//     that did not answer makes a miss thread_resolution_incomplete
//     naming it, never a confident not found, and makes a single match
//     elsewhere carry a partial note.
//   - One moved_to is followed: the computer that moved a thread away
//     records the new owner and answers with it instead of a miss.
//   - With no paired computers, local is the whole answer.

// resolve turns a reference into the thread and the computer that owns it.
func (c *session) resolve(ctx context.Context, ref, hint string) (Target, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return Target{}, invalidf("A thread id is required. Take it from a thread_search row or another result; never guess one.")
	}
	if len(ref) < MinPrefixLen {
		return Target{}, invalidf("%q is too short to name a thread. Use the full id, or a prefix of at least %d characters.", ref, MinPrefixLen)
	}
	if hint != "" {
		return c.resolveOn(ctx, ref, hint, true)
	}
	if entityid.Valid(ref) {
		answer := c.askLocal(ctx, ref)
		if len(answer.res.Matches) == 1 {
			return c.target(answer, nil), nil
		}
		if answer.res.MovedTo != "" {
			if target, err, followed := c.follow(ctx, ref, answer.res); followed {
				return target, err
			}
		}
		if len(answer.res.Matches) > 1 {
			return Target{}, ambiguous(ref, c.withComputer(answer), c.paired())
		}
		if !c.paired() {
			if answer.err != nil {
				return Target{}, answer.err
			}
			return Target{}, notFound(ref, nil)
		}
	}
	return c.fanOutResolve(ctx, ref)
}

// resolveOn asks one named computer and nothing else.
func (c *session) resolveOn(ctx context.Context, ref, id string, followMoves bool) (Target, error) {
	computer, local, ok := c.computerByID(id)
	if !ok {
		return Target{}, publicf(CodeInvalidRequest, "There is no computer %q. thread_options lists the computers you can reach: %s.", id, computerList(c.computers))
	}
	var answer resolveAnswer
	if local {
		answer = c.askLocal(ctx, ref)
	} else {
		answer = c.askPeer(ctx, computer, ref)
	}
	if answer.err != nil {
		return Target{}, answer.err
	}
	switch {
	case len(answer.res.Matches) == 1:
		return c.target(answer, nil), nil
	case len(answer.res.Matches) > 1:
		return Target{}, ambiguous(ref, c.withComputer(answer), c.paired())
	case answer.res.MovedTo != "" && followMoves:
		if target, err, followed := c.follow(ctx, ref, answer.res); followed {
			return target, err
		}
	}
	return Target{}, publicf(CodeNotFound, "No thread matches %q on %s.", ref, nameOf(computer))
}

// follow chases one recorded move. It never chases a second one: a chain
// would be a loop waiting to happen, and the new owner's own record is
// what the next call reads.
func (c *session) follow(ctx context.Context, ref string, res Resolution) (Target, error, bool) {
	computer, _, ok := c.computerByID(res.MovedTo)
	if !ok {
		name := res.MovedToName
		if name == "" {
			name = res.MovedTo
		}
		return Target{}, publicf(CodeNotFound, "Thread %s moved to %s, which is not paired with this computer.", ref, name), true
	}
	target, err := c.resolveOn(ctx, ref, computer.ID, false)
	return target, err, true
}

// fanOutResolve asks this computer and every paired one concurrently.
func (c *session) fanOutResolve(ctx context.Context, ref string) (Target, error) {
	answers := c.askEveryone(ctx, ref)

	var matches []Candidate
	var silent []Computer
	var moved *resolveAnswer
	seen := make(map[string]struct{}, len(answers))
	for index := range answers {
		answer := answers[index]
		if answer.err != nil {
			silent = append(silent, answer.computer)
			continue
		}
		for _, candidate := range c.withComputer(answer) {
			if _, duplicate := seen[candidate.ThreadID]; duplicate {
				continue
			}
			seen[candidate.ThreadID] = struct{}{}
			matches = append(matches, candidate)
		}
		if answer.res.MovedTo != "" && moved == nil {
			moved = &answers[index]
		}
	}
	switch {
	case len(matches) > 1:
		return Target{}, ambiguous(ref, matches, c.paired())
	case len(matches) == 1:
		for index := range answers {
			if answers[index].err == nil && len(answers[index].res.Matches) == 1 && answers[index].res.Matches[0].ThreadID == matches[0].ThreadID {
				return c.target(answers[index], silent), nil
			}
		}
	case moved != nil:
		if target, err, followed := c.follow(ctx, ref, moved.res); followed {
			return target, err
		}
	}
	if len(silent) > 0 {
		return Target{}, resolutionIncomplete(ref, silent)
	}
	return Target{}, notFound(ref, c.computers)
}

type resolveAnswer struct {
	computer Computer
	local    bool
	res      Resolution
	err      error
}

func (c *session) askLocal(ctx context.Context, ref string) resolveAnswer {
	res, err := c.app.ResolveThreadRef(ctx, ref)
	return resolveAnswer{computer: c.self(), local: true, res: res, err: err}
}

func (c *session) askPeer(ctx context.Context, computer Computer, ref string) resolveAnswer {
	peer, err := c.app.Peer(ctx, computer.ID)
	if err != nil {
		return resolveAnswer{computer: computer, err: err}
	}
	res, err := peer.Resolve(ctx, ref)
	return resolveAnswer{computer: computer, res: res, err: err}
}

// askEveryone runs the local lookup and every peer lookup concurrently
// under one bound. A peer that misses it is an error for that computer
// only; the others still answer.
func (c *session) askEveryone(ctx context.Context, ref string) []resolveAnswer {
	bounded, cancel := context.WithTimeout(ctx, ResolutionTimeout)
	defer cancel()

	answers := make([]resolveAnswer, len(c.computers)+1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		answers[0] = c.askLocal(bounded, ref)
	}()
	for index, computer := range c.computers {
		wg.Add(1)
		go func(slot int, computer Computer) {
			defer wg.Done()
			answers[slot] = c.askPeer(bounded, computer, ref)
		}(index+1, computer)
	}
	wg.Wait()
	return answers
}

// withComputer stamps one computer's candidates with that computer.
func (c *session) withComputer(answer resolveAnswer) []Candidate {
	out := make([]Candidate, 0, len(answer.res.Matches))
	for _, match := range answer.res.Matches {
		id, name := c.stamp(answer.computer)
		match.ComputerID, match.Computer = id, name
		out = append(out, match)
	}
	return out
}

func (c *session) target(answer resolveAnswer, silent []Computer) Target {
	match := answer.res.Matches[0]
	id, name := c.stamp(answer.computer)
	return Target{
		ThreadID:   match.ThreadID,
		Title:      match.Title,
		ComputerID: id,
		Computer:   name,
		Local:      answer.local,
		Partial:    silent,
	}
}

// partialNote is the recovery hint a result carries when the fan-out was
// not complete: the match stands, and the note says what was not asked.
func partialNote(silent []Computer) string {
	if len(silent) == 0 {
		return ""
	}
	return "Partial: " + computerList(silent) + " did not answer in time, so a thread of the same id there was not considered."
}

func nameOf(computer Computer) string {
	if computer.Name != "" {
		return computer.Name
	}
	if computer.ID != "" {
		return computer.ID
	}
	return "this computer"
}
