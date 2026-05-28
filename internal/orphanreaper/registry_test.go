package orphanreaper

import (
	"os"
	"path/filepath"
	"testing"
)

func newTestRegistry(t *testing.T) (*Registry, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "orphan-registry.json")
	return NewRegistry(path), path
}

func TestRegistryAddLoadRoundTrip(t *testing.T) {
	reg, _ := newTestRegistry(t)
	a := Record{UUID: "a", PID: 100, PGID: 100, CreateUnix: 111}
	b := Record{UUID: "b", PID: 200, PGID: 200, CreateUnix: 222}
	if err := reg.Add(a); err != nil {
		t.Fatal(err)
	}
	if err := reg.Add(b); err != nil {
		t.Fatal(err)
	}

	got, err := reg.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2: %+v", len(got), got)
	}
	// Sorted by pgid.
	if got[0] != a || got[1] != b {
		t.Errorf("records = %+v, want [%+v %+v]", got, a, b)
	}
}

func TestRegistryAddReplacesSamePGID(t *testing.T) {
	reg, _ := newTestRegistry(t)
	_ = reg.Add(Record{UUID: "old", PID: 100, PGID: 100, CreateUnix: 111})
	_ = reg.Add(Record{UUID: "new", PID: 100, PGID: 100, CreateUnix: 999})

	got, _ := reg.Load()
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1 (dedup by pgid)", len(got))
	}
	if got[0].UUID != "new" || got[0].CreateUnix != 999 {
		t.Errorf("record = %+v, want the replacement", got[0])
	}
}

func TestRegistryRemove(t *testing.T) {
	reg, _ := newTestRegistry(t)
	_ = reg.Add(Record{PID: 100, PGID: 100})
	_ = reg.Add(Record{PID: 200, PGID: 200})
	if err := reg.Remove(100); err != nil {
		t.Fatal(err)
	}
	got, _ := reg.Load()
	if len(got) != 1 || got[0].PGID != 200 {
		t.Fatalf("after remove = %+v, want only pgid 200", got)
	}
}

func TestRegistryClearRemovesFile(t *testing.T) {
	reg, path := newTestRegistry(t)
	_ = reg.Add(Record{PID: 100, PGID: 100})
	if err := reg.Clear(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("registry file should be gone after Clear, stat err = %v", err)
	}
	got, err := reg.Load()
	if err != nil {
		t.Fatalf("Load after Clear: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Load after Clear = %+v, want empty", got)
	}
}

func TestRegistryEmptyWhenAbsent(t *testing.T) {
	reg, _ := newTestRegistry(t)
	got, err := reg.Load()
	if err != nil {
		t.Fatalf("Load on absent file: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Load = %+v, want empty for absent file", got)
	}
}

func TestRegistryAddResetsCorruptFile(t *testing.T) {
	reg, path := newTestRegistry(t)
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Add must not fail on a corrupt file — it logs and starts fresh.
	rec := Record{PID: 300, PGID: 300, CreateUnix: 333}
	if err := reg.Add(rec); err != nil {
		t.Fatalf("Add over corrupt file: %v", err)
	}
	got, err := reg.Load()
	if err != nil {
		t.Fatalf("Load after reset: %v", err)
	}
	if len(got) != 1 || got[0] != rec {
		t.Errorf("after corrupt-reset add = %+v, want just %+v", got, rec)
	}
}
