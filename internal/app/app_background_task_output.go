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
// background command.
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

	snapshot, err := a.backgroundTaskTailer().Watch(threadID, launchItemID, path)
	if err != nil {
		if errors.Is(err, tasktail.ErrTooManyWatches) {
			return tasktail.Snapshot{}, ErrTooManyBackgroundTaskWatches
		}
		return tasktail.Snapshot{}, err
	}
	if state := transport.ConnStateFromContext(ctx); state != nil {
		// A false return means the connection is already tearing down:
		// the safety net it would have provided is gone, so release now.
		if !state.BindCleanup(backgroundTaskOutputCleanupKey(snapshot.WatchID), func() {
			a.backgroundTaskTailer().Release(snapshot.WatchID)
		}) {
			a.backgroundTaskTailer().Release(snapshot.WatchID)
			return tasktail.Snapshot{}, fmt.Errorf("background task output: connection closing")
		}
	}
	return snapshot, nil
}

// UnwatchBackgroundTaskOutput releases a watch WatchBackgroundTaskOutput
// answered with. Idempotent: the connection cleanup and the task's end
// may both have released it already.
//
//ao:scope threads:read
func (a *App) UnwatchBackgroundTaskOutput(ctx context.Context, threadID, watchID string) error {
	watchID = strings.TrimSpace(watchID)
	if watchID == "" {
		return nil
	}
	transport.ConnStateFromContext(ctx).UnbindCleanup(backgroundTaskOutputCleanupKey(watchID))
	a.backgroundTaskTailer().Release(watchID)
	return nil
}

func backgroundTaskOutputCleanupKey(watchID string) string {
	return "background-task-output:" + watchID
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
