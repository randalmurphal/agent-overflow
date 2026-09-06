package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/gitapp"
	"agent-overflow/internal/remotejobs"
	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
)

type RemoteCommandRequest = remotejobs.Request
type RemoteCommand = store.RemoteJob

func (a *App) initRemoteJobs(st *store.Store) error {
	manager, err := remotejobs.New(a.lifeCtx(), st, remotejobs.ProcessRunner(func() []string {
		env := os.Environ()
		out := make([]string, 0, len(env))
		for _, entry := range env {
			if !strings.HasPrefix(entry, "AO_") {
				out = append(out, entry)
			}
		}
		return out
	}))
	if err == nil {
		a.remoteJobs = manager
	}
	return err
}

// remoteCommandOwner uses the authenticated device, never the caller-supplied
// screen ID. Rotation and re-pairing with the same device key retain the owner;
// a different device key cannot read the former device's command output.
func (a *App) remoteCommandOwner(ctx context.Context) (string, error) {
	sessionID := transport.SessionFromContext(ctx)
	if sessionID == "" {
		return "local", nil
	}
	state := a.identityState()
	if state == nil {
		return "", errors.New("computer identity is not ready")
	}
	session, reason := state.sessions.Live(sessionID)
	if reason.Refused() {
		return "", transport.AuthRefused(reason.Code())
	}
	return session.DeviceID, nil
}

// RemoteCommandStart acknowledges a durable request before execution. Closing
// the requesting screen or its WebSocket does not cancel the command.
//
//ao:scope terminal:operate
func (a *App) RemoteCommandStart(ctx context.Context, workspace gitapp.WorkspaceRef, request RemoteCommandRequest) (RemoteCommand, error) {
	endAdmission, admitErr := a.workAdmission.begin(ctx)
	if admitErr != nil {
		return RemoteCommand{}, admitErr
	}
	defer endAdmission()

	if a.remoteJobs == nil {
		return RemoteCommand{}, errors.New("remote commands are not ready")
	}
	_, cwd, err := a.gitApplication().ResolveWorkspace(workspace)
	if err != nil {
		return RemoteCommand{}, err
	}
	owner, err := a.remoteCommandOwner(ctx)
	if err != nil {
		return RemoteCommand{}, err
	}
	return a.remoteJobs.Start(owner, workspace.ProjectID, cwd, request)
}

//ao:scope terminal:operate
//ao:route selected
func (a *App) RemoteCommandStatus(ctx context.Context, id string) (RemoteCommand, error) {
	if a.remoteJobs == nil {
		return RemoteCommand{}, errors.New("remote commands are not ready")
	}
	owner, err := a.remoteCommandOwner(ctx)
	if err != nil {
		return RemoteCommand{}, err
	}
	return a.remoteJobs.Get(owner, id)
}

//ao:scope terminal:operate
//ao:route selected
func (a *App) RemoteCommandCancel(ctx context.Context, id string) (RemoteCommand, error) {
	if a.remoteJobs == nil {
		return RemoteCommand{}, errors.New("remote commands are not ready")
	}
	owner, err := a.remoteCommandOwner(ctx)
	if err != nil {
		return RemoteCommand{}, err
	}
	return a.remoteJobs.Cancel(owner, id)
}

// RemoteCommandProjects advertises registered checkouts only, not arbitrary
// files. The receiving computer still validates membership before each start.
//
//ao:scope terminal:operate
//ao:route selected
func (a *App) RemoteCommandProjects() ([]RemoteCommandProject, error) {
	rows, err := a.store.ListProjects()
	if err != nil {
		return nil, err
	}
	out := make([]RemoteCommandProject, 0, len(rows))
	for _, row := range rows {
		out = append(out, RemoteCommandProject{ID: row.ID, Name: row.Name, Path: row.Path})
	}
	return out, nil
}

type RemoteCommandProject struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Path string `json:"path"`
}

type AgentComputer struct {
	ID       string                 `json:"id"`
	Name     string                 `json:"name"`
	Enabled  bool                   `json:"enabled"`
	Projects []RemoteCommandProject `json:"projects"`
	Error    string                 `json:"error,omitempty"`
}

// PairAgentComputer enrolls the selected originating computer with a peer.
// Confirmation still belongs to the destination's existing pairing ceremony;
// this call cannot grant access or enable agent commands by itself.
//
//ao:scope access:admin
//ao:route selected
//ao:stepup
func (a *App) PairAgentComputer(ctx context.Context, pairingLink string) (BackendAttachment, error) {
	if err := a.requireStepUp(ctx, "pair another computer for agent commands"); err != nil {
		return BackendAttachment{}, err
	}
	return a.AddBackend(pairingLink)
}

// ListAgentComputers is the opt-in configuration on the selected originating
// computer. It does not probe sleeping computers merely to draw settings.
//
//ao:scope terminal:operate
//ao:route selected
func (a *App) ListAgentComputers() ([]AgentComputer, error) {
	if a.backends == nil {
		return []AgentComputer{}, nil
	}
	rows, err := a.backends.List()
	if err != nil {
		return nil, err
	}
	access, err := a.backends.AgentAccess()
	if err != nil {
		return nil, err
	}
	out := make([]AgentComputer, 0, len(rows))
	for _, row := range rows {
		out = append(out, AgentComputer{ID: row.ID, Name: row.Name, Enabled: access[row.ID], Projects: []RemoteCommandProject{}})
	}
	return out, nil
}

//ao:scope terminal:operate
//ao:route selected
func (a *App) SetAgentComputerEnabled(ctx context.Context, id string, enabled bool) error {
	if a.backends == nil {
		return errNoBackendProfiles
	}
	if enabled {
		probe, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if err := a.backends.CheckAgentPeer(probe, id); err != nil {
			return fmt.Errorf("cannot enable agent commands: %w", err)
		}
	}
	if err := a.backends.SetAgentAccess(id, enabled); err != nil {
		return err
	}
	a.signalRemotePeers()
	a.emit(eventchan.AgentComputersChanged, struct{}{})
	return nil
}

func (a *App) remoteAgentScope(ctx context.Context) (transport.CallerScope, error) {
	scope, ok := transport.CallerScopeFrom(ctx)
	if !ok {
		return scope, errors.New("this command must originate in an Agent Overflow agent session")
	}
	if a.backends == nil {
		return scope, errNoBackendProfiles
	}
	return scope, nil
}

// AgentRemoteComputers lists only explicitly enabled peers and checks each
// one's identity/capability over its real connection. Offline peers return an
// error row, never redirect execution to a different computer.
//
//ao:scope terminal:operate
//ao:route selected
func (a *App) AgentRemoteComputers(ctx context.Context) ([]AgentComputer, error) {
	if _, err := a.remoteAgentScope(ctx); err != nil {
		return nil, err
	}
	rows, err := a.ListAgentComputers()
	if err != nil {
		return nil, err
	}
	return a.probeAgentComputers(ctx, rows), nil
}

func (a *App) probeAgentComputers(ctx context.Context, rows []AgentComputer) []AgentComputer {
	out := make([]AgentComputer, 0, len(rows))
	for _, row := range rows {
		if row.Enabled {
			out = append(out, row)
		}
	}
	var wg sync.WaitGroup
	limit := make(chan struct{}, 4)
	for i := range out {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case limit <- struct{}{}:
			case <-ctx.Done():
				out[i].Error = "request canceled"
				return
			}
			defer func() { <-limit }()
			probe, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			if err := a.backends.CallAgentPeer(probe, out[i].ID, "RemoteCommandProjects", &out[i].Projects); err != nil {
				out[i].Error = err.Error()
			}
		}()
	}
	wg.Wait()
	return out
}

type AgentRemoteRequest struct {
	ComputerID string               `json:"computerId"`
	Workspace  gitapp.WorkspaceRef  `json:"workspace"`
	Request    RemoteCommandRequest `json:"request"`
}

//ao:scope terminal:operate
//ao:route selected
func (a *App) AgentRemoteStart(ctx context.Context, input AgentRemoteRequest) (RemoteCommand, error) {
	scope, err := a.remoteAgentScope(ctx)
	if err != nil {
		return RemoteCommand{}, err
	}
	// The authenticated source session owns this provenance, not argv/JSON.
	input.Request.SourceThreadID = scope.ThreadID
	call, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var result RemoteCommand
	err = a.backends.CallAgentPeer(call, input.ComputerID, "RemoteCommandStart", &result, input.Workspace, input.Request)
	return result, err
}

//ao:scope terminal:operate
//ao:route selected
func (a *App) AgentRemoteStatus(ctx context.Context, computerID, id string) (RemoteCommand, error) {
	return a.agentRemoteResult(ctx, computerID, id, false)
}

//ao:scope terminal:operate
//ao:route selected
func (a *App) AgentRemoteCancel(ctx context.Context, computerID, id string) (RemoteCommand, error) {
	return a.agentRemoteResult(ctx, computerID, id, true)
}

func (a *App) agentRemoteResult(ctx context.Context, computerID, id string, cancelJob bool) (RemoteCommand, error) {
	scope, err := a.remoteAgentScope(ctx)
	if err != nil {
		return RemoteCommand{}, err
	}
	call, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var result RemoteCommand
	if err = a.backends.CallAgentPeer(call, computerID, "RemoteCommandStatus", &result, id); err != nil {
		return result, err
	}
	if result.SourceThreadID != scope.ThreadID {
		return RemoteCommand{}, errors.New("this command belongs to another conversation")
	}
	if cancelJob {
		err = a.backends.CallAgentPeer(call, computerID, "RemoteCommandCancel", &result, id)
	}
	return result, err
}
