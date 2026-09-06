package store

import (
	"strings"
	"testing"

	"agent-overflow/internal/transferwire"
)

func TestArchiveSealIsImmutableAndOnlyReleasesAnIndependentCopy(t *testing.T) {
	for _, kind := range []string{"move", "copy"} {
		t.Run(kind, func(t *testing.T) {
			s := newTestStore(t)
			row, err := s.CreateThreadTransfer(transferRequest("original", kind, "outgoing"))
			if err != nil {
				t.Fatal(err)
			}
			if err := s.CheckThreadTransferAccess(row.ThreadID); err == nil {
				t.Fatal("unsealed source not frozen")
			}
			archive := transferwire.Upload{SHA256: strings.Repeat("a", 64), Size: 1024}
			sealed, err := s.BindThreadTransferArchive(row.ID, archive)
			if err != nil || sealed.ManifestHash != archive.SHA256 || sealed.ArchiveSize != archive.Size {
				t.Fatalf("seal: %+v %v", sealed, err)
			}
			if err := s.CheckThreadTransferAccess(row.ThreadID); (err == nil) != (kind == "copy") {
				t.Fatalf("source ownership after %s seal: %v", kind, err)
			}
			// A slow independent upload must not prevent copying elsewhere or
			// moving the original later. An unsealed copy still owns its freeze.
			if kind == "copy" {
				if _, err := s.CreateThreadTransfer(transferRequest(row.ThreadID, "move", "outgoing")); err != nil {
					t.Fatalf("sealed copy blocks another handoff: %v", err)
				}
			}
			if _, err := s.BindThreadTransferArchive(row.ID, archive); err != nil {
				t.Fatal(err)
			}
			for _, changed := range []transferwire.Upload{{SHA256: strings.Repeat("b", 64), Size: archive.Size}, {SHA256: archive.SHA256, Size: archive.Size + 512}} {
				if _, err := s.BindThreadTransferArchive(row.ID, changed); err == nil {
					t.Fatal("replaced sealed snapshot")
				}
			}
			if _, err := s.AdvanceThreadTransfer(row.ID, "prepared", strings.Repeat("c", 64)); err == nil {
				t.Fatal("prepared a different snapshot")
			}
		})
	}
}

func TestArchiveReceiptDoesNotAuthorizeDestinationActivation(t *testing.T) {
	s := newTestStore(t)
	row, err := s.CreateThreadTransfer(transferRequest("incoming", "move", "incoming"))
	if err != nil {
		t.Fatal(err)
	}
	archive := transferwire.Upload{SHA256: strings.Repeat("a", 64), Size: 1024}
	if _, err := s.BindThreadTransferArchive(row.ID, archive); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CheckThreadTransferActivation(row.ID, archive.SHA256, transferTestSecret()); err == nil {
		t.Fatal("declared upload admitted activation without preparation")
	}
	if err := s.CheckThreadTransferAccess(row.ThreadID); err == nil {
		t.Fatal("declared upload made destination executable")
	}
}
