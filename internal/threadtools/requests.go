package threadtools

import (
	"context"
	"encoding/json"
	"slices"
	"time"
	"unicode/utf8"
)

// thread_reply, thread_status, thread_cancel and thread_remind: the four
// tools that work on requests rather than on threads.

type replyArgs struct {
	Token string `json:"token"`
	Text  string `json:"text"`
}

type replyResult struct {
	Token          string `json:"token"`
	Accepted       bool   `json:"accepted"`
	State          string `json:"state"`
	Revision       int64  `json:"revision"`
	Late           bool   `json:"late,omitempty"`
	SourceThreadID string `json:"source_thread_id,omitempty"`
	SourceComputer string `json:"source_computer,omitempty"`
	Note           string `json:"note,omitempty"`
}

func (c *session) reply(ctx context.Context, raw json.RawMessage) (any, error) {
	var args replyArgs
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	token := trim(args.Token)
	if token == "" {
		return nil, invalidf("token is required. It is in the \"Agent request\" footer of the message you are answering.")
	}
	text := trim(args.Text)
	if err := checkText(text, "text"); err != nil {
		return nil, err
	}
	ack, err := c.app.Reply(ctx, c.caller, ReplyCall{Token: token, Text: text})
	if err != nil {
		return nil, err
	}
	result := replyResult{
		Token: ack.Token, Accepted: ack.Accepted, State: ack.State, Revision: ack.Revision,
		Late: ack.Late, SourceThreadID: ack.SourceThreadID, SourceComputer: ack.SourceComputer,
	}
	switch {
	case !ack.Accepted:
		result.Note = "This token was already answered with the same text; the earlier reply stands. Reply once per token."
	case ack.Late:
		result.Note = "Your turn had already ended and the sender was told so. This reply still reaches it as a follow-up."
	default:
		result.Note = "Delivered to the sender's request record. It reaches the sender even if its computer is unreachable right now."
	}
	return result, nil
}

type statusArgs struct {
	Tokens        []string `json:"tokens"`
	ThreadIDs     []string `json:"thread_ids"`
	WaitSeconds   *int     `json:"wait_seconds"`
	AfterRevision int64    `json:"after_revision"`
	MaxBytes      int      `json:"max_bytes"`
	ToFile        bool     `json:"to_file"`
	Cursor        string   `json:"cursor"`
}

type statusResult struct {
	Requests []RequestState `json:"requests,omitempty"`
	Threads  []ThreadState  `json:"threads,omitempty"`
	WokeOn   string         `json:"woke_on,omitempty"`
	TimedOut bool           `json:"timed_out,omitempty"`
	More     bool           `json:"more,omitempty"`
	Cursor   string         `json:"cursor,omitempty"`
	File     *ExportFile    `json:"file,omitempty"`
	Note     string         `json:"note,omitempty"`
}

func (c *session) status(ctx context.Context, raw json.RawMessage) (any, error) {
	var args statusArgs
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	if len(args.Tokens) > 0 && len(args.ThreadIDs) > 0 {
		return nil, invalidf("Pass tokens or thread_ids, not both: one waits on requests you made, the other on threads' live state.")
	}
	if len(args.Tokens) > MaxStatusTokens || len(args.ThreadIDs) > MaxStatusTokens {
		return nil, invalidf("At most %d tokens or thread ids per call.", MaxStatusTokens)
	}
	wait, err := waitSeconds(args.WaitSeconds, 0)
	if err != nil {
		return nil, err
	}
	budget, err := showBudget(args.MaxBytes)
	if err != nil {
		return nil, err
	}
	if args.ToFile && len(args.Tokens) != 1 {
		return nil, invalidf("to_file writes one request's whole answer, so it needs exactly one token.")
	}

	// The cursor is decoded in the branch that minted it: a listing page
	// and an answer page are different cursors, and one is not the other.
	switch {
	case len(args.Tokens) > 0:
		page, err := decodeCursor(args.Cursor, cursorStatus)
		if err != nil {
			return nil, err
		}
		return c.statusTokens(ctx, args, wait, budget, page)
	case len(args.ThreadIDs) > 0:
		return c.statusThreads(ctx, args, wait)
	default:
		return c.listRequests(ctx, args.Cursor)
	}
}

func (c *session) statusTokens(ctx context.Context, args statusArgs, wait, budget int, page cursor) (any, error) {
	tokens := make([]string, 0, len(args.Tokens))
	for _, token := range args.Tokens {
		token = trim(token)
		if token == "" {
			return nil, invalidf("A token cannot be blank. Take tokens from a spawn, send, ask or reminder result, or list them with thread_status and no arguments.")
		}
		tokens = append(tokens, token)
	}
	if duplicate, found := dedupe(tokens); found {
		return nil, invalidf("token %s is listed twice. Each token may appear once per call.", duplicate)
	}
	if args.ToFile {
		file, err := c.app.ExportAnswer(ctx, c.caller, tokens[0])
		if err != nil {
			return nil, err
		}
		return statusResult{File: &file, Note: "The whole answer was written to that path. Read it with your own file tools."}, nil
	}
	report, err := c.app.RequestStates(ctx, c.caller, StatusCall{Tokens: tokens, WaitSeconds: wait, AfterRevision: args.AfterRevision})
	if err != nil {
		return nil, err
	}
	// Clone before stamping and clipping: the rows belong to the app and
	// a later read of the same request must still see the whole answer.
	result := statusResult{Requests: slices.Clone(report.Requests), WokeOn: report.WokeOn, TimedOut: report.TimedOut}
	c.stampRequests(result.Requests)
	note, clipped := c.clipAnswers(result.Requests, budget, page, tokens)
	result.Note = note
	if clipped != "" && len(tokens) == 1 {
		encoded, err := encodeCursor(cursor{Kind: cursorStatus, Token: clipped, Offset: page.Offset + int64(len(result.Requests[0].Answer))})
		if err != nil {
			return nil, err
		}
		result.Cursor, result.More = encoded, true
	}
	for _, request := range result.Requests {
		if request.WakeQueued {
			result.Note = appendNote(result.Note, "A message carrying this answer is already queued in this thread and will still arrive.")
			break
		}
	}
	return result, nil
}

// clipAnswers keeps the reply inside its budget. Each answer is clipped to
// an equal share and says how to read the rest; the whole answer stays in
// the request record for the request's lifetime, even after the thread
// that wrote it is gone. It names the first request it actually clipped,
// so an answer that fitted whole is never offered a continuation.
func (c *session) clipAnswers(requests []RequestState, budget int, page cursor, tokens []string) (note, clipped string) {
	if len(requests) == 0 {
		return "", ""
	}
	share := budget / len(requests)
	if share < MinShowBytes {
		share = MinShowBytes
	}
	for index := range requests {
		answer := requests[index].Answer
		if len(tokens) == 1 && page.Token == requests[index].Token && page.Offset > 0 {
			if page.Offset >= int64(len(answer)) {
				answer = ""
			} else {
				answer = answer[page.Offset:]
			}
		}
		if len(answer) > share {
			answer = clipBytes(answer, share)
			if clipped == "" {
				clipped = requests[index].Token
			}
			note = appendNote(note, "An answer was longer than max_bytes and is clipped here. Continue it with the cursor, read the whole of it with thread_status to_file, or raise max_bytes.")
		}
		requests[index].Answer = answer
	}
	return note, clipped
}

// statusThreads answers the thread_ids half of thread_status. A thread is
// addressed by its id wherever it lives, so the ids are resolved first and
// then grouped by the computer that holds them: this computer's run here,
// and each other computer's are forwarded to it as one thread_status call
// of its own.
func (c *session) statusThreads(ctx context.Context, args statusArgs, wait int) (any, error) {
	targets := make([]Target, 0, len(args.ThreadIDs))
	ids := make([]string, 0, len(args.ThreadIDs))
	for _, ref := range args.ThreadIDs {
		target, err := c.resolve(ctx, ref, "")
		if err != nil {
			return nil, err
		}
		if target.ThreadID == c.caller.ThreadID {
			return nil, publicf(CodeSelfSend, "A thread cannot watch itself. Watch the threads you started, or use thread_remind to wake yourself later.")
		}
		targets = append(targets, target)
		ids = append(ids, target.ThreadID)
	}
	if duplicate, found := dedupe(ids); found {
		return nil, invalidf("thread %s is listed twice. Each thread may appear once per call.", duplicate)
	}
	states, woke, err := c.watchThreadGroups(ctx, threadWatchGroups(targets), wait, args.AfterRevision)
	if err != nil {
		return nil, err
	}
	result := statusResult{WokeOn: woke, TimedOut: woke == ""}
	byID := make(map[string]ThreadState, len(states))
	for _, state := range states {
		byID[state.ThreadID] = state
	}
	// The rows come back in the order the call listed them, whichever
	// computer answered for each.
	for _, id := range ids {
		state, found := byID[id]
		if !found {
			continue
		}
		// Resting is derived here from the state, so a row cannot report
		// a state and a restedness that disagree.
		state.Resting = Resting(state.State)
		result.Threads = append(result.Threads, state)
	}
	if result.TimedOut {
		result.Note = "None of those threads rested or became blocked within the wait. They are still as reported."
	}
	return result, nil
}

// threadWatchGroup is the threads of one call that live on one computer.
type threadWatchGroup struct {
	computer Computer
	local    bool
	ids      []string
}

// threadWatchGroups splits resolved targets by the computer that holds
// them, keeping the order the call listed them in.
func threadWatchGroups(targets []Target) []threadWatchGroup {
	groups := make([]threadWatchGroup, 0, 2)
	// "local" cannot collide with a computer id, which is a UUID.
	slots := make(map[string]int, 2)
	for _, target := range targets {
		key := "local"
		if !target.Local {
			key = target.ComputerID
		}
		if slot, seen := slots[key]; seen {
			groups[slot].ids = append(groups[slot].ids, target.ThreadID)
			continue
		}
		slots[key] = len(groups)
		groups = append(groups, threadWatchGroup{
			computer: Computer{ID: target.ComputerID, Name: target.Computer},
			local:    target.Local,
			ids:      []string{target.ThreadID},
		})
	}
	return groups
}

// threadWatchAnswer is one group's half of the call.
type threadWatchAnswer struct {
	threads []ThreadState
	wokeOn  string
	err     error
}

// watchThreadGroups runs every group's wait at once and ends the call on
// the first group that reports something: the agent watching three threads
// wants the one that rested, not the one it happened to list first. The
// groups that were still waiting are cancelled and read once more without
// a wait, so every thread the call named is still reported.
func (c *session) watchThreadGroups(ctx context.Context, groups []threadWatchGroup, wait int, after int64) ([]ThreadState, string, error) {
	if len(groups) == 1 {
		answer := c.watchThreadGroup(ctx, groups[0], wait, after)
		return answer.threads, answer.wokeOn, answer.err
	}
	racing, cancel := context.WithCancel(ctx)
	defer cancel()
	answers := make([]threadWatchAnswer, len(groups))
	finished := make(chan int, len(groups))
	for index, group := range groups {
		go func(slot int, group threadWatchGroup) {
			answers[slot] = c.watchThreadGroup(racing, group, wait, after)
			finished <- slot
		}(index, group)
	}
	var failure error
	woke := ""
	for range groups {
		slot := <-finished
		answer := answers[slot]
		switch {
		case answer.err != nil:
			// A failure this call caused by cancelling the group is not a
			// failure of the call: that group is read again below.
			if failure == nil && racing.Err() == nil {
				failure = answer.err
				cancel()
			}
		case answer.wokeOn != "" && woke == "":
			woke = answer.wokeOn
			cancel()
		}
	}
	if failure != nil {
		return nil, "", failure
	}
	states := make([]ThreadState, 0, len(groups))
	for index := range answers {
		if answers[index].err != nil {
			// Cancelled mid-wait. Its threads are read as they stand now,
			// on the call's own context rather than the cancelled one.
			answers[index] = c.watchThreadGroup(ctx, groups[index], 0, after)
			if answers[index].err != nil {
				return nil, "", answers[index].err
			}
		}
		states = append(states, answers[index].threads...)
	}
	return states, woke, nil
}

// watchThreadGroup answers one group: this computer's threads through the
// app, another computer's through one forwarded thread_status call.
func (c *session) watchThreadGroup(ctx context.Context, group threadWatchGroup, wait int, after int64) threadWatchAnswer {
	if group.local {
		report, err := c.app.RequestStates(ctx, c.caller, StatusCall{ThreadIDs: group.ids, WaitSeconds: wait, AfterRevision: after})
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
		slice := remaining
		if slice > MaxForwardedWaitSeconds {
			slice = MaxForwardedWaitSeconds
		}
		args, err := json.Marshal(map[string]any{
			"thread_ids":     group.ids,
			"wait_seconds":   slice,
			"after_revision": after,
		})
		if err != nil {
			return threadWatchAnswer{err: err}
		}
		raw, err := peer.Query(ctx, "thread_status", args)
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

func (c *session) listRequests(ctx context.Context, rawCursor string) (any, error) {
	page, err := decodeCursor(rawCursor, cursorList)
	if err != nil {
		return nil, err
	}
	listing, err := c.app.ListRequests(ctx, c.caller, ListCall{Offset: int(page.Offset), Limit: DefaultRequestListLimit})
	if err != nil {
		return nil, err
	}
	result := statusResult{Requests: slices.Clone(listing.Requests), More: listing.More}
	c.stampRequests(result.Requests)
	if result.Requests == nil {
		result.Requests = []RequestState{}
	}
	result.Note = "Open requests first, then newest first. These are every request this thread has made; you never have to remember a token."
	if listing.More {
		encoded, err := encodeCursor(cursor{Kind: cursorList, Offset: page.Offset + int64(len(listing.Requests))})
		if err != nil {
			return nil, err
		}
		result.Cursor = encoded
	}
	return result, nil
}

func (c *session) stampRequests(requests []RequestState) {
	for index := range requests {
		id, name := c.stamp(Computer{ID: requests[index].ComputerID, Name: requests[index].Computer})
		requests[index].ComputerID, requests[index].Computer = id, name
	}
}

type cancelArgs struct {
	Token      string `json:"token"`
	ThreadID   string `json:"thread_id"`
	ComputerID string `json:"computer_id"`
}

type cancelResult struct {
	Token      string `json:"token,omitempty"`
	ThreadID   string `json:"thread_id,omitempty"`
	ComputerID string `json:"computer_id,omitempty"`
	Computer   string `json:"computer,omitempty"`
	State      string `json:"state"`
	Effect     string `json:"effect"`
	Note       string `json:"note,omitempty"`
}

func (c *session) cancel(ctx context.Context, raw json.RawMessage) (any, error) {
	var args cancelArgs
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	if err := c.checkComputerArg(args.ComputerID); err != nil {
		return nil, err
	}
	token, ref := trim(args.Token), trim(args.ThreadID)
	if count := exactlyOne(token != "", ref != ""); count != 1 {
		return nil, invalidf("Pass exactly one of token, to cancel a request you made, or thread_id, to interrupt a thread you started. This call passed %d.", count)
	}
	call := CancelCall{Token: token}
	if ref != "" {
		target, err := c.resolve(ctx, ref, trim(args.ComputerID))
		if err != nil {
			return nil, err
		}
		if target.ThreadID == c.caller.ThreadID {
			return nil, publicf(CodeInvalidRequest, "A thread cannot interrupt itself through these tools.")
		}
		call.ThreadID, call.ComputerID = target.ThreadID, target.Destination()
	}
	report, err := c.app.Cancel(ctx, c.caller, call)
	if err != nil {
		return nil, err
	}
	id, name := c.stamp(Computer{ID: report.ComputerID, Name: report.Computer})
	return cancelResult{
		Token: report.Token, ThreadID: report.ThreadID, ComputerID: id, Computer: name,
		State: report.State, Effect: report.Effect,
		Note: "This interrupted the work; it did not undo anything already done.",
	}, nil
}

type remindArgs struct {
	AfterSeconds int    `json:"after_seconds"`
	At           string `json:"at"`
	Note         string `json:"note"`
}

func (c *session) remind(ctx context.Context, raw json.RawMessage) (any, error) {
	var args remindArgs
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	note := trim(args.Note)
	if note == "" {
		return nil, invalidf("note is required: it is what the reminder tells you when it fires.")
	}
	if utf8.RuneCountInString(note) > MaxNoteRunes {
		return nil, invalidf("note must be at most %d characters.", MaxNoteRunes)
	}
	at := trim(args.At)
	if count := exactlyOne(args.AfterSeconds != 0, at != ""); count != 1 {
		return nil, invalidf("Pass exactly one of after_seconds or at. This call passed %d.", count)
	}
	var due int64
	if at != "" {
		parsed, err := parseTimestamp(at, "at")
		if err != nil {
			return nil, err
		}
		due = parsed
	} else {
		if args.AfterSeconds < 0 {
			return nil, invalidf("after_seconds must be a positive number of seconds from now.")
		}
		due = c.now().Add(time.Duration(args.AfterSeconds) * time.Second).UnixMilli()
	}
	ack, err := c.app.Remind(ctx, c.caller, RemindCall{DueAtUnixMs: due, Note: note})
	if err != nil {
		return nil, err
	}
	result := c.ack(ack, "remind")
	result.Note = "This thread will wake with that note at " + unixMsToRFC3339(due) + ". End your turn: the reminder starts a new one. Stop it with thread_cancel token " + ack.Token + "."
	return result, nil
}
