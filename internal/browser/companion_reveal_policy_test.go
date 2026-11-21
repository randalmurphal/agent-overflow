package browser

import (
	"context"
	"errors"
	"testing"
)

// Only a production boot on a real engine starts the OS file manager: an
// isolated boot and a test binary get a refusal, so neither opens a window
// on the desktop of whoever runs it.
func TestOnlyAProductionBootRevealsFiles(t *testing.T) {
	for _, tc := range []struct {
		fakeEngine, underTest, want bool
	}{
		{false, false, true},
		{true, false, false},
		{false, true, false},
		{true, true, false},
	} {
		if got := revealsFiles(tc.fakeEngine, tc.underTest); got != tc.want {
			t.Errorf("revealsFiles(fake engine %v, under test %v) = %v, want %v", tc.fakeEngine, tc.underTest, got, tc.want)
		}
	}
	for _, fakeEngine := range []bool{true, false} {
		if err := fileRevealer(fakeEngine, true)(context.Background(), "/nowhere"); !errors.Is(err, errNoFileManager) {
			t.Errorf("a test binary's revealer (fake engine %v) answered %v, want the refusal", fakeEngine, err)
		}
	}
	if err := fileRevealer(true, false)(context.Background(), "/nowhere"); !errors.Is(err, errNoFileManager) {
		t.Errorf("an isolated boot's revealer answered %v, want the refusal", err)
	}
	if err := revealFileInFileManager(context.Background(), "/nowhere"); !errors.Is(err, errNoFileManager) {
		t.Errorf("the production hand-off answered %v in a test binary, want the refusal", err)
	}
}

// A Manager built in a test refuses a reveal until the test gives it a
// recorder.
func TestAManagerBuiltInATestRevealsNothing(t *testing.T) {
	manager := NewManager(t.TempDir(), Config{Enabled: true}, ManagerOptions{})
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	if err := manager.revealFile(t.Context(), "/nowhere"); !errors.Is(err, errNoFileManager) {
		t.Fatalf("reveal answered %v, want the refusal", err)
	}
}
