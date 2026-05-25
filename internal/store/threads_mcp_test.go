package store

import (
	"testing"
)

func TestGetDisabledMcpServers_NullReturnsNotSnapshotted(t *testing.T) {
	s := newTestStore(t)
	thr := makeThread("t-mcp-null", "claude")
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Force column to NULL (simulating a pre-migration thread).
	if _, err := s.db.Exec(`UPDATE threads SET disabled_mcp_servers = NULL WHERE id = ?`, thr.ID); err != nil {
		t.Fatalf("force null: %v", err)
	}

	names, snapshotted, err := s.GetDisabledMcpServers(thr.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if snapshotted {
		t.Error("expected snapshotted=false for NULL column")
	}
	if names != nil {
		t.Errorf("expected nil names for NULL column, got %v", names)
	}
}

func TestGetDisabledMcpServers_EmptyArray(t *testing.T) {
	s := newTestStore(t)
	thr := makeThread("t-mcp-empty", "claude")
	empty := &[]string{}
	thr.DisabledMcpServers = empty
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("create: %v", err)
	}

	names, snapshotted, err := s.GetDisabledMcpServers(thr.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !snapshotted {
		t.Error("expected snapshotted=true for empty JSON array")
	}
	if len(names) != 0 {
		t.Errorf("expected empty names, got %v", names)
	}
}

func TestGetDisabledMcpServers_PopulatedArray(t *testing.T) {
	s := newTestStore(t)
	thr := makeThread("t-mcp-pop", "claude")
	set := []string{"server-a", "server-b"}
	thr.DisabledMcpServers = &set
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("create: %v", err)
	}

	names, snapshotted, err := s.GetDisabledMcpServers(thr.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !snapshotted {
		t.Error("expected snapshotted=true")
	}
	if len(names) != 2 || names[0] != "server-a" || names[1] != "server-b" {
		t.Errorf("expected [server-a server-b], got %v", names)
	}
}

func TestSetDisabledMcpServers_RoundTrip(t *testing.T) {
	s := newTestStore(t)
	thr := makeThread("t-mcp-set", "claude")
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := s.SetDisabledMcpServers(thr.ID, []string{"x", "y"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	names, snapshotted, err := s.GetDisabledMcpServers(thr.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !snapshotted {
		t.Error("expected snapshotted=true after set")
	}
	if len(names) != 2 || names[0] != "x" || names[1] != "y" {
		t.Errorf("expected [x y], got %v", names)
	}
}

func TestSetDisabledMcpServers_NilCoercesToEmptyArray(t *testing.T) {
	s := newTestStore(t)
	thr := makeThread("t-mcp-nil", "claude")
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := s.SetDisabledMcpServers(thr.ID, nil); err != nil {
		t.Fatalf("set nil: %v", err)
	}
	names, snapshotted, err := s.GetDisabledMcpServers(thr.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !snapshotted {
		t.Error("expected snapshotted=true after setting nil (coerced to [])")
	}
	if len(names) != 0 {
		t.Errorf("expected empty, got %v", names)
	}
}

func TestSetDisabledMcpServers_DoesNotBumpUpdatedAt(t *testing.T) {
	s := newTestStore(t)
	thr := makeThread("t-mcp-noupdate", "claude")
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := s.GetThread(thr.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	before := got.UpdatedAt

	if err := s.SetDisabledMcpServers(thr.ID, []string{"z"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, err = s.GetThread(thr.ID)
	if err != nil {
		t.Fatalf("get after set: %v", err)
	}
	if got.UpdatedAt != before {
		t.Errorf("updated_at changed from %d to %d — should not bump", before, got.UpdatedAt)
	}
}

func TestBuildForkedThread_CopiesDisabledMcpServers(t *testing.T) {
	set := []string{"disabled-server"}
	source := makeThread("fork-src", "claude")
	source.DisabledMcpServers = &set

	fork := BuildForkedThread(source)

	if fork.DisabledMcpServers == nil {
		t.Fatal("expected DisabledMcpServers to be copied, got nil")
	}
	if len(*fork.DisabledMcpServers) != 1 || (*fork.DisabledMcpServers)[0] != "disabled-server" {
		t.Errorf("expected [disabled-server], got %v", *fork.DisabledMcpServers)
	}
}

func TestMigrationV4_DisabledMcpServersColumn(t *testing.T) {
	s := newTestStore(t)

	cols, err := tableColumns(s.db, "threads")
	if err != nil {
		t.Fatalf("table_info: %v", err)
	}
	if !cols["disabled_mcp_servers"] {
		t.Fatal("disabled_mcp_servers column missing after migration v4")
	}

	// Valid JSON passes CHECK.
	thr := makeThread("t-mig-valid", "claude")
	empty := []string{}
	thr.DisabledMcpServers = &empty
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("create with valid JSON: %v", err)
	}

	// Invalid JSON fails CHECK.
	_, err = s.db.Exec(
		`UPDATE threads SET disabled_mcp_servers = ? WHERE id = ?`,
		"not-json", thr.ID,
	)
	if err == nil {
		t.Fatal("expected CHECK constraint to reject invalid JSON")
	}
}
