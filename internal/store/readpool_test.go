package store

import (
	"testing"
	"time"
)

// The read pool exists so UI reads run against WAL snapshots instead of
// queuing behind streaming flush transactions. These tests pin the
// properties the rest of the app relies on: file-backed stores get the
// pool, :memory: stores fall back to the writer, the pool can never
// write, reads don't block behind an open write transaction, and
// quiescing (the VACUUM guard) restores single-pool routing for its
// duration.

func TestReadPoolEnabledForFileBackedStore(t *testing.T) {
	s := newTestStore(t)
	if s.read == nil {
		t.Fatal("file-backed WAL store must open a read pool")
	}
	if s.reader() != s.read {
		t.Fatal("reader() must route to the read pool when it exists")
	}
	if err := s.SetUIState("client:test", map[string]string{"k": "v"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := s.GetUIState("client:test")
	if err != nil {
		t.Fatalf("read through pool: %v", err)
	}
	if got["k"] != "v" {
		t.Fatalf("read through pool: got %v, want k=v", got)
	}
}

func TestReadPoolDisabledForMemoryStore(t *testing.T) {
	s, err := New(":memory:")
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer s.Close()
	if s.read != nil {
		t.Fatal(":memory: store must not open a read pool (a second open is a different database)")
	}
	if s.reader() != s.db {
		t.Fatal("reader() must fall back to the writer without a pool")
	}
	if err := s.SetUIState("client:test", map[string]string{"k": "v"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got, err := s.GetUIState("client:test"); err != nil || got["k"] != "v" {
		t.Fatalf("fallback read: %v %v", err, got)
	}
}

func TestReadPoolRefusesWrites(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.read.Exec(
		"INSERT INTO ui_state (scope, key, value, updated_at) VALUES ('x', 'y', 'z', 0)",
	); err == nil {
		t.Fatal("write through the query_only read pool must fail loudly")
	}
}

func TestReadsDoNotBlockBehindOpenWriteTransaction(t *testing.T) {
	s := newTestStore(t)
	if err := s.SetUIState("client:test", map[string]string{"k": "before"}); err != nil {
		t.Fatalf("seed write: %v", err)
	}

	tx, err := s.db.Begin()
	if err != nil {
		t.Fatalf("begin write tx: %v", err)
	}
	if _, err := tx.Exec(
		"INSERT OR REPLACE INTO ui_state (scope, key, value, updated_at) VALUES ('client:test', 'k', 'during', 1)",
	); err != nil {
		t.Fatalf("tx write: %v", err)
	}

	// A read must complete while the write transaction is still open,
	// and must see the pre-transaction snapshot.
	type result struct {
		val string
		err error
	}
	done := make(chan result, 1)
	go func() {
		got, err := s.GetUIState("client:test")
		done <- result{got["k"], err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("concurrent read: %v", r.err)
		}
		if r.val != "before" {
			t.Fatalf("read during open write tx saw %q, want committed snapshot %q", r.val, "before")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("read blocked behind an open write transaction")
	}

	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	got, err := s.GetUIState("client:test")
	if err != nil || got["k"] != "during" {
		t.Fatalf("read after commit: %v %v", err, got)
	}
}

func TestQuiesceReadsRestoresRouting(t *testing.T) {
	s := newTestStore(t)
	ran := false
	err := s.quiesceReads(func() error {
		ran = true
		if s.reader() != s.db {
			t.Error("reads must route to the writer while quiesced")
		}
		return nil
	})
	if err != nil || !ran {
		t.Fatalf("quiesceReads: ran=%v err=%v", ran, err)
	}
	if s.reader() != s.read {
		t.Fatal("read-pool routing must be restored after quiesce")
	}
}

func TestVacuumRunsWithReadPoolOpen(t *testing.T) {
	s := newTestStore(t)
	// Zero thresholds force the VACUUM branch regardless of freelist
	// state, exercising the quiesce + exclusive-lock path with the read
	// pool open.
	ran, err := s.vacuumIfFragmented(0, 0)
	if err != nil {
		t.Fatalf("vacuum with read pool open: %v", err)
	}
	if !ran {
		t.Fatal("vacuum should run at zero thresholds")
	}
	// The store must still serve reads and writes afterwards.
	if err := s.SetUIState("client:test", map[string]string{"k": "post"}); err != nil {
		t.Fatalf("write after vacuum: %v", err)
	}
	if got, err := s.GetUIState("client:test"); err != nil || got["k"] != "post" {
		t.Fatalf("read after vacuum: %v %v", err, got)
	}
}
