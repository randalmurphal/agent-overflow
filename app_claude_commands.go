package main

import (
	"sync"

	"agent-overflow/internal/claudecommands"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/slicesx"
)

// claudeCommandCache holds the provider-executed slash-command list the Claude
// CLI reports, which rides along on the `initialize` response of the zero-token
// account probe (claude.ProbeConfig.OnCommands).
//
// Package-level for the same reason claudeModelCatalog is: the answer depends
// on the binary, the installed credentials, the workdir, and the custom
// environment — never on App configuration — and the same probe fills both, so
// splitting their lifetimes across App instances would let one hold an answer
// the other cannot see.
var (
	claudeCommandCacheMu sync.Mutex
	claudeCommandCacheV  *claudecommands.Cache
)

func claudeCommandCache() *claudecommands.Cache {
	claudeCommandCacheMu.Lock()
	defer claudeCommandCacheMu.Unlock()
	if claudeCommandCacheV == nil {
		claudeCommandCacheV = claudecommands.NewCache()
	}
	return claudeCommandCacheV
}

// resetClaudeCommandCacheForTest swaps the package-level cache for a fresh
// instance. Called by resetClaudeProbeCacheForTest rather than by tests
// directly — one probe fills the account, model, and command caches, so they
// reset together or one of them serves another test's answer.
func resetClaudeCommandCacheForTest() {
	claudeCommandCacheMu.Lock()
	defer claudeCommandCacheMu.Unlock()
	claudeCommandCacheV = claudecommands.NewCache()
}

// claudeProbeCommands holds what claude.ProbeConfig.OnCommands reported, so the
// caller can decide what to do with it once the probe has actually succeeded.
//
// reported distinguishes "the probe ran and the CLI said nothing about
// commands" (an answer that must clear a previous list) from "no probe ran at
// all" (a cache hit, which must leave the stored list alone) — the same
// distinction claudeProbeModels draws, for the same reason.
type claudeProbeCommands struct {
	reported bool
	commands []provider.SlashCommand
	err      error
}

func (c *claudeProbeCommands) capture(commands []provider.SlashCommand, err error) {
	c.reported = true
	c.commands = commands
	c.err = err
}

// storeClaudeWireCommands records one probe's command list.
//
// Silent on success and on an unreadable array alike: unlike the model
// catalog, there is no hand-maintained list for the wire to disagree with, so
// there is no drift to report to a maintainer — and a user who never opens a
// command menu is not affected by either outcome.
func (a *App) storeClaudeWireCommands(key provider.ProbeCacheKey, commands []provider.SlashCommand, wireErr error) {
	claudeCommandCache().Store(key, commands, wireErr)
}

// ClaudeSlashCommands is the wire shape of GetClaudeSlashCommands.
//
// Probed is the field that carries the nil-vs-empty distinction a JSON array
// cannot: false means NO PROBE HAS ANSWERED for the active Claude identity, so
// the list is unknown rather than empty, and a command menu must not render it
// as "this binary has none". Commands is always an array on the wire so the
// two facts stay on separate fields instead of overloading `null`.
type ClaudeSlashCommands struct {
	Probed   bool                    `json:"probed"`
	Commands []provider.SlashCommand `json:"commands"`
}

// GetClaudeSlashCommands returns the provider-executed slash commands the last
// zero-token account probe reported for the active Claude identity, with their
// descriptions and argument hints.
//
// This is the RICH half of a Claude thread's command menu and it is available
// before any session exists — the probe runs at startup, so a composer can seed
// its menu on a cold thread. It is NOT the whole answer: the probe answers for a
// probe identity, not for this thread's session, and only the live per-thread
// `provider:commands` frames carry MCP prompt commands (`mcp__server__prompt`).
// The frontend unions the two; deliberately not unioned here, because this
// method has no thread and triage may not reach into a provider-specific cache
// (see internal/triage/provider_commands.go).
//
// Never spawns — a pure read of what a probe already left behind.
func (a *App) GetClaudeSlashCommands() ClaudeSlashCommands {
	commands, probed := claudeCommandCache().AnswerFor(a.claudeProbeModelKey())
	return ClaudeSlashCommands{Probed: probed, Commands: slicesx.OrEmpty(commands)}
}
