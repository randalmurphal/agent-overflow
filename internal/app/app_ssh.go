package app

import (
	"context"
	"sync"

	"agent-overflow/internal/sshsetup"
)

type appSSHSetup struct {
	mu      sync.Mutex
	manager *sshsetup.Manager
	closed  bool
}

func (s *appSSHSetup) get() (*sshsetup.Manager, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrShuttingDown
	}
	if s.manager == nil {
		s.manager = sshsetup.New(sshsetup.OSRunner{})
	}
	return s.manager, nil
}
func (s *appSSHSetup) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	if s.manager != nil {
		s.manager.Close()
	}
}

// StartSSHConnection starts a bounded owner console on a configured SSH host.
// Closing the frontend does not stop the independently installed remote service.
//
//ao:scope host
//ao:route home
func (a *App) StartSSHConnection(request sshsetup.Request) (sshsetup.Status, error) {
	m, err := a.sshSetup.get()
	if err != nil {
		return sshsetup.Status{}, err
	}
	return m.Begin(a.lifeCtx(), request)
}

// GetSSHConnection reads only the selected setup's bounded status.
//
//ao:scope host
//ao:route home
func (a *App) GetSSHConnection(id string) (sshsetup.Status, error) {
	m, err := a.sshSetup.get()
	if err != nil {
		return sshsetup.Status{}, err
	}
	return m.Get(id)
}

// ConfirmSSHConnection forwards only the matching pairing number.
//
//ao:scope host
//ao:route home
func (a *App) ConfirmSSHConnection(ctx context.Context, id, number string) error {
	m, err := a.sshSetup.get()
	if err != nil {
		return err
	}
	return m.Confirm(ctx, id, number)
}

// CancelSSHConnection releases the SSH console, not the remote backend.
//
//ao:scope host
//ao:route home
func (a *App) CancelSSHConnection(id string) error {
	m, err := a.sshSetup.get()
	if err != nil {
		return err
	}
	m.Cancel(id)
	return nil
}

// StartSSHComputer starts a previously installed background service through
// this desktop's SSH configuration. Existing device pairing is preserved.
//
//ao:scope host
//ao:route home
func (a *App) StartSSHComputer(ctx context.Context, request sshsetup.Request) error {
	m, err := a.sshSetup.get()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(a.lifeCtx(), cancel)
	defer stop()
	return m.Start(ctx, request)
}
