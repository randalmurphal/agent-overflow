package threadtransfer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"agent-overflow/internal/store"
	"agent-overflow/internal/transferwire"
)

func TestHostSnapshotSealsOnceAndCopyReleasesOriginalDuringUpload(t *testing.T) {
	source, row, peer := sourceFixture(t, "copy", false, true)
	if err := source.store.CheckThreadTransferAccess(row.ThreadID); err == nil {
		t.Fatal("unsealed copy can change during capture")
	}
	var snapshots int
	source.snapshot = func(ctx context.Context, current store.ThreadTransfer, directory string) (transferwire.Upload, error) {
		snapshots++
		if ctx.Err() != nil || current.ID != row.ID || directory != filepath.Join(source.root, row.ID) {
			t.Fatal("snapshot lost its host job context/identity")
		}
		data, err := os.ReadFile(filepath.Join(directory, "archive.tar"))
		if err != nil {
			return transferwire.Upload{}, err
		}
		hash := sha256.Sum256(data)
		return transferwire.Upload{SHA256: hex.EncodeToString(hash[:]), Size: int64(len(data))}, nil
	}
	peer.lostChunk = true
	if _, err := source.Run(context.Background(), row.ID); err == nil {
		t.Fatal("expected dropped chunk acknowledgment")
	}
	if err := source.store.CheckThreadTransferAccess(row.ThreadID); err != nil {
		t.Fatalf("sealed copy blocks original: %v", err)
	}
	sealed, err := source.store.GetThreadTransfer(row.ID)
	if err != nil || sealed.ArchiveSize == 0 || sealed.ManifestHash == "" {
		t.Fatalf("snapshot not sealed: %+v %v", sealed, err)
	}
	restarted := resumeSource(t, source, peer)
	restarted.snapshot = func(context.Context, store.ThreadTransfer, string) (transferwire.Upload, error) {
		t.Fatal("restart recaptured the now-independent original")
		return transferwire.Upload{}, nil
	}
	finished, err := restarted.Run(context.Background(), row.ID)
	if err != nil || finished.Phase != "complete" || snapshots != 1 {
		t.Fatalf("copy restart: %+v %v snapshots=%d", finished, err, snapshots)
	}
}

func TestCancellationDoesNotRequireSnapshotting(t *testing.T) {
	source, row, peer := sourceFixture(t, "move", false, true)
	source.snapshot = func(context.Context, store.ThreadTransfer, string) (transferwire.Upload, error) {
		t.Fatal("cancellation built an archive")
		return transferwire.Upload{}, nil
	}
	if _, err := source.store.RequestThreadTransferCancellation(row.ID); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(source.root, row.ID, "archive.tar")); err != nil {
		t.Fatal(err)
	}
	finished, err := source.Run(context.Background(), row.ID)
	if err != nil || finished.Phase != "canceled" || peer.cancellations != 1 {
		t.Fatalf("cancel before snapshot: %+v %v", finished, err)
	}
}

func TestFailedSnapshotKeepsSourceOwnedAndDoesNotUpload(t *testing.T) {
	source, row, peer := sourceFixture(t, "move", false, true)
	source.snapshot = func(context.Context, store.ThreadTransfer, string) (transferwire.Upload, error) {
		return transferwire.Upload{}, errors.New("workspace changed")
	}
	if _, err := source.Run(context.Background(), row.ID); err == nil {
		t.Fatal("accepted failed snapshot")
	}
	current, err := source.store.GetThreadTransfer(row.ID)
	if err != nil || current.Phase != "preparing" || current.ArchiveSize != 0 || peer.chunks != 0 || peer.activations != 0 {
		t.Fatalf("failed snapshot retired/uploaded: %+v %v", current, err)
	}
}
