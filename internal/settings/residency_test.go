package settings

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// fakeTierStore is the ui_state table, minus SQLite. This package stays
// dependency-free, so it cannot take internal/store even in a test.
type fakeTierStore struct {
	scopes   map[string]map[string]string
	readErr  error
	writeErr error
}

func newFakeTierStore() *fakeTierStore {
	return &fakeTierStore{scopes: map[string]map[string]string{}}
}

func (f *fakeTierStore) GetUIState(scope string) (map[string]string, error) {
	if f.readErr != nil {
		return nil, f.readErr
	}
	return maps.Clone(f.scopes[scope]), nil
}

func (f *fakeTierStore) SetUIState(scope string, entries map[string]string) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	if f.scopes[scope] == nil {
		f.scopes[scope] = map[string]string{}
	}
	maps.Copy(f.scopes[scope], entries)
	return nil
}

const testBackendBucket = "client:backend-screen"

// tieredService is a Service with residency wired: a settings.json in a temp
// dir, a fake ui_state behind it, and the backend machine's own bucket.
func tieredService(t *testing.T) (*Service, *fakeTierStore) {
	t.Helper()
	svc := NewService(t.TempDir())
	store := newFakeTierStore()
	svc.AttachTierStore(store, testBackendBucket)
	return svc, store
}

func fileObject(t *testing.T, svc *Service) map[string]json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(svc.Path())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read settings file: %v", err)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		t.Fatalf("parse settings file: %v", err)
	}
	return object
}

// One patch, three destinations. The point of the whole wave: settings.json
// keeps only what configures this machine, the person's preferences land in
// the reserved user scope, and the screen's land in that screen's bucket.
func TestUpdateRoutesEachTierToItsOwnStorage(t *testing.T) {
	svc, store := tieredService(t)

	if _, err := svc.For("client:phone").Update(map[string]any{
		"retention":     map[string]any{"days": 7}, // host
		"confirmDelete": false,                     // user
		"fontSize":      17,                        // device
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	file := fileObject(t, svc)
	if _, ok := file["retention"]; !ok {
		t.Fatalf("settings.json is missing the host key: %v", slices.Sorted(maps.Keys(file)))
	}
	for _, key := range []string{"confirmDelete", "fontSize"} {
		if _, ok := file[key]; ok {
			t.Errorf("settings.json still holds the %s key", key)
		}
	}
	if got := store.scopes[UserScope]["confirmDelete"]; got != "false" {
		t.Errorf("%s[confirmDelete] = %q, want false", UserScope, got)
	}
	if got := store.scopes["client:phone"]["fontSize"]; got != "17" {
		t.Errorf("client:phone[fontSize] = %q, want 17", got)
	}
	if _, leaked := store.scopes[testBackendBucket]["fontSize"]; leaked {
		t.Error("the phone's font size landed in the backend's own bucket")
	}
}

// The device tier is resolved per caller. Two screens, one backend, two font
// sizes — and one shared answer for everything else.
func TestDeviceTierResolvesPerCaller(t *testing.T) {
	svc, _ := tieredService(t)

	if _, err := svc.For("client:phone").Update(map[string]any{"fontSize": 17}); err != nil {
		t.Fatalf("phone Update: %v", err)
	}
	if _, err := svc.For("client:desk").Update(map[string]any{"fontSize": 20, "confirmDelete": false}); err != nil {
		t.Fatalf("desk Update: %v", err)
	}

	if got := svc.For("client:phone").Get().FontSize; got != 17 {
		t.Errorf("phone FontSize = %d, want 17", got)
	}
	if got := svc.For("client:desk").Get().FontSize; got != 20 {
		t.Errorf("desk FontSize = %d, want 20", got)
	}
	// A caller with no device of its own reads the default, because it has no
	// screen whose preference it could be asking for.
	if got := svc.Get().FontSize; got != DefaultSettings.FontSize {
		t.Errorf("bucket-less FontSize = %d, want the default %d", got, DefaultSettings.FontSize)
	}
	// The user tier is shared: one write, both callers see it.
	for _, bucket := range []string{"client:phone", "client:desk"} {
		if svc.For(bucket).Get().ConfirmDelete {
			t.Errorf("%s still sees confirmDelete on", bucket)
		}
	}
}

// Validation is about the VALUE, not about where it is stored: every
// validator runs on the merged struct before any destination is chosen, so a
// device-tier key is refused exactly as a host-tier one is.
func TestValidatorsRunForEveryDestination(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		patch map[string]any
		scope string
		key   string
	}{
		{"device", map[string]any{"fontSize": 99}, "client:phone", "fontSize"},
		{"user", map[string]any{"defaultThreadEnvMode": "remote"}, UserScope, "defaultThreadEnvMode"},
		{"host", map[string]any{"retention": map[string]any{"days": -1}}, "", "retention"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			svc, store := tieredService(t)
			if _, err := svc.For("client:phone").Update(testCase.patch); err == nil {
				t.Fatalf("Update(%v) succeeded", testCase.patch)
			}
			if testCase.scope != "" {
				if _, written := store.scopes[testCase.scope][testCase.key]; written {
					t.Errorf("a refused value still reached %s", testCase.scope)
				}
			}
			if _, written := fileObject(t, svc)[testCase.key]; written {
				t.Errorf("a refused value still reached settings.json")
			}
		})
	}
}

// A file written before the residency split seeds the new scopes on attach:
// user values into the reserved scope, device values onto the BACKEND
// machine's own screen. Other devices start from the defaults.
func TestSeedingMovesFileValuesIntoTheirScopes(t *testing.T) {
	dir := t.TempDir()
	writeSettingsFile(t, dir, `{
		"retention": {"days": 7},
		"confirmDelete": false,
		"fontSize": 17,
		"paneDensity": "spacious"
	}`)

	svc := NewService(dir)
	store := newFakeTierStore()
	svc.AttachTierStore(store, testBackendBucket)

	if got := store.scopes[UserScope]["confirmDelete"]; got != "false" {
		t.Errorf("%s[confirmDelete] = %q, want false", UserScope, got)
	}
	if got := store.scopes[testBackendBucket]["fontSize"]; got != "17" {
		t.Errorf("%s[fontSize] = %q, want 17", testBackendBucket, got)
	}
	if _, seeded := store.scopes[UserScope]["retention"]; seeded {
		t.Error("a host key was seeded into the user scope")
	}
	// The values are live through the ordinary read paths.
	if got := svc.Get().ConfirmDelete; got {
		t.Error("the seeded user value is not visible through Get")
	}
	if got := svc.For(testBackendBucket).Get().PaneDensity; got != "spacious" {
		t.Errorf("seeded PaneDensity = %q, want spacious", got)
	}
	// A device that never paired starts from the defaults, not from this
	// machine's screen.
	if got := svc.For("client:phone").Get().FontSize; got != DefaultSettings.FontSize {
		t.Errorf("a fresh device's FontSize = %d, want the default %d", got, DefaultSettings.FontSize)
	}
}

// Seeding never overwrites. A row that exists is a key already migrated, and
// re-seeding one would resurrect the stale file value over whatever the user
// has since chosen — which is also what makes a second boot a no-op.
func TestSeedingNeverOverwritesAnExistingRow(t *testing.T) {
	dir := t.TempDir()
	writeSettingsFile(t, dir, `{"confirmDelete": false, "fontSize": 17}`)

	store := newFakeTierStore()
	if err := store.SetUIState(UserScope, map[string]string{"confirmDelete": "true"}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetUIState(testBackendBucket, map[string]string{"fontSize": "20"}); err != nil {
		t.Fatal(err)
	}

	svc := NewService(dir)
	svc.AttachTierStore(store, testBackendBucket)

	if got := store.scopes[UserScope]["confirmDelete"]; got != "true" {
		t.Errorf("%s[confirmDelete] = %q, want the existing true", UserScope, got)
	}
	if got := store.scopes[testBackendBucket]["fontSize"]; got != "20" {
		t.Errorf("%s[fontSize] = %q, want the existing 20", testBackendBucket, got)
	}
}

// A file value at its default is not seeded — there is nothing to carry over,
// and a row holding the default would pin a key the user never touched.
func TestSeedingSkipsValuesThatOnlyRestateADefault(t *testing.T) {
	dir := t.TempDir()
	writeSettingsFile(t, dir, fmt.Sprintf(`{"fontSize": %d}`, DefaultSettings.FontSize))

	store := newFakeTierStore()
	NewService(dir).AttachTierStore(store, testBackendBucket)

	if _, seeded := store.scopes[testBackendBucket]["fontSize"]; seeded {
		t.Errorf("a default-valued key was seeded: %v", store.scopes[testBackendBucket])
	}
}

// The moved keys leave settings.json on the next write that moves a host key.
// Until then they are inert: loadFromFile ignores a key that no longer lives
// there, so a stale copy can never outrank the row.
func TestMovedKeysLeaveTheFileOnTheNextHostWrite(t *testing.T) {
	dir := t.TempDir()
	writeSettingsFile(t, dir, `{"fontSize": 17, "confirmDelete": false}`)

	svc := NewService(dir)
	store := newFakeTierStore()
	svc.AttachTierStore(store, testBackendBucket)

	// Change the row out from under the stale file value.
	if _, err := svc.For(testBackendBucket).Update(map[string]any{"fontSize": 20}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got := svc.For(testBackendBucket).Get().FontSize; got != 20 {
		t.Fatalf("FontSize = %d, want the row's 20 rather than the file's 17", got)
	}

	if _, err := svc.Update(map[string]any{"retention": map[string]any{"days": 7}}); err != nil {
		t.Fatalf("host Update: %v", err)
	}
	file := fileObject(t, svc)
	for _, key := range []string{"fontSize", "confirmDelete"} {
		if _, ok := file[key]; ok {
			t.Errorf("settings.json still holds the moved key %s", key)
		}
	}
	if _, ok := file["retention"]; !ok {
		t.Error("settings.json lost the host key it was written for")
	}
}

// A device-tier save must not fsync settings.json. The file is untouched
// unless a key that still lives in it moved.
func TestADeviceOnlyWriteLeavesTheFileAlone(t *testing.T) {
	svc, _ := tieredService(t)
	if _, err := svc.Update(map[string]any{"retention": map[string]any{"days": 7}}); err != nil {
		t.Fatalf("host Update: %v", err)
	}
	before, err := os.Stat(svc.Path())
	if err != nil {
		t.Fatal(err)
	}

	if _, err := svc.For("client:phone").Update(map[string]any{"fontSize": 17}); err != nil {
		t.Fatalf("device Update: %v", err)
	}
	after, err := os.Stat(svc.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !before.ModTime().Equal(after.ModTime()) || before.Size() != after.Size() {
		t.Error("a device-tier write rewrote settings.json")
	}
}

// A write with no caller behind it still lands somewhere: the backend
// machine's own screen, rather than being dropped.
func TestABucketLessDeviceWriteLandsOnTheBackendScreen(t *testing.T) {
	svc, store := tieredService(t)
	svc.AddRecentWorkspace("/repo/one")

	if got := store.scopes[testBackendBucket]["recentWorkspaces"]; got != `["/repo/one"]` {
		t.Errorf("%s[recentWorkspaces] = %q", testBackendBucket, got)
	}
}

// BackendScreen is the backend machine's own screen, not "the device tier,
// globally": the host-side notification sender presents on that screen, so
// it must read the preferences set THERE and no other device's.
func TestBackendScreenReadsTheBackendMachinesOwnBucket(t *testing.T) {
	svc, _ := tieredService(t)

	if _, err := svc.For(testBackendBucket).Update(map[string]any{
		"notifyTurnComplete": false,
	}); err != nil {
		t.Fatalf("backend Update: %v", err)
	}
	if _, err := svc.For("client:phone").Update(map[string]any{
		"notifyApprovalNeeded": false,
	}); err != nil {
		t.Fatalf("phone Update: %v", err)
	}

	backend := svc.BackendScreen().Get()
	if backend.NotifyTurnComplete {
		t.Error("BackendScreen ignored the preference set on the backend's own screen")
	}
	if !backend.NotifyApprovalNeeded {
		t.Error("BackendScreen adopted another device's preference")
	}
	// And the phone keeps its own answer, unaffected by the backend's.
	phone := svc.For("client:phone").Get()
	if !phone.NotifyTurnComplete || phone.NotifyApprovalNeeded {
		t.Errorf("phone view = %+v, want only its own toggle off", phone)
	}
}

// A service with no backend bucket — one built before AttachTierStore, which
// main.go and main_desktop.go both do — answers defaults rather than failing.
func TestBackendScreenWithoutAStoreAnswersDefaults(t *testing.T) {
	svc := NewService(t.TempDir())
	if got := svc.BackendScreen().Get(); !got.NotificationsEnabled || !got.NotifyError {
		t.Fatalf("store-less BackendScreen = %+v, want the defaults", got)
	}
}

// Every notification preference defaults ON. Notifications were
// unconditional before these keys existed, so a default of off would be a
// silent behaviour change for every existing install.
func TestNotificationPreferencesDefaultOn(t *testing.T) {
	for key, on := range map[string]bool{
		"notificationsEnabled":    DefaultSettings.NotificationsEnabled,
		"notifyTurnComplete":      DefaultSettings.NotifyTurnComplete,
		"notifyApprovalNeeded":    DefaultSettings.NotifyApprovalNeeded,
		"notifyError":             DefaultSettings.NotifyError,
		"notifyProviderSignedOut": DefaultSettings.NotifyProviderSignedOut,
	} {
		if !on {
			t.Errorf("%s defaults off", key)
		}
		tier, ok := TierForKey(key)
		if !ok || tier != TierDevice {
			t.Errorf("%s is tier %q (known=%t), want %q: a notification interrupts a SCREEN",
				key, tier, ok, TierDevice)
		}
	}
}

// The recent list is per caller on both sides, so a screen sees the
// workspaces IT opened.
func TestRecentWorkspacesArePerCaller(t *testing.T) {
	svc, _ := tieredService(t)
	svc.For("client:phone").AddRecentWorkspace("/repo/phone")
	svc.For("client:desk").AddRecentWorkspace("/repo/desk")

	if got := svc.For("client:phone").Get().RecentWorkspaces; !slices.Equal(got, []string{"/repo/phone"}) {
		t.Errorf("phone RecentWorkspaces = %v", got)
	}
	if got := svc.For("client:desk").Get().RecentWorkspaces; !slices.Equal(got, []string{"/repo/desk"}) {
		t.Errorf("desk RecentWorkspaces = %v", got)
	}
}

// A store-less Service is the pre-phase-4 one: main.go and main_desktop.go
// build one before the database exists, and every tier stays in the file.
func TestAStoreLessServiceKeepsEveryTierInTheFile(t *testing.T) {
	svc := NewService(t.TempDir())
	if _, err := svc.Update(map[string]any{"fontSize": 17, "confirmDelete": false}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	file := fileObject(t, svc)
	for _, key := range []string{"fontSize", "confirmDelete"} {
		if _, ok := file[key]; !ok {
			t.Errorf("settings.json is missing %s", key)
		}
	}
	if got := svc.Get().FontSize; got != 17 {
		t.Errorf("FontSize = %d, want 17", got)
	}
}

// One unreadable row costs that row, never the rest of the bucket.
func TestAnUnreadableRowDoesNotCostTheWholeBucket(t *testing.T) {
	svc, store := tieredService(t)
	if err := store.SetUIState("client:phone", map[string]string{
		"fontSize":    `"not a number"`,
		"paneDensity": `"spacious"`,
	}); err != nil {
		t.Fatal(err)
	}
	got := svc.For("client:phone").Get()
	if got.PaneDensity != "spacious" {
		t.Errorf("PaneDensity = %q, want spacious", got.PaneDensity)
	}
	if got.FontSize != DefaultSettings.FontSize {
		t.Errorf("FontSize = %d, want the default %d", got.FontSize, DefaultSettings.FontSize)
	}
}

// A bucket that cannot be read renders on defaults rather than failing the
// read that asked.
func TestAnUnreadableBucketFallsBackToDefaults(t *testing.T) {
	svc, store := tieredService(t)
	store.readErr = fmt.Errorf("ui_state is unavailable")
	if got := svc.For("client:phone").Get().FontSize; got != DefaultSettings.FontSize {
		t.Errorf("FontSize = %d, want the default %d", got, DefaultSettings.FontSize)
	}
}

func writeSettingsFile(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A wipe that reaches ui_state around this service (the harness reset) must
// not leave the cached user tier standing.
func TestInvalidateTierCacheRereadsTheUserTier(t *testing.T) {
	svc, store := tieredService(t)
	if _, err := svc.Update(map[string]any{"confirmDelete": false}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	// A wipe the service never heard about.
	delete(store.scopes[UserScope], "confirmDelete")
	if svc.Get().ConfirmDelete {
		t.Fatal("the cache was not holding the user value to begin with")
	}
	svc.InvalidateTierCache()
	if !svc.Get().ConfirmDelete {
		t.Fatal("the user tier did not reload after its row was dropped")
	}
}
