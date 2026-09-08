package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/entityid"
	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/gitapp"
	"agent-overflow/internal/remotejobs"
	"agent-overflow/internal/rpcclient"
	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
)

type RemoteCommandRequest = remotejobs.Request
type RemoteCommand = store.RemoteJob

func (a *App) initRemoteJobs(st *store.Store, dataDir string) error {
	manager, err := remotejobs.New(a.lifeCtx(), st, remotejobs.ProcessRunner(func() []string {
		env := os.Environ()
		out := make([]string, 0, len(env))
		for _, entry := range env {
			if !strings.HasPrefix(entry, "AO_") {
				out = append(out, entry)
			}
		}
		return out
	}), remotejobs.Options{LogDir: filepath.Join(dataDir, "remote-jobs")})
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
		return RemoteCommand{}, errorsx.Public("remote_not_accepted", "The destination could not process this attempt because it stopped or the request ended. Check status, then retry the identical request with the same request_id when connected.", admitErr)
	}
	defer endAdmission()

	if a.remoteJobs == nil {
		return RemoteCommand{}, errorsx.Public("remote_not_ready", "Remote commands are not ready on this computer. Wait for startup to finish and retry with the same request_id.", nil)
	}
	if !entityid.Valid(workspace.ProjectID) {
		return RemoteCommand{}, errorsx.Public("remote_invalid_project", "project_id must be a destination project UUID from remote_computers.", nil)
	}
	_, cwd, err := a.gitApplication().ResolveWorkspace(workspace)
	if errors.Is(err, sql.ErrNoRows) {
		return RemoteCommand{}, errorsx.Public("remote_project_not_found", "The project no longer exists on the destination. Refresh remote_computers and choose one of its registered projects.", err)
	}
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
		return RemoteCommand{}, errorsx.Public("remote_not_ready", "Remote commands are not ready on this computer. Wait for startup to finish and retry with the same request_id.", nil)
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
		return RemoteCommand{}, errorsx.Public("remote_not_ready", "Remote commands are not ready on this computer. Wait for startup to finish and retry with the same request_id.", nil)
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
func (a *App) RemoteCommandProjects(ctx context.Context) ([]RemoteCommandProject, error) {
	rows, err := a.store.ListProjects()
	if err != nil {
		return nil, err
	}
	out := make([]RemoteCommandProject, 0, len(rows))
	scan, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	for _, row := range rows {
		project := RemoteCommandProject{ID: row.ID, Name: row.Name, Path: row.Path}
		if _, err := os.Lstat(filepath.Join(row.Path, ".git")); err == nil {
			worktrees, err := a.gitCore().ListWorktreesContext(scan, row.Path)
			if err != nil {
				project.WorktreesError = remoteErrorText(remoteOperationError("inspect project worktrees", "", "", err))
			}
			for _, worktree := range worktrees {
				if info, err := os.Stat(worktree.Path); err != nil || !info.IsDir() {
					continue
				}
				_, path, err := a.gitApplication().ResolveWorkspace(gitapp.WorkspaceRef{ProjectID: row.ID, WorkspacePath: worktree.Path})
				if err == nil {
					project.Worktrees = append(project.Worktrees, RemoteCommandWorktree{Path: path, Branch: worktree.Branch, HEAD: worktree.HEAD})
				}
			}
		}
		out = append(out, project)
	}
	return out, nil
}

type RemoteCommandProject struct {
	ID             string                  `json:"id"`
	Name           string                  `json:"name"`
	Path           string                  `json:"path"`
	Worktrees      []RemoteCommandWorktree `json:"worktrees,omitempty"`
	WorktreesError string                  `json:"worktreesError,omitempty"`
}

type RemoteCommandWorktree struct {
	Path   string `json:"path"`
	Branch string `json:"branch,omitempty"`
	HEAD   string `json:"head,omitempty"`
}

type AgentComputer struct {
	ID               string                    `json:"id"`
	Name             string                    `json:"name"`
	Enabled          bool                      `json:"enabled"`
	Projects         []RemoteCommandProject    `json:"projects"`
	Error            string                    `json:"error,omitempty"`
	Environment      *RemoteCommandEnvironment `json:"environment,omitempty"`
	EnvironmentError string                    `json:"environmentError,omitempty"`
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
		return scope, errorsx.Public("remote_session_required", "Remote tools require an active Agent Overflow agent session. Reopen the conversation and try again.", nil)
	}
	if scope.IsPhase() && !scope.HasGrant("remote-commands") {
		return scope, errorsx.Public("remote_grant_required", "This workflow phase does not grant remote-commands. Ask the user to update the workflow definition before starting a new phase.", nil)
	}
	if !a.remoteMCPServer().ThreadEnabled(scope.ThreadID) {
		return scope, errorsx.Public("remote_tools_disabled", "Remote tools are disabled for this conversation. The user can enable ao-remote-tools in the composer’s MCP menu.", nil)
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
		return nil, remoteOperationError("discover", "", "", err)
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
				out[i].Error = remoteErrorText(remoteOperationError("discover", out[i].ID, "", err))
				return
			}
			var environment RemoteCommandEnvironment
			if err := a.backends.CallAgentPeer(probe, out[i].ID, "RemoteCommandEnvironment", &environment); err == nil {
				out[i].Environment = &environment
			} else {
				var remote *rpcclient.Error
				if !errors.As(err, &remote) || remote.Code != transport.ErrCodeMethodNotFound {
					out[i].EnvironmentError = remoteErrorText(remoteOperationError("inspect environment", out[i].ID, "", err))
				}
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
	Label      string               `json:"label,omitempty"`
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
	if err := remotejobs.Validate(input.Request); err != nil {
		return RemoteCommand{}, remoteOperationError("run", input.ComputerID, input.Request.ID, err)
	}
	if !entityid.Valid(input.Workspace.ProjectID) {
		return RemoteCommand{}, errorsx.Public("remote_invalid_project", "project_id must be a destination project UUID from remote_computers.", nil)
	}
	if !entityid.Valid(input.ComputerID) {
		return RemoteCommand{}, errorsx.Public("remote_invalid_computer", "computer_id must be a UUID from remote_computers.", nil)
	}
	// Serialize identical network attempts, not threads: a refused attempt must
	// settle before a retry can become uncertain or accepted under the same ID.
	unlock, err := a.remoteStartLocks().LockCtx(ctx, input.ComputerID+":"+input.Request.ID)
	if err != nil {
		return RemoteCommand{}, err
	}
	defer unlock()
	freshWatch, err := a.registerRemoteWatch(input)
	if err != nil {
		return RemoteCommand{}, err
	}
	call, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var result RemoteCommand
	err = a.backends.CallAgentPeer(call, input.ComputerID, "RemoteCommandStart", &result, input.Workspace, input.Request)
	if err == nil {
		if saveErr := a.observeRemoteCommand(input.ComputerID, input.Request.ID, scope.ThreadID, result); saveErr != nil {
			return result, saveErr
		}
	}
	if err != nil {
		publicErr := remoteOperationError("run", input.ComputerID, input.Request.ID, err)
		code, _, _ := errorsx.PublicDetails(publicErr)
		switch code {
		case "remote_invalid_request", "remote_invalid_project", "remote_project_not_found", "workspace_not_registered", "remote_capacity", "remote_request_conflict", "remote_log_unavailable":
			if freshWatch {
				_ = a.store.RefuseRemoteWatch(input.ComputerID, input.Request.ID, remoteErrorText(publicErr))
			}
		}
		return result, publicErr
	}

	return result, remoteOperationError("run", input.ComputerID, input.Request.ID, err)
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
	if !entityid.Valid(id) {
		return RemoteCommand{}, errorsx.Public("remote_invalid_request", "request_id must be the UUID returned for the original command.", nil)
	}
	action := "status"
	if cancelJob {
		action = "cancel"
	}
	call, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var result RemoteCommand
	if err = a.backends.CallAgentPeer(call, computerID, "RemoteCommandStatus", &result, id); err != nil {
		return result, remoteOperationError(action, computerID, id, err)
	}
	if err = a.observeRemoteCommand(computerID, id, scope.ThreadID, result); err != nil {
		return RemoteCommand{}, err
	}
	if cancelJob {
		err = a.backends.CallAgentPeer(call, computerID, "RemoteCommandCancel", &result, id)
		if err == nil {
			err = a.observeRemoteCommand(computerID, id, scope.ThreadID, result)
		}
	}
	return result, remoteOperationError(action, computerID, id, err)
}
