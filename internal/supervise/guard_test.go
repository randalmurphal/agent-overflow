package supervise

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func pendingState(t *testing.T, id string) State {
	t.Helper()
	state, err := Adopt("1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	state, err = state.Begin(id, "2.0.0", time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func TestPrepareDataRootFinishesAMarkedRestoreInEitherLayout(t *testing.T) {
	for _, c := range []struct {
		name   string
		layout func(t *testing.T, dataDir string) Layout
	}{
		{"app-update", appLayout},
		{"serve", func(t *testing.T, dataDir string) Layout {
			layout, err := NewLayout(dataDir)
			if err != nil {
				t.Fatal(err)
			}
			return layout
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			dataDir := t.TempDir()
			layout := c.layout(t, dataDir)
			writeDatabase(t, dataDir, "before")
			if _, err := TakeSnapshot(layout, dataDir, time.Now(), SnapshotOptions{}); err != nil {
				t.Fatal(err)
			}
			writeDatabase(t, dataDir, "trial")
			// An interrupted restore: the marker is down, the copy never ran.
			writeFile(t, layout.MarkerPath(), `{"updateId":"u1","dataDir":`+quote(dataDir)+`,"reason":"x","writtenAtMs":1}`)

			var logged []string
			err := PrepareDataRoot(dataDir, PrepareOptions{Log: func(format string, args ...any) { logged = append(logged, format) }})
			if err != nil {
				t.Fatalf("PrepareDataRoot: %v", err)
			}
			if got := readFile(t, filepath.Join(dataDir, "agent-overflow.db")); got != "before agent-overflow.db" {
				t.Fatalf("database = %q, want the restored snapshot", got)
			}
			if !absent(t, layout.MarkerPath()) {
				t.Fatal("the marker survived its restore")
			}
			if len(logged) != 1 {
				t.Fatalf("logged %v, want one line naming the restore", logged)
			}
		})
	}
}

func TestPrepareDataRootRefusesAnUpdateItDoesNotOwn(t *testing.T) {
	dataDir := t.TempDir()
	serve, err := NewLayout(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveState(serve, pendingState(t, "serve-u")); err != nil {
		t.Fatal(err)
	}
	err = PrepareDataRoot(dataDir, PrepareOptions{})
	var pending *PendingUpdateError
	if !errors.As(err, &pending) || pending.Update.ID != "serve-u" || !strings.Contains(err.Error(), "agent-overflow supervise") {
		t.Fatalf("PrepareDataRoot = %v, want the serve update's refusal", err)
	}
	if !IsPendingUpdate(err) {
		t.Fatal("IsPendingUpdate missed it")
	}
	// The serve supervisor owns its layout and passes.
	if err := PrepareDataRoot(dataDir, PrepareOptions{OwnsServeLayout: true}); err != nil {
		t.Fatalf("the serve supervisor was refused its own update: %v", err)
	}

	// A settled serve record is no obstacle.
	settled, err := pendingState(t, "serve-u").Settle(UpdateCommitted, "", time.Unix(2, 0))
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveState(serve, settled); err != nil {
		t.Fatal(err)
	}
	if err := PrepareDataRoot(dataDir, PrepareOptions{}); err != nil {
		t.Fatalf("a settled record refused the boot: %v", err)
	}

	// A pending in-app update refuses everyone, the serve supervisor too.
	if err := SaveState(appLayout(t, dataDir), pendingState(t, "app-u")); err != nil {
		t.Fatal(err)
	}
	for _, opts := range []PrepareOptions{{}, {OwnsServeLayout: true}} {
		err := PrepareDataRoot(dataDir, opts)
		if !errors.As(err, &pending) || pending.Update.ID != "app-u" || !strings.Contains(err.Error(), "the Agent Overflow app") {
			t.Fatalf("PrepareDataRoot(%+v) = %v, want the app update's refusal", opts, err)
		}
	}
}

func TestPrepareDataRootFailsClosedOnAnUnreadableRecord(t *testing.T) {
	dataDir := t.TempDir()
	serve, err := NewLayout(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, serve.StatePath(), "{")
	if err := PrepareDataRoot(dataDir, PrepareOptions{}); err == nil {
		t.Fatal("an unreadable update record was treated as none")
	}
}

func TestPrepareDataRootIsANoOpOnAnOrdinaryDataRoot(t *testing.T) {
	dataDir := t.TempDir()
	writeDatabase(t, dataDir, "live")
	if err := PrepareDataRoot(dataDir, PrepareOptions{}); err != nil {
		t.Fatalf("PrepareDataRoot: %v", err)
	}
	if got := readFile(t, filepath.Join(dataDir, "agent-overflow.db")); got != "live agent-overflow.db" {
		t.Fatalf("database = %q", got)
	}
	if !absent(t, filepath.Join(dataDir, "runtime")) {
		t.Fatal("the guard created runtime directories on an ordinary data root")
	}
}
