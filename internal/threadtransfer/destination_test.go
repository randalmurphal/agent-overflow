package threadtransfer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/entityid"
	"agent-overflow/internal/store"
	"agent-overflow/internal/store/storetest"
	"agent-overflow/internal/transferclient"
	"agent-overflow/internal/transferfiles"
	"agent-overflow/internal/transferwire"
	"agent-overflow/internal/transport"
)

// This installer exercises the durable file/history boundary with inert native
// bytes. Provider-specific portability is tested in the provider packages.
type destinationTestInstaller struct {
	t                           *testing.T
	st                          *store.Store
	target                      store.Thread
	native                      string
	preparations, installations int
	failAfterFiles              bool
}

func (i *destinationTestInstaller) Prepare(ctx context.Context, row store.ThreadTransfer, stage string, files []transferfiles.File) (json.RawMessage, error) {
	i.preparations++
	// Parse the whole history into an isolated database. Validation must not
	// hold the destination's live SQLite writer while reading a large history.
	scratch, err := store.New(storetest.ClonePath(i.t))
	if err != nil {
		return nil, err
	}
	defer scratch.Close()
	history, err := os.Open(filepath.Join(stage, "history.ndjson"))
	if err != nil {
		return nil, err
	}
	defer history.Close()
	if err := scratch.ImportThreadHistory(ctx, i.target, history); err != nil {
		return nil, err
	}
	var targets []transferfiles.InstallTarget
	for _, file := range files {
		if file.Name == "native/session.jsonl" {
			targets = append(targets, transferfiles.InstallTarget{File: file, Root: "native", Path: "session.jsonl"})
		}
	}
	if len(targets) != 1 {
		return nil, errors.New("missing native session")
	}
	plan, err := transferfiles.PrepareInstallation(ctx, map[string]string{"native": i.native}, targets)
	if err != nil {
		return nil, err
	}
	return json.Marshal(plan)
}

func (i *destinationTestInstaller) Install(ctx context.Context, row store.ThreadTransfer, stage string, plan json.RawMessage, secret []byte) error {
	i.installations++
	var installs []transferfiles.Installation
	if err := json.Unmarshal(plan, &installs); err != nil {
		return err
	}
	if err := transferfiles.InstallPreparedFiles(ctx, stage, map[string]string{"native": i.native}, installs); err != nil {
		return err
	}
	if i.failAfterFiles {
		i.failAfterFiles = false
		return io.ErrUnexpectedEOF
	}
	history, err := os.Open(filepath.Join(stage, "history.ndjson"))
	if err != nil {
		return err
	}
	defer history.Close()
	_, err = i.st.CommitIncomingThreadTransfer(ctx, row.ID, row.ManifestHash, secret, i.target, history)
	return err
}

type destinationFixture struct {
	d               *Destination
	installer       *destinationTestInstaller
	dbPath, root    string
	row             store.ThreadTransfer
	upload          transferwire.Upload
	archive, secret []byte
	grant           string
	wakes           int
}

func TestDestinationStatusReportsAttentionWithoutPrivateError(t *testing.T) {
	f := newDestinationFixture(t)
	if err := f.d.store.SetThreadTransferError(f.row.ID, "private/path secret diagnostic"); err != nil {
		t.Fatal(err)
	}
	state, err := f.d.Status(context.Background(), f.row.ID)
	if err != nil || !state.NeedsAttention {
		t.Fatalf("missing attention: %+v %v", state, err)
	}
	encoded, err := json.Marshal(state)
	if err != nil || bytes.Contains(encoded, []byte("private")) || bytes.Contains(encoded, []byte("secret")) {
		t.Fatalf("status leaked diagnostics: %s %v", encoded, err)
	}
}

func TestDestinationDiscardIsLimitedToUnpreparedOffers(t *testing.T) {
	for _, prepared := range []bool{false, true} {
		t.Run(fmt.Sprint(prepared), func(t *testing.T) {
			f := newDestinationFixture(t)
			ctx := context.Background()
			if prepared {
				f.uploadAndPrepare(t)
			}
			err := f.d.DiscardUnprepared(ctx, f.row.ID)
			if prepared {
				if !errors.Is(err, transferwire.ErrConflict) {
					t.Fatalf("discarded prepared offer: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := f.d.DiscardUnprepared(ctx, f.row.ID); err != nil {
				t.Fatal(err)
			}
			state, err := f.d.Status(ctx, f.row.ID)
			if err != nil || state.Phase != "canceled" {
				t.Fatalf("lost cancellation: %+v %v", state, err)
			}
			if err := f.d.BeginUpload(ctx, f.row.ID, f.upload); !errors.Is(err, transferwire.ErrConflict) {
				t.Fatalf("revived discarded offer: %v", err)
			}
		})
	}
}

func TestDestinationCannotCancelAfterAcceptingActivation(t *testing.T) {
	f := newDestinationFixture(t)
	f.uploadAndPrepare(t)
	ctx := context.Background()
	if err := f.d.Activate(ctx, f.row.ID, f.secret); err != nil {
		t.Fatal(err)
	}
	if err := f.d.Cancel(ctx, f.row.ID, f.secret); !errors.Is(err, transferwire.ErrConflict) {
		t.Fatalf("accepted cancellation after durable activation: %v", err)
	}
	if row, err := f.d.Run(ctx, f.row.ID); err != nil || row.Phase != "complete" {
		t.Fatalf("activation did not finish: %+v %v", row, err)
	}
}

func newDestinationFixture(t *testing.T, invalidHistory ...bool) *destinationFixture {
	t.Helper()
	f := &destinationFixture{dbPath: storetest.ClonePath(t), root: t.TempDir(), secret: bytes.Repeat([]byte{0xa5}, 32)}
	st, err := store.New(f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	f.installer = &destinationTestInstaller{t: t, st: st, native: t.TempDir()}
	t.Cleanup(func() { f.installer.st.Close() })
	id := entityid.New()
	f.installer.target = store.Thread{ID: entityid.New(), Provider: "claude", Title: "Transferred conversation", WorkspacePath: t.TempDir(), ProjectPath: t.TempDir(), Mode: "chat", Model: "test", RuntimeMode: "auto"}
	source, err := store.New(storetest.ClonePath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if err := source.CreateThread(f.installer.target); err != nil {
		t.Fatal(err)
	}
	if err := source.InsertItem(store.Item{ID: "answer", ThreadID: f.installer.target.ID, Kind: "assistant_text", Role: "assistant", Summary: "Native and AO history arrived together"}); err != nil {
		t.Fatal(err)
	}
	inputs := t.TempDir()
	history, err := os.Create(filepath.Join(inputs, "history.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	if err := source.ExportThreadHistory(context.Background(), f.installer.target.ID, history); err != nil {
		t.Fatal(err)
	}
	if err := history.Close(); err != nil {
		t.Fatal(err)
	}
	if len(invalidHistory) > 0 && invalidHistory[0] {
		if err := os.WriteFile(filepath.Join(inputs, "history.ndjson"), []byte("{}\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(inputs, "session.jsonl"), []byte("opaque native session"), 0600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "archive.tar")
	digest, err := transferfiles.Create(context.Background(), archive, []transferfiles.Source{
		{Root: inputs, Path: "history.ndjson", Name: "history.ndjson"},
		{Root: inputs, Path: "session.jsonl", Name: "native/session.jsonl"},
	})
	if err != nil {
		t.Fatal(err)
	}
	f.archive, err = os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	f.upload = transferwire.Upload{SHA256: digest, Size: int64(len(f.archive))}
	grant := bytes.Repeat([]byte{0xb6}, 32)
	f.grant = base64.RawURLEncoding.EncodeToString(grant)
	grantHash, activationHash := sha256.Sum256(grant), sha256.Sum256(f.secret)
	// Production can authorize a destination before the source has snapshotted
	// its files. The first upload seals its immutable content identity.
	private, _ := json.Marshal(DestinationData{GrantHash: hex.EncodeToString(grantHash[:])})
	f.row, err = st.CreateThreadTransfer(store.ThreadTransfer{ID: id, ThreadID: f.installer.target.ID, PeerBackendID: entityid.New(), Kind: "move", Direction: "incoming", OwnershipEpoch: 1, ActivationHash: hex.EncodeToString(activationHash[:]), PrivateState: private})
	if err != nil {
		t.Fatal(err)
	}
	f.d, err = NewDestination(st, f.root, f.installer, func(string) { f.wakes++ })
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *destinationFixture) uploadAndPrepare(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if err := f.d.BeginUpload(ctx, f.row.ID, f.upload); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(f.archive)
	if err := f.d.ReceiveChunk(ctx, f.row.ID, 0, int64(len(f.archive)), hex.EncodeToString(hash[:]), bytes.NewReader(f.archive)); err != nil {
		t.Fatal(err)
	}
	if err := f.d.Prepare(ctx, f.row.ID); err != nil {
		t.Fatal(err)
	}
	row, err := f.d.Run(ctx, f.row.ID)
	if !errors.Is(err, ErrPending) || row.Phase != "prepared" {
		t.Fatalf("prepare: %+v %v", row, err)
	}
}

func TestDestinationRestartsDamagedUnpreparedUpload(t *testing.T) {
	for _, damage := range []string{"checkpoint", "missing prefix"} {
		t.Run(damage, func(t *testing.T) {
			f := newDestinationFixture(t)
			ctx := context.Background()
			if err := f.d.BeginUpload(ctx, f.row.ID, f.upload); err != nil {
				t.Fatal(err)
			}
			chunk := f.archive[:512]
			hash := sha256.Sum256(chunk)
			if err := f.d.ReceiveChunk(ctx, f.row.ID, 0, 512, hex.EncodeToString(hash[:]), bytes.NewReader(chunk)); err != nil {
				t.Fatal(err)
			}
			directory := filepath.Join(f.root, f.row.ID)
			if damage == "checkpoint" {
				if err := os.WriteFile(filepath.Join(directory, "upload.json"), []byte("broken checkpoint"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Truncate(filepath.Join(directory, "archive.part"), 10); err != nil {
					t.Fatal(err)
				}
			}
			state, err := f.d.Status(ctx, f.row.ID)
			if err != nil || state.SHA256 != "" || state.Received != 0 || state.Phase != "preparing" {
				t.Fatalf("no upload restart offered: %+v %v", state, err)
			}
			// The independently bound journal identity still rejects another
			// snapshot, even though its local byte checkpoint is unreadable.
			changed := f.upload
			changed.SHA256 = strings.Repeat("a", 64)
			if err := f.d.BeginUpload(ctx, f.row.ID, changed); err == nil {
				t.Fatal("reset rebound snapshot")
			}
			f.uploadAndPrepare(t)
			if err := f.d.BeginUpload(ctx, f.row.ID, f.upload); !errors.Is(err, transferwire.ErrConflict) {
				t.Fatal("prepared upload could be reset", err)
			}
			if err := f.d.Activate(ctx, f.row.ID, f.secret); err != nil {
				t.Fatal(err)
			}
			if row, err := f.d.Run(ctx, f.row.ID); err != nil || row.Phase != "complete" {
				t.Fatalf("resumed upload did not activate: %+v %v", row, err)
			}
			if _, err := os.Stat(directory); !os.IsNotExist(err) {
				t.Fatal("receiver retained completed archive", err)
			}
			state, err = f.d.Status(ctx, f.row.ID)
			if err != nil || state.SHA256 != f.upload.SHA256 || state.Received != f.upload.Size {
				t.Fatalf("cleanup erased receipt: %+v %v", state, err)
			}
		})
	}
}

func TestDestinationRecoversCorruptBytesButRetainsInvalidArchiveRefusal(t *testing.T) {
	for _, mode := range []string{"header", "body", "padding", "invalid source"} {
		t.Run(mode, func(t *testing.T) {
			f := newDestinationFixture(t)
			ctx := context.Background()
			if mode == "invalid source" {
				f.archive[0] ^= 1
				sum := sha256.Sum256(f.archive)
				f.upload.SHA256 = hex.EncodeToString(sum[:])
			}
			if err := f.d.BeginUpload(ctx, f.row.ID, f.upload); err != nil {
				t.Fatal(err)
			}
			if err := f.d.ReceiveChunk(ctx, f.row.ID, 0, f.upload.Size, f.upload.SHA256, bytes.NewReader(f.archive)); err != nil {
				t.Fatal(err)
			}
			if mode != "invalid source" {
				damaged := append([]byte(nil), f.archive...)
				offset := 0
				if mode == "body" {
					offset = 512
				}
				if mode == "padding" {
					offset = len(damaged) - 1
				}
				damaged[offset] ^= 1
				if err := os.WriteFile(filepath.Join(f.root, f.row.ID, "archive.part"), damaged, 0600); err != nil {
					t.Fatal(err)
				}
			}
			row, err := f.d.Run(ctx, f.row.ID)
			if row.Phase != "preparing" {
				t.Fatal("damaged bytes became prepared", row.Phase)
			}
			state, statusErr := f.d.Status(ctx, f.row.ID)
			if statusErr != nil {
				t.Fatal(statusErr)
			}
			if mode == "invalid source" {
				if err == nil || errors.Is(err, ErrPending) || state.Received != f.upload.Size {
					t.Fatalf("invalid source was silently reset: %+v %v", state, err)
				}
				return
			}
			if !errors.Is(err, ErrPending) || state.Received != 0 || state.SHA256 != f.upload.SHA256 {
				t.Fatalf("damaged disk bytes not reset: %+v %v", state, err)
			}
			f.uploadAndPrepare(t)
		})
	}
}

func TestDestinationActivationResumesAfterRestartWithoutSource(t *testing.T) {
	f := newDestinationFixture(t)
	ctx := context.Background()
	if !f.d.Authorize(ctx, f.row.ID, f.grant) || f.d.Authorize(ctx, f.row.ID, "wrong") || f.d.Authorize(ctx, entityid.New(), f.grant) {
		t.Fatal("grant authorization")
	}
	f.uploadAndPrepare(t)
	if _, err := os.Stat(filepath.Join(f.installer.native, "session.jsonl")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("preparation installed runnable files")
	}
	if rows, err := f.installer.st.ListThreads(); err != nil || len(rows) != 0 {
		t.Fatalf("preparation published history: %v %v", rows, err)
	}
	if err := f.d.Activate(ctx, f.row.ID, bytes.Repeat([]byte{0x77}, 32)); !errors.Is(err, transferwire.ErrConflict) {
		t.Fatal("wrong proof accepted")
	}
	if err := f.d.Activate(ctx, f.row.ID, f.secret); err != nil {
		t.Fatal(err)
	}
	f.installer.failAfterFiles = true
	if _, err := f.d.Run(ctx, f.row.ID); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("install fault: %v", err)
	}
	if err := f.installer.st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err := store.New(f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	f.installer.st = st
	f.d, err = NewDestination(st, f.root, f.installer, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	// No new Activate request: proof and installation baseline survived.
	row, err := f.d.Run(ctx, f.row.ID)
	if err != nil || row.Phase != "complete" {
		t.Fatalf("restart: %+v %v", row, err)
	}
	if f.installer.preparations != 1 {
		t.Fatal("re-baselined destination after retirement")
	}
	hits, err := st.SearchThreadMessages("arrived together", 10)
	if err != nil || len(hits) != 1 || hits[0].OwnershipEpoch != 1 {
		t.Fatalf("history missing: %+v %v", hits, err)
	}
	if err := f.d.Activate(ctx, f.row.ID, f.secret); err != nil {
		t.Fatal(err)
	}
	if _, err := f.d.Run(ctx, f.row.ID); err != nil {
		t.Fatal(err)
	}
	if f.installer.installations != 2 {
		t.Fatal("completed activation reinstalled")
	}
}

func TestDestinationCancellationAndConflictingFilesNeverActivate(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		t.Run(map[bool]string{false: "changed destination", true: "canceled"}[cancel], func(t *testing.T) {
			f := newDestinationFixture(t)
			ctx := context.Background()
			f.uploadAndPrepare(t)
			if cancel {
				if err := f.d.Cancel(ctx, f.row.ID, f.secret); err != nil {
					t.Fatal(err)
				}
				if err := f.d.Activate(ctx, f.row.ID, f.secret); !errors.Is(err, transferwire.ErrConflict) {
					t.Fatal("canceled operation activated")
				}
			} else {
				if err := os.WriteFile(filepath.Join(f.installer.native, "session.jsonl"), []byte("another writer"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := f.d.Activate(ctx, f.row.ID, f.secret); err != nil {
					t.Fatal(err)
				}
				if _, err := f.d.Run(ctx, f.row.ID); err == nil {
					t.Fatal("overwrote unprepared destination")
				}
			}
			if rows, err := f.installer.st.ListThreads(); err != nil || len(rows) != 0 {
				t.Fatalf("published failed transfer: %+v %v", rows, err)
			}
		})
	}
}

func TestDestinationInvalidHistoryCannotPrepareOrAcceptActivation(t *testing.T) {
	f := newDestinationFixture(t, true)
	ctx := context.Background()
	if err := f.d.BeginUpload(ctx, f.row.ID, f.upload); err != nil {
		t.Fatal(err)
	}
	if err := f.d.Prepare(ctx, f.row.ID); !errors.Is(err, transferwire.ErrNotReady) {
		t.Fatalf("prepared incomplete upload: %v", err)
	}
	if _, err := f.d.Run(ctx, f.row.ID); !errors.Is(err, ErrPending) {
		t.Fatalf("incomplete upload should wait: %v", err)
	}
	hash := sha256.Sum256(f.archive)
	if err := f.d.ReceiveChunk(ctx, f.row.ID, 0, int64(len(f.archive)), hex.EncodeToString(hash[:]), bytes.NewReader(f.archive)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.d.Run(ctx, f.row.ID); err == nil || errors.Is(err, ErrPending) {
		t.Fatalf("invalid history prepared: %v", err)
	}
	state, err := f.d.Status(ctx, f.row.ID)
	if err != nil || state.Phase != "preparing" {
		t.Fatalf("invalid history state: %+v %v", state, err)
	}
	if err := f.d.Activate(ctx, f.row.ID, f.secret); !errors.Is(err, transferwire.ErrConflict) {
		t.Fatal("invalid history accepted activation")
	}
}

type pausedDestinationInstaller struct {
	DestinationInstaller
	started, release chan struct{}
}

func (p pausedDestinationInstaller) Install(ctx context.Context, row store.ThreadTransfer, stage string, plan json.RawMessage, secret []byte) error {
	close(p.started)
	select {
	case <-p.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return p.DestinationInstaller.Install(ctx, row, stage, plan, secret)
}

func TestDestinationStatusDoesNotWaitForInstallation(t *testing.T) {
	f := newDestinationFixture(t)
	f.uploadAndPrepare(t)
	ctx := context.Background()
	paused := pausedDestinationInstaller{DestinationInstaller: f.installer, started: make(chan struct{}), release: make(chan struct{})}
	f.d.installer = paused
	if err := f.d.Activate(ctx, f.row.ID, f.secret); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := f.d.Run(ctx, f.row.ID); done <- err }()
	<-paused.started
	defer func() {
		close(paused.release)
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	readCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	state, err := f.d.Status(readCtx, f.row.ID)
	if err != nil || state.Phase != "prepared" {
		t.Fatalf("status blocked behind install: %+v %v", state, err)
	}
}

func TestSourceAndDestinationHandoffThroughRealHTTP(t *testing.T) {
	f := newDestinationFixture(t)
	ctx := context.Background()
	backendID := entityid.New()
	server, err := transport.New(transport.Config{Dispatcher: transport.NewDispatcher(), EventBus: transport.NewEventBus(8),
		ThreadTransfers: f.d, BackendIdentity: func() (string, string) { return backendID, "generation" }})
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	defer server.Shutdown(ctx)
	sourceStore, err := store.New(storetest.ClonePath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer sourceStore.Close()
	if err := sourceStore.CreateThread(f.installer.target); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	directory := filepath.Join(root, f.row.ID)
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "archive.tar"), f.archive, 0600); err != nil {
		t.Fatal(err)
	}
	private, _ := json.Marshal(SourceData{ActivationSecret: base64.RawURLEncoding.EncodeToString(f.secret)})
	row, err := sourceStore.CreateThreadTransfer(store.ThreadTransfer{ID: f.row.ID, ThreadID: f.row.ThreadID,
		PeerBackendID: backendID, Direction: "outgoing", Kind: "move", ActivationHash: f.row.ActivationHash, PrivateState: private})
	if err != nil {
		t.Fatal(err)
	}
	offer, _ := json.Marshal(transferclient.Offer{Version: transferwire.Version, BackendID: backendID, OperationID: row.ID,
		OwnershipEpoch: row.OwnershipEpoch, Endpoint: "http://" + server.Addr(), Grant: f.grant})
	if _, err := sourceStore.BindThreadTransferPeer(row.ID, offer); err != nil {
		t.Fatal(err)
	}
	if _, err := sourceStore.BindThreadTransferArchive(row.ID, f.upload); err != nil {
		t.Fatal(err)
	}
	source, err := NewSource(sourceStore, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	// These are independent app jobs. Neither borrows a browser request's
	// lifetime, and each round uses the real HTTP grant/identity/chunk checks.
	row, err = source.Run(ctx, row.ID)
	if !errors.Is(err, ErrPending) || row.Phase != "preparing" {
		t.Fatalf("source upload: %+v %v", row, err)
	}
	if _, err := f.d.Run(ctx, row.ID); !errors.Is(err, ErrPending) {
		t.Fatalf("destination preparation: %v", err)
	}
	row, err = source.Run(ctx, row.ID)
	if !errors.Is(err, ErrPending) || row.Phase != "committed" {
		t.Fatalf("source retirement: %+v %v", row, err)
	}
	var retired *store.ThreadTransferError
	if !errors.As(sourceStore.CheckThreadTransferAccess(row.ThreadID), &retired) || !retired.Moved {
		t.Fatal("source still runnable after releasing activation")
	}
	if _, err := f.d.Run(ctx, row.ID); err != nil {
		t.Fatalf("destination activation: %v", err)
	}
	row, err = source.Run(ctx, row.ID)
	if err != nil || row.Phase != "complete" {
		t.Fatalf("source confirmation: %+v %v", row, err)
	}
	if err := f.installer.st.CheckThreadTransferAccess(row.ThreadID); err != nil {
		t.Fatal(err)
	}
}
