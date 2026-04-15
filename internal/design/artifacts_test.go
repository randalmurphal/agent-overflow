package design

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/store"
)

func newArtifactStore(t *testing.T) (*ArtifactStore, *store.Store, string) {
	t.Helper()

	st, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	baseDir := filepath.Join(t.TempDir(), "design-artifacts")
	return NewArtifactStore(baseDir, st), st, baseDir
}

func createThread(t *testing.T, st *store.Store, id string) {
	t.Helper()

	now := int64(1000)
	if err := st.CreateThread(store.Thread{
		ID:            id,
		Title:         "Thread " + id,
		Provider:      "codex",
		WorkspacePath: "/tmp/workspace",
		Model:         "gpt-5.4",
		CreatedAt:     now,
		UpdatedAt:     now,
	}); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
}

func TestArtifactStoreRoundTrip(t *testing.T) {
	as, st, baseDir := newArtifactStore(t)
	createThread(t, st, "thread-1")

	html := "<html><body>Hello design</body></html>"
	artifact, err := as.Store("thread-1", html, "Homepage", "Primary mockup", "render")
	if err != nil {
		t.Fatalf("Store: %v", err)
	}

	if !strings.HasPrefix(artifact.HTMLPath, filepath.Join(baseDir, "thread-1")) {
		t.Fatalf("artifact path %q does not live under thread directory", artifact.HTMLPath)
	}

	gotHTML, err := as.Get("thread-1", artifact.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if gotHTML != html {
		t.Fatalf("html mismatch: got %q want %q", gotHTML, html)
	}

	artifacts, err := as.List("thread-1", "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(artifacts) != 1 {
		t.Fatalf("artifact count = %d, want 1", len(artifacts))
	}
	if artifacts[0].ID != artifact.ID {
		t.Fatalf("artifact id = %q, want %q", artifacts[0].ID, artifact.ID)
	}
}

func TestArtifactStoreListFiltersByKind(t *testing.T) {
	as, st, _ := newArtifactStore(t)
	createThread(t, st, "thread-2")

	if _, err := as.Store("thread-2", "<html>render</html>", "Render", "", "render"); err != nil {
		t.Fatalf("Store render: %v", err)
	}
	option, err := as.Store("thread-2", "<html>option</html>", "Option", "Alternate", "option")
	if err != nil {
		t.Fatalf("Store option: %v", err)
	}

	filtered, err := as.List("thread-2", "option")
	if err != nil {
		t.Fatalf("List(option): %v", err)
	}
	if len(filtered) != 1 {
		t.Fatalf("filtered count = %d, want 1", len(filtered))
	}
	if filtered[0].ID != option.ID {
		t.Fatalf("filtered id = %q, want %q", filtered[0].ID, option.ID)
	}
}

func TestArtifactStoreRemovesHTMLOnMetadataFailure(t *testing.T) {
	as, _, baseDir := newArtifactStore(t)

	_, err := as.Store("missing-thread", "<html>orphan</html>", "Broken", "", "render")
	if err == nil {
		t.Fatal("expected Store to fail for missing thread, got nil")
	}

	matches, globErr := filepath.Glob(filepath.Join(baseDir, "missing-thread", "*.html"))
	if globErr != nil {
		t.Fatalf("Glob: %v", globErr)
	}
	if len(matches) != 0 {
		t.Fatalf("expected cleanup of orphaned html files, found %v", matches)
	}
}

func TestArtifactStoreGetReturnsReadErrorWhenFileMissing(t *testing.T) {
	as, st, _ := newArtifactStore(t)
	createThread(t, st, "thread-3")

	artifact, err := as.Store("thread-3", "<html>content</html>", "Card", "", "render")
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	if err := os.Remove(artifact.HTMLPath); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	_, err = as.Get("thread-3", artifact.ID)
	if err == nil {
		t.Fatal("expected Get to fail when html file is missing")
	}
	if !strings.Contains(err.Error(), "read design artifact") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestArtifactStoreRequiresStoreDependency(t *testing.T) {
	as := NewArtifactStore(t.TempDir(), nil)

	if _, err := as.Store("thread", "<html></html>", "Title", "", "render"); err == nil {
		t.Fatal("expected Store to fail without backing store")
	}
	if _, err := as.Get("thread", "artifact"); err == nil {
		t.Fatal("expected Get to fail without backing store")
	}
	if _, err := as.List("thread", ""); err == nil {
		t.Fatal("expected List to fail without backing store")
	}
}

func TestArtifactStoreValidatesRequiredFields(t *testing.T) {
	as, st, _ := newArtifactStore(t)
	createThread(t, st, "thread-4")

	testCases := []struct {
		name string
		call func() error
	}{
		{
			name: "missing thread",
			call: func() error {
				_, err := as.Store("", "<html></html>", "Title", "", "render")
				return err
			},
		},
		{
			name: "missing title",
			call: func() error {
				_, err := as.Store("thread-4", "<html></html>", "", "", "render")
				return err
			},
		},
		{
			name: "missing html",
			call: func() error {
				_, err := as.Store("thread-4", "", "Title", "", "render")
				return err
			},
		},
	}

	for _, tc := range testCases {
		if err := tc.call(); err == nil {
			t.Fatalf("%s: expected validation error", tc.name)
		}
	}
}

func TestArtifactStoreDefaultsKindToRender(t *testing.T) {
	as, st, _ := newArtifactStore(t)
	createThread(t, st, "thread-5")

	artifact, err := as.Store("thread-5", "<html>default</html>", "Default Kind", "", "")
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	if artifact.Kind != "render" {
		t.Fatalf("kind = %q, want render", artifact.Kind)
	}
}

func TestArtifactStoreReturnsWriteErrorWhenBaseDirIsAFile(t *testing.T) {
	st, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer st.Close()

	createThread(t, st, "thread-6")

	baseDir := filepath.Join(t.TempDir(), "artifacts-root")
	if err := os.WriteFile(baseDir, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	as := NewArtifactStore(baseDir, st)
	_, err = as.Store("thread-6", "<html>broken</html>", "Broken", "", "render")
	if err == nil {
		t.Fatal("expected Store to fail when baseDir is a file")
	}
	if !strings.Contains(err.Error(), "create artifact directory") {
		t.Fatalf("unexpected error: %v", err)
	}
}
