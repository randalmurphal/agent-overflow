package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"agent-overflow/internal/provideraccounts"
)

// The boot's steps before clients, run as the boot runs them, remove the
// slot of this data root's unfinished sign-in, keep another root's slot on
// the same home, and settle before the step that stamps the account metadata
// with its provider home and sweeps the ephemeral homes.
func TestTheBootSettlesUnfinishedSignInsBeforeItStampsTheProviderHome(t *testing.T) {
	a := newBootTestApp(t)
	home, err := a.providerHome()
	if err != nil {
		t.Fatal(err)
	}
	dbDir := filepath.Join(a.dataDirOverride, "agent-overflow")
	if err := os.MkdirAll(dbDir, 0o700); err != nil {
		t.Fatal(err)
	}
	seeded, err := provideraccounts.NewStore(dbDir)
	if err != nil {
		t.Fatal(err)
	}
	unfinished, err := seeded.ReserveAccountID("codex", filepath.Join(home, ".codex"))
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := provideraccounts.NewCredentialsWithFileKeychain(home, providerCredentialPolicy())
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{unfinished, "another-roots-login"} {
		if err := credentials.WriteAccountCredential("codex", id, []byte(`{"account":"`+id+`"}`)); err != nil {
			t.Fatal(err)
		}
	}
	stamped := func() bool {
		data, err := os.ReadFile(filepath.Join(dbDir, "provider-accounts.json"))
		if err != nil {
			t.Fatal(err)
		}
		var state struct {
			ProviderHome string `json:"providerHome"`
		}
		if err := json.Unmarshal(data, &state); err != nil {
			t.Fatal(err)
		}
		return state.ProviderHome != ""
	}
	settled := func() bool {
		_, err := credentials.ReadCredential("codex", unfinished, false)
		if err != nil && !provideraccounts.IsCredentialMissing(err) {
			t.Fatal(err)
		}
		return err != nil
	}

	ParkUnattendedWork(a)
	t.Cleanup(func() {
		if err := a.Shutdown(context.Background()); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if settled() || stamped() {
		t.Fatal("a parked boot touched the provider accounts before its steps ran")
	}
	settledAfter, stampedAfter := -1, -1
	for i, step := range a.startBeforeClients() {
		if err := step(); err != nil {
			t.Fatalf("boot step %d: %v", i, err)
		}
		if settledAfter < 0 && settled() {
			settledAfter = i
		}
		if stampedAfter < 0 && stamped() {
			stampedAfter = i
		}
	}
	if settledAfter < 0 {
		t.Fatal("no boot step removed the unfinished sign-in's slot")
	}
	if stampedAfter < 0 {
		t.Fatal("no boot step stamped the account metadata with its provider home")
	}
	if settledAfter >= stampedAfter {
		t.Fatalf("the unfinished sign-in was settled by step %d, not before the stamp and sweep in step %d", settledAfter, stampedAfter)
	}
	if _, err := credentials.ReadCredential("codex", "another-roots-login", false); err != nil {
		t.Fatalf("the boot deleted another data root's slot: %v", err)
	}
}
