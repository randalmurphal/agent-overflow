package provideraccountapp

import (
	"context"
	"encoding/json"
	"errors"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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
	return signInManagerWith(t, dir, t.TempDir(), CredentialPolicy())
}

// signInManagerWith is signInManagerAt over userHome with policy.
func signInManagerWith(
	t *testing.T,
	dir, userHome string,
	policy provideraccounts.Policy,
) (*Manager, *provideraccounts.Store, *provideraccounts.Credentials) {
	t.Helper()
	kerneltest.IsolateSpawns(t)
	store, err := provideraccounts.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := provideraccounts.NewCredentialsWithFileKeychain(userHome, policy)
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

// slotWriteWatch observes the metadata file on disk at the moment each slot
// is about to receive credential bytes. Every slot write consults the
// credential policy's ChainPosition just before it stores the bytes, after
// the slot directory exists, so the watch hooks that. A slot directory that
// the file then names neither as an account nor as a reservation is a slot
// a crash at that moment would leave with no record.
type slotWriteWatch struct {
	t          *testing.T
	dir        string
	userHome   string
	mu         sync.Mutex
	seen       map[string]bool
	violations []string
}

func newSlotWriteWatch(t *testing.T, dir, userHome string) *slotWriteWatch {
	return &slotWriteWatch{t: t, dir: dir, userHome: userHome, seen: make(map[string]bool)}
}

func (w *slotWriteWatch) policy() provideraccounts.Policy {
	policy := CredentialPolicy()
	position := policy.ChainPosition
	policy.ChainPosition = func(providerName string, data []byte) (int64, bool) {
		w.check(providerName)
		return position(providerName, data)
	}
	return policy
}

func (w *slotWriteWatch) check(providerName string) {
	recorded := make(map[string]bool)
	data, err := os.ReadFile(filepath.Join(w.dir, "provider-accounts.json"))
	if err != nil && !os.IsNotExist(err) {
		w.t.Error(err)
		return
	}
	if err == nil {
		var state struct {
			Providers map[string]struct {
				Accounts      []struct{ ID string }      `json:"accounts"`
				ReservedSlots map[string]json.RawMessage `json:"reservedSlots"`
			} `json:"providers"`
		}
		if err := json.Unmarshal(data, &state); err != nil {
			w.t.Error(err)
			return
		}
		for _, account := range state.Providers[providerName].Accounts {
			recorded[account.ID] = true
		}
		for id := range state.Providers[providerName].ReservedSlots {
			recorded[id] = true
		}
	}
	entries, err := os.ReadDir(filepath.Join(w.userHome, "."+providerName, "agent-overflow-accounts"))
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		w.t.Error(err)
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, entry := range entries {
		w.seen[entry.Name()] = true
		if !recorded[entry.Name()] {
			w.violations = append(w.violations, entry.Name())
		}
	}
}

// requireRecordedAt fails unless the watch saw accountID's slot written and
// every slot it saw was on record on disk at that moment.
func (w *slotWriteWatch) requireRecordedAt(accountID string) {
	w.t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.seen[accountID] {
		w.t.Fatalf("the watch never saw slot %s written (saw %v)", accountID, w.seen)
	}
	if len(w.violations) > 0 {
		w.t.Fatalf("slots written with no account or reservation on disk: %v", w.violations)
	}
}

// Each of the four places that mint a new account writes its slot only once
// the reservation for it is on disk.
func TestEveryNewAccountIsReservedOnDiskBeforeItsSlotIsWritten(t *testing.T) {
	claudeName := string(provider.Claude)
	sessionEnd := time.Now().Add(30 * 24 * time.Hour)
	for _, tc := range []struct {
		name string
		run  func(t *testing.T, manager *Manager, store *provideraccounts.Store, credentials *provideraccounts.Credentials) string
	}{
		{name: "first sign-in", run: func(t *testing.T, manager *Manager, _ *provideraccounts.Store, credentials *provideraccounts.Credentials) string {
			result, err := manager.adoptLogin(context.Background(), completedLogin(t, manager, credentials, claudeLoginAs("first@example.com", "first", sessionEnd)))
			if err != nil {
				t.Fatal(err)
			}
			return result.ID
		}},
		{name: "sign-in beside the selected account", run: func(t *testing.T, manager *Manager, store *provideraccounts.Store, credentials *provideraccounts.Credentials) string {
			seedClaudeAccounts(t, manager, store, credentials, claudeLogin("current", sessionEnd), "other", claudeLogin("other", sessionEnd))
			result, err := manager.adoptLogin(context.Background(), completedLogin(t, manager, credentials, claudeLoginAs("new@example.com", "new", sessionEnd)))
			if err != nil {
				t.Fatal(err)
			}
			return result.ID
		}},
		{name: "adopting a canonical login with no email", run: func(t *testing.T, manager *Manager, _ *provideraccounts.Store, credentials *provideraccounts.Credentials) string {
			login := claudeLoginAs("first@example.com", "first", sessionEnd)
			if err := credentials.WriteNativeCredentialForTest(claudeName, login); err != nil {
				t.Fatal(err)
			}
			snapshot := provideraccounts.NewCredentialSnapshot(login)
			account, _, _, err := manager.adoptCurrentProviderAccount(claudeName, provider.AccountInfo{}, &snapshot)
			if err != nil {
				t.Fatal(err)
			}
			return account.ID
		}},
		{name: "adopting the canonical login at a probe", run: func(t *testing.T, manager *Manager, _ *provideraccounts.Store, credentials *provideraccounts.Credentials) string {
			if err := credentials.WriteNativeCredentialForTest(claudeName, claudeLoginAs("first@example.com", "first", sessionEnd)); err != nil {
				t.Fatal(err)
			}
			manager.mu.Lock()
			signIn, err := manager.adoptCanonicalProviderAccountLocked(claudeName, manager.providerBinaryPath(claudeName))
			manager.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			return signIn.Account.ID
		}},
		{name: "reconciling an outside login", run: func(t *testing.T, manager *Manager, store *provideraccounts.Store, credentials *provideraccounts.Credentials) string {
			seedClaudeAccounts(t, manager, store, credentials, claudeLogin("current", sessionEnd), "other", claudeLogin("other", sessionEnd))
			login := claudeLoginAs("outside@example.com", "outside", sessionEnd)
			if err := credentials.WriteNativeCredentialForTest(claudeName, login); err != nil {
				t.Fatal(err)
			}
			manager.mu.Lock()
			account, _, _, err := manager.reconcileObservedAccountLocked(claudeName,
				provider.AccountInfo{Email: "outside@example.com", OrgID: testClaudeOrgID}, provideraccounts.NewCredentialSnapshot(login))
			manager.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			return account.ID
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, userHome := t.TempDir(), t.TempDir()
			watch := newSlotWriteWatch(t, dir, userHome)
			manager, store, credentials := signInManagerWith(t, dir, userHome, watch.policy())
			accountID := tc.run(t, manager, store, credentials)
			watch.requireRecordedAt(accountID)
			if got := reservedIDs(t, dir, claudeName); len(got) != 0 {
				t.Fatalf("reservations after the account was saved = %v, want none", got)
			}
		})
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

	_, err := manager.adoptLogin(context.Background(), completedLogin(t, manager, credentials, fresh))
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
	_, err := manager.adoptLogin(context.Background(), attempt)
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
