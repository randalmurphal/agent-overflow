package threadmcp

import "testing"

func TestJSONContentTypeAllowsParametersAndCasing(t *testing.T) {
	for _, accepted := range []string{"application/json", "application/json; charset=utf-8", "Application/JSON", " application/json "} {
		if !jsonContentType(accepted) {
			t.Errorf("jsonContentType(%q) = false", accepted)
		}
	}
	for _, refused := range []string{"", "text/plain", "text/plain;charset=UTF-8", "application/json-patch+json", "multipart/form-data"} {
		if jsonContentType(refused) {
			t.Errorf("jsonContentType(%q) = true", refused)
		}
	}
}

func TestCapabilitiesRotateAndCloseCannotReopen(t *testing.T) {
	server := New("test-tools", "", func(string) []map[string]any { return nil }, nil)
	t.Cleanup(func() { _ = server.Close() })
	first, err := server.RegisterThread("thread", "first")
	if err != nil {
		t.Fatal(err)
	}
	server.SetThreadEnabled("thread", false)
	second, err := server.RegisterThread("thread", "second")
	if err != nil {
		t.Fatal(err)
	}
	if first["test-tools"].(map[string]any)["url"] == second["test-tools"].(map[string]any)["url"] {
		t.Fatal("capability not rotated")
	}
	server.RevokeThread("thread", "first")
	if !server.HasThread("thread") || server.ThreadEnabled("thread") {
		t.Fatal("stale cleanup revoked replacement or reset toggle")
	}
	server.RevokeThread("thread", "second")
	if server.HasThread("thread") {
		t.Fatal("capability not revoked")
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := server.RegisterThread("thread", "third"); err == nil {
		t.Fatal("closed server reopened")
	}
}
