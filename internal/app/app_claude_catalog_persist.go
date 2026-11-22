package app

import (
	"log"
	"time"

	"agent-overflow/internal/claudecatalog"
	"agent-overflow/internal/claudemodels"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provideraccounts"
)

// rememberClaudeCatalog persists what one Claude probe reported, stamped with
// the probe identity that reported it (key, the adopted account included) and
// the file its binary resolved to.
//
// The stamp is the whole point: a model list is a claim about a binary in one
// environment, so a record that cannot name the file and the identity it came
// from could never be validated at boot and is not written at all. A write
// failure is logged and swallowed: the probe it hangs off has already
// delivered the live answer, and failing it over a cache that only speeds up
// the NEXT boot would trade a working session for a picker optimization.
func (a *App) rememberClaudeCatalog(key provider.ProbeCacheKey, snapshot claudemodels.Snapshot) {
	if a.providerAccounts == nil || key.AccountID == "" {
		return
	}
	providerName := string(provider.Claude)
	identity, ok := resolveBinaryIdentity(providerName, key.Binary)
	if !ok {
		log.Printf(
			"claude model catalog: not persisting for account %s: cannot resolve the binary at %q",
			key.AccountID, key.Binary,
		)
		return
	}
	record := provideraccounts.ClaudeCatalogRecordFor(key)
	record.ResolvedPath = identity.path
	record.Size = identity.size
	record.ModUnixNano = identity.modUnixNano
	record.ProbedAt = time.Now().UnixMilli()
	record.Wire = snapshot.Wire
	record.Learned = snapshot.Learned
	if err := a.providerAccounts.RememberClaudeCatalog(key.AccountID, record); err != nil {
		log.Printf("claude model catalog: persist for account %s: %v", key.AccountID, err)
	}
}

// seedClaudeCatalogFromAccounts restores the persisted Claude model answers
// before anything can read the picker.
//
// Without it every cold start serves the shipped list until the boot probe
// answers seconds later, which is indistinguishable on the wire from a binary
// that genuinely no longer offers a model.
//
// A record is served only when it still describes the probe this app would
// run: the same probe identity (configured binary, work directory and custom
// environment) AND the same resolved file, size and mtime. A mismatch is
// silent, because it is the normal state after a CLI upgrade, a reinstall or
// an environment edit, and the boot probe replaces the record moments later.
//
// The active account is seeded LAST so it is the newest entry, which is what
// claudemodels' same-binary fallback serves to an identity that has no entry
// of its own.
func (a *App) seedClaudeCatalogFromAccounts() {
	if a.providerAccounts == nil {
		return
	}
	providerName := string(provider.Claude)
	binary := a.providerBinaryPath(providerName)
	if binary == "" {
		return
	}
	identity, ok := a.resolveProviderBinaryIdentity(providerName)
	if !ok {
		return
	}
	accounts := a.providerAccounts.MetadataAccounts(providerName)
	active, hasActive := a.providerAccounts.ActiveAccount(providerName)

	seed := func(accountID string) {
		record, ok := a.providerAccounts.ClaudeCatalog(accountID)
		key := a.providerProbeCacheKeyForAccount(providerName, binary, accountID)
		if !ok ||
			record.ProbeKey(accountID) != key ||
			record.ResolvedPath != identity.path ||
			record.Size != identity.size ||
			record.ModUnixNano != identity.modUnixNano {
			return
		}
		claudecatalog.Seed(key, claudemodels.Snapshot{Wire: record.Wire, Learned: record.Learned})
	}
	for _, account := range accounts {
		if hasActive && account.ID == active.ID {
			continue
		}
		seed(account.ID)
	}
	if hasActive {
		seed(active.ID)
	}
}
