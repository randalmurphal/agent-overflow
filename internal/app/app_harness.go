// app_harness.go wires the isolated harness RPC receiver to the live App.
// The receiver implementation lives in internal/harnessrpc; transport boot
// still registers it explicitly as main.Harness and LocalOnly.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	appbrowser "agent-overflow/internal/browser"
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/harnessrpc"
	"agent-overflow/internal/keybindings"
	"agent-overflow/internal/notify"
	replaylog "agent-overflow/internal/observability/replay"
	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/workflow/def"
)

type harnessHost struct {
	app     *App
	dataDir string
}

// HarnessPaths is the filesystem/configuration snapshot prepared by the
// executable's harness or soak boot path.
type HarnessPaths struct {
	DataRoot        string
	DataDir         string
	HomeDir         string
	CredentialHome  string
	MockProvider    string
	BuildStamp      string
	AssetsFreshness string
	AssetsDigest    string
	ShutdownTimeout time.Duration
	TerminateSelf   func() error
	Window          harnessrpc.WindowController
}

func NewHarness(app *App, paths HarnessPaths) *harnessrpc.Harness {
	host := &harnessHost{app: app, dataDir: paths.DataDir}
	return harnessrpc.New(harnessrpc.Config{
		Host:            host,
		Window:          paths.Window,
		Version:         app.version,
		BuildStamp:      paths.BuildStamp,
		DataRoot:        paths.DataRoot,
		DataDir:         paths.DataDir,
		HomeDir:         paths.HomeDir,
		CredentialHome:  paths.CredentialHome,
		MockProvider:    paths.MockProvider,
		AssetsFreshness: paths.AssetsFreshness,
		AssetsDigest:    paths.AssetsDigest,
		ShutdownTimeout: paths.ShutdownTimeout,
		TerminateSelf:   paths.TerminateSelf,
	})
}

func (h *harnessHost) Store() *store.Store { return h.app.store }

// PushSent and ForgetPushSent expose the harness push recorder. Both are
// no-ops on a boot that never installed one (app_push_harness.go).
func (h *harnessHost) PushSent() []harnessrpc.PushMessage { return h.app.harnessPushMessages() }

func (h *harnessHost) ForgetPushSent() { h.app.harnessPushForget() }

func (h *harnessHost) ReplayLog() *replaylog.Manager { return h.app.replay }

func (h *harnessHost) Shutdown(ctx context.Context) error { return h.app.Shutdown(ctx) }

func (h *harnessHost) ExpectedOrigin() string {
	if server := h.app.transportServer.Load(); server != nil {
		// Origin, not AppURL: this answers "which origin is ours" and
		// nothing here navigates, so minting a page ticket per check
		// would spend one for nothing.
		return server.Origin()
	}
	return ""
}

func (h *harnessHost) ConnectedClients() (int, bool) {
	bus := h.app.eventBus.Load()
	if bus == nil {
		return 0, false
	}
	return bus.SubscriberCount(), true
}

func (h *harnessHost) SessionEnv(threadID string) map[string]string {
	return h.app.sessionAOEnv(threadID)
}

func (h *harnessHost) ListVisibleThreads() ([]store.Thread, error) {
	return h.app.ListThreads()
}

func (h *harnessHost) Emit(channel eventchan.Channel, data any) {
	// Dynamic harness and replay channels deliberately flow through the same
	// transport-aware App helper as every production event.
	h.app.emit(channel, data)
}

// Notify drives the send pipe and the activation pipe from one RPC, which is
// what lets the e2e rig observe both halves without a presenter.
//
// It sends through notifyOSUngated, the one bypass of the preference and
// attended-screen gates (app_notifications.go), because every one of those
// reads something the test cannot see or control. The kind toggle would need
// a preference the test never set, and the attended-screen rules read window
// focus — a Playwright page HAS focus, so the default `notifyMuteWhenFocused`
// would silence every harness notification the moment a spec opened the app.
// Riding KindWorkflowAttention because it had no toggle was the old version
// of this argument; it has one now, so the bypass is explicit instead.
//
// An allocated id rather than a stable one, for the same reason: this call
// names no moment to retract.
func (h *harnessHost) Notify(title, body string, target notify.Target) error {
	return errors.Join(
		h.app.notifyOSUngated(notify.Send{
			ID:     notify.NewID(h.app.notifications.harnessSeq.Add(1)),
			Kind:   notify.KindWorkflowAttention,
			Title:  title,
			Body:   body,
			Target: target,
		}),
		h.app.activateNotificationTarget(target),
	)
}

func (h *harnessHost) BrowserPressKey(threadID, pageID string, chord keybindings.Accelerator) error {
	if h.app.browser.manager == nil {
		return errors.New("browser manager unavailable")
	}
	access, err := h.app.browserAccess(threadID)
	if err != nil {
		return err
	}
	return h.app.browser.manager.HarnessPressKey(context.Background(), access, pageID, chord)
}

func (h *harnessHost) BrowserScroll(threadID, pageID string, x, y float64) error {
	if h.app.browser.manager == nil {
		return errors.New("browser manager unavailable")
	}
	access, err := h.app.browserAccess(threadID)
	if err != nil {
		return err
	}
	_, err = h.app.browser.manager.Scroll(context.Background(), access, pageID, "", x, y)
	return err
}

func (h *harnessHost) BrowserScreenshot(threadID, pageID string) ([]byte, error) {
	if h.app.browser.manager == nil {
		return nil, errors.New("browser manager unavailable")
	}
	access, err := h.app.browserAccess(threadID)
	if err != nil {
		return nil, err
	}
	manager := h.app.browser.manager
	previous := manager.CompanionState(access)
	mount, err := manager.AttachPane(access)
	if err != nil {
		return nil, err
	}
	defer func() {
		if previous.Visible != nil && *previous.Visible {
			visible := true
			_, _ = manager.Visibility(context.Background(), access, &visible, previous.ActivePageID)
		} else {
			if previous.ActivePageID != "" {
				_ = manager.ActivateCompanionPage(access, previous.ActivePageID)
			}
			visible := false
			_, _ = manager.Visibility(context.Background(), access, &visible, "")
		}
		manager.DetachPane(mount.ID)
	}()
	// A native page with no mounted UI pane is deliberately parked at 1x1.
	// Give the diagnostic a deterministic CSS rect so its capture remains
	// meaningful even when a shell driver, rather than the frontend, invokes it.
	if err := manager.SetPaneRect(mount.ID, appbrowser.PaneRect{
		X: 20, Y: 80, Width: 900, Height: 600,
		ClipX: 20, ClipY: 80, ClipWidth: 900, ClipHeight: 600,
		ViewportWidth: 1280, ViewportHeight: 800,
		Visible: true, Background: "#ffffff",
	}); err != nil {
		return nil, err
	}
	visible := true
	if _, err := manager.Visibility(context.Background(), access, &visible, pageID); err != nil {
		return nil, err
	}
	return manager.Screenshot(context.Background(), access, appbrowser.ScreenshotOptions{PageID: pageID})
}

func (h *harnessHost) CreateProject(path string) (store.Project, error) {
	return h.app.CreateProject(path)
}

func (h *harnessHost) CreateThread(options harnessrpc.ThreadOptions) (store.Thread, error) {
	// The harness RPC is its own screenless caller, so the created thread
	// carries no device attribution — the same answer a script driving the
	// app locally should get.
	return h.app.CreateThread(context.Background(), CreateThreadOptions{
		ProjectID:   options.ProjectID,
		Title:       options.Title,
		Provider:    options.Provider,
		Model:       options.Model,
		Mode:        options.Mode,
		RuntimeMode: options.RuntimeMode,
	})
}

func (h *harnessHost) ArchiveThread(threadID string) error {
	return h.app.ArchiveThread(threadID)
}

func (h *harnessHost) StopSession(threadID string) error {
	return h.app.StopSession(threadID)
}

func (h *harnessHost) DeleteProject(projectID string) error {
	_, err := h.app.DeleteProject(projectID)
	return err
}

func (h *harnessHost) RecoverCrashedTurns() error {
	if h.app.triage == nil {
		return nil
	}
	_, err := h.app.triage.RecoverCrashedTurns()
	return err
}

func (h *harnessHost) RecoverOrphanedBackgroundTasks() error {
	if h.app.triage == nil {
		return nil
	}
	_, err := h.app.triage.RecoverOrphanedBackgroundTasks()
	return err
}

func (h *harnessHost) ClearUIState() error {
	if h.app.store == nil {
		return fmt.Errorf("store unavailable")
	}
	if err := h.app.store.ClearUIState(); err != nil {
		return err
	}
	// The wipe took the user tier's rows with it (internal/settings/residency.go),
	// and the settings cache keys on the FILE — which this did not touch. Tell
	// it what happened, or the next read serves preferences whose rows are gone.
	if h.app.settings != nil {
		h.app.settings.InvalidateTierCache()
	}
	return nil
}

func (h *harnessHost) ResetSessionImporter() {
	h.app.sessionImporter().Reset()
}

func (h *harnessHost) HasWorkflowEngine() bool { return h.app.workflowApplication().HasEngine() }

func (h *harnessHost) RequireWorkflowEngine() error {
	_, err := h.app.requireWorkflowEngine()
	return err
}

func (h *harnessHost) ResolveProjectWorkflow(projectID, workflowID string) error {
	_, definitions := workflowSources(h.app.store, h.dataDir)
	_, err := definitions.Resolve(context.Background(), store.WorkItem{
		ProjectID:     projectID,
		WorkflowID:    workflowID,
		WorkflowScope: string(def.ScopeProject),
	})
	return err
}

func (h *harnessHost) StartWorkflowRun(projectID, workflowID, goal string, seeds json.RawMessage, stepMode bool) (store.WorkItem, error) {
	return h.app.WorkflowStartRun(projectID, workflowID, string(def.ScopeProject), goal, seeds, nil, "", stepMode)
}

func (h *harnessHost) SubscribeEvents() (*transport.Subscriber, error) {
	bus := h.app.eventBus.Load()
	if bus == nil {
		return nil, fmt.Errorf("event bus unavailable")
	}
	return bus.Subscribe(), nil
}

func (h *harnessHost) SetWorkflowGlobalPause(paused bool) error {
	return h.app.WorkflowSetGlobalPause(paused)
}

func (h *harnessHost) CancelWorkflowItem(ctx context.Context, itemID string) error {
	return h.app.WorkflowCancelItem(ctx, itemID)
}

func (h *harnessHost) SyncWorkflowEngine() error {
	return h.app.workflowApplication().SyncEngine()
}

func (h *harnessHost) PublishStoreIdentity(identity store.Identity) {
	h.app.storeIdentity.Store(&identity)
}

func (h *harnessHost) SendMessage(threadID, message string) error {
	return h.app.SendMessage(threadID, message, nil)
}

var _ harnessrpc.Host = (*harnessHost)(nil)
