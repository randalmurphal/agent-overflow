package frontendclient

import (
	"context"
	"errors"
	"log"
	"sync"

	"agent-overflow/internal/appupdate"
	"agent-overflow/internal/assetwatch"
	"agent-overflow/internal/attachedbackends"
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/externalurl"
	"agent-overflow/internal/highlight"
	"agent-overflow/internal/highlightapp"
	"agent-overflow/internal/keybindings"
	"agent-overflow/internal/spinner"
	"agent-overflow/internal/sshsetup"
	"agent-overflow/internal/theme"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/uitrace"
)

// Only these methods are registered on the local controller. Their names use
// the existing App wire IDs; all actual behavior stays in its owning package.
type service struct {
	ctx          context.Context
	cancel       context.CancelFunc
	cfg          Config
	computers    *attachedbackends.Manager
	bus          *transport.EventBus
	theme        *theme.Service
	spinner      *spinner.Service
	keys         *keybindings.Service
	highlight    *highlightapp.Service
	updater      *appupdate.Service
	errors       *uitrace.Tracer
	ssh          *sshsetup.Manager
	themeWatch   *assetwatch.ThemeWatcher
	spinnerWatch *assetwatch.SpinnerWatcher
	mu           sync.Mutex
	closed       bool
	jobs         sync.WaitGroup
}

func newService(ctx context.Context, cancel context.CancelFunc, cfg Config, computers *attachedbackends.Manager, bus *transport.EventBus) (*service, error) {
	s := &service{ctx: ctx, cancel: cancel, cfg: cfg, computers: computers, bus: bus, ssh: sshsetup.New(sshsetup.OSRunner{})}
	s.highlight = highlightapp.New(highlightapp.Config{IsShuttingDown: func() bool { return ctx.Err() != nil }})
	s.updater = appupdate.New(cfg.Version, appupdate.Deps{
		Context:        func() context.Context { return ctx },
		IsShuttingDown: func() bool { return ctx.Err() != nil }, Emit: s.emit,
	})
	var err error
	if s.theme, err = theme.New(cfg.ConfigDir); err != nil {
		return nil, err
	}
	if s.spinner, err = spinner.New(cfg.ConfigDir); err != nil {
		return nil, err
	}
	if s.keys, err = keybindings.New(cfg.ConfigDir); err != nil {
		return nil, err
	}
	if s.errors, err = uitrace.NewErrors(cfg.ConfigDir); err != nil {
		return nil, err
	}
	// Failed seeding is retained by the services and returned with their file
	// warnings; a read-only configuration does not prevent opening the frontend.
	if err := s.theme.EnsureBoot("system"); err != nil {
		log.Printf("frontend client: theme files: %v", err)
	}
	if err := s.spinner.EnsureBoot(); err != nil {
		log.Printf("frontend client: spinner files: %v", err)
	}
	s.themeWatch, err = assetwatch.NewThemeWatcher(s.theme.Dir(), func() { s.emit(eventchan.ThemeChanged, nil) })
	if err != nil {
		log.Printf("frontend client: theme watcher: %v", err)
	}
	s.spinnerWatch, err = assetwatch.NewSpinnerWatcher(s.spinner.Dir(), func() { s.emit(eventchan.SpinnerChanged, nil) })
	if err != nil {
		log.Printf("frontend client: spinner watcher: %v", err)
	}
	if cfg.ConfigureUpdater != nil {
		cfg.ConfigureUpdater(s.updater)
	}
	return s, nil
}

func (s *service) emit(channel eventchan.Channel, value any) {
	if s.ctx.Err() != nil {
		return
	}
	if _, err := s.bus.Emit(channel, value); err != nil {
		log.Printf("frontend client: event: %v", err)
	}
}

func (s *service) close() {
	s.mu.Lock()
	s.closed = true
	s.cancel()
	s.mu.Unlock()
	s.ssh.Close()
	s.jobs.Wait()
	_ = s.themeWatch.Close()
	_ = s.spinnerWatch.Close()
	s.bus.Close()
}

func (s *service) ListBackends() ([]attachedbackends.Attached, error) {
	rows, err := s.computers.List()
	if rows == nil {
		rows = []attachedbackends.Attached{}
	}
	return rows, err
}

func (s *service) AddBackend(link string) (attachedbackends.Attachment, error) {
	attachment, err := s.computers.Add(s.ctx, link)
	if err != nil {
		return attachment, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return attachment, errors.New("This frontend is closing.")
	}
	s.jobs.Go(func() {
		err := s.computers.Await(s.ctx, attachment.ID)
		outcome := struct {
			ID       string `json:"id"`
			Attached bool   `json:"attached"`
			Error    string `json:"error,omitempty"`
		}{ID: attachment.ID, Attached: err == nil}
		if err != nil {
			outcome.Error = err.Error()
		}
		s.emit(eventchan.BackendAttach, outcome)
	})
	return attachment, nil
}

func (s *service) RepairBackendAddress(ctx context.Context, id, endpoint string) (string, error) {
	return s.computers.RepairAddress(ctx, id, endpoint)
}
func (s *service) RemoveBackend(id string) error {
	if err := s.computers.Remove(id); err != nil {
		return err
	}
	s.emit(eventchan.BackendSetChanged, map[string]string{"action": "removed", "id": id})
	return nil
}
func (s *service) RenameBackend(id, nickname string) error {
	if err := s.computers.Rename(id, nickname); err != nil {
		return err
	}
	s.emit(eventchan.BackendSetChanged, map[string]string{"action": "renamed", "id": id, "nickname": nickname})
	return nil
}
func (s *service) StartSSHConnection(request sshsetup.Request) (sshsetup.Status, error) {
	return s.ssh.Begin(s.ctx, request)
}
func (s *service) GetSSHConnection(id string) (sshsetup.Status, error) { return s.ssh.Get(id) }
func (s *service) ConfirmSSHConnection(ctx context.Context, id, number string) error {
	return s.ssh.Confirm(ctx, id, number)
}
func (s *service) CancelSSHConnection(id string) error { s.ssh.Cancel(id); return nil }
func (s *service) StartSSHComputer(ctx context.Context, request sshsetup.Request) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	return s.ssh.Start(ctx, request)
}

func (s *service) Version() string { return s.cfg.Version }

func (s *service) CheckForUpdate() (appupdate.UpdateAvailability, error) {
	return s.updater.CheckForUpdate()
}
func (s *service) ListReleases() ([]appupdate.ReleaseSummary, error) {
	rows, err := s.updater.ListReleases()
	if rows == nil {
		rows = []appupdate.ReleaseSummary{}
	}
	return rows, err
}
func (s *service) DownloadUpdate(tag string) error { return s.updater.DownloadUpdate(tag) }
func (s *service) RestartToUpdate() error          { return s.updater.RestartToUpdate() }

// No backend layout exists to migrate. Subsequent layout writes stay in this
// frontend's storage, just as they do in the ordinary desktop and phone.
func (s *service) GetUIState() map[string]string       { return map[string]string{} }
func (s *service) GetThemeFiles() (theme.Files, error) { return s.theme.Files(), nil }
func (s *service) SetAppearance(value theme.Appearance) error {
	s.themeWatch.Suppress(s.theme.AppearancePath())
	defer s.themeWatch.Suppress(s.theme.AppearancePath())
	return s.theme.SetAppearance(value)
}
func (s *service) SetWindowBackgroundColor(hex string) error {
	r, g, b, err := theme.ParseHexColor(hex)
	if err == nil && s.cfg.SetWindowBackground != nil {
		s.cfg.SetWindowBackground(r, g, b)
	}
	return err
}
func (s *service) GetSpinnerFiles() (spinner.Files, error)         { return s.spinner.Files(), nil }
func (s *service) GetKeybindings() (keybindings.LoadResult, error) { return s.keys.Get(), nil }
func (s *service) UpdateKeybindings(value []keybindings.Keybinding) error {
	if err := s.keys.Update(value); err != nil {
		return err
	}
	s.emit(eventchan.KeybindingsUpdated, nil)
	return nil
}
func (s *service) ResetKeybindings() error {
	if err := s.keys.Reset(); err != nil {
		return err
	}
	s.emit(eventchan.KeybindingsUpdated, nil)
	return nil
}
func (s *service) HighlightClassNames() []string  { return highlight.ClassNames() }
func (s *service) HighlightSchemaVersion() string { return highlight.SchemaVersion() }
func (s *service) HighlightCode(req highlightapp.CodeRequest) (highlightapp.Result, error) {
	return s.highlight.Code(req.Lang, req.Source)
}
func (s *service) HighlightPatch(req highlightapp.PatchRequest) (highlightapp.Result, error) {
	return s.highlight.Patch(req.Path, req.Patch)
}
func (s *service) ReportFrontendErrorBatch(lines []string) (string, error) {
	return s.errors.Append(lines)
}
func (s *service) OpenExternalURL(rawURL string) error { return externalurl.Open(s.ctx, rawURL) }
