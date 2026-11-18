package threadtools

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"slices"

	"agent-overflow/internal/errorsx"
)

// statusThreads answers the thread_ids half of thread_status. A thread is
// addressed by its id wherever it lives, so the ids are resolved first and
// then grouped by the computer that holds them: this computer's run here,
// and each other computer's are forwarded to it as one thread_status call
// of its own.
//
// The call returns what it observed. A reference whose lookup a computer
// did not answer reads unresolved while the threads that resolved are
// waited on, and on the computer a wait was forwarded to, a thread that
// left it since the caller resolved it reads as gone and ends the wait
// (unresolvedRow). The call fails only when nothing answered.
func (c *session) statusThreads(ctx context.Context, args statusArgs, wait int) (any, error) {
	// rows holds each reference's row, in the order the call listed them,
	// whichever computer answered for it.
	rows := make([]ThreadState, len(args.ThreadIDs))
	targets := make([]Target, 0, len(args.ThreadIDs))
	slots := make([]int, 0, len(args.ThreadIDs))
	ids := make([]string, 0, len(args.ThreadIDs))
	var failures []errorRow
	var refusal error
	gone, unresolved := "", false
	for index, ref := range args.ThreadIDs {
		target, err := c.resolve(ctx, ref, "")
		if err == nil {
			if target.ThreadID == c.caller.ThreadID {
				return nil, publicf(CodeSelfSend, "A thread cannot watch itself. Watch the threads you started, or use thread_remind to wake yourself later.")
			}
			targets = append(targets, target)
			slots = append(slots, index)
			ids = append(ids, target.ThreadID)
			continue
		}
		row, silent, kept := c.unresolvedRow(ctx, ref, err)
		if !kept {
			return nil, err
		}
		rows[index] = row
		ids = append(ids, ref)
		if row.State == StateUnresolved {
			unresolved = true
			refusal = cmp.Or(refusal, err)
			for _, computer := range silent {
				failures = addFailure(failures, computer, err)
			}
		} else {
			gone = cmp.Or(gone, ref)
		}
	}
	if duplicate, found := dedupe(ids); found {
		return nil, invalidf("thread %s is listed twice. Each thread may appear once per call.", duplicate)
	}
	watched := watchedThreads{}
	switch {
	case len(targets) > 0:
		// A thread already gone is the wake: the others are read as they
		// stand rather than waited on.
		waitFor := wait
		if gone != "" {
			waitFor = 0
		}
		var err error
		if watched, err = c.watchThreadGroups(ctx, targets, groupTargetsByComputer(targets), waitFor, args.AfterRevision); err != nil {
			return nil, err
		}
	case gone == "":
		return nil, refusal
	}
	byID := make(map[string]ThreadState, len(watched.threads))
	for _, state := range watched.threads {
		byID[state.ThreadID] = state
	}
	for position, index := range slots {
		if state, found := byID[targets[position].ThreadID]; found {
			rows[index] = state
		}
	}
	wokeOn := cmp.Or(watched.wokeOn, gone)
	// A wait of zero is a read, not a wait that ran out.
	result := statusResult{WokeOn: wokeOn, TimedOut: wokeOn == "" && wait > 0, Errors: failures}
	for _, failure := range watched.failures {
		result.Errors = addFailureRow(result.Errors, failure)
	}
	for _, row := range rows {
		if row.ThreadID == "" {
			continue
		}
		// Resting is derived here from the state and what the thread still
		// waits on, so a row cannot report a restedness that disagrees with
		// them.
		row.Resting = Rests(row)
		result.Threads = append(result.Threads, row)
	}
	if result.TimedOut {
		result.Note = "None of those threads rested or became blocked within the wait. They are still as reported."
	}
	if len(watched.failures) > 0 {
		result.Note = appendNote(result.Note, "A computer under errors did not answer, so its threads read unavailable; the other threads are as their computers reported them.")
	}
	if unresolved {
		result.Note = appendNote(result.Note, "A reference that reads unresolved matched nothing on the computers that answered, and a computer under errors did not answer its lookup, so the thread may be there.")
	}
	return result, nil
}

// unresolvedRow reads a reference whose lookup failed as a row of its own
// when the failure is something observed rather than a mistake in the
// call. A computer that did not answer the lookup (unansweredLookup) leaves
// the reference unresolved, under its owner only when a recorded move or
// the caller named one, and is returned for errors. On the computer a wait
// was forwarded to, a thread that is no longer there (deleted, or moved
// away) is the reason on its row. Anything else is the call's refusal.
func (c *session) unresolvedRow(ctx context.Context, ref string, err error) (row ThreadState, silent []Computer, kept bool) {
	var lookup *unansweredLookup
	if errors.As(err, &lookup) {
		row = FailedRow(ref, err)
		row.State = StateUnresolved
		if lookup.owner != nil {
			row.ComputerID, row.Computer = c.stamp(*lookup.owner)
		}
		return row, lookup.computers, true
	}
	// The computer that asked resolved the thread here a moment ago.
	if code, _, _ := errorsx.PublicDetails(err); code == CodeNotFound && Forwarded(ctx) {
		return FailedRow(ref, err), nil, true
	}
	return ThreadState{}, nil, false
}

// addFailure names computer under errors once, with the first reason it
// failed.
func addFailure(failures []errorRow, computer Computer, err error) []errorRow {
	return addFailureRow(failures, newErrorRow(computer, err))
}

func addFailureRow(failures []errorRow, failure errorRow) []errorRow {
	if slices.ContainsFunc(failures, func(row errorRow) bool { return row.ComputerID == failure.ComputerID }) {
		return failures
	}
	return append(failures, failure)
}

// threadIDs reads one group's thread ids out of the targets it indexes.
func threadIDs(targets []Target, group targetGroup) []string {
	ids := make([]string, 0, len(group.indexes))
	for _, index := range group.indexes {
		ids = append(ids, targets[index].ThreadID)
	}
	return ids
}

// threadWatchAnswer is one group's half of the call.
type threadWatchAnswer struct {
	threads []ThreadState
	wokeOn  string
	err     error
}

// slottedAnswer is a group's answer and its position in the call's groups.
type slottedAnswer struct {
	slot int
	threadWatchAnswer
}

// watchedThreads is what a wait over several computers observed: the
// wake, every thread that was read, and the computers that did not answer,
// whose threads read StateUnavailable.
type watchedThreads struct {
	threads  []ThreadState
	wokeOn   string
	failures []errorRow
}

// watchThreadGroups runs every group's wait at once and ends the call on
// the first group that reports a wake: the agent watching three threads
// wants the one that rested, not the one it happened to list first. The
// groups still waiting then are read once more without a wait, all at once
// under WakeRefreshTimeout, so every thread the call named is reported and
// the wake is held no longer than that; a computer still outstanding at
// that bound reads as one that did not answer.
//
// A group that fails costs the call only that group (watchedGroups). The
// call fails only when no group answered at all.
func (c *session) watchThreadGroups(ctx context.Context, targets []Target, groups []targetGroup, wait int, after int64) (watchedThreads, error) {
	racing, cancel := context.WithCancel(ctx)
	defer cancel()
	every := make([]int, len(groups))
	for slot := range every {
		every[slot] = slot
	}
	answers := make([]threadWatchAnswer, len(groups))
	answered := make([]bool, len(groups))
	arriving := c.startGroups(racing, targets, groups, every, wait, after)
	woke := ""
	for range groups {
		got := <-arriving
		answers[got.slot], answered[got.slot] = got.threadWatchAnswer, true
		if got.err == nil && got.wokeOn != "" {
			woke = got.wokeOn
			break
		}
	}
	var cutShort []int
	for slot := range groups {
		if !answered[slot] {
			cutShort = append(cutShort, slot)
		}
	}
	if len(cutShort) > 0 {
		// The waits a wake cut short end on the cancel; their answers go to
		// a buffered channel nobody reads.
		cancel()
		refresh, done := context.WithTimeout(ctx, WakeRefreshTimeout)
		defer done()
		fresh := c.startGroups(refresh, targets, groups, cutShort, 0, after)
		for range cutShort {
			got := <-fresh
			answers[got.slot] = got.threadWatchAnswer
		}
	}
	return c.watchedGroups(targets, groups, answers, woke)
}

// startGroups runs the groups at slots concurrently and returns the channel
// their answers arrive on, which holds every one of them.
func (c *session) startGroups(ctx context.Context, targets []Target, groups []targetGroup, slots []int, wait int, after int64) <-chan slottedAnswer {
	answers := make(chan slottedAnswer, len(slots))
	for _, slot := range slots {
		go func() {
			answers <- slottedAnswer{slot: slot, threadWatchAnswer: c.watchThreadGroup(ctx, targets, groups[slot], wait, after)}
		}()
	}
	return answers
}

// watchedGroups assembles the groups' answers. A group that answered gives
// its threads as they were read. One whose computer refused the wait gives
// each of its threads that refusal, because that computer answered. One
// whose computer did not answer (the pairing, the connection, a timeout)
// has its threads read unavailable and the computer named under errors;
// only that is a failure of the call's, and only when every group failed.
func (c *session) watchedGroups(targets []Target, groups []targetGroup, answers []threadWatchAnswer, woke string) (watchedThreads, error) {
	watched := watchedThreads{wokeOn: woke}
	var failure error
	answered := false
	for slot, group := range groups {
		answer := answers[slot]
		switch {
		case answer.err == nil:
			answered = true
			watched.threads = append(watched.threads, answer.threads...)
		case answeredRefusal(answer.err):
			answered = true
			for _, id := range threadIDs(targets, group) {
				watched.threads = append(watched.threads, c.stampThreads(group.computer, []ThreadState{FailedRow(id, answer.err)})...)
			}
		default:
			failure = cmp.Or(failure, answer.err)
			watched.failures = append(watched.failures, newErrorRow(group.computer, answer.err))
			for _, id := range threadIDs(targets, group) {
				watched.threads = append(watched.threads, c.stampThreads(group.computer,
					[]ThreadState{{ThreadID: id, State: StateUnavailable}})...)
			}
		}
	}
	if !answered {
		return watchedThreads{}, failure
	}
	return watched, nil
}

// watchThreadGroup answers one group: this computer's threads through the
// app, another computer's through one forwarded thread_status call.
func (c *session) watchThreadGroup(ctx context.Context, targets []Target, group targetGroup, wait int, after int64) threadWatchAnswer {
	ids := threadIDs(targets, group)
	if group.local {
		report, err := c.app.RequestStates(ctx, c.caller, StatusCall{ThreadIDs: ids, WaitSeconds: wait, AfterRevision: after})
		if err != nil {
			return threadWatchAnswer{err: err}
		}
		return threadWatchAnswer{threads: c.stampThreads(group.computer, report.Threads), wokeOn: report.WokeOn}
	}
	peer, err := c.app.Peer(ctx, group.computer.ID)
	if err != nil {
		return threadWatchAnswer{err: err}
	}
	remaining := wait
	for {
		slice := min(remaining, MaxForwardedWaitSeconds)
		args, err := json.Marshal(map[string]any{
			"thread_ids":     ids,
			"wait_seconds":   slice,
			"after_revision": after,
		})
		if err != nil {
			return threadWatchAnswer{err: err}
		}
		raw, err := peer.Watch(ctx, args)
		if err != nil {
			return threadWatchAnswer{err: err}
		}
		var result statusResult
		if err := peerResult(raw, &result); err != nil {
			return threadWatchAnswer{err: err}
		}
		remaining -= slice
		if result.WokeOn != "" || remaining <= 0 {
			return threadWatchAnswer{threads: c.stampThreads(group.computer, result.Threads), wokeOn: result.WokeOn}
		}
	}
}

// stampThreads writes this computer's view of the answering computer onto
// every row. The destination answers in its own shape and does not know
// what this computer calls it.
func (c *session) stampThreads(computer Computer, threads []ThreadState) []ThreadState {
	out := slices.Clone(threads)
	for index := range out {
		id, name := c.stamp(Computer{ID: computer.ID, Name: computer.Name})
		out[index].ComputerID, out[index].Computer = id, name
	}
	return out
}
