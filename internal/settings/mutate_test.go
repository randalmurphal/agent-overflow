package settings

import (
	"slices"
	"testing"
)

func observedService(t *testing.T) (*Service, *[][]TierChange) {
	t.Helper()
	svc := NewService(t.TempDir())
	var seen [][]TierChange
	svc.SetChangeObserver(func(changes []TierChange) {
		seen = append(seen, changes)
	})
	return svc, &seen
}

func TestUpdateAnnouncesChangedKeysWithTheirTier(t *testing.T) {
	svc, seen := observedService(t)
	if _, err := svc.Update(map[string]any{"fontSize": 17, "confirmDelete": false}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(*seen) != 1 {
		t.Fatalf("observer fired %d times, want 1", len(*seen))
	}
	changes := (*seen)[0]
	if len(changes) != 2 {
		t.Fatalf("changes = %+v, want one entry per tier touched", changes)
	}
	if changes[0].Tier != TierUser || !slices.Equal(changes[0].Keys, []string{"confirmDelete"}) {
		t.Errorf("changes[0] = %+v", changes[0])
	}
	if changes[1].Tier != TierDevice || !slices.Equal(changes[1].Keys, []string{"fontSize"}) {
		t.Errorf("changes[1] = %+v", changes[1])
	}
}

// A repeat save of an unchanged form must stay silent, or every attached
// client re-reads settings for nothing.
func TestUpdateWithNoChangeAnnouncesNothing(t *testing.T) {
	svc, seen := observedService(t)
	if _, err := svc.Update(map[string]any{"fontSize": 17}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(*seen) != 1 {
		t.Fatalf("first Update fired %d times, want 1", len(*seen))
	}
	if _, err := svc.Update(map[string]any{"fontSize": 17}); err != nil {
		t.Fatalf("repeat Update: %v", err)
	}
	if len(*seen) != 1 {
		t.Fatalf("repeat Update fired the observer again: %+v", *seen)
	}
}

// A rejected patch must leave the file and the observer alone: an announced
// change nobody persisted would converge every other client onto a value this
// backend does not hold.
func TestRejectedUpdateAnnouncesNothing(t *testing.T) {
	svc, seen := observedService(t)
	if _, err := svc.Update(map[string]any{"sansFont": "not-a-font"}); err == nil {
		t.Fatal("Update accepted an invalid font")
	}
	if len(*seen) != 0 {
		t.Fatalf("observer fired on a rejected patch: %+v", *seen)
	}
}

// Remote endpoints are mutated through their own CRUD (Update rejects the
// key), so they are the path most likely to be forgotten by a per-call-site
// emit. The chokepoint covers them for free.
func TestRemoteEndpointCRUDAnnouncesItsTier(t *testing.T) {
	svc, seen := observedService(t)
	endpoint, err := svc.AddRemoteEndpoint("home", "wss://example.test:1234", "0123456789abcdef")
	if err != nil {
		t.Fatalf("AddRemoteEndpoint: %v", err)
	}
	if len(*seen) != 1 || len((*seen)[0]) != 1 {
		t.Fatalf("add announced %+v", *seen)
	}
	if got := (*seen)[0][0]; got.Tier != TierDevice || !slices.Equal(got.Keys, []string{"remoteEndpoints"}) {
		t.Fatalf("add announced %+v", got)
	}
	if err := svc.DeleteRemoteEndpoint(endpoint.ID); err != nil {
		t.Fatalf("DeleteRemoteEndpoint: %v", err)
	}
	if len(*seen) != 2 {
		t.Fatalf("delete announced %+v", *seen)
	}
}

// The announcement names keys only. Tokens and sensitive environment values
// are redacted on every read path, and a change set carrying values would be
// the one place they left the backend in the clear.
func TestChangeAnnouncementCarriesNoValues(t *testing.T) {
	svc, seen := observedService(t)
	const secret = "0123456789abcdef"
	if _, err := svc.AddRemoteEndpoint("home", "wss://example.test:1234", secret); err != nil {
		t.Fatalf("AddRemoteEndpoint: %v", err)
	}
	for _, change := range (*seen)[0] {
		for _, key := range change.Keys {
			if key == secret {
				t.Fatalf("change set carried the token")
			}
		}
	}
}

func TestProviderEnvMutatorsAnnounceTheirTier(t *testing.T) {
	svc, seen := observedService(t)
	if _, err := svc.SetProviderEnvVar("claude", "MY_VAR", "value", true); err != nil {
		t.Fatalf("SetProviderEnvVar: %v", err)
	}
	if len(*seen) != 1 || len((*seen)[0]) != 1 {
		t.Fatalf("set announced %+v", *seen)
	}
	if got := (*seen)[0][0]; got.Tier != TierHost || !slices.Equal(got.Keys, []string{"claudeCustomEnv"}) {
		t.Fatalf("set announced %+v", got)
	}
	if _, err := svc.DeleteProviderEnvVar("claude", "MY_VAR"); err != nil {
		t.Fatalf("DeleteProviderEnvVar: %v", err)
	}
	if len(*seen) != 2 {
		t.Fatalf("delete announced %+v", *seen)
	}
}

func TestAddRecentWorkspaceAnnouncesOnceThenStaysSilent(t *testing.T) {
	svc, seen := observedService(t)
	svc.AddRecentWorkspace("/tmp/one")
	if len(*seen) != 1 {
		t.Fatalf("first add announced %+v", *seen)
	}
	if got := (*seen)[0][0]; got.Tier != TierDevice || !slices.Equal(got.Keys, []string{"recentWorkspaces"}) {
		t.Fatalf("first add announced %+v", got)
	}
	svc.AddRecentWorkspace("/tmp/one")
	if len(*seen) != 1 {
		t.Fatalf("re-adding the head workspace announced again: %+v", *seen)
	}
}

// The observer runs with the write lock released, so reading settings back
// from inside it must not deadlock — the settings:updated broadcast's own
// consumers do exactly that.
func TestObserverMayReadSettingsBack(t *testing.T) {
	svc := NewService(t.TempDir())
	var readBack Settings
	svc.SetChangeObserver(func([]TierChange) { readBack = svc.Get() })
	if _, err := svc.Update(map[string]any{"fontSize": 19}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if readBack.FontSize != 19 {
		t.Fatalf("observer read fontSize %d, want 19", readBack.FontSize)
	}
}
