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
	CreateProject(path string) (store.Project, error)
	CreateThread(ThreadOptions) (store.Thread, error)
	ArchiveThread(threadID string) error
	StopSession(threadID string) error
	DeleteProject(projectID string) error
	RecoverCrashedTurns() error
	RecoverOrphanedBackgroundTasks() error
	ClearUIState() error
	ResetSessionImporter()
	HasWorkflowEngine() bool
	RequireWorkflowEngine() error
	ResolveProjectWorkflow(projectID, workflowID string) error
	StartWorkflowRun(projectID, workflowID, goal string, seeds json.RawMessage, stepMode bool) (store.WorkItem, error)
	SubscribeEvents() (*transport.Subscriber, error)
	SetWorkflowGlobalPause(paused bool) error
	CancelWorkflowItem(context.Context, string) error
	SyncWorkflowEngine() error
	PublishStoreIdentity(store.Identity)
	SendMessage(threadID, message string) error
}
