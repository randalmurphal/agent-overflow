//go:build !windows

package filepreview

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestFIFOAssetIsRefusedWithoutWaitingForAWriter(t *testing.T) {
	dir := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(dir, "pipe.html"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	rr := httptest.NewRecorder()
	files{root}.ServeHTTP(rr, httptest.NewRequest("GET", "/pipe.html", nil))
	if rr.Code != 404 {
		t.Fatalf("status %d", rr.Code)
	}
}
