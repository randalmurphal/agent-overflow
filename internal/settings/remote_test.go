package settings

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateRemoteEndpointURL(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
		err  bool
	}{
		{"empty", "", "", true},
		{"whitespace", "   ", "", true},
		{"http rejected", "http://host/?token=x", "", true},
		{"https rejected", "https://host/?token=x", "", true},
		{"ws ok", "ws://host:1234/", "ws://host:1234/", false},
		{"wss ok", "wss://host/path", "wss://host/path", false},
		{"missing host", "ws:///?token=x", "", true},
		{"trims whitespace", "  ws://h/  ", "ws://h/", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ValidateRemoteEndpointURL(c.in)
			if c.err {
				if err == nil {
					t.Fatalf("ValidateRemoteEndpointURL(%q) = nil error, want error", c.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidateRemoteEndpointURL(%q) error = %v", c.in, err)
			}
			if got != c.want {
				t.Fatalf("ValidateRemoteEndpointURL(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestValidateRemoteEndpointToken(t *testing.T) {
	if _, err := ValidateRemoteEndpointToken(""); err == nil {
		t.Fatal("empty token accepted")
	}
	if _, err := ValidateRemoteEndpointToken("   "); err == nil {
		t.Fatal("whitespace token accepted")
	}
	got, err := ValidateRemoteEndpointToken("  abc  ")
	if err != nil {
		t.Fatalf("trimmed token error: %v", err)
	}
	if got != "abc" {
		t.Fatalf("token = %q, want %q", got, "abc")
	}
}

func TestNewRemoteEndpointID(t *testing.T) {
	a, err := NewRemoteEndpointID()
	if err != nil {
		t.Fatalf("NewRemoteEndpointID: %v", err)
	}
	b, err := NewRemoteEndpointID()
	if err != nil {
		t.Fatalf("NewRemoteEndpointID: %v", err)
	}
	if a == "" || b == "" {
		t.Fatalf("empty id (a=%q, b=%q)", a, b)
	}
	if a == b {
		t.Fatalf("two NewRemoteEndpointID returned identical values: %q", a)
	}
}

func TestAddRemoteEndpointPersistsAndReturnsRecord(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)

	got, err := svc.AddRemoteEndpoint("Tailnet", "ws://host:1234/", "tok")
	if err != nil {
		t.Fatalf("AddRemoteEndpoint: %v", err)
	}
	if got.ID == "" {
		t.Fatalf("returned record has no ID: %+v", got)
	}
	if got.Name != "Tailnet" || got.URL != "ws://host:1234/" || got.Token != "tok" {
		t.Fatalf("returned record = %+v", got)
	}

	// Reload from disk: the new endpoint must persist.
	reloaded := NewService(dir).Get()
	if len(reloaded.RemoteEndpoints) != 1 {
		t.Fatalf("reloaded list len = %d, want 1: %+v", len(reloaded.RemoteEndpoints), reloaded.RemoteEndpoints)
	}
	if reloaded.RemoteEndpoints[0].ID != got.ID {
		t.Fatalf("reloaded ID mismatch")
	}
}

func TestAddRemoteEndpointRejectsInvalidURL(t *testing.T) {
	svc := NewService(t.TempDir())
	if _, err := svc.AddRemoteEndpoint("X", "http://nope/", "tok"); err == nil {
		t.Fatal("AddRemoteEndpoint accepted http://, want error")
	}
	if _, err := svc.AddRemoteEndpoint("X", "ws://host/", ""); err == nil {
		t.Fatal("AddRemoteEndpoint accepted empty token, want error")
	}
}

func TestUpdateRemoteEndpointPartialPatch(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)

	created, err := svc.AddRemoteEndpoint("Old", "ws://old/", "oldtoken")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Empty fields must leave existing values untouched.
	updated, err := svc.UpdateRemoteEndpoint(created.ID, "New name", "", "")
	if err != nil {
		t.Fatalf("Update name only: %v", err)
	}
	if updated.Name != "New name" {
		t.Fatalf("Name = %q, want %q", updated.Name, "New name")
	}
	if updated.URL != "ws://old/" {
		t.Fatalf("URL changed unexpectedly: %q", updated.URL)
	}
	if updated.Token != "oldtoken" {
		t.Fatalf("Token changed unexpectedly: %q", updated.Token)
	}

	// Validation runs against the new value when supplied.
	if _, err := svc.UpdateRemoteEndpoint(created.ID, "", "http://nope/", ""); err == nil {
		t.Fatal("Update accepted http URL, want error")
	}
}

func TestUpdateRemoteEndpointMissingID(t *testing.T) {
	svc := NewService(t.TempDir())
	if _, err := svc.UpdateRemoteEndpoint("nope", "x", "", ""); err == nil {
		t.Fatal("Update on missing ID accepted, want error")
	}
}

func TestDeleteRemoteEndpoint(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)

	a, err := svc.AddRemoteEndpoint("A", "ws://a/", "ta")
	if err != nil {
		t.Fatalf("Add A: %v", err)
	}
	if _, err := svc.AddRemoteEndpoint("B", "ws://b/", "tb"); err != nil {
		t.Fatalf("Add B: %v", err)
	}

	if err := svc.DeleteRemoteEndpoint(a.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	got := svc.Get().RemoteEndpoints
	if len(got) != 1 {
		t.Fatalf("post-delete len = %d, want 1", len(got))
	}
	if got[0].Name != "B" {
		t.Fatalf("survivor = %q, want B", got[0].Name)
	}

	// Deleting again surfaces an error so a stale UI sees a clear signal.
	if err := svc.DeleteRemoteEndpoint(a.ID); err == nil {
		t.Fatal("re-delete accepted, want error")
	}
}

func TestDeleteRemoteEndpointSparseAfterEmpty(t *testing.T) {
	// Once the list is empty it must round-trip back to "no key in
	// JSON" so the file stays sparse.
	dir := t.TempDir()
	svc := NewService(dir)

	created, err := svc.AddRemoteEndpoint("X", "ws://x/", "tx")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := svc.DeleteRemoteEndpoint(created.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var fileMap map[string]any
	if err := json.Unmarshal(data, &fileMap); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := fileMap["remoteEndpoints"]; ok {
		t.Fatalf("file still contains remoteEndpoints after delete-to-empty: %s", string(data))
	}
}

func TestTouchRemoteEndpointUpdatesTimestamp(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)

	created, err := svc.AddRemoteEndpoint("X", "ws://x/", "t")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if created.LastUsedAt != 0 {
		t.Fatalf("new endpoint already has LastUsedAt = %d", created.LastUsedAt)
	}

	if err := svc.TouchRemoteEndpoint(created.ID); err != nil {
		t.Fatalf("Touch: %v", err)
	}

	got := svc.Get().RemoteEndpoints
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0].LastUsedAt == 0 {
		t.Fatalf("LastUsedAt still zero after Touch")
	}

	if err := svc.TouchRemoteEndpoint("nope"); err == nil {
		t.Fatal("Touch missing ID accepted, want error")
	}
}

func TestRemoteEndpointsSparseWhenEmpty(t *testing.T) {
	// Defaults imply zero endpoints; the file should not include the
	// key at all so a fresh install stays sparse.
	dir := t.TempDir()
	svc := NewService(dir)
	if _, err := svc.Update(map[string]any{"theme": "dark"}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if strings.Contains(string(data), "remoteEndpoints") {
		t.Fatalf("settings file leaks remoteEndpoints when empty: %s", string(data))
	}
}
