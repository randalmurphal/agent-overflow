package store

import (
	"slices"
	"testing"
)

func TestThreadIDsOlderThan(t *testing.T) {
	s := newTestStore(t)

	// Empty DB returns nothing.
	if ids, err := s.ThreadIDsOlderThan(1_000_000); err != nil {
		t.Fatalf("empty store: %v", err)
	} else if len(ids) != 0 {
		t.Fatalf("empty store: got %v, want nil", ids)
	}

	// Seed four threads spanning the cutoff. UpdatedAt is what TTL keys
	// off; CreatedAt is unrelated but must be set to satisfy NOT NULL.
	threads := []struct {
		id        string
		updatedAt int64
	}{
		{"oldest", 100},
		{"middle", 500},
		{"borderline", 1000}, // equal to cutoff: NOT eligible (strict <)
		{"newest", 2000},
	}
	for _, tc := range threads {
		thr := makeThread(tc.id, "claude")
		thr.UpdatedAt = tc.updatedAt
		thr.CreatedAt = tc.updatedAt
		if err := s.CreateThread(thr); err != nil {
			t.Fatalf("create %s: %v", tc.id, err)
		}
	}

	// Cutoff before everything → none eligible.
	if ids, err := s.ThreadIDsOlderThan(0); err != nil {
		t.Fatalf("cutoff=0: %v", err)
	} else if len(ids) != 0 {
		t.Fatalf("cutoff=0: got %v, want nil", ids)
	}

	// Cutoff after everything → all eligible, oldest-first order.
	got, err := s.ThreadIDsOlderThan(3000)
	if err != nil {
		t.Fatalf("cutoff=3000: %v", err)
	}
	want := []string{"oldest", "middle", "borderline", "newest"}
	if !slices.Equal(got, want) {
		t.Fatalf("cutoff=3000: got %v, want %v", got, want)
	}

	// Cutoff equal to "borderline" updated_at → strict-less excludes it.
	got, err = s.ThreadIDsOlderThan(1000)
	if err != nil {
		t.Fatalf("cutoff=1000: %v", err)
	}
	want = []string{"oldest", "middle"}
	if !slices.Equal(got, want) {
		t.Fatalf("cutoff=1000: got %v, want %v", got, want)
	}

	// Confirms archived rows are included (uniform policy).
	if _, _, err := s.ArchiveThread("middle"); err != nil {
		t.Fatalf("archive middle: %v", err)
	}
	// ArchiveThread bumps updated_at to nowMillis(), so middle no longer
	// qualifies under the 1000 cutoff. Re-set it directly via the DB to
	// keep the test focused on the inclusion policy.
	if _, err := s.db.Exec(`UPDATE threads SET updated_at = ? WHERE id = ?`, int64(500), "middle"); err != nil {
		t.Fatalf("reset middle updated_at: %v", err)
	}
	got, err = s.ThreadIDsOlderThan(1000)
	if err != nil {
		t.Fatalf("post-archive cutoff=1000: %v", err)
	}
	want = []string{"oldest", "middle"}
	if !slices.Equal(got, want) {
		t.Fatalf("post-archive cutoff=1000: got %v, want %v (archived rows must still match)", got, want)
	}
}

// An assistant's current and superseded threads are never retention's,
// however old; an ordinary archived thread of the same age is.
func TestThreadIDsOlderThanSkipsAssistants(t *testing.T) {
	s := newTestStore(t)
	for _, id := range []string{"current", "past", "plain"} {
		thr := makeThread(id, "claude")
		thr.UpdatedAt, thr.CreatedAt = 100, 100
		if err := s.CreateThread(thr); err != nil {
			t.Fatal(err)
		}
	}
	mustCreateAssistant(t, s, "past", "/h/past")
	if _, err := s.HandOffAssistant("past", "current", 150); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ArchiveThread("plain"); err != nil {
		t.Fatal(err)
	}
	mustExec(t, s.db, `UPDATE threads SET updated_at = 100`)
	got, err := s.ThreadIDsOlderThan(1000)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"plain"}; !slices.Equal(got, want) {
		t.Fatalf("eligible = %v, want %v", got, want)
	}
}
