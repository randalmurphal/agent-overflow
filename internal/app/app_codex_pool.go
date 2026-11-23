package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/harness/control"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/provideraccounts"
)

// codexConnIdleGrace keeps an idle shared Codex app-server warm for 5 minutes
// after its last thread detaches so switching between threads does not pay
// another ~190 MiB cold process startup.
const codexConnIdleGrace = 5 * time.Minute

func codexPoolKey(
	binary string,
	identity providerBinaryIdentity,
	accountSelection providerAccountSelection,
	env map[string]string,
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
	writePart("accountId", accountSelection.AccountID)
	writePart("accountGen", fmt.Sprintf("%d", accountSelection.Generation))
	if isolateThreadID != "" {
		writePart("thread", isolateThreadID)
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		writePart("env:"+k, env[k])
	}
	// Prefix places provider and account id before the hash so DrainPrefix and
	// BorrowMatching never misparse binary paths containing '|'.
	return fmt.Sprintf("codex|%s|%s", accountSelection.AccountID, hex.EncodeToString(h.Sum(nil)[:16]))
}

// codexVersionRefusal holds the identity of the Codex binary the last
// shared dial refused for its version, with the refusal. A start against
// the same file answers that refusal without launching it again, so a
// loop, a watch or a notice retry cannot relaunch a binary that cannot
// pass; a changed file (an update, a different configured path) has
// another identity and is dialed.
type codexVersionRefusal struct {
	mu       sync.Mutex
	identity providerBinaryIdentity
	err      error
}

func (r *codexVersionRefusal) refusal(identity providerBinaryIdentity) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil && r.identity == identity {
		return r.err
	}
	return nil
}

// note records a dial's outcome for identity: a version refusal is kept,
// anything else forgets the refusal it held.
func (r *codexVersionRefusal) note(identity providerBinaryIdentity, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if errors.Is(err, provider.ErrCodexVersionUnsupported) {
		r.identity, r.err = identity, err
		return
	}
	r.identity, r.err = providerBinaryIdentity{}, nil
}

// acquireCodexConn returns a shared *codex.Conn for this thread's pool key,
// dialing and verifying the account on first acquire. In isolated harness
// boots, each thread gets its own pool key to preserve single-engine mock
// isolation across existing Playwright specs.
func (a *App) acquireCodexConn(
	ctx context.Context,
	threadID string,
	binary string,
	workDir string,
	env map[string]string,
	accountSelection *providerAccountSelection,
) (*codex.Conn, func(), string, error) {
	identity, identified := a.resolveProviderBinaryIdentity(string(provider.Codex))
	if identified {
		if err := a.codexRefusal.refusal(identity); err != nil {
			return nil, nil, "", err
		}
	}
	isolateThreadID := ""
	idleGrace := codexConnIdleGrace
	connWorkDir := providerProbeWorkDir()
	if a.providerExtraEnv[control.EnvAddr] != "" {
		isolateThreadID = threadID
		idleGrace = 0
		connWorkDir = workDir
	}
	key := codexPoolKey(binary, identity, *accountSelection, env, isolateThreadID)

	rawConn, release, err := a.sessionManager().runtime.Connections().Acquire(
		ctx,
		key,
		idleGrace,
		func(dialCtx context.Context) (provider.Connection, error) {
			conn, dialErr := codex.Dial(dialCtx, codex.ConnConfig{
				Binary:      binary,
				WorkDir:     connWorkDir,
				Env:         env,
				EventLogger: a.logger,
				Reaper:      a.groupReaper("codex session"),
				ThreadID:    "codex-conn",
			})
			if identified {
				a.codexRefusal.note(identity, dialErr)
			}
			if dialErr != nil {
				return nil, dialErr
			}
			if accountSelection.AccountID != "" {
				accountCtx, cancelAccountRead := context.WithTimeout(
					dialCtx,
					codexAccountReadTimeout,
				)
				accountInfo, accountErr := conn.AccountInfo(accountCtx)
				cancelAccountRead()
				if accountErr == nil {
					var observedAccountID string
					var updatedAccount *provideraccounts.Account
					observedAccountID, updatedAccount, accountErr = a.accountIDForObservedIdentity(
						string(provider.Codex),
						accountSelection.AccountID,
						accountInfo,
					)
					if accountErr == nil && updatedAccount != nil {
						a.emitProviderAccountIfCurrent(
							string(provider.Codex),
							*updatedAccount,
							providerAccountInfo(*updatedAccount),
						)
					}
					if accountErr == nil && observedAccountID != accountSelection.AccountID {
						accountErr = fmt.Errorf(
							"codex app-server authenticated as account %q instead of selected account %q",
							observedAccountID,
							accountSelection.AccountID,
						)
					}
				}
				if accountErr != nil {
					return nil, errors.Join(accountErr, conn.Close())
				}
			}
			conn.SetSkillsChangedHandler(func() {
				a.handleCodexSkillsChanged()
			})
			conn.SetMCPStartupUpdateHandler(func(u codex.MCPStartupUpdate) {
				a.handleCodexMCPStartupUpdate(u)
			})
			conn.SetMCPOAuthCompletedHandler(func(_ string, serverName string, success bool, errMsg string) {
				a.handleCodexMCPOAuthCompleted("", serverName, success, errMsg)
			})
			return conn, nil
		},
	)
	if err != nil {
		return nil, nil, "", err
	}
	if accountSelection.AccountID != "" && a.providerAccounts != nil {
		if account, ok := a.providerAccounts.Get(
			string(provider.Codex),
			accountSelection.AccountID,
			time.Now(),
		); ok {
			accountSelection.Account = providerAccountInfo(account)
		}
	}
	return rawConn.(*codex.Conn), release, key, nil
}

func (a *App) borrowCodexConnForBinary(binary string) (*codex.Conn, func()) {
	if a.sessionRuntime == nil {
		return nil, nil
	}
	want := strings.TrimSpace(binary)
	if want == "" {
		want = "codex"
	}
	conn, release, ok := a.sessionRuntime.Connections().BorrowMatching(func(key string, c provider.Connection) bool {
		cc, isCodex := c.(*codex.Conn)
		if !isCodex {
			return false
		}
		got := strings.TrimSpace(cc.Binary())
		if got == "" {
			got = "codex"
		}
		return got == want
	})
	if !ok {
		return nil, nil
	}
	return conn.(*codex.Conn), release
}

func (a *App) borrowCodexConnForAccount(accountID string) (*codex.Conn, func()) {
	if a.sessionRuntime == nil {
		return nil, nil
	}
	conn, release, ok := a.sessionRuntime.Connections().BorrowMatching(func(key string, c provider.Connection) bool {
		if _, isCodex := c.(*codex.Conn); !isCodex {
			return false
		}
		parts := strings.SplitN(key, "|", 3)
		return len(parts) == 3 && parts[0] == "codex" && parts[1] == accountID
	})
	if !ok {
		return nil, nil
	}
	return conn.(*codex.Conn), release
}

func (a *App) drainCodexConnections() {
	if a.sessionRuntime == nil {
		return
	}
	a.sessionRuntime.Connections().DrainPrefix("codex|")
}
