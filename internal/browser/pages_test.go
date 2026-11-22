package browser

import (
	"errors"
	"testing"
)

// A live page whose read fails stays in the listing, with the address and
// title last known and the read's failure, and the listing succeeds.
func TestPagesListsALivePageWhoseReadFailed(t *testing.T) {
	m, engine := newLifecycleManager(t)
	access := m.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
	page := mustOpen(t, m, access, "https://example.test/a")
	engine.mu.Lock()
	engine.infoErr = errors.New("read refused")
	engine.mu.Unlock()
	pages, err := m.Pages(t.Context(), access)
	if err != nil {
		t.Fatalf("pages: %v", err)
	}
	if len(pages) != 1 || pages[0].ID != page.ID || pages[0].URL != "https://example.test/a" || pages[0].ReadError == "" {
		t.Fatalf("pages %+v, want the page as last known with its read failure", pages)
	}
	engine.mu.Lock()
	engine.infoErr = nil
	engine.mu.Unlock()
	if pages, err := m.Pages(t.Context(), access); err != nil || len(pages) != 1 || pages[0].ReadError != "" {
		t.Fatalf("pages %+v, %v after the reads recovered", pages, err)
	}
}
