package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"agent-overflow/internal/harness/control"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/acp"
	"agent-overflow/internal/provider/agy"
	"agent-overflow/internal/sessionimport"
)

// maxAgyReplayUpdates bounds a single ACP session/load replay capture while
// staying large enough for long conversations; hitting it fails loudly rather
// than silently truncating imported history.
const maxAgyReplayUpdates = 100000

// agyConnIdleGrace keeps an idle shared agy acp server warm for 5 minutes
// after its last thread detaches so switching threads does not pay another
// cold ACP server startup.
const agyConnIdleGrace = 5 * time.Minute

func agyPoolKey(
	binary string,
	identity providerBinaryIdentity,
	accountID string,
	accountGen uint64,
	env provider.EnvInput,
	isolateThreadID string,
) string {
	h := sha256.New()
	writePart := func(label, val string) {
		_, _ = fmt.Fprintf(h, "%s=%s\x00", label, val)
	}
	writePart("binary", strings.TrimSpace(binary))
	writePart("resolved", identity.path)
	writePart("size", fmt.Sprintf("%d", identity.size))
	writePart("mtime", fmt.Sprintf("%d", identity.modUnixNano))
	writePart("account", accountID)
	writePart("accountGen", fmt.Sprintf("%d", accountGen))
	if isolateThreadID != "" {
		writePart("thread", isolateThreadID)
	}
	writePart("env", env.Fingerprint())
	return fmt.Sprintf("agy|%s|%s", strings.TrimSpace(binary), hex.EncodeToString(h.Sum(nil)[:16]))
}

// acquireAgyConn returns a shared *acp.Conn for this thread's pool key,
// dialing `agy acp` on first acquire. In isolated harness boots, each thread
// gets its own pool key to preserve single-engine mock isolation.
func (a *App) acquireAgyConn(
	ctx context.Context,
	threadID string,
	binary string,
	workDir string,
	env provider.EnvInput,
	accountSelection *providerAccountSelection,
) (*acp.Conn, func(), string, error) {
	identity, _ := a.resolveProviderBinaryIdentity(string(provider.Agy))
	var selection providerAccountSelection
	if accountSelection != nil {
		selection = *accountSelection
	} else {
		selection = a.captureProviderAccountSelection(string(provider.Agy))
	}
	isolateThreadID := ""
	idleGrace := agyConnIdleGrace
	connWorkDir := providerProbeWorkDir()
	if a.providerExtraEnv[control.EnvAddr] != "" {
		isolateThreadID = threadID
		idleGrace = 0
		connWorkDir = workDir
	}
	resolvedBinary := agy.ResolveBinary(binary)
	key := agyPoolKey(resolvedBinary, identity, selection.AccountID, selection.Generation, env, isolateThreadID)

	launchEnv := agy.LaunchEnv(env)
	if a.hasManagedAgyCredentialFile() {
		launchEnv = agy.WithFileStorage(launchEnv)
	}

	rawConn, release, err := a.sessionManager().runtime.Connections().Acquire(
		ctx,
		key,
		idleGrace,
		func(dialCtx context.Context) (provider.Connection, error) {
			return acp.Dial(dialCtx, acp.ConnConfig{
				Binary:       resolvedBinary,
				Args:         agy.LaunchArgs("", ""),
				WorkDir:      connWorkDir,
				Env:          launchEnv,
				EventLogger:  a.logger,
				Reaper:       a.groupReaper("agy session"),
				ThreadID:     "agy-conn",
				ProviderName: string(provider.Agy),
			})
		},
	)
	if err != nil {
		return nil, nil, "", err
	}
	return rawConn.(*acp.Conn), release, key, nil
}

func (a *App) hasManagedAgyCredentialFile() bool {
	return a.hasManagedAgyCredentialFileAt(a.agyAppDataDir())
}

func (a *App) hasManagedAgyCredentialFileAt(appData string) bool {
	return a.ensureProviderAccountManager().AgyShouldForceFileStorage(appData)
}

// borrowAgyConnForCurrentAccount borrows a warm pooled *acp.Conn ONLY when its
// full pool identity (resolved binary + file identity + active account ID +
// account generation + session process env) matches the current account state.
// Never borrows across account generations, stale binaries, or harness-isolated
// per-thread connections.
func (a *App) borrowAgyConnForCurrentAccount(binary string) (*acp.Conn, func()) {
	if a.sessionRuntime == nil {
		return nil, nil
	}
	if a.providerExtraEnv[control.EnvAddr] != "" {
		// In isolated harness boots, live sessions are keyed per-thread with
		// AO_HARNESS_CONTROL; probes and imports must never borrow them.
		return nil, nil
	}
	resolvedBinary := agy.ResolveBinary(binary)
	identity, _ := a.resolveProviderBinaryIdentity(string(provider.Agy))
	selection := a.captureProviderAccountSelection(string(provider.Agy))
	// acquireAgyConn keys live pooled connections with sessionProcessEnv(agy).
	sessionEnv := a.sessionProcessEnv(string(provider.Agy))
	wantKey := agyPoolKey(resolvedBinary, identity, selection.AccountID, selection.Generation, sessionEnv, "")
	conn, release, ok := a.sessionRuntime.Connections().BorrowMatching(func(key string, c provider.Connection) bool {
		if key != wantKey {
			return false
		}
		_, isACP := c.(*acp.Conn)
		return isACP
	})
	if !ok {
		return nil, nil
	}
	return conn.(*acp.Conn), release
}

func (a *App) drainAgyConnections() {
	if a.sessionRuntime == nil {
		return
	}
	a.sessionRuntime.Connections().DrainPrefix("agy|")
}

// agyAppDataDir returns Antigravity's per-home appData directory
// (<providerHome>/.gemini/antigravity-cli).
func (a *App) agyAppDataDir() string {
	home, err := a.providerHome()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".gemini", agy.DefaultAppDataName)
}

func (a *App) withAgyImportConn(ctx context.Context, appDataDir, avoidSessionID string, fn func(*acp.Conn) error) error {
	binary := a.providerBinaryPath(string(provider.Agy))
	canonicalDir := a.agyAppDataDir()
	useCanonical := appDataDir == "" || (canonicalDir != "" && filepath.Clean(appDataDir) == filepath.Clean(canonicalDir))
	if useCanonical {
		if conn, release := a.borrowAgyConnForCurrentAccount(binary); conn != nil {
			if avoidSessionID != "" && conn.HasActiveSession(avoidSessionID) {
				// Never issue `session/load` for a session that is currently
				// live on this pooled server: use an isolated one-shot conn
				// instead so live session state is never perturbed.
				release()
			} else {
				err := fn(conn)
				release()
				if !errors.Is(err, acp.ErrSessionBusy) {
					return err
				}
			}
		}
	}
	var geminiDir, appDataSubdir string
	if !useCanonical || strings.TrimSpace(a.credentialHomeOverride) != "" {
		cleaned := filepath.Clean(appDataDir)
		geminiDir = filepath.Dir(cleaned)
		appDataSubdir = filepath.Base(cleaned)
	}
	env := agy.LaunchEnv(a.ensureProviderAccountManager().ProbeEnv(string(provider.Agy), nil))
	if geminiDir != "" || a.hasManagedAgyCredentialFileAt(appDataDir) {
		env = agy.WithFileStorage(env)
	}
	conn, err := acp.Dial(ctx, acp.ConnConfig{
		Binary:       agy.ResolveBinary(binary),
		Args:         agy.LaunchArgs(geminiDir, appDataSubdir),
		WorkDir:      providerProbeWorkDir(),
		Env:          env,
		Reaper:       a.groupReaper("agy session import"),
		ProviderName: string(provider.Agy),
	})
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	return fn(conn)
}

func (a *App) listAgyImportSessions(ctx context.Context, appDataDir string) ([]sessionimport.AgySessionSummary, error) {
	var summaries []sessionimport.AgySessionSummary
	err := a.withAgyImportConn(ctx, appDataDir, "", func(conn *acp.Conn) error {
		items, listErr := conn.ListSessions(ctx, "")
		if listErr != nil {
			return listErr
		}
		summaries = make([]sessionimport.AgySessionSummary, 0, len(items))
		for _, item := range items {
			var updatedMs int64
			if item.UpdatedAt != "" {
				if parsed, parseErr := time.Parse(time.RFC3339Nano, item.UpdatedAt); parseErr == nil {
					updatedMs = parsed.UnixMilli()
				}
			}
			dbPath := filepath.Join(appDataDir, "conversations", item.SessionID+".db")
			summaries = append(summaries, sessionimport.AgySessionSummary{
				SessionID:   item.SessionID,
				Title:       item.Title,
				WorkDir:     item.CWD,
				UpdatedAtMs: updatedMs,
				SourcePath:  dbPath,
			})
		}
		return nil
	})
	return summaries, err
}

func (a *App) replayAgyImportSession(ctx context.Context, appDataDir, sessionID, workDir string) ([]provider.ProviderEvent, error) {
	var events []provider.ProviderEvent
	err := a.withAgyImportConn(ctx, appDataDir, sessionID, func(conn *acp.Conn) error {
		res, replayErr := conn.ReplayLoad(ctx, acp.SessionLoadParams{
			SessionID:  sessionID,
			CWD:        workDir,
			MCPServers: []acp.MCPServerConfig{},
		}, maxAgyReplayUpdates)
		if replayErr != nil {
			return replayErr
		}
		if res.Truncated {
			return fmt.Errorf("agy session %s exceeded replay limit (%d updates) and could not be imported completely", sessionID, maxAgyReplayUpdates)
		}
		events = acp.ReplayUpdatesToEvents(res.Updates, agy.NewFlavor(appDataDir))
		return nil
	})
	return events, err
}
