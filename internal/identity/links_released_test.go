package identity

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"agent-overflow/internal/owndevices"
	"agent-overflow/internal/store"
)

// A paired computer's owned spawn is linked to its thread here through the
// device that made it, and a session end that leaves that device no way in
// releases the link in its own write (store.endSessionsTx). Every such end
// hands the released threads to one function (OnLinksReleased), so the app
// can give them their sidebar rows; none drops them.

// linkThread makes a thread here that a spawn from device links, and
// returns its id.
func linkThread(t *testing.T, st *store.Store, deviceID string) string {
	t.Helper()
	id := uuid.NewString()
	if err := st.CreateThread(store.Thread{ID: id, Title: "worker", Provider: "claude", WorkspacePath: t.TempDir(), CreatedAt: 1, UpdatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	token := uuid.NewString()
	if _, _, err := st.AcceptThreadRequestReceipt(store.ThreadRequestReceipt{
		Token: token, OwnerDeviceID: deviceID, SourceComputerID: "desk", SourceThreadID: "lead",
		Kind: store.ThreadRequestSpawn, SpawnLink: true, CreatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if named, _, err := st.SetThreadReceiptTarget(token, id, true); err != nil || !named {
		t.Fatalf("name the linked thread: %v, %v", named, err)
	}
	receipt, found, err := st.GetThreadRequestReceipt(token)
	if err != nil || !found || !receipt.SpawnLink {
		t.Fatalf("the link from %s was not written: %+v, %v, %v", deviceID, receipt, found, err)
	}
	return id
}

// recordReleases installs a hook that keeps every released thread.
func recordReleases(s *Sessions) func() []string {
	var mu sync.Mutex
	var released []string
	s.OnLinksReleased(func(threadIDs []string) {
		mu.Lock()
		defer mu.Unlock()
		released = append(released, threadIDs...)
	})
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(released)
	}
}

func TestEverySessionEndThatReleasesALinkHandsItOver(t *testing.T) {
	t.Run("revoking the device's last session", func(t *testing.T) {
		s, st, _, owner, device := newFixture(t)
		released := recordReleases(s)
		first, _ := mustMint(t, s, owner, device, time.Hour)
		second, _ := mustMint(t, s, owner, device, time.Hour)
		worker := linkThread(t, st, device.ID)
		if _, err := s.RevokeSession(first.ID); err != nil {
			t.Fatal(err)
		}
		if got := released(); len(got) != 0 {
			t.Fatalf("revoking one of two sessions released %v", got)
		}
		if _, err := s.RevokeSession(second.ID); err != nil {
			t.Fatal(err)
		}
		if got := released(); !slices.Equal(got, []string{worker}) {
			t.Fatalf("revoking the last session released %v, want %s", got, worker)
		}
	})
	t.Run("revoking the device", func(t *testing.T) {
		s, st, _, owner, device := newFixture(t)
		released := recordReleases(s)
		mustMint(t, s, owner, device, time.Hour)
		worker := linkThread(t, st, device.ID)
		if _, err := s.RevokeDevice(device.ID); err != nil {
			t.Fatal(err)
		}
		if got := released(); !slices.Equal(got, []string{worker}) {
			t.Fatalf("revoking the device released %v, want %s", got, worker)
		}
	})
	t.Run("pruning an expired session", func(t *testing.T) {
		s, st, c, owner, device := newFixture(t)
		released := recordReleases(s)
		mustMint(t, s, owner, device, time.Hour)
		worker := linkThread(t, st, device.ID)
		c.at = c.at.Add(48 * time.Hour)
		s.PruneCredentials(time.Hour)
		if got := released(); !slices.Equal(got, []string{worker}) {
			t.Fatalf("pruning the expired session released %v, want %s", got, worker)
		}
	})
	t.Run("a merge removing a member", func(t *testing.T) {
		s, st, _, owner, _ := newFixture(t)
		released := recordReleases(s)
		member := newSigningDevice(t)
		paired := personalPair(t, s, owner, member)
		session, err := st.GetSession(paired.Tokens.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		worker := linkThread(t, st, session.DeviceID)
		row, err := st.OwnDevice(member.thumbprint())
		if err != nil {
			t.Fatal(err)
		}
		row.Removed = true
		if _, err := s.MergeOwnDevices([]owndevices.Member{row}); err != nil {
			t.Fatal(err)
		}
		if got := released(); !slices.Equal(got, []string{worker}) {
			t.Fatalf("removing the member released %v, want %s", got, worker)
		}
	})
}

// linkReleasingWrites are the store writes that end sessions and return the
// threads whose links they released, with that result's position.
var linkReleasingWrites = map[string]int{
	"RevokeSession":               1,
	"RevokeDevice":                0, // DeviceRevocation.ReleasedThreads
	"DeleteDevice":                1,
	"MergeOwnDevices":             2,
	"RetireOwnIntroductions":      1,
	"DeleteSessionsExpiredBefore": 1,
	"RetirePendingPairings":       0,
}

// No caller outside the store drops what a session end released: every
// call to one of those writes, here and in the app, keeps the released
// threads in a variable (which the compiler then requires it to use), and
// only this package makes such a call.
func TestNoCallerDropsTheThreadsASessionEndReleased(t *testing.T) {
	calls := 0
	for _, dir := range []string{".", filepath.Join("..", "app")} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			name := entry.Name()
			if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			path := filepath.Join(dir, name)
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			ast.Inspect(file, func(node ast.Node) bool {
				assign, isAssign := node.(*ast.AssignStmt)
				var call *ast.CallExpr
				if isAssign && len(assign.Rhs) == 1 {
					call, _ = assign.Rhs[0].(*ast.CallExpr)
				}
				if expr, ok := node.(*ast.ExprStmt); ok {
					call, _ = expr.X.(*ast.CallExpr)
				}
				if call == nil {
					return true
				}
				selector, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				position, releases := linkReleasingWrites[selector.Sel.Name]
				if !releases || !storeReceiver(selector.X) {
					return true
				}
				calls++
				if dir != "." {
					t.Errorf("%s calls store.%s outside the session core, which hands what it released to OnLinksReleased", path, selector.Sel.Name)
					return true
				}
				if !isAssign || len(assign.Lhs) <= position {
					t.Errorf("%s drops what store.%s released", path, selector.Sel.Name)
					return true
				}
				if ident, ok := assign.Lhs[position].(*ast.Ident); ok && ident.Name == "_" {
					t.Errorf("%s discards what store.%s released", path, selector.Sel.Name)
				}
				return true
			})
		}
	}
	if calls < len(linkReleasingWrites) {
		t.Fatalf("found %d calls of the %d link-releasing writes; the scan no longer sees them", calls, len(linkReleasingWrites))
	}
}

// storeReceiver reports whether x names a store: s.store, a.store or st.
func storeReceiver(x ast.Expr) bool {
	switch x := x.(type) {
	case *ast.SelectorExpr:
		return x.Sel.Name == "store"
	case *ast.Ident:
		return x.Name == "st" || x.Name == "store"
	}
	return false
}
