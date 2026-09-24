//go:build !windows && !nogui

package main

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/store"
)

// The desktop boot's gate as runDesktop wires it (reconcileDesktopUpdate),
// which only a build with the desktop has.

// TestDesktopBootWithoutItsHelperStillRefusesToMigrate: a boot whose update
// half cannot be built (here an AppImage path that is not absolute) opens
// the store with the refusal all the same. A database it would migrate
// fails Start with the page naming why, and keeps its schema; one at this
// build's schema launches.
func TestDesktopBootWithoutItsHelperStillRefusesToMigrate(t *testing.T) {
	r := newDesktopApplyRig(t)
	r.release()
	previousLock := heldBackendLock
	t.Cleanup(func() {
		if heldBackendLock != nil && heldBackendLock != previousLock {
			heldBackendLock.file.Close()
		}
		heldBackendLock = previousLock
	})
	heldBackendLock = nil
	t.Setenv("APPIMAGE", "Agent-Overflow.AppImage")

	dbPath := filepath.Join(r.dir, "agent-overflow.db")
	if err := os.Remove(dbPath); err != nil {
		t.Fatal(err)
	}
	st, err := store.New(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	latest := latestSchema(t)
	if got, err := store.ReadSchemaVersion(dbPath); err != nil || got != latest {
		t.Fatalf("a new store is at v%d (%v), want v%d", got, err, latest)
	}

	appService := newApp()
	t.Cleanup(func() {
		if err := appService.Shutdown(context.Background()); err != nil {
			t.Errorf("shut down the App after its refused Start: %v", err)
		}
	})
	gate, plan := reconcileDesktopUpdate(appService)
	if gate.boot != nil || gate.unavailable == nil || !strings.Contains(gate.unavailable.Error(), "is not absolute") {
		t.Fatalf("gate = %+v; want no update half", gate)
	}
	if !plan.launch || plan.page != nil {
		t.Fatalf("a database at this build's schema: plan = %+v, want a launch", plan)
	}
	// The same open the boot's Start makes, which the refusal lets through.
	refusing, err := store.NewWithOptions(dbPath, store.Options{RefusePendingMigrations: true})
	if err != nil {
		t.Fatalf("the refusing open of a current database = %v", err)
	}
	if err := refusing.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`DELETE FROM migration_versions WHERE version = ?`, latest); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	// The version the refused boot must leave in place: the chain's
	// previous migration, whatever number it carries.
	previous, err := store.ReadSchemaVersion(dbPath)
	if err != nil || previous >= latest {
		t.Fatalf("after dropping v%d the database reads v%d (%v)", latest, previous, err)
	}
	startErr := appService.Start(t.Context())
	page := gate.startFailed(startErr)
	if page == nil || page.Title != "Agent Overflow could not start the database upgrade this version needs." ||
		!strings.Contains(page.Detail, "Reason: the AppImage path \"Agent-Overflow.AppImage\" is not absolute.") ||
		!strings.Contains(page.Detail, "Nothing was changed.") || page.Log != gate.logPath || gate.logPath == "" {
		t.Fatalf("Start = %v; page = %+v", startErr, page)
	}
	if got, err := store.ReadSchemaVersion(dbPath); err != nil || got != previous {
		t.Fatalf("the database is at v%d (%v) after the boot, want v%d", got, err, previous)
	}
}
