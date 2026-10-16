package harnessrpc

import (
	"context"
	"encoding/json"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/keybindings"
	"agent-overflow/internal/notify"
	replaylog "agent-overflow/internal/observability/replay"
	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
)

// ThreadOptions is the harness-owned projection of the production thread
// creation input. The root adapter translates it explicitly so this package
// does not depend on package main.
type ThreadOptions struct {
	ProjectID   string
	Title       string
	Provider    string
	Model       string
	Mode        string
	RuntimeMode string
}

// Host is the narrow set of production application capabilities the harness
// must exercise. Persistence owned entirely by the harness is supplied through
// Config.Store; these methods are the operations whose production behavior
// must not be reimplemented here.
type Host interface {
	Store() *store.Store
	ReplayLog() *replaylog.Manager
	Shutdown(context.Context) error
	ExpectedOrigin() string
	ConnectedClients() (count int, available bool)
	SessionEnv(threadID string) map[string]string
	ListVisibleThreads() ([]store.Thread, error)
	Emit(eventchan.Channel, any)
	Notify(title, body string, target notify.Target) error
	// BrowserPressKey delivers one key chord to a browser page's NATIVE view
	// through the window's own event path, as if the user had clicked into
	// the page and typed it. Refused on engines with no native view.
	BrowserPressKey(threadID, pageID string, chord keybindings.Accelerator) error
	// BrowserScroll and BrowserScreenshot drive the production browser page
	// directly so native-engine rendering can be checked without a provider
	// turn or OS-level input synthesis.
	BrowserScroll(threadID, pageID string, x, y float64) error
	BrowserScreenshot(threadID, pageID string) ([]byte, error)
	CreateProject(path string) (store.Project, error)
	CreateThread(ThreadOptions) (store.Thread, error)
	ArchiveThread(threadID string) error
	// MakeAssistant makes a thread an assistant's current thread, with its
	// record and home, as New Assistant does, without the opening message.
	MakeAssistant(threadID string) (store.StandingAgent, error)
	// BroadcastThreadRow re-reads one thread and emits it to connected
	// clients as a full row. The seeder writes history behind the store,
	// which changes derived row columns (IsDraft) no store write announces.
	BroadcastThreadRow(threadID string)
	StopSession(threadID string) error
	// AwaitSessionEndWork waits for the work stopped sessions set off (a
	// request a stop settled, the wake that answer owes another thread, and
	// the session that wake starts) and returns the threads with a live
	// session afterwards.
	AwaitSessionEndWork() []string
	DeleteProject(projectID string) error
	RecoverCrashedTurns() error
	RecoverOrphanedBackgroundTasks() error
	ClearUIState() error
	ResetSessionImporter()
	// PauseMergedPRCleanup stops the merged pull request cleanup, joins it
	// and drops its in-memory state; resume starts it again from nothing.
	PauseMergedPRCleanup() (resume func())
	// DropRetainedEvents empties the event bus replay rings so nothing
	// emitted before a reset is replayable by the next test's page.
	DropRetainedEvents()
	HasWorkflowEngine() bool
	RequireWorkflowEngine() error
	ResolveProjectWorkflow(projectID, workflowID string) error
	StartWorkflowRun(projectID, workflowID, goal string, seeds json.RawMessage, stepMode bool) (store.WorkItem, error)
	SubscribeEvents() (*transport.Subscriber, error)
	// HoldWorkflowStarts pauses the workflow engine in memory only, for the
	// span of a reset. ReleaseWorkflowStarts unpauses it and clears a pause
	// a spec persisted; the settings file is written only in that case.
	HoldWorkflowStarts() error
	ReleaseWorkflowStarts() error
	CancelWorkflowItem(context.Context, string) error
	SyncWorkflowEngine() error
	PublishStoreIdentity(store.Identity)
	SendMessage(threadID, message string) error
	// PushSent answers what the harness push recorder has been handed, and
	// ForgetPushSent drops it. Both answer harmlessly on a boot with no
	// recorder, which is every shipping one.
	PushSent() []PushMessage
	ForgetPushSent()
	// UpdateWorkReason is the production update admission check: what a
	// service update or restart would wait for now, "" when nothing.
	UpdateWorkReason() (string, error)
	// StallProviderEvents queues a handler that holds the thread's
	// provider event worker until ReleaseProviderEvents, and
	// PendingProviderEvents counts the thread's queued events not handled
	// yet, the stall included.
	StallProviderEvents(threadID string) error
	ReleaseProviderEvents(threadID string) error
	PendingProviderEvents(threadID string) int
}
