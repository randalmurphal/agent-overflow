package mcpapp

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/mcpstatus"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/codex"
)

const workspaceMCPAuthLifetime = 6 * time.Minute

type workspaceMCPAuthKey struct {
	provider  string
	workspace string
	server    string
}

// WorkspaceAuthOutcome is the terminal provider observation for a temporary
// workspace-scoped OAuth process.
type WorkspaceAuthOutcome struct {
	Success  bool
	TimedOut bool
	Error    string
	Status   mcpstatus.Status
	Raw      string
}

// WorkspaceAuthHandle keeps the provider process alive across the browser hop.
// Wait and Close are required when a starter returns a handle.
type WorkspaceAuthHandle struct {
	Result MCPAuthInitResult
	Wait   func(context.Context) WorkspaceAuthOutcome
	Close  func() error
}

// WorkspaceAuthStarter creates a temporary provider-owned OAuth flow. It is an
// injected seam so lifecycle policy can be tested without spawning a CLI.
type WorkspaceAuthStarter func(
	ctx context.Context,
	providerName, workspacePath, serverName string,
) (*WorkspaceAuthHandle, error)

// workspaceMCPAuthRun is both the single-flight startup and the identity held
// in Service.workspaceAuthFlows. Closing ready publishes result/err to callers
// that arrived while the provider process was starting.
type workspaceMCPAuthRun struct {
	ready  chan struct{}
	result MCPAuthInitResult
	err    error
	cancel context.CancelFunc
}

// TriggerWorkspaceMcpAuth authenticates a provider/workspace MCP server
// without creating an Agent Overflow thread. A temporary provider process owns
// the loopback listener through the browser hop, then exits when completion is
// confirmed, rejected, timed out, or the app shuts down.
func (a *Service) TriggerWorkspaceMcpAuth(providerName, workspacePath, serverName string) (MCPAuthInitResult, error) {
	if a.isShuttingDown() {
		return MCPAuthInitResult{}, a.shutdownError()
	}
	providerName = strings.TrimSpace(providerName)
	workspacePath = filepath.Clean(strings.TrimSpace(workspacePath))
	serverName = strings.TrimSpace(serverName)
	if serverName == "" {
		return MCPAuthInitResult{}, errors.New("trigger workspace mcp auth: server name required")
	}
	if _, err := parseMCPStatusProvider(providerName); err != nil {
		return MCPAuthInitResult{}, err
	}
	if err := provider.ValidateProbeWorkDir(providerName, workspacePath); err != nil {
		return MCPAuthInitResult{}, err
	}

	key := workspaceMCPAuthKey{provider: providerName, workspace: workspacePath, server: serverName}
	a.workspaceAuthMu.Lock()
	if run := a.workspaceAuthFlows[key]; run != nil {
		a.workspaceAuthMu.Unlock()
		select {
		case <-run.ready:
			return run.result, run.err
		case <-a.lifeCtx().Done():
			return MCPAuthInitResult{}, a.lifeCtx().Err()
		}
	}
	if a.workspaceAuthFlows == nil {
		a.workspaceAuthFlows = make(map[workspaceMCPAuthKey]*workspaceMCPAuthRun)
	}
	flowCtx, cancel := context.WithTimeout(a.lifeCtx(), workspaceMCPAuthLifetime)
	run := &workspaceMCPAuthRun{ready: make(chan struct{}), cancel: cancel}
	a.workspaceAuthFlows[key] = run
	a.workspaceAuthMu.Unlock()

	starter := a.workspaceAuthStarter
	if starter == nil {
		starter = a.startWorkspaceMCPAuth
	}
	handle, err := starter(flowCtx, providerName, workspacePath, serverName)
	if err != nil {
		run.err = err
		close(run.ready)
		cancel()
		a.removeWorkspaceMCPAuthRun(key, run)
		return MCPAuthInitResult{}, err
	}
	if handle == nil || handle.Wait == nil || handle.Close == nil {
		err = errors.New("trigger workspace mcp auth: provider returned an incomplete flow handle")
		if handle != nil && handle.Close != nil {
			_ = handle.Close()
		}
		run.err = err
		close(run.ready)
		cancel()
		a.removeWorkspaceMCPAuthRun(key, run)
		return MCPAuthInitResult{}, err
	}
	run.result = handle.Result
	close(run.ready)

	go a.finishWorkspaceMCPAuth(flowCtx, key, run, handle)
	return run.result, nil
}

func (a *Service) removeWorkspaceMCPAuthRun(key workspaceMCPAuthKey, run *workspaceMCPAuthRun) {
	a.workspaceAuthMu.Lock()
	if a.workspaceAuthFlows[key] == run {
		delete(a.workspaceAuthFlows, key)
	}
	a.workspaceAuthMu.Unlock()
}

func (a *Service) finishWorkspaceMCPAuth(
	ctx context.Context,
	key workspaceMCPAuthKey,
	run *workspaceMCPAuthRun,
	handle *WorkspaceAuthHandle,
) {
	defer func() {
		if err := handle.Close(); err != nil {
			log.Printf("mcp: close temporary %s OAuth process for %s: %v", key.provider, key.server, err)
		}
		run.cancel()
		a.removeWorkspaceMCPAuthRun(key, run)
	}()
	outcome := handle.Wait(ctx)
	// App shutdown owns this cancellation. A terminal event emitted after the
	// event bus and triage drain would be both stale and unsafe.
	if a.lifeCtx().Err() != nil {
		return
	}
	if ctx.Err() != nil && !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		outcome = WorkspaceAuthOutcome{
			TimedOut: true,
			Error:    "sign-in not confirmed",
		}
	}

	statusKey := mcpstatus.Key{Provider: mcpstatus.Provider(key.provider), Name: key.server}
	if outcome.Status == mcpstatus.StatusConnected || outcome.Status == mcpstatus.StatusFailed {
		a.mcpStatus().Put(mcpstatus.ServerStatus{
			Key:    statusKey,
			Status: outcome.Status,
			Raw:    outcome.Raw,
			Error:  sanitizeMCPError(outcome.Error),
			// The observing process closes with this flow. Treat the result like
			// the existing one-shot status fetch so it cannot overwrite a row
			// sourced from a still-live thread session in the frontend.
			Source:    mcpstatus.SourceEphemeralFetch,
			CheckedAt: time.Now(),
		})
	} else {
		a.mcpStatus().Invalidate(statusKey)
	}
	if outcome.Success {
		a.applyWorkspaceMCPAuthToLiveSessions(key)
	}
	a.emit(eventchan.MCPOAuthCompleted, map[string]any{
		"threadId":   "",
		"provider":   key.provider,
		"serverName": key.server,
		"success":    outcome.Success,
		"timedOut":   outcome.TimedOut,
		"error":      sanitizeMCPError(outcome.Error),
	})
}

func (a *Service) startWorkspaceMCPAuth(
	ctx context.Context,
	providerName, workspacePath, serverName string,
) (*WorkspaceAuthHandle, error) {
	switch providerName {
	case string(provider.Claude):
		cfg := claude.Config{
			Binary:      a.providerBinaryPath(providerName),
			WorkDir:     workspacePath,
			Env:         a.sessionProcessEnv(providerName),
			EventLogger: a.logger,
		}
		flow, result, err := claude.StartMCPAuth(ctx, claude.MCPAuthConfig{
			Config:         cfg,
			ReadCredential: a.claudeMCPAuthCredentialReader(),
			RequestTimeout: mcpAuthRoundTripTimeout,
		}, serverName)
		if err != nil {
			return nil, err
		}
		return &WorkspaceAuthHandle{
			Result: MCPAuthInitResult{
				AuthURL:            result.AuthURL,
				Provider:           providerName,
				RequiresUserAction: result.RequiresUserAction,
			},
			Wait: func(waitCtx context.Context) WorkspaceAuthOutcome {
				return waitForClaudeWorkspaceMCPAuth(waitCtx, flow, serverName, defaultClaudeMCPOAuthIntervals)
			},
			Close: flow.Close,
		}, nil

	case string(provider.Codex):
		flow, result, err := codex.StartMCPAuth(ctx, codex.MCPAuthConfig{
			Binary:         a.providerBinaryPath(providerName),
			WorkDir:        workspacePath,
			Env:            a.sessionProcessEnv(providerName),
			RequestTimeout: mcpAuthRoundTripTimeout,
		}, serverName)
		if err != nil {
			return nil, err
		}
		return &WorkspaceAuthHandle{
			Result: MCPAuthInitResult{
				AuthURL:            result.AuthorizationURL,
				Provider:           providerName,
				RequiresUserAction: true,
			},
			Wait: func(waitCtx context.Context) WorkspaceAuthOutcome {
				success, errMessage, err := flow.Wait(waitCtx)
				if err != nil {
					return WorkspaceAuthOutcome{Error: err.Error()}
				}
				return WorkspaceAuthOutcome{Success: success, Error: errMessage}
			},
			Close: func() error {
				flow.Close()
				return nil
			},
		}, nil
	default:
		return nil, fmt.Errorf("%w: %s", ErrMCPProviderUnsupported, providerName)
	}
}

func (a *Service) claudeMCPAuthCredentialReader() func() ([]byte, error) {
	return func() ([]byte, error) {
		if a == nil || a.deps.ReadClaudeCredential == nil {
			return nil, errors.New("provider credential store unavailable")
		}
		return a.deps.ReadClaudeCredential()
	}
}

func waitForClaudeWorkspaceMCPAuth(
	ctx context.Context,
	flow *claude.MCPAuthFlow,
	serverName string,
	intervals []time.Duration,
) WorkspaceAuthOutcome {
	observation := waitForClaudeMCPOAuth(
		ctx,
		serverName,
		intervals,
		func() ClaudeMCPStatusQuerier { return flow.QueryStatus },
	)
	if observation.aborted {
		return WorkspaceAuthOutcome{}
	}
	return WorkspaceAuthOutcome{
		Success:  observation.status == mcpstatus.StatusConnected,
		TimedOut: observation.timedOut,
		Error:    observation.error,
		Status:   observation.status,
		Raw:      observation.raw,
	}
}

func (a *Service) applyWorkspaceMCPAuthToLiveSessions(key workspaceMCPAuthKey) {
	switch key.provider {
	case string(provider.Codex):
		var sessions []CodexLiveSession
		if a.deps.CodexSessions != nil {
			sessions = a.deps.CodexSessions()
		}
		for _, live := range sessions {
			live.Session.ForgetMCPStartupState(key.server)
			a.requestCodexMCPReload(live.ThreadID)
		}
	case string(provider.Claude):
		var sessions []ClaudeLiveSession
		if a.deps.ClaudeSessions != nil {
			sessions = a.deps.ClaudeSessions(key.workspace)
		}
		for _, live := range sessions {
			ctx, cancel := context.WithTimeout(a.lifeCtx(), mcpLiveApplyTimeout)
			err := live.Session.ReconnectMCPServer(ctx, key.server)
			cancel()
			if err != nil {
				log.Printf("mcp: reconnect %s on Claude thread %s after workspace OAuth: %v", key.server, live.ThreadID, err)
			}
		}
	}
}
