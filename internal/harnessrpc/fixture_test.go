package harnessrpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/keybindings"
	"agent-overflow/internal/notify"
	replaylog "agent-overflow/internal/observability/replay"
	"agent-overflow/internal/store"
	"agent-overflow/internal/store/storetest"
	"agent-overflow/internal/transport"

	"github.com/google/uuid"
)

func TestMain(m *testing.M) { os.Exit(storetest.Run(m)) }

type testHost struct {
	store             *store.Store
	replay            *replaylog.Manager
	emit              func(eventchan.Channel, any)
	eventBus          *transport.EventBus
	origin            string
	published         *store.Identity
	sent              []string
	browserScroll     func(string, string, float64, float64) error
	browserScreenshot func(string, string) ([]byte, error)
	pushSent          []PushMessage
}

func newHarnessTestHost(t *testing.T) (*Harness, *testHost) {
	t.Helper()
	root := t.TempDir()
	dataDir := filepath.Join(root, "agent-overflow")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	host := &testHost{store: storetest.Clone(t)}
	host.replay = replaylog.NewManager(replaylog.ManagerConfig{
		RootDir: filepath.Join(dataDir, "replay"),
		Enabled: true,
	})
	t.Cleanup(func() { host.replay.SetEnabled(false) })
	return New(Config{
		Host:     host,
		Version:  "test-version",
		DataRoot: root,
		DataDir:  dataDir,
	}), host
}

func TestHarnessResolvesStoreAfterConstruction(t *testing.T) {
	host := &testHost{}
	receiver := New(Config{Host: host})
	if _, err := receiver.HarnessListThreadRows(); err == nil {
		t.Fatal("HarnessListThreadRows succeeded before App-style store initialization")
	}
	host.store = storetest.Clone(t)
	rows, err := receiver.HarnessListThreadRows()
	if err != nil || len(rows) != 0 {
		t.Fatalf("HarnessListThreadRows after store initialization = %+v, %v", rows, err)
	}
}

func (h *testHost) Store() *store.Store            { return h.store }
func (h *testHost) ReplayLog() *replaylog.Manager  { return h.replay }
func (h *testHost) Shutdown(context.Context) error { return nil }
func (h *testHost) ExpectedOrigin() string         { return h.origin }
func (h *testHost) ConnectedClients() (int, bool) {
	if h.eventBus == nil {
		return 0, false
	}
	return h.eventBus.SubscriberCount(), true
}
func (h *testHost) SessionEnv(string) map[string]string { return nil }
func (h *testHost) ListVisibleThreads() ([]store.Thread, error) {
	return h.store.ListThreads()
}
func (h *testHost) Emit(channel eventchan.Channel, data any) {
	if h.emit != nil {
		h.emit(channel, data)
	}
}
func (h *testHost) Notify(string, string, notify.Target) error { return nil }

// The fixture never installs a push recorder, which is what a boot with no
// push configured looks like.
func (h *testHost) PushSent() []PushMessage { return h.pushSent }
func (h *testHost) ForgetPushSent()         { h.pushSent = nil }
func (h *testHost) BrowserPressKey(string, string, keybindings.Accelerator) error {
	return errors.New("no browser engine in the fixture")
}
func (h *testHost) BrowserScroll(threadID, pageID string, x, y float64) error {
	if h.browserScroll == nil {
		return errors.New("no browser engine in the fixture")
	}
	return h.browserScroll(threadID, pageID, x, y)
}
func (h *testHost) BrowserScreenshot(threadID, pageID string) ([]byte, error) {
	if h.browserScreenshot == nil {
		return nil, errors.New("no browser engine in the fixture")
	}
	return h.browserScreenshot(threadID, pageID)
}
func (h *testHost) CreateProject(path string) (store.Project, error) {
	now := time.Now().UnixMilli()
	project := store.Project{ID: uuid.NewString(), Path: path, Name: filepath.Base(path), CreatedAt: now, UpdatedAt: now}
	return h.store.CreateProject(project)
}
func (h *testHost) CreateThread(options ThreadOptions) (store.Thread, error) {
	now := time.Now().UnixMilli()
	thread := store.Thread{
		ID: uuid.NewString(), ProjectID: options.ProjectID, Title: options.Title,
		Provider: options.Provider, Model: options.Model, Mode: options.Mode,
		RuntimeMode: options.RuntimeMode, CreatedAt: now, UpdatedAt: now,
	}
	project, err := h.store.GetProject(options.ProjectID)
	if err != nil {
		return store.Thread{}, err
	}
	thread.WorkspacePath = project.Path
	return thread, h.store.CreateThread(thread)
}
func (h *testHost) ArchiveThread(threadID string) error {
	_, _, err := h.store.ArchiveThread(threadID)
	return err
}
func (h *testHost) StopSession(string) error              { return nil }
func (h *testHost) DeleteProject(projectID string) error  { return h.store.DeleteProject(projectID) }
func (h *testHost) RecoverCrashedTurns() error            { return nil }
func (h *testHost) RecoverOrphanedBackgroundTasks() error { return nil }
func (h *testHost) ClearUIState() error {
	if h.store == nil {
		return nil
	}
	return h.store.ClearUIState()
}
func (h *testHost) ResetSessionImporter()        {}
func (h *testHost) HasWorkflowEngine() bool      { return false }
func (h *testHost) RequireWorkflowEngine() error { return fmt.Errorf("workflow engine unavailable") }
func (h *testHost) ResolveProjectWorkflow(string, string) error {
	return fmt.Errorf("workflow engine unavailable")
}
func (h *testHost) StartWorkflowRun(string, string, string, json.RawMessage, bool) (store.WorkItem, error) {
	return store.WorkItem{}, fmt.Errorf("workflow engine unavailable")
}
func (h *testHost) SubscribeEvents() (*transport.Subscriber, error) {
	return nil, fmt.Errorf("event bus unavailable")
}
func (h *testHost) SetWorkflowGlobalPause(bool) error                { return nil }
func (h *testHost) CancelWorkflowItem(context.Context, string) error { return nil }
func (h *testHost) SyncWorkflowEngine() error                        { return nil }
func (h *testHost) PublishStoreIdentity(identity store.Identity)     { h.published = &identity }
func (h *testHost) SendMessage(threadID, message string) error {
	h.sent = append(h.sent, threadID+":"+message)
	return nil
}

func seedHarnessThread(t *testing.T, database *store.Store, threadID string) {
	t.Helper()
	now := time.Now().UnixMilli()
	if _, err := database.CreateProject(store.Project{
		ID: "proj-" + threadID, Path: filepath.Join(t.TempDir(), "ws"), Name: "p",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if err := database.CreateThread(store.Thread{
		ID: threadID, ProjectID: "proj-" + threadID, Title: "t",
		Provider: "claude", Model: "claude-opus-4-7", Mode: "chat",
		WorkspacePath: "/tmp/work", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
}

var _ Host = (*testHost)(nil)
