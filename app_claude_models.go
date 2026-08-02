package main

import (
	"log"
	"sync"

	"agent-overflow/internal/claudemodels"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
)

// claudeModelCatalog holds the picker catalog enriched from the Claude CLI's
// own model list, which rides along on the `initialize` response of the
// zero-token account probe (claude.ProbeConfig.OnModels).
//
// Package-level for the same reason claudeProbeCache is: the answer depends on
// the binary, the installed credentials, and the custom environment — never on
// App configuration — and both are filled by the same probe, so splitting
// their lifetimes across App instances would let one hold an answer the other
// cannot see. Same mutex-not-sync.Once construction so a test can swap it.
var (
	claudeModelCatalogMu sync.Mutex
	claudeModelCatalogV  *claudemodels.Catalog
)

func claudeModelCatalog() *claudemodels.Catalog {
	claudeModelCatalogMu.Lock()
	defer claudeModelCatalogMu.Unlock()
	if claudeModelCatalogV == nil {
		claudeModelCatalogV = claudemodels.NewCatalog()
	}
	return claudeModelCatalogV
}

// resetClaudeModelCatalogForTest swaps the package-level catalog for a fresh
// instance. Called by resetClaudeProbeCacheForTest rather than by tests
// directly — the two caches are filled by one probe, so they are reset
// together or one of them serves another test's answer.
func resetClaudeModelCatalogForTest() {
	claudeModelCatalogMu.Lock()
	defer claudeModelCatalogMu.Unlock()
	claudeModelCatalogV = claudemodels.NewCatalog()
}

// claudeProbeModelKey is the identity a Claude model list is stored and read
// under: the account probe's own cache key, so the enrichment can never
// outlive the binary, account, workdir, or environment that produced it.
//
// Always Claude's identity, including when the caller asked on claude-tui's
// behalf. One binary, one login, one probe — claude-tui never probes under its
// own name, so asking the account store for it would resolve an empty account
// id and key a second, permanently empty slot (the same fold
// afterProviderCustomEnvChange makes for the probe caches).
func (a *App) claudeProbeModelKey() provider.ProbeCacheKey {
	binary := a.providerBinaryPath(string(provider.Claude))
	return a.providerProbeCacheKey(string(provider.Claude), binary)
}

// claudeProbeModels holds what claude.ProbeConfig.OnModels reported, so the
// caller can decide what to do with it once the probe has actually succeeded.
//
// reported distinguishes "the probe ran and the CLI said nothing about models"
// (an answer that must clear a previous list) from "no probe ran at all" (a
// cache hit, which must leave the stored list alone).
type claudeProbeModels struct {
	reported bool
	models   []claude.WireModel
	err      error
}

func (c *claudeProbeModels) capture(models []claude.WireModel, err error) {
	c.reported = true
	c.models = models
	c.err = err
}

// storeClaudeWireModels records one probe's model list and logs any drift
// between it and the shipped catalog.
//
// The log line is the whole feedback loop for the hand-maintained catalog:
// a capability we have wrong, or a model the CLI ships that we had to
// family-default, shows up here once per distinct report. Deliberately not a
// toast — none of it is actionable by the user, and none of it degrades the
// session they are trying to start.
func (a *App) storeClaudeWireModels(key provider.ProbeCacheKey, models []claude.WireModel, wireErr error) {
	if drift := claudeModelCatalog().Store(key, models, wireErr); len(drift) > 0 {
		log.Printf("claude model catalog: %s", claudemodels.FormatDrift(drift))
	}
}

// claudeModelsForProvider returns the picker catalog for claude / claude-tui.
// Never spawns: it reads what the last probe left behind, falling back to the
// shipped catalog until one has run.
func (a *App) claudeModelsForProvider(providerName string) []provider.ModelInfo {
	return claudeModelCatalog().ModelsFor(a.claudeProbeModelKey(), providerName)
}
