package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/keyedlock"
	"agent-overflow/internal/remotejobs"
	"agent-overflow/internal/rpcclient"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/usermessage"
)

func (a *App) remoteStartLocks() *keyedlock.Registry {
	a.remoteStartsOnce.Do(func() { a.remoteStarts = keyedlock.New() })
	return a.remoteStarts
}

// Every successful command RPC learns the destination's receipt immediately.
// The watcher still owns durable completion delivery; returning a tool result
// is not an acknowledgement that the provider received it.
func (a *App) observeRemoteCommand(computerID, requestID, threadID string, receipt RemoteCommand) error {
	w, watched, err := a.remoteReceiptOwnedBy(computerID, requestID, threadID, receipt)
	if err != nil || !watched {
		return err // Receipts from work predating source watches remain readable.
	}
	receipt.Output = ""
	terminal := w.Receipt.ID != "" && w.Receipt.State != "running"
	if (w.Receipt == receipt && w.Error == "") || (terminal && (receipt.State != w.Receipt.State || w.Error == "")) {
		return nil
	}
	next := int64(0)
	if receipt.State == "running" {
		next = time.Now().Add(2 * time.Second).UnixMilli()
	}
	if err = a.store.ObserveRemoteWatch(computerID, requestID, receipt, "", next); err != nil {
		return err
	}
	a.nudgeRemoteWatches()
	a.emitBackgroundChanged(threadID)
	return nil
}

// remoteReceiptOwnedBy checks that receipt is job requestID and that
// threadID owns it: the thread its watch names, the receipt naming the
// thread that launched the job (New Thread, Same Assistant moves the watch
// to the assistant's next thread while the destination keeps naming the
// launcher). Without a watch the receipt must name threadID itself.
func (a *App) remoteReceiptOwnedBy(computerID, requestID, threadID string, receipt RemoteCommand) (store.RemoteWatch, bool, error) {
	w, err := a.store.GetRemoteWatch(computerID, requestID)
	watched := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return w, false, err
	}
	if watched && w.ThreadID != threadID {
		return w, true, errorsx.Public("remote_wrong_conversation", "This command belongs to another conversation. Read or cancel it from the conversation that submitted it.", nil)
	}
	launcher := threadID
	if watched {
		launcher = w.LaunchedIn
	}
	if receipt.ID != requestID || receipt.SourceThreadID != launcher {
		return w, watched, errorsx.Public("remote_wrong_conversation", "The destination returned a command receipt for another request or conversation. Use the original request ID from the conversation that submitted it.", nil)
	}
	return w, watched, nil
}

// Only a definite refusal of a still-unaccepted attempt releases a watch; the
// store keeps an accepted receipt over any late refusal.
func (a *App) refuseRemoteWatch(computerID, requestID, threadID string, refusal error) error {
	if err := a.store.RefuseRemoteWatch(computerID, requestID, remoteErrorText(refusal)); err != nil {
		return err
	}
	a.emitBackgroundChanged(threadID)
	return nil
}

func (a *App) registerRemoteWatch(input AgentRemoteRequest) (bool, error) {
	command := remoteJobCommand(input.Request)
	label, err := remoteJobLabel(input.Label, command)
	if err != nil {
		return false, err
	}
	// Forgetting a computer must not discard the only cancellation handle,
	// and no command enters a pairing being ended.
	unlockComputer := a.lockComputerPairing(input.ComputerID)
	defer unlockComputer()
	enabled, err := a.backends.AgentAccess()
	if err != nil {
		return false, err
	}
	if !enabled[input.ComputerID] {
		return false, errorsx.Public("remote_access_disabled", "Agent commands are not enabled for this computer. Enable it in Remote access → Agent remote tools.", nil)
	}
	// Admission and final deletion/transfer share the short mutation fence,
	// which also rederives execution ownership (a moved or fenced conversation
	// is refused here). A destination must never accept a job after its
	// source has disappeared.
	unlock, err := a.threadApplication().LockMutable(a.lifeCtx(), input.Request.SourceThreadID)
	if err != nil {
		return false, err
	}
	defer unlock()
	if _, err := a.store.GetThread(input.Request.SourceThreadID); err != nil {
		return false, err
	}
	// Labels belong to the source presentation, never execution identity. Keep
	// the omitted-label encoding identical to watches admitted before labels.
	execution := input
	execution.Label = ""
	raw, err := json.Marshal(execution)
	if err != nil {
		return false, err
	}
	digest := sha256.Sum256(raw)
	// With the name the person knows the computer by, which names the
	// command once the pairing ends.
	profile, paired, err := a.backends.Lookup(input.ComputerID)
	if err != nil {
		return false, err
	}
	if !paired {
		return false, errorsx.Public("remote_not_paired", "This computer is no longer paired. Reconnect it in Remote access before retrying.", nil)
	}
	registered, err := a.store.RegisterRemoteWatch(store.RemoteWatch{ComputerID: input.ComputerID, RequestID: input.Request.ID, ThreadID: input.Request.SourceThreadID,
		Fingerprint: hex.EncodeToString(digest[:]), Label: label, Command: command, ComputerName: truncateRunes(attachedComputerName(profile), remoteJobCommandMaxRunes)})
	if registered {
		a.nudgeRemoteWatches()
	}
	return registered, err
}

const remoteJobLabelMaxRunes = 120

// remoteJobCommandMaxRunes bounds the stored display text; the store caps the
// column at 1024 bytes.
const remoteJobCommandMaxRunes = 240

// remoteJobLabel is the caller's label, or the command text when omitted.
func remoteJobLabel(label, command string) (string, error) {
	if !utf8.ValidString(label) || strings.ContainsFunc(label, func(r rune) bool {
		return unicode.IsControl(r) || r == '\u2028' || r == '\u2029'
	}) {
		return "", errorsx.Public("remote_invalid_label", "label must be a single line without control characters.", nil)
	}
	label = strings.TrimSpace(label)
	if utf8.RuneCountInString(label) > remoteJobLabelMaxRunes {
		return "", errorsx.Public("remote_invalid_label", "label must be at most 120 characters; use a short description of the job.", nil)
	}
	if label != "" {
		return label, nil
	}
	return truncateRunes(command, remoteJobLabelMaxRunes), nil
}

// remoteJobCommand renders what a request runs for people: argv with
// arguments quoted where boundaries would otherwise be ambiguous, or the
// interpreter plus "script" for an inline script. It is display text, never
// an executable shell string. The frontend renders the same text from a live
// tool call's arguments (`frontend/src/lib/components/chat/aoTools.ts`).
func remoteJobCommand(request RemoteCommandRequest) string {
	argv := request.Argv
	if request.Script != "" {
		argv = request.Interpreter
	}
	var command strings.Builder
	for i, arg := range argv {
		if i > 0 {
			command.WriteByte(' ')
		}
		if arg == "" || strings.ContainsAny(arg, "\"'\\") || strings.ContainsFunc(arg, func(r rune) bool {
			return unicode.IsSpace(r) || unicode.IsControl(r)
		}) {
			command.WriteString(strconv.Quote(arg))
		} else {
			command.WriteString(arg)
		}
	}
	if request.Script != "" {
		command.WriteString(" script")
	}
	return truncateRunes(command.String(), remoteJobCommandMaxRunes)
}

func truncateRunes(text string, max int) string {
	if utf8.RuneCountInString(text) <= max {
		return text
	}
	n := 0
	for offset := range text {
		if n == max-1 {
			return text[:offset] + "…"
		}
		n++
	}
	return text
}

func (a *App) checkRemoteWatch(w store.RemoteWatch) {
	if a.shuttingDown.Load() {
		return
	}
	current, e := a.store.GetRemoteWatch(w.ComputerID, w.RequestID)
	if e != nil || current.Notification != "pending" {
		return
	}
	w = current
	// A tool call parked on this job polls it and will deliver its result in
	// the reply; the watcher takes over only once that call has returned.
	if a.remoteWaitActive(w.ComputerID, w.RequestID) {
		_ = a.store.ObserveRemoteWatch(w.ComputerID, w.RequestID, w.Receipt, w.Error, time.Now().Add(5*time.Second).UnixMilli())
		return
	}
	thread, err := a.store.GetThread(w.ThreadID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			_ = a.store.DismissRemoteWatch(w.ComputerID, w.RequestID)
		}
		return
	}

	// A moved conversation retains its receipts but cannot be driven
	// from the old owner. Do not turn a completion into a workflow takeover.
	// A thread a delete claimed takes it into its queue, as every
	// app-written message (store.CheckThreadDeliverable).
	if err = a.store.CheckThreadDeliverable(thread); err != nil {
		_ = a.store.ObserveRemoteWatch(w.ComputerID, w.RequestID, w.Receipt, "Completion is waiting for this conversation’s execution ownership.", time.Now().Add(30*time.Second).UnixMilli())
		return
	}
	receipt := w.Receipt
	if receipt.ID == "" || receipt.State == "running" {
		// A computer no longer paired here is asked nothing: the end of its
		// pairing records the outcome as unknown, which the next check
		// delivers with no call to that computer.
		ended, err := a.endUnpaired(w.ComputerID)
		if err != nil {
			_ = a.store.ObserveRemoteWatch(w.ComputerID, w.RequestID, w.Receipt, remoteIssueText("status", err), time.Now().Add(30*time.Second).UnixMilli())
			return
		}
		if ended {
			return
		}
	}
	// One bounded context covers every wait below: the attempt lock, the peer
	// call and the thread action lock. A lock another action holds costs this
	// watch one poll; it never stalls the other jobs' checks.
	ctx, cancel := context.WithTimeout(a.lifeCtx(), 10*time.Second)
	defer cancel()
	switch {
	case receipt.ID != "" && receipt.State != "running":
		// A direct reply settled the canonical receipt without retaining its
		// tail. The saved log supplies the excerpt below; delivery never waits
		// on it, and an older destination simply omits it.
	case receipt.ID != "":
		err = a.backends.CallAgentPeer(ctx, w.ComputerID, "RemoteCommandStatus", &receipt, w.RequestID)
	default:
		var settled bool
		if settled, err = a.probeUnacceptedRemoteWatch(ctx, w, &receipt); settled {
			if err != nil {
				log.Printf("remote job observation: %v", err)
			}
			return
		}
	}
	issue := ""
	if err != nil {
		if a.unpairedAfterFailure(w.ComputerID) {
			// The refusal ended the pairing, which recorded the outcome as
			// unknown.
			return
		}
		issue = remoteIssueText("status", err)
		receipt = w.Receipt
	} else if receipt.ID != w.RequestID || receipt.SourceThreadID != w.LaunchedIn {
		issue = "The destination returned a receipt for a different conversation. Completion delivery is paused."
		receipt = w.Receipt
	}
	next := time.Now().Add(a.remoteWatchPollInterval()).UnixMilli()
	if issue != "" {
		next = time.Now().Add(30 * time.Second).UnixMilli()
	}
	if hook := a.remoteStatusReadForTest; hook != nil {
		hook(w)
	}
	if err = a.store.ObserveRemoteWatch(w.ComputerID, w.RequestID, receipt, issue, next); err != nil {
		log.Printf("remote job observation: %v", err)
		return
	}
	if receipt.State != w.Receipt.State || issue != w.Error {
		a.emitBackgroundChanged(w.ThreadID)
	}
	if issue != "" || receipt.ID == "" || receipt.State == "running" {
		return
	}
	w.Receipt = receipt
	output := a.remoteCompletionExcerpt(ctx, w.ComputerID, w.RequestID, receipt)
	if a.remoteCompletionReadForTest != nil {
		a.remoteCompletionReadForTest(w)
	}
	if err = a.deliverRemoteCompletion(w, output); err != nil {
		_ = a.store.ObserveRemoteWatch(w.ComputerID, w.RequestID, receipt, "Completion could not enter the message queue: "+remoteErrorText(err), next)
	}
}

// remoteWatchPollInterval is how often the watcher asks the destination
// about a running job, or the cadence an isolated test boot set
// (IsolationConfig.RemoteWatchPoll).
func (a *App) remoteWatchPollInterval() time.Duration {
	if a.remoteWatchPoll > 0 {
		return a.remoteWatchPoll
	}
	return 5 * time.Second
}

// An unacknowledged start settles under its attempt lock. Once no retry is in
// flight, a destination holding no receipt never accepted the request, so the
// watch is released and an identical retry registers it again. A lock held by
// an in-flight attempt defers this poll; that attempt records its own outcome.
func (a *App) probeUnacceptedRemoteWatch(ctx context.Context, w store.RemoteWatch, receipt *RemoteCommand) (settled bool, err error) {
	unlock, err := a.remoteStartLocks().LockCtx(ctx, w.ComputerID+":"+w.RequestID)
	if err != nil {
		return true, a.store.ObserveRemoteWatch(w.ComputerID, w.RequestID, w.Receipt, w.Error, time.Now().Add(5*time.Second).UnixMilli())
	}
	defer unlock()
	err = a.backends.CallAgentPeer(ctx, w.ComputerID, "RemoteCommandStatus", receipt, w.RequestID)
	var remote *rpcclient.Error
	if !errors.As(err, &remote) || remote.Code != "remote_job_not_found" {
		return false, err
	}
	return true, a.refuseRemoteWatch(w.ComputerID, w.RequestID, w.ThreadID, a.remoteOperationError("status", w.ComputerID, w.RequestID, err))
}

// remoteCompletionExcerpt prefers the saved log, which has both ends of the
// output; the inline receipt tail is the fallback for an unreachable or older
// destination, and an empty one is reported as unavailable. A command whose
// outcome is unknown (store.EndPairing) has no output to read: the computer
// that ran it is no longer paired.
func (a *App) remoteCompletionExcerpt(ctx context.Context, computerID, requestID string, receipt RemoteCommand) remoteCompletionOutput {
	if receipt.State == store.RemoteOutcomeUnknown {
		return remoteCompletionOutput{}
	}
	if excerpt, ok := a.remoteLogExcerpt(ctx, computerID, requestID, remoteCompletionHeadBytes, remoteCompletionOutputBytes); ok && !excerpt.Info.Expired {
		omitted := excerpt.Info.RetainedBytes > int64(len(excerpt.Head)+len(excerpt.Tail))
		return remoteCompletionOutput{Head: excerpt.Head, Tail: excerpt.Tail, Truncated: excerpt.Info.Truncated, Omitted: omitted}
	}
	if receipt.Output == "" {
		return remoteCompletionOutput{Unavailable: true}
	}
	return remoteCompletionOutput{Tail: receipt.Output, Truncated: receipt.Truncated, Omitted: len(receipt.Output) > remoteCompletionOutputBytes}
}

// deliverRemoteCompletion queues the finished job's report for its thread.
// The excerpt is fetched before this call, so the queue write never waits on
// the destination network.
func (a *App) deliverRemoteCompletion(w store.RemoteWatch, output remoteCompletionOutput) error {
	err := a.queueAgentNotice(a.remoteCompletionNotice(w, output))
	if errors.Is(err, errThreadArchived) {
		// An archive that landed after prepare: the refusal came before
		// the watch moved, so the completion is still owed, and arrives
		// once the thread is unarchived.
		return nil
	}
	return err
}

// remoteCompletionNotice is the notice that carries w's completion.
func (a *App) remoteCompletionNotice(w store.RemoteWatch, output remoteCompletionOutput) agentNotice {
	return agentNotice{
		threadID: w.ThreadID,
		sendID:   remoteCompletionSendID(w),
		// The app's report, not the person's: an agent row naming the job
		// (remoteCompletionMessage).
		prepare: func() (usermessage.AgentMessage, bool, error) { return a.prepareRemoteCompletion(w, output) },
		persist: func(item store.FlushQueueItem) error {
			return a.store.QueueRemoteCompletion(w.ComputerID, w.RequestID, item)
		},
		startFailed: func(err error) string {
			// Admission already handed the message to the ordinary durable
			// queue. Startup's normal flush trigger dispatches it.
			return "Remote job finished; its completion is queued, but the agent could not start: " + remoteErrorText(err)
		},
	}
}

// prepareRemoteCompletion is this notice's own policy: whether the
// completion is still owed, and what it says. Every read it makes can have
// changed since the watcher observed the receipt.
func (a *App) prepareRemoteCompletion(w store.RemoteWatch, output remoteCompletionOutput) (usermessage.AgentMessage, bool, error) {
	current, err := a.store.GetRemoteWatch(w.ComputerID, w.RequestID)
	if err != nil {
		return usermessage.AgentMessage{}, false, err
	}
	if current.Notification != "pending" || current.CancelOwedAt != 0 {
		// A job whose cancel is owed is delivered once the cancel settles.
		return usermessage.AgentMessage{}, false, nil
	}
	// A direct status/cancel can finish while this watcher is in flight. The
	// durable receipt wins; only reuse this excerpt when it describes that
	// same receipt.
	metadata := w.Receipt
	metadata.Output = ""
	if metadata != current.Receipt {
		output = remoteCompletionOutput{Unavailable: true}
	}
	w = current
	if w.Receipt.ID == "" || w.Receipt.State == "running" {
		return usermessage.AgentMessage{}, false, nil
	}
	thread, err := a.store.GetThread(w.ThreadID)
	if err != nil {
		return usermessage.AgentMessage{}, false, err
	}
	if thread.Archived {
		// Owed once the thread is unarchived; the watcher skips it until then.
		return usermessage.AgentMessage{}, false, nil
	}
	if err = a.store.CheckThreadDeliverable(thread); err != nil {
		return usermessage.AgentMessage{}, false, err
	}
	if thread.Mode == threadmode.ModeWorkflow {
		// A workflow owns its phase lifetime. Finishing a remote job cannot reopen
		// a completed phase or launch a phase whose session the engine closed.
		_, live := a.sessionManager().get(thread.ID)
		unit, found, e := a.store.GetWorkItemUnitByThread(thread.ID)
		if e != nil {
			return usermessage.AgentMessage{}, false, e
		}
		itemID := unit.ItemID
		running := found && unit.Status == store.WorkItemUnitRunning
		if !found {
			phase, exists, e := a.store.GetWorkItemPhaseByThread(thread.ID)
			if e != nil {
				return usermessage.AgentMessage{}, false, e
			}
			itemID = phase.ItemID
			running = exists && phase.Status == "running"
		}
		if running {
			item, e := a.store.GetWorkItem(itemID)
			if e != nil {
				return usermessage.AgentMessage{}, false, e
			}
			running = item.State == "running"
		}
		if !running || !live {
			return usermessage.AgentMessage{}, false, a.store.DismissRemoteWatch(w.ComputerID, w.RequestID)
		}
	}
	message, err := remoteCompletionMessage(w, remoteWatchComputerName(a.remoteComputers(), w), output)
	return message, err == nil, err
}

const remoteCompletionSendIDRoot = "remote-completion:"

func remoteCompletionSendID(w store.RemoteWatch) string {
	return remoteCompletionSendIDRoot + w.ComputerID + ":" + w.RequestID
}

// remoteCompletionFromSendID reads a completion's watch back out of its send
// id. Computer and request ids are UUIDs, so it splits at its first colon.
func remoteCompletionFromSendID(sendID string) (computerID, requestID string, ok bool) {
	ids, isCompletion := strings.CutPrefix(sendID, remoteCompletionSendIDRoot)
	computerID, requestID, ok = strings.Cut(ids, ":")
	return computerID, requestID, isCompletion && ok
}

// ListThreadRemoteCommands lists a conversation's tracked remote jobs, pending
// first, with each computer's name. The transcript resolves job labels and
// computer names from it; the tray reads its own projection.
//
//ao:scope threads:read
func (a *App) ListThreadRemoteCommands(threadID string) ([]RemoteJobRecord, error) {
	watches, err := a.store.ListRemoteWatches(threadID, 0, 256)
	if err != nil {
		return nil, err
	}
	computers := a.remoteComputers()
	rows := make([]RemoteJobRecord, 0, len(watches))
	for _, w := range watches {
		rows = append(rows, remoteJobRecord(w, remoteWatchComputerName(computers, w)))
	}
	return rows, nil
}

//ao:scope terminal:operate
func (a *App) CancelThreadRemoteCommand(ctx context.Context, threadID, computerID, requestID string) (RemoteCommand, error) {
	if err := a.requireScope(ctx, transport.ScopeTerminalOperate, "cancel a remote command"); err != nil {
		return RemoteCommand{}, err
	}
	if a.backends == nil {
		return RemoteCommand{}, errNoBackendProfiles
	}
	w, err := a.store.GetRemoteWatch(computerID, requestID)
	if err != nil {
		return RemoteCommand{}, err
	}
	if w.ThreadID != threadID {
		return RemoteCommand{}, errorsx.Public("remote_wrong_conversation", "This job belongs to another conversation.", nil)
	}
	// The cancel and the record of its answer finish once sent, even at
	// shutdown; only the settle wait below ends with the context.
	call, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cancel()
	var receipt RemoteCommand
	err = a.backends.CallAgentPeer(call, computerID, "RemoteCommandCancel", &receipt, requestID)
	if err == nil {
		err = a.observeRemoteCommand(computerID, requestID, threadID, receipt)
	}
	if err != nil {
		return receipt, a.remoteUserError("stop", computerID, requestID, err)
	}
	// Cancel only asks; the destination gives the process a TERM grace
	// before KILL (remotejobs.Manager). The caller pressed Stop and is
	// watching the row, so stay until the receipt settles or the grace has
	// clearly passed. A settle wait that fails leaves the cancel delivered
	// and the watcher to observe the outcome.
	deadline := time.Now().Add(remoteCancelSettleWait)
	for receipt.State == "running" && time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return receipt, nil
		case <-time.After(remoteCancelSettlePoll):
		}
		status, statusErr := a.remoteCommandStatus(ctx, w)
		if statusErr != nil {
			break
		}
		receipt = status
		if observeErr := a.observeRemoteCommand(computerID, requestID, threadID, receipt); observeErr != nil {
			return receipt, a.remoteUserError("stop", computerID, requestID, observeErr)
		}
	}
	return receipt, nil
}

// The TERM grace is five seconds (remotejobs.Manager); one more poll after it
// sees the KILL land.
const (
	remoteCancelSettleWait = 8 * time.Second
	remoteCancelSettlePoll = 500 * time.Millisecond
)

// remoteCommandStatus asks for the receipt of the command w watches.
func (a *App) remoteCommandStatus(ctx context.Context, w store.RemoteWatch) (RemoteCommand, error) {
	call, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var receipt RemoteCommand
	err := a.backends.CallAgentPeer(call, w.ComputerID, "RemoteCommandStatus", &receipt, w.RequestID)
	return receipt, err
}

// cancelThreadRemoteCommands stops every remote command a conversation still
// owns and drops their pending notifications. Deleting or moving the
// conversation drops all of them: nothing remains here to receive the
// result. Archiving (keepFinished) stops only what is still running and
// keeps the notification of a job that has finished, so the watcher
// delivers it once the conversation is unarchived. The watcher may not have
// seen a finish yet; the destination answers the cancel with the receipt as
// it stood, and a finished one there is recorded and kept. Parked tool calls
// return at once. A job whose cancel could not be delivered is owed it, and
// the watcher sends it again (retryOwedRemoteCancels) while the watch stays;
// a delete cascades the watch, leaving it to the destination's owner grace
// (remotejobs.DefaultOwnerGrace). It returns, apart, the cancels it could not
// deliver (undelivered) and the store failures that left commands untracked
// (err). Each caller decides whether its operation proceeds past either.
func (a *App) cancelThreadRemoteCommands(ctx context.Context, threadID string, keepFinished bool) (undelivered, err error) {
	rows, err := a.disarmRemoteCommands(threadID)
	if err != nil {
		return nil, err
	}
	return a.cancelRemoteCommands(ctx, threadID, rows, keepFinished)
}

// disarmRemoteCommands ends the thread's parked remote waits and returns the
// commands it started that still await completion.
func (a *App) disarmRemoteCommands(threadID string) ([]store.RemoteWatch, error) {
	a.cancelRemoteWaits(threadID)
	rows, err := a.store.ListRemoteWatches(threadID, 0, 256)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(rows, func(w store.RemoteWatch) bool { return w.Notification != "pending" }), nil
}

// cancelRemoteCommands cancels and dismisses exactly the commands named,
// keeping a finished one's notification when keepFinished is set
// (cancelThreadRemoteCommands). A cancel the destination did not take is
// owed and named in undelivered; the dismissal goes ahead, so the thread is
// told nothing of the job either way. err gathers the store failures. One
// failure does not stop the rest.
func (a *App) cancelRemoteCommands(ctx context.Context, threadID string, rows []store.RemoteWatch, keepFinished bool) (undelivered, err error) {
	var cancels, failed []error
	for _, w := range rows {
		unfinished := w.Receipt.ID == "" || w.Receipt.State == "running"
		if !unfinished && keepFinished {
			continue
		}
		if unfinished && a.backends != nil {
			call, cancel := context.WithTimeout(ctx, 20*time.Second)
			var receipt RemoteCommand
			callErr := a.backends.CallAgentPeer(call, w.ComputerID, "RemoteCommandCancel", &receipt, w.RequestID)
			cancel()
			if callErr != nil {
				now := time.Now()
				if oweErr := a.store.OweRemoteCancel(w.ComputerID, w.RequestID, now.UnixMilli(), now.Add(remoteCancelRetryDelay).UnixMilli()); oweErr != nil {
					failed = append(failed, fmt.Errorf("remote command %s on %s: owe its cancel: %w", w.RequestID, w.ComputerID, oweErr))
				} else {
					a.nudgeRemoteWatches()
				}
				cancels = append(cancels, fmt.Errorf("cancel remote command %q: %s; it is sent again until the computer takes it",
					w.Label, remoteErrorText(a.remoteOperationError("cancel", w.ComputerID, w.RequestID, callErr))))
			} else if keepFinished && receipt.ID == w.RequestID && receipt.SourceThreadID == w.LaunchedIn && receipt.State != "running" {
				// The destination snapshots the receipt under the lock that
				// signals the cancel, so a finished one ended on its own.
				if observeErr := a.observeRemoteCommand(w.ComputerID, w.RequestID, threadID, receipt); observeErr != nil {
					failed = append(failed, fmt.Errorf("remote command %s on %s: record its finish: %w", w.RequestID, w.ComputerID, observeErr))
				}
				continue
			}
		}
		if dismissErr := a.store.StopRemoteWatch(w.ComputerID, w.RequestID, keepFinished); dismissErr != nil {
			failed = append(failed, fmt.Errorf("remote command %s on %s: dismiss: %w", w.RequestID, w.ComputerID, dismissErr))
		}
	}
	if len(rows) > 0 {
		a.emitBackgroundChanged(threadID)
	}
	return errors.Join(cancels...), errors.Join(failed...)
}

// remoteCancelRetryDelay is how long an owed cancel waits before it is sent
// again.
const remoteCancelRetryDelay = 30 * time.Second

// retryOwedRemoteCancels sends again each owed cancel due by now. One the
// destination takes, or answers holding no such job of this computer's, is
// settled. One owed longer than the destination's owner grace is settled
// too: nothing here has polled the job since its cancel was first owed, so
// the destination has ended it on its own.
func (a *App) retryOwedRemoteCancels(now time.Time) {
	rows, err := a.store.ListOwedRemoteCancels(now.UnixMilli(), 32)
	if err != nil {
		log.Printf("remote command cancels: %v", err)
		return
	}
	for _, w := range rows {
		if now.Sub(time.UnixMilli(w.CancelOwedAt)) > remotejobs.DefaultOwnerGrace {
			log.Printf("remote command %s on %s: owed cancel settled, the destination has ended it after the owner grace", w.RequestID, w.ComputerID)
			a.settleOwedRemoteCancel(w)
			continue
		}
		// A computer no longer paired here is sent nothing: the end of its
		// pairing settles every cancel owed there.
		ended, err := a.endUnpaired(w.ComputerID)
		if ended {
			continue
		}
		if err == nil {
			call, cancel := context.WithTimeout(a.lifeCtx(), 20*time.Second)
			var receipt RemoteCommand
			err = a.backends.CallAgentPeer(call, w.ComputerID, "RemoteCommandCancel", &receipt, w.RequestID)
			cancel()
			code := ""
			if err != nil {
				code, _, _ = remoteErrorDetails("cancel", err)
			}
			if err == nil || code == "remote_job_not_found" || code == "remote_wrong_owner" {
				a.settleOwedRemoteCancel(w)
				continue
			}
			if a.unpairedAfterFailure(w.ComputerID) {
				continue
			}
		}
		log.Printf("remote command %s on %s: owed cancel not delivered, sent again later: %v", w.RequestID, w.ComputerID, err)
		if err := a.store.OweRemoteCancel(w.ComputerID, w.RequestID, w.CancelOwedAt, now.Add(remoteCancelRetryDelay).UnixMilli()); err != nil {
			log.Printf("remote command %s on %s: reschedule the owed cancel: %v", w.RequestID, w.ComputerID, err)
		}
	}
}

func (a *App) settleOwedRemoteCancel(w store.RemoteWatch) {
	if err := a.store.SettleRemoteCancel(w.ComputerID, w.RequestID); err != nil {
		log.Printf("remote command %s on %s: settle the owed cancel: %v", w.RequestID, w.ComputerID, err)
		return
	}
	// Its watch is checked again.
	a.nudgeRemoteWatches()
}

// remoteTrayToolName is the tray projection's tool name: the remote_run call
// the job came from, in the `MCP/<tool>` form both providers normalize to.
const remoteTrayToolName = "MCP/remote_run"

func (a *App) remoteTrayItems(threadID string, cutoff int64) ([]store.Item, error) {
	rows, err := a.store.ListRemoteWatches(threadID, 0, 256)
	if err != nil {
		return nil, err
	}
	out := []store.Item{}
	computers := a.remoteComputers()
	for _, w := range rows {
		if w.Notification == "dismissed" {
			continue
		}
		r := w.Receipt
		finished := r.ID != "" && r.State != "running"
		if finished && r.FinishedAt < cutoff {
			continue
		}
		issue := w.Error
		if issue == "" && r.State == store.RemoteOutcomeUnknown {
			// Why its outcome is unknown is all there is to say of it.
			issue = r.Error
		}
		// The row is a remote_run tool call as far as presentation goes:
		// `mcp` and `input` are what a live MCP row carries, with the
		// command pre-rendered, so the tray reads through the same table
		// as the transcript (frontend aoTools.ts). `remoteJob` is the
		// tray's own handle for Stop and the log.
		computerName := remoteWatchComputerName(computers, w)
		meta, _ := json.Marshal(map[string]any{
			"mcp":       map[string]string{"server": remoteMCPName, "tool": "remote_run"},
			"input":     map[string]any{"computer_id": w.ComputerID, "computer_name": computerName, "request_id": w.RequestID, "label": w.Label, "command": w.Command},
			"remoteJob": map[string]any{"computerId": w.ComputerID, "requestId": w.RequestID, "notification": w.Notification, "error": issue, "warning": r.Warning, "workspace": r.Workspace},
		})
		id := "remote-job:" + w.ComputerID + ":" + w.RequestID
		label := w.Label
		if computerName != "" {
			label = computerName + " · " + label
		}
		start := w.CreatedAt
		if r.StartedAt != 0 {
			start = r.StartedAt
		}
		// A job is background work once no tool call is parked on it. While
		// remote_run still waits, the transcript row is the running one and
		// this row reads as running too; the wait's end backgrounds both.
		backgrounded := !a.remoteWaitActive(w.ComputerID, w.RequestID)
		item := store.Item{ID: id, ThreadID: threadID, Kind: "tool_call", Role: "assistant", Status: "running", ToolName: remoteTrayToolName, Summary: label, CreatedAt: start, IsBackground: backgrounded, Meta: string(meta)}
		out = append(out, item)
		if finished {
			item.ID = id + ":done"
			item.CompletionOf = id
			item.Kind = "tool_completion"
			item.CreatedAt = r.FinishedAt
			item.Status = "completed"
			if r.State == "canceled" {
				item.Status = "killed"
			} else if r.State != "succeeded" {
				item.Status = "errored"
			}
			out = append(out, item)
		}
	}
	return out, nil
}
