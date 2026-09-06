package threadtransfer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"agent-overflow/internal/entityid"
	"agent-overflow/internal/store"
	"agent-overflow/internal/store/storetest"
	"agent-overflow/internal/transferclient"
	"agent-overflow/internal/transferfiles"
	"agent-overflow/internal/transferwire"
)

func TestMain(m *testing.M) { os.Exit(storetest.Run(m)) }

type sourceTestPeer struct {
	storePath                                                 string
	t                                                         *testing.T
	store                                                     *store.Store
	id, secret                                                string
	state                                                     transferwire.State
	bytes                                                     []byte
	lostChunk, lostActivation, lostCancel, slow, asyncPrepare bool
	onPrepare                                                 func()
	activations, cancellations, chunks                        int
	sizes                                                     []int
}

func (p *sourceTestPeer) Close()                                             {}
func (p *sourceTestPeer) Status(context.Context) (transferwire.State, error) { return p.state, nil }
func (p *sourceTestPeer) BeginUpload(_ context.Context, u transferwire.Upload) (transferwire.State, error) {
	p.state.SHA256, p.state.Size = u.SHA256, u.Size
	return p.state, nil
}
func (p *sourceTestPeer) Chunk(_ context.Context, offset int64, digest string, data []byte) (transferwire.State, error) {
	p.sizes = append(p.sizes, len(data))
	if p.slow {
		p.slow = false
		return p.state, context.DeadlineExceeded
	}
	hash := sha256.Sum256(data)
	if digest != hex.EncodeToString(hash[:]) || offset != p.state.Received {
		p.t.Fatal("sender used wrong byte checkpoint or content digest")
	}
	p.bytes = append(p.bytes, data...)
	p.state.Received += int64(len(data))
	p.chunks++
	if p.lostChunk {
		p.lostChunk = false
		return transferwire.State{}, io.ErrUnexpectedEOF
	}
	return p.state, nil
}
func (p *sourceTestPeer) Prepare(context.Context) (transferwire.State, error) {
	hash := sha256.Sum256(p.bytes)
	if p.state.Received != p.state.Size || hex.EncodeToString(hash[:]) != p.state.SHA256 {
		p.t.Fatal("prepared incomplete source content")
	}
	if p.onPrepare != nil {
		p.onPrepare()
	}
	if p.asyncPrepare {
		p.asyncPrepare = false
		return p.state, nil
	}
	p.state.Phase = "prepared"
	return p.state, nil
}
func (p *sourceTestPeer) Activate(_ context.Context, secret string) (transferwire.State, error) {
	row, err := p.store.GetThreadTransfer(p.id)
	if err != nil || row.Phase != "committed" || row.CancelRequested || secret != p.secret {
		p.t.Fatalf("activation released before durable source retirement: %+v %v", row, err)
	}
	p.activations++
	p.state.Phase = "complete"
	if p.lostActivation {
		p.lostActivation = false
		return transferwire.State{}, io.ErrUnexpectedEOF
	}
	return p.state, nil
}
func (p *sourceTestPeer) Cancel(_ context.Context, secret string) (transferwire.State, error) {
	row, err := p.store.GetThreadTransfer(p.id)
	if err != nil || !row.CancelRequested || row.Phase == "committed" || secret != p.secret {
		p.t.Fatalf("cancel proof released without durable source intent: %+v %v", row, err)
	}
	if p.state.Phase != "canceled" {
		p.cancellations++
	}
	p.state.Phase = "canceled"
	if p.lostCancel {
		p.lostCancel = false
		return transferwire.State{}, io.ErrUnexpectedEOF
	}
	return p.state, nil
}

func TestSourceReportsRecipientFailureWithoutRetiring(t *testing.T) {
	source, row, peer := sourceFixture(t, "move", false)
	peer.asyncPrepare = true
	peer.state.NeedsAttention = true
	current, err := source.Run(context.Background(), row.ID)
	if err == nil || errors.Is(err, ErrPending) || current.Phase == "committed" || peer.activations != 0 {
		t.Fatalf("recipient failure hidden or source retired: phase=%s err=%v activations=%d", current.Phase, err, peer.activations)
	}
}

func sourceFixture(t *testing.T, kind string, large bool, hostSnapshot ...bool) (*Source, store.ThreadTransfer, *sourceTestPeer) {
	t.Helper()
	dbPath := storetest.ClonePath(t)
	st, err := store.New(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	peer := &sourceTestPeer{t: t, store: st, storePath: dbPath, state: transferwire.State{Phase: "preparing"}}
	t.Cleanup(func() {
		if err := peer.store.Close(); err != nil {
			t.Error(err)
		}
	})
	root := t.TempDir()
	id := entityid.New()
	dir := filepath.Join(root, id)
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	native := t.TempDir()
	content := []byte("native history")
	if large {
		content = bytes.Repeat(content, 150_000)
	}
	if err := os.WriteFile(filepath.Join(native, "session.jsonl"), content, 0600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(dir, "archive.tar")
	digest, err := transferfiles.Create(context.Background(), archive, []transferfiles.Source{{Root: native, Path: "session.jsonl", Name: "native/session.jsonl"}})
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(archive)
	if err != nil {
		t.Fatal(err)
	}
	secret := bytes.Repeat([]byte{0xa5}, 32)
	hash := sha256.Sum256(secret)
	data := SourceData{ActivationSecret: base64.RawURLEncoding.EncodeToString(secret)}
	private, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	request := store.ThreadTransfer{ID: id, ThreadID: entityid.New(), PeerBackendID: entityid.New(), Direction: "outgoing", Kind: kind, ActivationHash: hex.EncodeToString(hash[:]), PrivateState: private}
	row, err := st.CreateThreadTransfer(request)
	if err != nil {
		t.Fatal(err)
	}
	if len(hostSnapshot) == 0 || !hostSnapshot[0] {
		row, err = st.BindThreadTransferArchive(row.ID, transferwire.Upload{SHA256: digest, Size: info.Size()})
		if err != nil {
			t.Fatal(err)
		}
	}
	offer, _ := json.Marshal(transferclient.Offer{Version: 1, BackendID: row.PeerBackendID, OperationID: row.ID, OwnershipEpoch: row.OwnershipEpoch})
	if _, err := st.BindThreadTransferPeer(row.ID, offer); err != nil {
		t.Fatal(err)
	}
	source, err := NewSource(st, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	peer.id, peer.secret = row.ID, data.ActivationSecret
	peer.state.OwnershipEpoch = row.OwnershipEpoch
	source.peer = func(transferclient.Offer) (sourcePeer, error) { return peer, nil }
	return source, row, peer
}

func resumeSource(t *testing.T, previous *Source, peer *sourceTestPeer) *Source {
	t.Helper()
	if err := previous.store.Close(); err != nil {
		t.Fatal(err)
	}
	opened, err := store.New(peer.storePath)
	if err != nil {
		t.Fatal(err)
	}
	previous.store, peer.store = opened, opened
	resumed, err := NewSource(previous.store, previous.root, previous.snapshot)
	if err != nil {
		t.Fatal(err)
	}
	resumed.peer = func(transferclient.Offer) (sourcePeer, error) { return peer, nil }
	return resumed
}

func TestSourceAcceptsDestinationDiscardWithoutRetiring(t *testing.T) {
	source, row, peer := sourceFixture(t, "move", false, true)
	peer.state.Phase = "canceled"
	result, err := source.Run(context.Background(), row.ID)
	if err != nil || result.Phase != "canceled" || peer.activations != 0 {
		t.Fatalf("discard did not release source: %+v %v, activations %d", result, err, peer.activations)
	}
}

func TestSourceMoveAndCopyCommitBeforeActivation(t *testing.T) {
	for _, kind := range []string{"move", "copy"} {
		t.Run(kind, func(t *testing.T) {
			source, row, peer := sourceFixture(t, kind, false)
			result, err := source.Run(context.Background(), row.ID)
			if err != nil || result.Phase != "complete" || peer.activations != 1 {
				t.Fatalf("handoff: %+v %v", result, err)
			}
			err = source.store.CheckThreadTransferAccess(row.ThreadID)
			var moved *store.ThreadTransferError
			if kind == "move" && (!errors.As(err, &moved) || !moved.Moved) {
				t.Fatal("completed move left source runnable")
			}
			if kind == "copy" && err != nil {
				t.Fatal("completed copy fenced its source")
			}
			if _, err := source.Run(context.Background(), row.ID); err != nil || peer.activations != 1 {
				t.Fatal("duplicate run repeated activation")
			}
		})
	}
}

func TestSourceRecoversUnknownUploadAndActivationOutcomes(t *testing.T) {
	for _, loss := range []string{"upload", "activation"} {
		t.Run(loss, func(t *testing.T) {
			source, row, peer := sourceFixture(t, "move", false)
			peer.lostChunk, peer.lostActivation = loss == "upload", loss == "activation"
			if _, err := source.Run(context.Background(), row.ID); err == nil {
				t.Fatal("unknown outcome was reported complete")
			}
			pending, err := source.store.GetThreadTransfer(row.ID)
			if err != nil {
				t.Fatal(err)
			}
			if loss == "activation" {
				if pending.Phase != "committed" {
					t.Fatal("lost activation reply undid retirement")
				}
				// A successful peer does not need the source archive a second
				// time; completion is resolved from its durable acknowledgment.
				if err := os.Remove(filepath.Join(source.root, row.ID, "archive.tar")); err != nil {
					t.Fatal(err)
				}
			} else if pending.Phase != "preparing" {
				t.Fatal("retired before preparation")
			}
			result, err := resumeSource(t, source, peer).Run(context.Background(), row.ID)
			if err != nil || result.Phase != "complete" || peer.activations != 1 || peer.chunks != 1 {
				t.Fatalf("recovery repeated work: %+v %v, chunks %d, activations %d", result, err, peer.chunks, peer.activations)
			}
		})
	}
}

func TestSourceCancellationRecoversLostReplyAndRacingPrepare(t *testing.T) {
	for _, when := range []string{"before upload", "during prepare"} {
		t.Run(when, func(t *testing.T) {
			source, row, peer := sourceFixture(t, "move", false)
			requestCancel := func() {
				if _, err := source.store.RequestThreadTransferCancellation(row.ID); err != nil {
					t.Fatal(err)
				}
			}
			if when == "before upload" {
				requestCancel()
				peer.lostCancel = true
			} else {
				peer.onPrepare = requestCancel
			}
			if _, err := source.Run(context.Background(), row.ID); err == nil {
				t.Fatal("expected interrupted cancel or commit refusal")
			}
			if peer.activations != 0 {
				t.Fatal("canceling transfer activated")
			}
			result, err := resumeSource(t, source, peer).Run(context.Background(), row.ID)
			if err != nil || result.Phase != "canceled" || peer.cancellations != 1 {
				t.Fatalf("cancel recovery: %+v %v", result, err)
			}
			if err := source.store.CheckThreadTransferAccess(row.ThreadID); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSourceWaitsForAsyncPreparationWithoutRetransmitting(t *testing.T) {
	source, row, peer := sourceFixture(t, "move", false)
	peer.asyncPrepare = true
	if _, err := source.Run(context.Background(), row.ID); !errors.Is(err, ErrPending) {
		t.Fatalf("preparation wait: %v", err)
	}
	if peer.activations != 0 {
		t.Fatal("activated before prepare finished")
	}
	result, err := source.Run(context.Background(), row.ID)
	if err != nil || result.Phase != "complete" || peer.chunks != 1 {
		t.Fatalf("async prepare resume: %+v %v", result, err)
	}
}

func TestSourceShrinksTimedOutChunksWithoutGuessingTheCheckpoint(t *testing.T) {
	source, row, peer := sourceFixture(t, "move", true)
	peer.slow = true
	result, err := source.Run(context.Background(), row.ID)
	if err != nil || result.Phase != "complete" {
		t.Fatalf("slow connection: %+v %v", result, err)
	}
	if len(peer.sizes) < 3 || peer.sizes[1] >= peer.sizes[0] {
		t.Fatalf("did not reduce request size: %v", peer.sizes)
	}
}

func TestSourceRefusesAnUnexpectedPeerSnapshotOrEarlyActivation(t *testing.T) {
	for _, change := range []string{"digest", "early activation"} {
		t.Run(change, func(t *testing.T) {
			source, row, peer := sourceFixture(t, "move", false)
			if change == "digest" {
				peer.state.SHA256 = hex.EncodeToString(bytes.Repeat([]byte{0xb6}, 32))
				peer.state.Size = 1024
			} else {
				peer.state.Phase = "complete"
				peer.state.SHA256, peer.state.Size, peer.state.Received = row.ManifestHash, row.ArchiveSize, row.ArchiveSize
			}
			if _, err := source.Run(context.Background(), row.ID); err == nil {
				t.Fatal("accepted inconsistent peer")
			}
			pending, err := source.store.GetThreadTransfer(row.ID)
			if err != nil || pending.Phase != "preparing" || peer.activations != 0 {
				t.Fatalf("unexpected peer changed source ownership: %+v %v", pending, err)
			}
		})
	}
}
