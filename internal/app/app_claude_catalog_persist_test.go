package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provideraccounts"
)

// seedTestApp builds an app whose Claude binary setting points at path, with
// one saved Claude account carrying record. No provider is ever spawned: the
// "binary" is an ordinary temporary file, and nothing here probes.
func seedTestApp(
	t *testing.T,
	binary string,
	record *provideraccounts.ClaudeCatalogRecord,
) *App {
	t.Helper()
	resetClaudeProbeCacheForTest()
	t.Cleanup(resetClaudeProbeCacheForTest)
	app := newTestAppWithStore(t)
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatalf("set claude binary: %v", err)
	}
	accounts, err := provideraccounts.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := accounts.UpsertAndActivate(provideraccounts.Account{
		ID:       "acct-1",
		Provider: string(provider.Claude),
		Email:    "acct-1@example.com",
	}); err != nil {
		t.Fatal(err)
	}
	if record != nil {
		if err := accounts.RememberClaudeCatalog("acct-1", *record); err != nil {
			t.Fatal(err)
		}
	}
	attachProviderAccountStoresForTest(t, app, accounts, newTestProviderCredentials(t, t.TempDir()))
	return app
}

// writeFakeProviderBinaryFile writes a file that stands in for a provider
// binary for identity purposes only. It is never executed.
func writeFakeProviderBinaryFile(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(path, []byte(contents), 0o755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}
	return path
}

func identityRecordFor(t *testing.T, app *App, binary string) provideraccounts.ClaudeCatalogRecord {
	t.Helper()
	identity, ok := app.resolveProviderBinaryIdentity(string(provider.Claude))
	if !ok {
		t.Fatalf("could not resolve the fake binary at %s", binary)
	}
	return provideraccounts.ClaudeCatalogRecord{
		Binary:       binary,
		ResolvedPath: identity.path,
		Size:         identity.size,
		ModUnixNano:  identity.modUnixNano,
		ProbedAt:     1700000000000,
		Wire:         []claude.WireModel{{Value: "claude-newthing-1", DisplayName: "Newthing"}},
	}
}

// The cold-start case this whole record exists for: the picker answers with
// the previous process's enriched catalog, and says it is a probed answer,
// before any probe of this process has run.
func TestSeedClaudeCatalogServesThePersistedAnswerBeforeAnyProbe(t *testing.T) {
	binary := writeFakeProviderBinaryFile(t, "#!/bin/sh\nexit 0\n")
	app := seedTestApp(t, binary, nil)
	record := identityRecordFor(t, app, binary)
	if err := app.providerAccounts.RememberClaudeCatalog("acct-1", record); err != nil {
		t.Fatal(err)
	}

	before, err := app.GetModelsForProvider(string(provider.Claude))
	if err != nil {
		t.Fatal(err)
	}
	if before.Provenance != provider.CatalogShipped {
		t.Fatalf("provenance before the seed = %q, want shipped", before.Provenance)
	}

	app.seedClaudeCatalogFromAccounts()

	after, err := app.GetModelsForProvider(string(provider.Claude))
	if err != nil {
		t.Fatal(err)
	}
	if after.Provenance != provider.CatalogProbed {
		t.Errorf("provenance after the seed = %q, want probed", after.Provenance)
	}
	if !slices.Contains(modelSlugs(after.Models), "claude-newthing-1") {
		t.Errorf("catalog = %v, want the persisted wire-only model", modelSlugs(after.Models))
	}
	// The shipped list is still whole: a restored answer is an enrichment,
	// not a replacement.
	for _, shipped := range modelSlugs(provider.ClaudeModels) {
		if !slices.Contains(modelSlugs(after.Models), shipped) {
			t.Errorf("%s dropped out of the seeded catalog", shipped)
		}
	}
}

// An upgrade or reinstall between two runs leaves a record describing software
// that is gone. Serving it would put the OLD binary's models in the picker of
// the new one, which is the subtraction rule (DropBinary) read backwards.
func TestSeedClaudeCatalogSkipsAChangedBinary(t *testing.T) {
	binary := writeFakeProviderBinaryFile(t, "#!/bin/sh\nexit 0\n")
	app := seedTestApp(t, binary, nil)
	record := identityRecordFor(t, app, binary)
	record.Size += 1 // The file on disk is no longer the one that reported.
	if err := app.providerAccounts.RememberClaudeCatalog("acct-1", record); err != nil {
		t.Fatal(err)
	}

	app.seedClaudeCatalogFromAccounts()

	answer, err := app.GetModelsForProvider(string(provider.Claude))
	if err != nil {
		t.Fatal(err)
	}
	if answer.Provenance != provider.CatalogShipped {
		t.Errorf("provenance = %q, want shipped for a stale record", answer.Provenance)
	}
	if slices.Contains(modelSlugs(answer.Models), "claude-newthing-1") {
		t.Errorf("catalog = %v, want no model from the stale record", modelSlugs(answer.Models))
	}
}

// The configured path is a key dimension, not a detail: a record captured
// against a different `claudeBinaryPath` describes another installation, and
// the same-binary fallback would otherwise hand its models to this one.
func TestSeedClaudeCatalogSkipsADifferentConfiguredBinary(t *testing.T) {
	binary := writeFakeProviderBinaryFile(t, "#!/bin/sh\nexit 0\n")
	app := seedTestApp(t, binary, nil)
	record := identityRecordFor(t, app, binary)
	record.Binary = filepath.Join(t.TempDir(), "other-claude")
	if err := app.providerAccounts.RememberClaudeCatalog("acct-1", record); err != nil {
		t.Fatal(err)
	}

	app.seedClaudeCatalogFromAccounts()

	answer, err := app.GetModelsForProvider(string(provider.Claude))
	if err != nil {
		t.Fatal(err)
	}
	if answer.Provenance != provider.CatalogShipped {
		t.Errorf("provenance = %q, want shipped for another installation's record", answer.Provenance)
	}
	if slices.Contains(modelSlugs(answer.Models), "claude-newthing-1") {
		t.Errorf("catalog = %v, want no model from another installation", modelSlugs(answer.Models))
	}
}

// A missing binary is a normal state (the provider is not installed), not a
// failure to report: the seed does nothing and the picker still answers.
func TestSeedClaudeCatalogWithNoResolvableBinary(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "not-installed", "claude")
	app := seedTestApp(t, missing, nil)
	if _, err := exec.LookPath(missing); err == nil {
		t.Fatal("the fixture path must not resolve")
	}
	app.seedClaudeCatalogFromAccounts()

	answer, err := app.GetModelsForProvider(string(provider.Claude))
	if err != nil {
		t.Fatal(err)
	}
	if answer.Provenance != provider.CatalogShipped {
		t.Errorf("provenance = %q, want shipped", answer.Provenance)
	}
	if len(answer.Models) == 0 {
		t.Error("an uninstalled provider must still have a shipped picker")
	}
}

// The write half, end to end: a probe that enriches the catalog leaves a
// record stamped with the binary that produced it, so the next process can
// validate it before serving it.
func TestClaudeProbePersistsTheCatalogAgainstItsBinary(t *testing.T) {
	binary := writeProbeMockBinaryWithModels(t,
		`{"subscriptionType":"Claude Max","emailAddress":"person@example.com"}`,
		`[{"value":"claude-newthing-1","displayName":"Newthing"}]`,
	)
	app := seedTestApp(t, binary, nil)

	if _, err := app.ProbeClaudeAccount(); err != nil {
		t.Fatalf("ProbeClaudeAccount: %v", err)
	}

	var record provideraccounts.ClaudeCatalogRecord
	var found bool
	for _, account := range app.providerAccounts.MetadataAccounts(string(provider.Claude)) {
		if saved, ok := app.providerAccounts.ClaudeCatalog(account.ID); ok {
			record, found = saved, true
		}
	}
	if !found {
		t.Fatal("the probe left no persisted catalog for any account")
	}
	identity, ok := app.resolveProviderBinaryIdentity(string(provider.Claude))
	if !ok {
		t.Fatal("could not resolve the probe binary")
	}
	if record.Binary != binary || record.ResolvedPath != identity.path ||
		record.Size != identity.size || record.ModUnixNano != identity.modUnixNano {
		t.Errorf("record identity = %+v, want the probed binary %s", record, identity.path)
	}
	if record.ProbedAt <= 0 {
		t.Errorf("ProbedAt = %d, want a capture time", record.ProbedAt)
	}
	if len(record.Wire) != 1 || record.Wire[0].Value != "claude-newthing-1" {
		t.Errorf("record wire = %+v, want the probe's rows", record.Wire)
	}
}

// The whole point, in one test: what one process's probe learned is what the
// next process serves from its first answer, without probing again.
func TestPersistedClaudeCatalogSurvivesARestart(t *testing.T) {
	binary := writeProbeMockBinaryWithModels(t,
		`{"subscriptionType":"Claude Max","emailAddress":"person@example.com"}`,
		`[{"value":"claude-newthing-1","displayName":"Newthing"}]`,
	)
	accountDir := t.TempDir()
	credentialHome := t.TempDir()

	newAppOn := func(t *testing.T) *App {
		t.Helper()
		app := newTestAppWithStore(t)
		if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
			t.Fatalf("set claude binary: %v", err)
		}
		accounts, err := provideraccounts.NewStore(accountDir)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := accounts.UpsertAndActivate(provideraccounts.Account{
			ID: "acct-1", Provider: string(provider.Claude), Email: "person@example.com",
		}); err != nil {
			t.Fatal(err)
		}
		attachProviderAccountStoresForTest(t, app, accounts, newTestProviderCredentials(t, credentialHome))
		return app
	}

	resetClaudeProbeCacheForTest()
	t.Cleanup(resetClaudeProbeCacheForTest)
	first := newAppOn(t)
	if _, err := first.ProbeClaudeAccount(); err != nil {
		t.Fatalf("ProbeClaudeAccount: %v", err)
	}

	// A restart: new App, new in-process catalog, same metadata on disk.
	resetClaudeProbeCacheForTest()
	second := newAppOn(t)
	cold, err := second.GetModelsForProvider(string(provider.Claude))
	if err != nil {
		t.Fatal(err)
	}
	if cold.Provenance != provider.CatalogShipped {
		t.Fatalf("a fresh process must start un-enriched, got %q", cold.Provenance)
	}

	second.seedClaudeCatalogFromAccounts()

	answer, err := second.GetModelsForProvider(string(provider.Claude))
	if err != nil {
		t.Fatal(err)
	}
	if answer.Provenance != provider.CatalogProbed {
		t.Errorf("provenance after the restart = %q, want probed", answer.Provenance)
	}
	if !slices.Contains(modelSlugs(answer.Models), "claude-newthing-1") {
		t.Errorf("catalog = %v, want the model the previous process learned", modelSlugs(answer.Models))
	}
}
