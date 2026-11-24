package app

import (
	"log"
	"strings"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/triage"
)

// threadAgentView renders request rows for one read: the parked waits, the
// live workers and the settled window are read once for the whole listing.
// A view for
// counts renders each row's state and what it is blocked on, and reads
// none of the text a row shows: no answer head, no target names, no last
// messages.
type threadAgentView struct {
	app     *App
	waiting []string
	live    []string
	since   int64
	counts  bool
}

func newThreadAgentView(a *App) threadAgentView {
	return threadAgentView{
		app: a, waiting: a.parkedRequestTokens(), live: a.liveWorkerThreads(),
		since: time.Now().Add(-a.agentSettledWindowSpan()).UnixMilli(),
	}
}

// agentSettledWindowSpan is how long a settled request stays in a listing:
// threadAgentSettledWindow, or the window an isolated test boot set
// (IsolationConfig.AgentSettledWindow).
func (a *App) agentSettledWindowSpan() time.Duration {
	if a.agentSettledWindowOverride > 0 {
		return a.agentSettledWindowOverride
	}
	return threadAgentSettledWindow
}

// tree reads root's requests nested at most depth levels, or only those
// of them tokens names.
func (v threadAgentView) tree(root string, depth int, tokens []string, limit int) ([]store.ThreadAgentRequest, bool, error) {
	head := threadAgentAnswerHead
	if v.counts {
		head = 0
	}
	return v.app.store.ListThreadAgentTree(store.ThreadAgentTreeRead{
		Root: root, Waiting: v.waiting, Live: v.live, SettledSince: v.since, Depth: depth,
		Limit: limit, AnswerHead: head, Tokens: tokens,
	})
}

// rows renders requests as their rows, with the threads they went to read
// once for them all.
func (v threadAgentView) rows(requests []store.ThreadAgentRequest) ([]ThreadAgentRow, error) {
	if v.counts {
		out := make([]ThreadAgentRow, 0, len(requests))
		for _, request := range requests {
			out = append(out, v.row(request, nil))
		}
		return out, nil
	}
	ids := make([]string, 0, len(requests))
	for _, request := range requests {
		if request.TargetComputerID == "" && request.TargetThreadID != "" {
			ids = append(ids, request.TargetThreadID)
			if request.OriginThreadID != "" {
				ids = append(ids, request.OriginThreadID)
			}
		}
	}
	names, err := v.app.store.ThreadNames(ids, time.Now())
	if err != nil {
		return nil, err
	}
	out := make([]ThreadAgentRow, 0, len(requests))
	for _, request := range requests {
		out = append(out, v.row(request, names))
	}
	v.attachLastMessages(out)
	return out, nil
}

func (v threadAgentView) row(request store.ThreadAgentRequest, names map[string]store.ThreadName) ThreadAgentRow {
	row := request.ThreadRequest
	settled := threadRequestSettled(row)
	local := row.TargetComputerID == "" && row.TargetThreadID != ""
	// A spawn's worker is read whatever its request's state, until its run
	// ended: one that settled is open while the thread it made is not at
	// rest, by its own state or, waiting on them, by its workers'. Once
	// the worker rested after it, what the worker does later is not this
	// row's (app_thread_agent_rows_runs.go). A remote spawn's own state is
	// its computer's answer (nestPeerAgentRows).
	var rest workerRest
	if local && row.Kind == store.ThreadRequestSpawn && !row.RunEnded() {
		rest = v.app.liveWorkerUnrest(row)
	}
	if rest.unrest == "" && local && !settled && v.app.restHeld(row.TargetThreadID, row.Token) {
		// A request held for its thread's rest, a send to the caller's own
		// worker among them, reads as that thread waits.
		rest = v.app.liveThreadRest(row.TargetThreadID)
		if rest.unrest == "" {
			rest.unrest = workerWaiting
		}
	}
	if rest.unrest == "" && (local || settled) && row.Kind == store.ThreadRequestSend && row.RunOpen() && row.TargetThreadID != "" {
		// A send to the caller's own worker shows the worker's state while
		// the run it started or joined is open, as a spawn's row does, and
		// is open while it is: the worker's state here, or as its computer
		// last reported it once the send settled.
		if local {
			rest = v.app.liveThreadRest(row.TargetThreadID)
		} else {
			rest = v.app.remoteRunWorker(row)
		}
	}
	if rest.unrest == "" && !local && !settled && row.TargetThreadID != "" {
		// So does one a paired computer holds for its thread's rest, as
		// that computer last reported it.
		if line := v.app.remoteRequestBlocked(row.Token).waitingOn; line != "" {
			rest = workerRest{unrest: workerWaiting, detail: line}
		}
	}
	if rest.unrest == "" && request.TargetOpen {
		// Nothing of its own, and open work below it: it waits on that.
		rest.unrest = workerWaiting
	}
	worker := rest.unrest
	live := !settled || worker != ""
	blocked := false
	var remote remoteBlocked
	var waiting threadWaiting
	switch {
	case !live || row.TargetThreadID == "":
	case !local:
		remote = v.app.remoteRequestBlocked(row.Token)
		blocked = remote.blocked
	default:
		waiting = v.app.threadWaitingOn(row.TargetThreadID)
		blocked = waiting.live.Blocked()
	}
	state := requestStateFrom(row, request.AnswerHead, request.HasLateReply, requestTitleFrom(row, names), blocked)
	out := ThreadAgentRow{
		Token: row.Token, CallerThreadID: row.CallerThreadID, Kind: row.Kind,
		ThreadID: row.TargetThreadID, OpenThreadID: row.TargetThreadID,
		ComputerID: row.TargetComputerID, Title: state.Title, State: state.State, Worker: worker,
		LateReply: request.HasLateReply, CreatedAt: row.CreatedAt, SettledAt: row.SettledAt,
		Depth: request.Depth, NestedHidden: request.NestedHidden,
		RestUnreported: row.RestUnreported, LateReplyExpiredAt: state.LateReplyExpiredAt,
	}
	if row.Nudge {
		out.Kind = threadAgentKindNudge
	}
	if row.Kind == store.ThreadRequestAsk {
		// The copy that answers an ask is the worker: the row opens it while
		// it runs. Once the ask settles the copy is deleted, and the row
		// names the thread asked as that, never as its worker.
		out.AskedThreadID = row.OriginThreadID
		if _, there := names[row.TargetThreadID]; settled || (local && !there && !v.counts) {
			out.OpenThreadID = ""
		}
	}
	if row.TargetComputerID != "" {
		out.Computer, _ = v.app.threadTools().requestComputerName(row)
		out.Provider = row.TargetProvider
	}
	if settled {
		out.Answer = clipLine(state.Answer)
		out.AnswerKind = state.AnswerKind
		out.WakeItemID = request.WakeItemID
	}
	if name, ok := names[row.TargetThreadID]; ok && local {
		out.Provider = name.Provider
		if row.Kind == store.ThreadRequestSpawn || row.RunBase != nil {
			// A tracked send went to a thread its caller spawned: its row
			// stands for that worker as the spawn's does.
			out.Placement, out.Archived = name.Placement, name.Archived
			// What the thread did in the request's run, read with its name.
			tally := name.Tally
			out.runFigures(row, threadAgentFigures{
				toolCalls: tally.ToolCalls, tokens: tally.Tokens, runtimeMs: tally.RuntimeMs,
				turnOpen: tally.Running, turnStartedAt: tally.TurnStartedAt,
			})
		}
	}
	if !local && !v.counts && row.Kind == store.ThreadRequestSend && row.RunBase != nil && row.TargetThreadID != "" {
		// A send's run on a paired computer's worker counts from what that
		// computer reported when the run began (peerSpawnReport reads a
		// spawn's).
		answer, ok := v.app.remoteRunAnswer(row)
		if ok {
			out.Placement, out.Archived = answer.listing.Placement, answer.listing.Archived
		}
		if row.RunTotal != nil {
			out.runFigures(row, threadAgentFigures{})
		} else if ok {
			out.runFigures(row, peerFigures(answer.listing))
		}
	}
	if asked, ok := names[row.OriginThreadID]; ok && local && row.Kind == store.ThreadRequestAsk {
		// An ask's row is the thread asked, while its copy runs and once
		// the copy is deleted; its kind says it was an ask.
		out.Title, out.Provider = asked.Title, asked.Provider
	}
	switch {
	case !live:
	case row.TargetComputerID != "":
		// That computer reads its target's prompts; it says whether one is
		// routed to a thread there rather than the person, or back to the
		// spawner here.
		if state.State == threadtools.RequestBlocked {
			out.BlockedOn, out.BlockedRoutedTo = "person", remote.routedTo
			if remote.routedHere() {
				// That computer names the thread the link came from: this
				// row's caller, or another thread here, the spawner of the
				// thread this row's request went to.
				named := remote.routedToThread
				if remote.routedToCaller {
					named = row.CallerThreadID
				}
				out.BlockedRoutedTo = v.app.routedHereTitle(row.TargetComputerID, row.TargetThreadID, named, remote.routedTo)
				out.BlockedRoutedHere = true
			}
		}
		out.Activity = rest.detail
	case row.TargetThreadID != "":
		out.BlockedOn, out.BlockedDetail, out.BlockedRoutedTo = waiting.on, waiting.detail, waiting.routedTo()
		out.Activity = v.app.threadAgentActivity(row.TargetThreadID)
		if out.Activity == "" {
			// A waiting worker's line is what it waits on.
			out.Activity = rest.detail
		}
	}
	return out
}

// attachLastMessages reads who spoke last in every local target at once.
func (v threadAgentView) attachLastMessages(rows []ThreadAgentRow) {
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		if row.ComputerID == "" && row.ThreadID != "" {
			ids = append(ids, row.ThreadID)
		}
	}
	if len(ids) == 0 {
		return
	}
	messages, err := v.app.store.ListThreadLastMessages(ids, threadtools.FooterStart)
	if err != nil {
		// The line is a convenience beside the row; the row stands.
		log.Printf("agent rows: read last messages: %v", err)
		return
	}
	for index := range rows {
		message, ok := messages[rows[index].ThreadID]
		if !ok || rows[index].ComputerID != "" {
			continue
		}
		rows[index].LastMessage = clipLine(message.Tail)
		rows[index].LastMessageRole = "assistant"
		if message.Kind == "user_text" {
			rows[index].LastMessageRole = "user"
		}
	}
}

// threadAgentBlockedOn says what a target is waiting on a person for: its
// first approval, else its first question, with the route that prompt took
// (nil while the person owns it).
func threadAgentBlockedOn(snapshot triage.LiveStateSnapshot) (on, detail string, route *provider.PromptRoute) {
	if approvals := snapshot.Interactive.Approvals; len(approvals) > 0 {
		request := approvals[0]
		detail = request.ToolName
		// A provider titles a plain tool prompt with the tool's name.
		title := request.Title
		if strings.TrimSpace(title) == request.ToolName {
			title = ""
		}
		if text := firstNonEmpty(title, request.Description); text != "" {
			detail = request.ToolName + ": " + text
		}
		return "approval", clipLine(detail), request.RoutedTo
	}
	if questions := snapshot.Interactive.UserInputs; len(questions) > 0 {
		request := questions[0]
		detail = request.Title
		if len(request.Questions) > 0 {
			detail = firstNonEmpty(request.Questions[0].Question, request.Questions[0].Header, detail)
		}
		return "question", clipLine(detail), request.RoutedTo
	}
	return "", "", nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
