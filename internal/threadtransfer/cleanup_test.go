package threadtransfer

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"agent-overflow/internal/store"
)

func TestTransferCleanupWaitsForConfirmationAndPreservesOwnership(t *testing.T) {
	ctx := context.Background()
	source, row, peer := sourceFixture(t, "move", false)
	peer.lostActivation = true
	if _, err := source.Run(ctx, row.ID); err == nil {
		t.Fatal("expected lost confirmation")
	}
	archive := filepath.Join(source.root, row.ID, "archive.tar")
	if _, err := os.Stat(archive); err != nil {
		t.Fatal("removed unconfirmed recovery bytes", err)
	}
	if row, err := source.Run(ctx, row.ID); err != nil || row.Phase != "complete" {
		t.Fatalf("confirmation: %+v %v", row, err)
	}
	if _, err := os.Stat(filepath.Dir(archive)); !os.IsNotExist(err) {
		t.Fatal("completed archive retained", err)
	}
	if err := source.store.CheckThreadTransferAccess(row.ThreadID); err == nil {
		t.Fatal("cleanup forgot retirement")
	}
	if jobs, err := source.store.NextThreadTransferJobs(8); err != nil || len(jobs) != 0 {
		t.Fatalf("cleanup kept polling: %+v %v", jobs, err)
	}
	if _, err := source.Run(ctx, row.ID); err != nil {
		t.Fatal("completed retry needs deleted archive", err)
	}
}

func TestTransferCleanupRecoversTerminalJournalAfterRestart(t *testing.T) {
	ctx := context.Background()
	source, row, peer := sourceFixture(t, "copy", false)
	// Simulate the crash after terminal SQL commit, before private bytes were
	// removed. Restart must schedule cleanup even without a bound peer.
	if _, err := source.store.RequestThreadTransferCancellation(row.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := source.store.AdvanceThreadTransfer(row.ID, "canceled", row.ManifestHash); err != nil {
		t.Fatal(err)
	}
	private := filepath.Join(source.root, row.ID)
	if err := os.WriteFile(filepath.Join(private, ".chunk-crash"), []byte("uncommitted chunk"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := source.store.FinishThreadTransferAttempt(row.ID, 1<<62, 4, "cleanup interrupted"); err != nil {
		t.Fatal(err)
	}
	restarted := resumeSource(t, source, peer)
	finished := make(chan string, 8)
	unused := runnerFunc(func(context.Context, string) (store.ThreadTransfer, error) {
		t.Error("unexpected receiver")
		return store.ThreadTransfer{}, ErrPending
	})
	jobs := testJobs(t, restarted.store, restarted, unused, func(row store.ThreadTransfer) { finished <- row.ID })
	if id := nextJobSignal(t, finished); id != row.ID {
		t.Fatal("wrong cleanup", id)
	}
	jobs.Close()
	if _, err := os.Stat(private); !os.IsNotExist(err) {
		t.Fatal("restart left private bytes", err)
	}
	if err := restarted.store.CheckThreadTransferAccess(row.ThreadID); err != nil {
		t.Fatal("canceled source stayed fenced", err)
	}
	if _, err := restarted.Run(ctx, row.ID); err != nil {
		t.Fatal(err)
	}
}

func TestTransferCleanupRefusesLiveOperation(t *testing.T) {
	source, row, _ := sourceFixture(t, "move", false)
	if err := cleanupTransfer(context.Background(), source.store, source.root, row); err == nil {
		t.Fatal("cleaned pending recovery bytes")
	}
	if err := source.store.FinishThreadTransferCleanup(row.ID); err == nil {
		t.Fatal("marked pending operation disposable")
	}
	if _, err := os.Stat(filepath.Join(source.root, row.ID, "archive.tar")); err != nil {
		t.Fatal(err)
	}
}
