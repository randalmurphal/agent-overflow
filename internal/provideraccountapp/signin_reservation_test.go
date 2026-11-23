package provideraccountapp

import (
	"errors"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/kerneltest"
	"agent-overflow/internal/procutil/procutiltest"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/claude/claudefake"
	"agent-overflow/internal/provideraccounts"
)

// signInManagerAt is signInManager over a metadata store in dir.
func signInManagerAt(t *testing.T, dir string) (*Manager, *provideraccounts.Store, *provideraccounts.Credentials) {
	t.Helper()
	kerneltest.IsolateSpawns(t)
	store, err := provideraccounts.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := provideraccounts.NewCredentialsWithFileKeychain(t.TempDir(), CredentialPolicy())
	if err != nil {
		t.Fatal(err)
	}
	probe := writeClaudeIdentityProbe(t, staleProbeEmail)
	manager := NewManager(Deps{
		Reaper:          procutiltest.Reaper(),
		ProviderBinary:  func(string) string { return probe },
		ClaudeRetention: func(string) claude.Retention { return claude.UserHomeRetention(0) },
		HTTPClient:      func() *http.Client { return &http.Client{Transport: claudefake.Transport{}} },
		Accounts:        &recordingAccountSink{},
	})
	if err := manager.Attach(store, credentials, ""); err != nil {
		t.Fatal(err)
	}
	return manager, store, credentials
}

// A new account's sign-in has its reservation on disk before its slot
// exists, and saving the account ends the reservation. The store's selection
// guard runs inside the row insert, after the slot write.
func TestANewAccountsSignInIsReservedBeforeItsSlotIsWritten(t *testing.T) {
	dir := t.TempDir()
	manager, store, credentials := signInManagerAt(t, dir)
	fresh := claudeLoginAs("first@example.com", "first", time.Now().Add(30*24*time.Hour))
	var reservedAtInsert []string
	store.GuardSelection(func(_, _, to string) error {
		reservedAtInsert = reservedIDs(t, dir, string(provider.Claude))
		if !slices.Contains(reservedAtInsert, to) {
			t.Errorf("the slot of %s was written with no reservation on disk (%v)", to, reservedAtInsert)
		}
		return nil
	})

	result, err := manager.adoptLogin(completedLogin(t, manager, credentials, fresh))
	if err != nil {
		t.Fatal(err)
	}
	if reservedAtInsert == nil {
		t.Fatal("the sign-in never inserted its row through the guarded write")
	}
	if !slotPresent(t, credentials, string(provider.Claude), result.ID) {
		t.Fatal("the new account has no slot")
	}
	if got := reservedIDs(t, dir, string(provider.Claude)); len(got) != 0 {
		t.Fatalf("reservations after the sign-in = %v, want none", got)
	}
}

// A sign-in that fails after reserving settles its own reservation: its
// slot is gone and nothing is left for the boot.
func TestAFailedSignInSettlesItsOwnReservation(t *testing.T) {
	dir := t.TempDir()
	manager, store, credentials := signInManagerAt(t, dir)
	fresh := claudeLoginAs("first@example.com", "first", time.Now().Add(30*24*time.Hour))
	refused := errors.New("refused for the test")
	store.GuardSelection(func(string, string, string) error { return refused })

	_, err := manager.adoptLogin(completedLogin(t, manager, credentials, fresh))
	if !errors.Is(err, refused) {
		t.Fatalf("adoptLogin error = %v, want the refused insert", err)
	}
	if got := reservedIDs(t, dir, string(provider.Claude)); len(got) != 0 {
		t.Fatalf("reservations after the failed sign-in = %v, want none", got)
	}
	slotRoot := filepath.Dir(filepath.Dir(mustAccountCredentialPath(t, credentials, "probe")))
	if names, err := os.ReadDir(slotRoot); err == nil && len(names) != 0 {
		t.Fatalf("slots after the failed sign-in = %d, want none", len(names))
	}
}

// A sign-in whose reservation cannot be saved fails before writing a slot.
func TestASignInWhoseReservationCannotBeSavedWritesNoSlot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	manager, store, credentials := signInManagerAt(t, dir)
	canonical := claudeLogin("current", time.Now().Add(20*24*time.Hour))
	seedClaudeAccounts(t, manager, store, credentials, canonical, "other", claudeLogin("other", time.Now().Add(20*24*time.Hour)))
	fresh := claudeLoginAs("new@example.com", "new", time.Now().Add(30*24*time.Hour))
	attempt := completedLogin(t, manager, credentials, fresh)
	slotRoot := filepath.Dir(filepath.Dir(mustAccountCredentialPath(t, credentials, "other")))
	before := slotNames(t, slotRoot)

	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	_, err := manager.adoptLogin(attempt)
	if err == nil || !strings.Contains(err.Error(), "reserve a new claude account id") {
		t.Fatalf("adoptLogin error = %v, want the reservation's failure", err)
	}
	if got := slotNames(t, slotRoot); !slices.Equal(got, before) {
		t.Fatalf("slots after the failed sign-in = %v, want %v", got, before)
	}
}

func mustAccountCredentialPath(t *testing.T, credentials *provideraccounts.Credentials, accountID string) string {
	t.Helper()
	path, err := credentials.AccountCredentialPath(string(provider.Claude), accountID)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func slotNames(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// reserveAccount is the only source of a new account id: an id minted
// anywhere else has no durable reservation, and a crash after its slot write
// leaves a slot nothing will ever delete. This package's production code may
// generate random values only for usage-reset spends.
func TestOnlyTheReservationMintsAccountIDs(t *testing.T) {
	generators := map[string]bool{
		`"github.com/google/uuid"`: true, `"crypto/rand"`: true, `"math/rand"`: true, `"math/rand/v2"`: true,
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") || name == "usage_resets.go" {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, spec := range file.Imports {
			if generators[spec.Path.Value] {
				t.Errorf("%s imports %s; mint account ids with reserveAccount", name, spec.Path.Value)
			}
		}
	}
}
