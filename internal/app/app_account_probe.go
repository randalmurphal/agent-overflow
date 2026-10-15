package app

import (
	"context"
	"log"
	"time"

	"agent-overflow/internal/provider"
)

// ProviderAccountEvent is the payload for the `provider:account` event.
// Fires once per provider after the startup account probe completes
// (success or "succeeded but unauthenticated"). The frontend
// accountInfoStore hydrates from this event.
//
// On probe failure (binary missing, RPC error) we DO NOT emit — the
// existing `provider:status` channel already carries that diagnostic
// via probeStartupProviderStatuses and we don't want two banners
// fighting for the same condition.
type ProviderAccountEvent struct {
	Provider   string               `json:"provider"`
	AccountID  string               `json:"accountId,omitempty"`
	Account    provider.AccountInfo `json:"account"`
	Generation uint64               `json:"generation,omitempty"`
	Cleared    bool                 `json:"cleared,omitempty"`
}

// probeStartupAccountInfo runs ProbeClaudeAccount and ProbeCodexAccount
// concurrently. The bound methods themselves emit the
// `provider:account` event on cache miss, so this function is the
// orchestrator: spawn one goroutine per provider, swallow errors
// (provider:status carries the binary-level diagnostic), don't block
// app startup.
//
// Called from ServiceStartup in a goroutine — the probes spawn
// short-lived subprocesses (5–8s each) that we never want blocking app
// boot. Fire-and-forget by design: nothing in-app waits on either
// probe.
func (a *App) probeStartupAccountInfo() {
	if a.settings == nil {
		return
	}

	a.startupProbeWG.Add(2)
	go func() {
		defer a.startupProbeWG.Done()
		if a.retryClaudeStartupProbe(a.lifeCtx()) {
			a.providerLifecycleService().RequestClaudeUsage()
		}
	}()
	go func() {
		defer a.startupProbeWG.Done()
		_, _ = a.ProbeCodexAccount()
	}()
}

// claudeStartupProbeBackoff is the wait before each retry of a failed
// startup Claude probe; the last delay repeats. Tests shorten it.
var claudeStartupProbeBackoff = []time.Duration{
	15 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute,
}

// retryClaudeStartupProbe runs the Claude account probe until it succeeds or
// ctx ends, and reports whether it succeeded. The probe is what reports the
// CLI's model list; until one succeeds the picker serves the saved or shipped
// list. A failed probe emits nothing, and an expired login fails before any
// process is spawned, so a retry costs at most one bounded subprocess.
//
// While the binary does not resolve nothing is spawned or logged; the loop
// only looks again, so a CLI installed after launch is probed when it lands.
func (a *App) retryClaudeStartupProbe(ctx context.Context) bool {
	lookPath := a.resolveLookPath()
	for attempt := 0; ; attempt++ {
		delay := claudeStartupProbeBackoff[min(attempt, len(claudeStartupProbeBackoff)-1)]
		if binary := a.providerBinaryPath(string(provider.Claude)); binary != "" && lookPath(binary) == nil {
			_, err := a.ProbeClaudeAccount()
			if err == nil {
				return true
			}
			log.Printf("claude startup probe failed (attempt %d), retrying in %s: %v", attempt+1, delay, err)
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-timer.C:
		}
	}
}
