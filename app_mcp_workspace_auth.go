package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"path/filepath"
	"strings"
	"time"

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

type workspaceMCPAuthOutcome struct {
	success  bool
	timedOut bool
	error    string
	status   mcpstatus.Status
	raw      string
}

type workspaceMCPAuthHandle struct {
	result MCPAuthInitResult
	wait   func(context.Context) workspaceMCPAuthOutcome
	close  func() error
}

type workspaceMCPAuthStarter func(
	ctx context.Context,
	providerName, workspacePath, serverName string,
) (*workspaceMCPAuthHandle, error)

// workspaceMCPAuthRun is both the single-flight startup and the identity held
// in App.workspaceMCPAuthFlows. Closing ready publishes result/err to callers
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
func (a *App) TriggerWorkspaceMcpAuth(providerName, workspacePath, serverName string) (MCPAuthInitResult, error) {
	if a.shuttingDown.Load() {
		return MCPAuthInitResult{}, ErrShuttingDown
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
	a.workspaceMCPAuthMu.Lock()
	if run := a.workspaceMCPAuthFlows[key]; run != nil {
		a.workspaceMCPAuthMu.Unlock()
		select {
		case <-run.ready:
			return run.result, run.err
		case <-a.lifeCtx().Done():
			return MCPAuthInitResult{}, a.lifeCtx().Err()
		}
	}
	if a.workspaceMCPAuthFlows == nil {
		a.workspaceMCPAuthFlows = make(map[workspaceMCPAuthKey]*workspaceMCPAuthRun)
	}
	flowCtx, cancel := context.WithTimeout(a.lifeCtx(), workspaceMCPAuthLifetime)
	run := &workspaceMCPAuthRun{ready: make(chan struct{}), cancel: cancel}
	a.workspaceMCPAuthFlows[key] = run
	a.workspaceMCPAuthMu.Unlock()

	starter := a.workspaceMCPAuthStarter
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
	if handle == nil || handle.wait == nil || handle.close == nil {
		err = errors.New("trigger workspace mcp auth: provider returned an incomplete flow handle")
		if handle != nil && handle.close != nil {
			_ = handle.close()
		}
		run.err = err
		close(run.ready)
		cancel()
		a.removeWorkspaceMCPAuthRun(key, run)
		return MCPAuthInitResult{}, err
	}
	run.result = handle.result
	close(run.ready)

	go a.finishWorkspaceMCPAuth(flowCtx, key, run, handle)
	return run.result, nil
}

func (a *App) removeWorkspaceMCPAuthRun(key workspaceMCPAuthKey, run *workspaceMCPAuthRun) {
	a.workspaceMCPAuthMu.Lock()
	if a.workspaceMCPAuthFlows[key] == run {
		delete(a.workspaceMCPAuthFlows, key)
	}
	a.workspaceMCPAuthMu.Unlock()
}

func (a *App) finishWorkspaceMCPAuth(
	ctx context.Context,
	key workspaceMCPAuthKey,
	run *workspaceMCPAuthRun,
	handle *workspaceMCPAuthHandle,
) {
	defer func() {
		if err := handle.close(); err != nil {
			log.Printf("mcp: close temporary %s OAuth process for %s: %v", key.provider, key.server, err)
		}
		run.cancel()
		a.removeWorkspaceMCPAuthRun(key, run)
	}()
	outcome := handle.wait(ctx)
	// App shutdown owns this cancellation. A terminal event emitted after the
	// event bus and triage drain would be both stale and unsafe.
	if a.lifeCtx().Err() != nil {
		return
	}
	if ctx.Err() != nil && !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		outcome = workspaceMCPAuthOutcome{
			timedOut: true,
			error:    "sign-in not confirmed",
		}
	}

	statusKey := mcpstatus.Key{Provider: mcpstatus.Provider(key.provider), Name: key.server}
	if outcome.status == mcpstatus.StatusConnected || outcome.status == mcpstatus.StatusFailed {
		a.mcpStatus().Put(mcpstatus.ServerStatus{
			Key:    statusKey,
			Status: outcome.status,
			Raw:    outcome.raw,
			Error:  sanitizeMCPError(outcome.error),
			// The observing process closes with this flow. Treat the result like
			// the existing one-shot status fetch so it cannot overwrite a row
			// sourced from a still-live thread session in the frontend.
			Source:    mcpstatus.SourceEphemeralFetch,
			CheckedAt: time.Now(),
		})
	} else {
		a.mcpStatus().Invalidate(statusKey)
	}
	if outcome.success {
		a.applyWorkspaceMCPAuthToLiveSessions(key)
	}
	a.emit("mcp:oauth-completed", map[string]any{
		"threadId":   "",
		"provider":   key.provider,
		"serverName": key.server,
		"success":    outcome.success,
		"timedOut":   outcome.timedOut,
		"error":      sanitizeMCPError(outcome.error),
	})
}

func (a *App) startWorkspaceMCPAuth(
	ctx context.Context,
	providerName, workspacePath, serverName string,
) (*workspaceMCPAuthHandle, error) {
	switch providerName {
	case string(provider.Claude):
		cfg := claude.Config{
			Binary:      a.providerBinaryPath(providerName),
			WorkDir:     workspacePath,
			Env:         a.sessionProcessEnv(providerName, nil, aoSessionCredential{}),
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
		return &workspaceMCPAuthHandle{
			result: MCPAuthInitResult{
				AuthURL:            result.AuthURL,
				Provider:           providerName,
				RequiresUserAction: result.RequiresUserAction,
			},
			wait: func(waitCtx context.Context) workspaceMCPAuthOutcome {
				return waitForClaudeWorkspaceMCPAuth(waitCtx, flow, serverName, defaultClaudeMCPOAuthIntervals)
			},
			close: flow.Close,
		}, nil

	case string(provider.Codex):
		flow, result, err := codex.StartMCPAuth(ctx, codex.MCPAuthConfig{
			Binary:         a.providerBinaryPath(providerName),
			WorkDir:        workspacePath,
			Env:            a.sessionProcessEnv(providerName, nil, aoSessionCredential{}),
			RequestTimeout: mcpAuthRoundTripTimeout,
		}, serverName)
		if err != nil {
			return nil, err
		}
		return &workspaceMCPAuthHandle{
			result: MCPAuthInitResult{
				AuthURL:            result.AuthorizationURL,
				Provider:           providerName,
				RequiresUserAction: true,
			},
			wait: func(waitCtx context.Context) workspaceMCPAuthOutcome {
				success, errMessage, err := flow.Wait(waitCtx)
				if err != nil {
					return workspaceMCPAuthOutcome{error: err.Error()}
				}
				return workspaceMCPAuthOutcome{success: success, error: errMessage}
			},
			close: func() error {
				flow.Close()
				return nil
			},
		}, nil
	default:
		return nil, fmt.Errorf("%w: %s", ErrMCPProviderUnsupported, providerName)
	}
}

func (a *App) claudeMCPAuthCredentialReader() func() ([]byte, error) {
	return func() ([]byte, error) {
		if a.providerCredentials == nil {
			return nil, errors.New("provider credential store unavailable")
		}
		snapshot, present, err := a.readCanonicalCredentialIfPresent(string(provider.Claude))
		if err != nil {
			return nil, err
		}
		if !present {
			return nil, fs.ErrNotExist
		}
		return snapshot.Data, nil
	}
}

func waitForClaudeWorkspaceMCPAuth(
	ctx context.Context,
	flow *claude.MCPAuthFlow,
	serverName string,
	intervals []time.Duration,
) workspaceMCPAuthOutcome {
	observation := waitForClaudeMCPOAuth(
		ctx,
		serverName,
		intervals,
		func() claudeMCPStatusQuerier { return flow.QueryStatus },
	)
	if observation.aborted {
		return workspaceMCPAuthOutcome{}
	}
	return workspaceMCPAuthOutcome{
		success:  observation.status == mcpstatus.StatusConnected,
		timedOut: observation.timedOut,
		error:    observation.error,
		status:   observation.status,
		raw:      observation.raw,
	}
}

func (a *App) applyWorkspaceMCPAuthToLiveSessions(key workspaceMCPAuthKey) {
	switch key.provider {
	case string(provider.Codex):
		for _, live := range a.sessionManager().codexMCPSessions() {
			live.session.ForgetMCPStartupState(key.server)
			a.requestCodexMCPReload(live.threadID)
		}
	case string(provider.Claude):
		for _, live := range a.sessionManager().claudeMCPSessions(key.workspace) {
			ctx, cancel := context.WithTimeout(a.lifeCtx(), mcpLiveApplyTimeout)
			err := live.session.ReconnectMCPServer(ctx, key.server)
			cancel()
			if err != nil {
				log.Printf("mcp: reconnect %s on Claude thread %s after workspace OAuth: %v", key.server, live.threadID, err)
			}
		}
	}
}
