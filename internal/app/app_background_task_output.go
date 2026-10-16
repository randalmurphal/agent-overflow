package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/tasktail"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/triage"
)

// The live tail of a running background command's output, for the tray
// row a client expanded (docs/specs/agent-visibility.md §Background tray).
//
// Claude writes a backgrounded Bash or Monitor command's output to a file
// the launch row names (triage.BackgroundTaskOutputFile). A watch follows
// that file through one tasktail.Tailer shared by every connection
// (internal/tasktail), which answers with the tail as it stands and then
// streams what the file gains as provider:background_task_output frames
// until the watch is released, the connection drops, or the task ends.
// The task's end is read off the tray delta the completion sibling
// announces (noteBackgroundTrayForTail), the same frame that takes the
// row's running state away on the client.
//
// A watch reads only a running background COMMAND's file. An agent's
// output_file is its transcript, which the app never reads (decisions.md,
// 2026-09-23); a Codex thread's running commands have no file (their
// output is a wire stream); and a row that already ended has its output
// on its completion row.

// Errors a watch refuses with. Each names the row's state, so the client
// shows why instead of an empty body.
var (
	ErrBackgroundTaskOutputProvider   = errors.New("background task output: the thread's provider has no task output file to tail")
	ErrBackgroundTaskOutputLaunch     = errors.New("background task output: no such background command launch")
	ErrBackgroundTaskOutputAgent      = errors.New("background task output: an agent's output file is its transcript and is not tailed")
	ErrBackgroundTaskOutputNotRunning = errors.New("background task output: the launch is not a running background command")
	ErrBackgroundTaskOutputNoFile     = errors.New("background task output: the launch names no output file")
	ErrTooManyBackgroundTaskWatches   = errors.New("background task output: too many output watches")
)

type appBackgroundTaskOutputState struct {
	once   sync.Once
	tailer atomic.Pointer[tasktail.Tailer]

	// bindings is the app's own record of every watch bound to a
	// connection cleanup: which connection, and which thread the watch
	// was opened on. The tailer lets go of a watch on its own when the
	// task ends or its read fails, so an Unwatch that follows gets no
	// release from it; the binding is unbound from this record, by the
	// caller's thread, and from the tailer's Dropped report, so a
	// long-lived connection does not keep a cleanup per task it watched.
	// The record is made, under the app's own id, before the tailer is
	// asked for the watch, so a drop always finds it.
	mu       sync.Mutex
	bindings map[string]backgroundTaskOutputBinding

	// afterAcquire, when set by a test, runs once the tailer holds the
	// watch and before the method reads its record back.
	afterAcquire func(watchID string)
}

// has reports whether watchID's record still stands.
func (s *appBackgroundTaskOutputState) has(watchID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.bindings[watchID]
	return ok
}

type backgroundTaskOutputBinding struct {
	threadID string
	state    *transport.ConnState
}

// bindWatch records a watch bound to state's cleanup.
func (s *appBackgroundTaskOutputState) bindWatch(watchID, threadID string, state *transport.ConnState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.bindings == nil {
		s.bindings = make(map[string]backgroundTaskOutputBinding)
	}
	s.bindings[watchID] = backgroundTaskOutputBinding{threadID: threadID, state: state}
}

// unbindWatch drops the record of watchID and unbinds its cleanup, when
// the record exists and, if a caller is named, was made by that caller
// for threadID. It reports whether the record was dropped.
func (s *appBackgroundTaskOutputState) unbindWatch(watchID, threadID string, caller *transport.ConnState) bool {
	s.mu.Lock()
	binding, ok := s.bindings[watchID]
	if ok && caller != nil && (binding.state != caller || binding.threadID != threadID) {
		ok = false
	}
	if ok {
		delete(s.bindings, watchID)
	}
	s.mu.Unlock()
	if ok {
		binding.state.UnbindCleanup(backgroundTaskOutputCleanupKey(watchID))
	}
	return ok
}

// backgroundTaskTailer builds the tailer on first use. Its frames leave
// through a.emit like every other event.
func (a *App) backgroundTaskTailer() *tasktail.Tailer {
	a.taskOutput.once.Do(func() {
		a.taskOutput.tailer.Store(tasktail.New(tasktail.Config{
			Emit: func(evt tasktail.Event) {
				a.emit(eventchan.ProviderBackgroundTaskOutput, evt)
			},
			Resolve: triage.ResolveClaudeTaskOutputPath,
			// A watch the tailer let go of (task ended, read failed) has
			// nothing left for its connection cleanup to release.
			Dropped: func(watchIDs []string) {
				for _, watchID := range watchIDs {
					a.taskOutput.unbindWatch(watchID, "", nil)
				}
			},
		}))
	})
	return a.taskOutput.tailer.Load()
}

// closeBackgroundTaskTailer stops the follows and joins the poll loop. A
// tailer never built needs nothing.
func (a *App) closeBackgroundTaskTailer() {
	a.taskOutput.once.Do(func() {})
	if tailer := a.taskOutput.tailer.Load(); tailer != nil {
		tailer.Close()
	}
}

// WatchBackgroundTaskOutput follows the output file of a running Claude
// background command launched by launchItemID and answers with its tail
// as it stands. Later growth arrives as provider:background_task_output
// frames whose seq continues the snapshot's; a seq the client did not
// expect means a missed frame, and the client watches again. The watch
// ends with UnwatchBackgroundTaskOutput, the connection, or the task: the
// last frame of a task that ended carries ended:true. A task that already
// ended answers ended:true with no watch; its output is on its completion
// row.
//
// It rides threads:read, which also serves the tray (ListLiveBackgroundTasks)
// and the completion payload the same bytes land in. Refused for an agent
// launch, whose file is a transcript, and for anything but a running
// background command. The watch belongs to the calling connection: only
// that connection's UnwatchBackgroundTaskOutput releases it.
//
//ao:scope threads:read
func (a *App) WatchBackgroundTaskOutput(ctx context.Context, threadID, launchItemID string) (tasktail.Snapshot, error) {
	if a.shuttingDown.Load() {
		return tasktail.Snapshot{}, ErrShuttingDown
	}
	launchItemID = strings.TrimSpace(launchItemID)
	if launchItemID == "" {
		return tasktail.Snapshot{}, ErrBackgroundTaskOutputLaunch
	}
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return tasktail.Snapshot{}, fmt.Errorf("background task output: %w", err)
	}
	if thread.Provider != string(provider.Claude) {
		return tasktail.Snapshot{}, ErrBackgroundTaskOutputProvider
	}
	launch, found, err := a.store.GetThreadItem(threadID, launchItemID)
	if err != nil {
		return tasktail.Snapshot{}, fmt.Errorf("background task output: %w", err)
	}
	if !found || launch.Kind != "tool_call" {
		return tasktail.Snapshot{}, ErrBackgroundTaskOutputLaunch
	}
	if store.IsAgentTranscriptLaunch(launch) {
		return tasktail.Snapshot{}, ErrBackgroundTaskOutputAgent
	}
	if !launch.IsBackground || launch.Status != "running" {
		return tasktail.Snapshot{}, ErrBackgroundTaskOutputNotRunning
	}
	path := triage.BackgroundTaskOutputFile(launch)
	if path == "" && triage.LaunchIsWatchTask(launch) {
		// A Monitor's launch ack names no file (claude-wire.md §E7); its
		// notifications do, and the launch row is history that no later
		// event rewrites, so the file is read off the newest one here.
		note, found, err := a.store.FindNotificationItemByTaskID(threadID, triage.TaskIDFromItemMeta(launch.Meta))
		if err != nil {
			return tasktail.Snapshot{}, fmt.Errorf("background task output: %w", err)
		}
		if found {
			path = triage.BackgroundTaskOutputFile(note)
		}
	}
	if path == "" {
		return tasktail.Snapshot{}, ErrBackgroundTaskOutputNoFile
	}
	// A background launch keeps status running as immutable history; its
	// completion sibling is what says it ended.
	if _, ended, err := a.store.GetThreadItem(threadID, triage.ToolCompletionID(launchItemID)); err != nil {
		return tasktail.Snapshot{}, fmt.Errorf("background task output: %w", err)
	} else if ended {
		return tasktail.Snapshot{Ended: true}, nil
	}

	state := transport.ConnStateFromContext(ctx)
	owner := backgroundTaskOutputOwner(state)
	watchID, err := tasktail.NewWatchID()
	if err != nil {
		return tasktail.Snapshot{}, fmt.Errorf("background task output: %w", err)
	}
	tailer := a.backgroundTaskTailer()
	if state != nil {
		// The record and the cleanup come first, under an id the tailer
		// does not know yet: a drop the tailer reports (the task ending
		// under the seed read) then always finds the record to unbind. A
		// false return means the connection is already tearing down.
		if !state.BindCleanup(backgroundTaskOutputCleanupKey(watchID), func() {
			a.taskOutput.unbindWatch(watchID, "", nil)
			tailer.Release(threadID, watchID, owner)
		}) {
			return tasktail.Snapshot{}, fmt.Errorf("background task output: connection closing")
		}
		a.taskOutput.bindWatch(watchID, threadID, state)
	}
	snapshot, err := tailer.Watch(threadID, launchItemID, path, owner, watchID)
	if err != nil {
		a.taskOutput.unbindWatch(watchID, "", nil)
		if errors.Is(err, tasktail.ErrTooManyWatches) {
			return tasktail.Snapshot{}, ErrTooManyBackgroundTaskWatches
		}
		return tasktail.Snapshot{}, err
	}
	if hook := a.taskOutput.afterAcquire; hook != nil {
		hook(watchID)
	}
	if snapshot.WatchID == "" {
		// The seed read failed: the tailer holds nothing for the id.
		a.taskOutput.unbindWatch(watchID, "", nil)
		return snapshot, nil
	}
	if state != nil && !a.taskOutput.has(watchID) {
		// The record went while the watch was being opened: the task
		// ended (the tailer dropped the watch and said so) or the
		// connection did (its cleanup ran). Either way nothing holds the
		// watch now, and the tail answers as ended.
		tailer.Release(threadID, watchID, owner)
		snapshot.WatchID = ""
		snapshot.Ended = true
	}
	return snapshot, nil
}

// UnwatchBackgroundTaskOutput releases a watch WatchBackgroundTaskOutput
// answered this connection with. Idempotent: the connection cleanup and
// the task's end may both have released it already. A watch another
// connection holds, or one on another thread, is left alone.
//
//ao:scope threads:read
func (a *App) UnwatchBackgroundTaskOutput(ctx context.Context, threadID, watchID string) error {
	watchID = strings.TrimSpace(watchID)
	if watchID == "" {
		return nil
	}
	state := transport.ConnStateFromContext(ctx)
	// The binding is this connection's record of the watch, dropped only
	// by the connection that made it, for the thread it was made on: a
	// release that cannot land (wrong thread) leaves the watch and its
	// cleanup, and the connection's end still releases it. The tailer's
	// own release may already have nothing to do: a task that ended took
	// its watches with it, and the binding still goes.
	if state != nil {
		a.taskOutput.unbindWatch(watchID, threadID, state)
	}
	a.backgroundTaskTailer().Release(threadID, watchID, backgroundTaskOutputOwner(state))
	return nil
}

func backgroundTaskOutputCleanupKey(watchID string) string {
	return "background-task-output:" + watchID
}

// backgroundTaskOutputOwner names the connection a watch belongs to. The
// connection state is server-owned, so its identity cannot be declared by
// a peer; an in-process caller has none and shares the empty owner.
func backgroundTaskOutputOwner(state *transport.ConnState) string {
	if state == nil {
		return ""
	}
	return fmt.Sprintf("conn:%p", state)
}

// noteBackgroundTrayForTail reads a tray delta for the launches it says
// ended: a named launch with a completion row, or with no running launch
// row at all, has left the running set, and its follow reads the file
// once more and ends. A refresh frame names nothing. Only a tailer that
// was built is consulted; a thread with no watch costs nothing here.
func (a *App) noteBackgroundTrayForTail(data any) {
	tailer := a.taskOutput.tailer.Load()
	if tailer == nil {
		return
	}
	evt, ok := data.(triage.BackgroundTrayEvent)
	if !ok || evt.Refresh || len(evt.LaunchIDs) == 0 {
		return
	}
	running := make(map[string]bool, len(evt.Rows))
	ended := make(map[string]bool)
	for _, row := range evt.Rows {
		switch {
		case row.CompletionOf != "":
			ended[row.CompletionOf] = true
		case row.Kind == "tool_call" && row.Status == "running" && row.IsBackground:
			running[row.ID] = true
		}
	}
	for _, id := range evt.LaunchIDs {
		if ended[id] || !running[id] {
			tailer.EndLaunch(evt.ThreadID, id)
		}
	}
}
