package main

import (
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
	Provider string               `json:"provider"`
	Account  provider.AccountInfo `json:"account"`
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

	go func() { _, _ = a.ProbeClaudeAccount() }()
	go func() { _, _ = a.ProbeCodexAccount() }()
}
