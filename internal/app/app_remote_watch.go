package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/keyedlock"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/transport"
)

func (a *App) remoteStartLocks() *keyedlock.Registry {
	a.remoteStartsOnce.Do(func() { a.remoteStarts = keyedlock.New() })
	return a.remoteStarts
}

// Every successful command RPC learns the destination's receipt immediately.
// The watcher still owns durable completion delivery; returning a tool result
// is not an acknowledgement that the provider received it.
func (a *App) observeRemoteCommand(computerID, requestID, threadID string, receipt RemoteCommand) error {
	if receipt.ID != requestID || receipt.SourceThreadID != threadID {
		return errorsx.Public("remote_wrong_conversation", "The destination returned a command receipt for another request or conversation. Use the original request ID from the conversation that submitted it.", nil)
	}
	w, err := a.store.GetRemoteWatch(computerID, requestID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // Receipts from work predating source watches remain readable.
	}
	if err != nil {
		return err
	}
	if w.ThreadID != threadID {
		return errorsx.Public("remote_wrong_conversation", "This command belongs to another conversation. Read or cancel it from the conversation that submitted it.", nil)
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
	a.emit(eventchan.ProviderBackgroundTasksChanged, map[string]any{"threadId": threadID})
	return nil
}

func (a *App) registerRemoteWatch(input AgentRemoteRequest) (bool, error) {
	label, err := remoteJobLabel(input.Label, input.Request)
	if err != nil {
		return false, err
	}
	// Forgetting a computer must not discard the only cancellation handle.
	unlockComputer := a.remoteStartLocks().Lock("computer:" + input.ComputerID)
	defer unlockComputer()
	enabled, err := a.backends.AgentAccess()
	if err != nil {
		return false, err
	}
	if !enabled[input.ComputerID] {
		return false, errorsx.Public("remote_access_disabled", "Agent commands are not enabled for this computer. Enable it in Remote access → Agent access.", nil)
	}
	// Admission and final deletion/transfer share the short mutation fence.
	// A destination must never accept a job after its source has disappeared.
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
	return a.store.RegisterRemoteWatch(store.RemoteWatch{ComputerID: input.ComputerID, RequestID: input.Request.ID, ThreadID: input.Request.SourceThreadID, Fingerprint: hex.EncodeToString(digest[:]), Label: label})
}

const remoteJobLabelMaxRunes = 120

func remoteJobLabel(label string, request RemoteCommandRequest) (string, error) {
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
	argv := request.Argv
	if request.Script != "" {
		argv = request.Interpreter
	}
	var command strings.Builder
	for i, arg := range argv {
		if i > 0 {
			command.WriteByte(' ')
		}
		// Preserve argument boundaries without suggesting the display is an
		// executable shell string. Quoting also escapes multiline script argv.
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
	label = command.String()
	if utf8.RuneCountInString(label) > remoteJobLabelMaxRunes {
		n := 0
		for offset := range label {
			if n == remoteJobLabelMaxRunes-1 {
				label = label[:offset] + "…"
				break
			}
			n++
		}
	}
	return label, nil
}

func (a *App) startRemoteWatches() {
	if a.backends == nil {
		return
	}
	a.remoteWatchWG.Add(1)
	go func() {
		defer a.remoteWatchWG.Done()
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-a.lifeCtx().Done():
				return
			case <-ticker.C:
			}
			rows, err := a.store.ListRemoteWatches("", time.Now().UnixMilli(), 32)
			if err != nil {
				log.Printf("remote completion watches: %v", err)
				continue
			}
			var wg sync.WaitGroup
			limit := make(chan struct{}, 4)
			for _, w := range rows {
				select {
				case limit <- struct{}{}:
				case <-a.lifeCtx().Done():
					wg.Wait()
					return
				}
				wg.Add(1)
				go func() { defer wg.Done(); defer func() { <-limit }(); a.checkRemoteWatch(w) }()
			}
			wg.Wait()
		}
	}()
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
	thread, err := a.store.GetThread(w.ThreadID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			_ = a.store.DismissRemoteWatch(w.ComputerID, w.RequestID)
		}
		return
	}

	// A moved conversation retains its receipts but cannot be driven
	// from the old owner. Do not turn a completion into a workflow takeover.
	if err = a.store.CheckThreadExecutionAccess(thread); err != nil {
		_ = a.store.ObserveRemoteWatch(w.ComputerID, w.RequestID, w.Receipt, "Completion is waiting for this conversation’s execution ownership.", time.Now().Add(30*time.Second).UnixMilli())
		return
	}
	ctx, cancel := context.WithTimeout(a.lifeCtx(), 10*time.Second)
	defer cancel()
	receipt := w.Receipt
	outputUnavailable := receipt.ID != "" && receipt.State != "running"
	if !outputUnavailable {
		err = a.backends.CallAgentPeer(ctx, w.ComputerID, "RemoteCommandStatus", &receipt, w.RequestID)
	}
	issue := ""
	if err != nil {
		issue = remoteErrorText(remoteOperationError("status", w.ComputerID, w.RequestID, err))
		receipt = w.Receipt
	} else if receipt.ID != w.RequestID || receipt.SourceThreadID != w.ThreadID {
		issue = "The destination returned a receipt for a different conversation. Completion delivery is paused."
		receipt = w.Receipt
	}
	next := time.Now().Add(5 * time.Second).UnixMilli()
	if issue != "" {
		next = time.Now().Add(30 * time.Second).UnixMilli()
	}
	if err = a.store.ObserveRemoteWatch(w.ComputerID, w.RequestID, receipt, issue, next); err != nil {
		log.Printf("remote job observation: %v", err)
		return
	}
	if receipt.State != w.Receipt.State || issue != w.Error {
		a.emit(eventchan.ProviderBackgroundTasksChanged, map[string]any{"threadId": w.ThreadID})
	}
	if issue != "" || receipt.ID == "" || receipt.State == "running" {
		return
	}
	w.Receipt = receipt
	if err = a.deliverRemoteCompletion(w, outputUnavailable); err != nil {
		_ = a.store.ObserveRemoteWatch(w.ComputerID, w.RequestID, receipt, "Completion could not enter the message queue: "+remoteErrorText(err), next)
	}
}

// Serialize admission and optional lazy start against archive/transfer/stop,
// using the same action→mutation lock order as ordinary sends. Never hold a
// thread lock while waiting on the destination network.
func (a *App) deliverRemoteCompletion(w store.RemoteWatch, outputUnavailable bool) error {
	unlock, err := a.threadLocks().LockCtx(a.lifeCtx(), w.ThreadID)
	if err != nil {
		return err
	}
	defer unlock()
	current, err := a.store.GetRemoteWatch(w.ComputerID, w.RequestID)
	if err != nil {
		return err
	}
	if current.Notification != "pending" {
		return nil
	}
	// A direct status/cancel can finish while this watcher is in flight. The
	// durable receipt wins; only reuse this response's unretained output when
	// it describes that same receipt.
	metadata := w.Receipt
	metadata.Output = ""
	if metadata == current.Receipt {
		current.Receipt.Output = w.Receipt.Output
	} else {
		outputUnavailable = true
	}
	w = current
	if w.Receipt.ID == "" || w.Receipt.State == "running" {
		return nil
	}
	thread, err := a.store.GetThread(w.ThreadID)
	if err != nil {
		return err
	}
	if thread.Archived {
		return a.store.DismissRemoteWatch(w.ComputerID, w.RequestID)
	}
	if err = a.store.CheckThreadExecutionAccess(thread); err != nil {
		return err
	}
	if thread.Mode == threadmode.ModeWorkflow {
		// A workflow owns its phase lifetime. Finishing a remote job cannot reopen
		// a completed phase or launch a phase whose session the engine closed.
		_, live := a.sessionManager().get(thread.ID)
		unit, found, e := a.store.GetWorkItemUnitByThread(thread.ID)
		if e != nil {
			return e
		}
		itemID := unit.ItemID
		running := found && unit.Status == store.WorkItemUnitRunning
		if !found {
			phase, exists, e := a.store.GetWorkItemPhaseByThread(thread.ID)
			if e != nil {
				return e
			}
			itemID = phase.ItemID
			running = exists && phase.Status == "running"
		}
		if running {
			item, e := a.store.GetWorkItem(itemID)
			if e != nil {
				return e
			}
			running = item.State == "running"
		}
		if !running || !live {
			return a.store.DismissRemoteWatch(w.ComputerID, w.RequestID)
		}
	}
	if err = a.queueRemoteCompletion(w, outputUnavailable); err != nil {
		return err
	}
	if _, live := a.sessionManager().get(w.ThreadID); !live {
		// Admission already handed the message to the ordinary durable queue.
		// Startup's normal flush trigger dispatches it. Errors retain that queue.
		if err = a.startSession(a.lifeCtx(), w.ThreadID); err != nil {
			a.emitWireErrorToThread(w.ThreadID, "Remote job finished; its completion is queued, but the agent could not start: "+remoteErrorText(err))
		}
	}
	return nil
}

func (a *App) queueRemoteCompletion(w store.RemoteWatch, outputUnavailable bool) error {
	message := remoteCompletionMessage(w, a.remoteComputerNames()[w.ComputerID], outputUnavailable)
	_, err := a.registerQueueItem(w.ThreadID, message, SendMessageOptions{SendID: remoteCompletionSendID(w)}, injectedQueueOptions{
		preserveDraft: true,
		persist: func(item store.FlushQueueItem) error {
			return a.store.QueueRemoteCompletion(w.ComputerID, w.RequestID, item)
		},
	})
	return err
}

func remoteCompletionSendID(w store.RemoteWatch) string {
	return "remote-completion:" + w.ComputerID + ":" + w.RequestID
}

//ao:scope threads:read
func (a *App) ListThreadRemoteCommands(threadID string) ([]store.RemoteWatch, error) {
	return a.store.ListRemoteWatches(threadID, 0, 256)
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
	call, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var receipt RemoteCommand
	err = a.backends.CallAgentPeer(call, computerID, "RemoteCommandCancel", &receipt, requestID)
	if err == nil {
		err = a.observeRemoteCommand(computerID, requestID, threadID, receipt)
	}
	return receipt, remoteOperationError("cancel", computerID, requestID, err)
}

func (a *App) remoteTrayItems(threadID string, cutoff int64) ([]store.Item, error) {
	rows, err := a.store.ListRemoteWatches(threadID, 0, 256)
	if err != nil {
		return nil, err
	}
	out := []store.Item{}
	names := a.remoteComputerNames()
	for _, w := range rows {
		if w.Notification == "dismissed" {
			continue
		}
		r := w.Receipt
		finished := r.ID != "" && r.State != "running"
		if finished && r.FinishedAt < cutoff {
			continue
		}
		meta, _ := json.Marshal(map[string]any{"remoteJob": map[string]any{"computerId": w.ComputerID, "requestId": w.RequestID, "notification": w.Notification, "error": w.Error, "workspace": r.Workspace}})
		id := "remote-job:" + w.ComputerID + ":" + w.RequestID
		label := w.Label
		if name := names[w.ComputerID]; name != "" {
			label = name + " · " + label
		}
		start := w.CreatedAt
		if r.StartedAt != 0 {
			start = r.StartedAt
		}
		item := store.Item{ID: id, ThreadID: threadID, Kind: "tool_call", Role: "assistant", Status: "running", ToolName: "remote_command", Summary: label, CreatedAt: start, IsBackground: true, Meta: string(meta)}
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
