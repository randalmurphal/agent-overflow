package store

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func remoteReceipt(t *testing.T) RemoteJob {
	t.Helper()
	return RemoteJob{ID: uuid.NewString(), OwnerID: "owner", Fingerprint: strings.Repeat("a", 64), SourceThreadID: uuid.NewString(), ProjectID: uuid.NewString(), Workspace: t.TempDir()}
}

func TestRemoteReceiptsSurviveRestoreAndRetainRetryIdentity(t *testing.T) {
	s := newTestStore(t)
	path := filepath.Join(t.TempDir(), "snapshot.db")
	if err := s.SnapshotTo(path); err != nil {
		t.Fatal(err)
	}
	r := remoteReceipt(t)
	if _, fresh, err := s.AcceptRemoteJob(r); err != nil || !fresh {
		t.Fatalf("accept: %v %v", fresh, err)
	}
	if _, err := s.RestoreFrom(path); err == nil || !strings.Contains(err.Error(), "remote commands") {
		t.Fatalf("restore during active command: %v", err)
	}
	r.State, r.Output, r.ExitCode = "succeeded", "result", 0
	if err := s.FinishRemoteJob(r); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishRemoteJob(r); err != nil {
		t.Fatalf("repeated settlement: %v", err)
	}
	if _, err := s.RestoreFrom(path); err != nil {
		t.Fatal(err)
	}
	got, fresh, err := s.AcceptRemoteJob(r)
	if err != nil || fresh || got.State != "succeeded" || got.Output != "result" {
		t.Fatalf("restored retry: %#v %v %v", got, fresh, err)
	}
}

func TestRemoteOutputRetentionNeverForgetsAcceptance(t *testing.T) {
	s := newTestStore(t)
	var first RemoteJob
	for i := range 132 {
		r := remoteReceipt(t)
		if i == 0 {
			first = r
		}
		if _, _, err := s.AcceptRemoteJob(r); err != nil {
			t.Fatal(err)
		}
		r.State, r.Output, r.ExitCode = "succeeded", "output", 0
		if err := s.FinishRemoteJob(r); err != nil {
			t.Fatal(err)
		}
	}
	var tails, receipts int
	if err := s.reader().QueryRow(`SELECT count(*),sum(output != '') FROM remote_jobs`).Scan(&receipts, &tails); err != nil {
		t.Fatal(err)
	}
	if receipts != 132 || tails != 128 {
		t.Fatalf("receipts=%d tails=%d", receipts, tails)
	}
	// Monotonic ordering uses finished_at then UUID for ties; query an actually
	// expired receipt, rather than assuming a particular clock resolution.
	var id string
	if err := s.reader().QueryRow(`SELECT id FROM remote_jobs WHERE output='' LIMIT 1`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	row, err := s.GetRemoteJob(id)
	if err != nil || !row.Truncated {
		t.Fatalf("expired: %#v %v", row, err)
	}
	_, fresh, err := s.AcceptRemoteJob(first)
	if err != nil || fresh {
		t.Fatalf("old retry: %v %v", fresh, err)
	}
}
