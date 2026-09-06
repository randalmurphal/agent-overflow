// Package threadtransfer coordinates the fixed conversation handoff protocol.
// Provider snapshots and destination installation are supplied by the app;
// durable ownership belongs to store, and bytes belong to transferfiles.
package threadtransfer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"agent-overflow/internal/keyedlock"
	"agent-overflow/internal/store"
	"agent-overflow/internal/transferclient"
	"agent-overflow/internal/transferwire"
)

// SourceData is private journal state. The archive is always the fixed
// <operations root>/<operation>/archive.tar, not a path supplied over the wire.
// The app may add snapshot/recovery details without teaching this pipe provider
// formats. Those details remain covered by the immutable journal request.
type SourceData struct {
	ActivationSecret string          `json:"activationSecret"`
	Details          json.RawMessage `json:"details,omitempty"`
}

// SourceSnapshotter creates archive.tar and a durable completion marker in the
// operation directory under app/native action locks. It is restartable: once
// marked complete, it returns the SAME snapshot after a lost SQLite seal write.
// This runs on the host after both ends are authorized, not in a frontend RPC.
type SourceSnapshotter func(context.Context, store.ThreadTransfer, string) (transferwire.Upload, error)

// ErrPending means the peer accepted asynchronous preparation. Keep showing
// transfer progress and retry through the app-owned job's ordinary backoff.
var ErrPending = errors.New("transfer: waiting for destination preparation")
var errCancelRequested = errors.New("transfer: source cancellation requested")

type sourcePeer interface {
	Status(context.Context) (transferwire.State, error)
	BeginUpload(context.Context, transferwire.Upload) (transferwire.State, error)
	Chunk(context.Context, int64, string, []byte) (transferwire.State, error)
	Prepare(context.Context) (transferwire.State, error)
	Activate(context.Context, string) (transferwire.State, error)
	Cancel(context.Context, string) (transferwire.State, error)
	Close()
}

// Source holds only active-operation coordination, never a cached projection
// of journal rows. One app owns one Source. App-lifetime contexts keep accepted
// transfers alive after the initiating frontend disconnects.
type Source struct {
	store    *store.Store
	root     string
	locks    *keyedlock.Registry
	slots    chan struct{}
	peer     func(transferclient.Offer) (sourcePeer, error)
	snapshot SourceSnapshotter
}

func NewSource(st *store.Store, root string, snapshot SourceSnapshotter) (*Source, error) {
	if st == nil || !filepath.IsAbs(root) {
		return nil, errors.New("transfer: missing source store or operations directory")
	}
	return &Source{store: st, root: root, locks: keyedlock.New(), slots: make(chan struct{}, 4), snapshot: snapshot,
		peer: func(offer transferclient.Offer) (sourcePeer, error) { return transferclient.New(offer) }}, nil
}

// Run resumes the durable operation. Network errors preserve its phase and
// archive. In particular, a committed source is never made runnable by a failed
// Activate response; the next run asks whether the destination completed it.
func (s *Source) Run(ctx context.Context, id string) (result store.ThreadTransfer, runErr error) {
	unlock, err := s.locks.LockCtx(ctx, id)
	if err != nil {
		return store.ThreadTransfer{}, err
	}
	defer unlock()
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	case <-ctx.Done():
		return store.ThreadTransfer{}, ctx.Err()
	}
	row, err := s.store.GetThreadTransfer(id)
	if err != nil {
		return row, err
	}
	if row.Direction != "outgoing" {
		return row, errors.New("transfer: operation is not outgoing")
	}
	defer func() {
		if runErr == nil && (result.Phase == "complete" || result.Phase == "canceled") {
			runErr = cleanupTransfer(ctx, s.store, s.root, result)
		}
	}()
	if row.Phase == "complete" || row.Phase == "canceled" {
		return row, nil
	}
	if len(row.PeerState) == 0 && row.CancelRequested && row.Phase != "committed" {
		return s.store.AdvanceThreadTransfer(id, "canceled", row.ManifestHash)
	}
	var data SourceData
	if err := json.Unmarshal(row.PrivateState, &data); err != nil {
		return row, errors.New("transfer: source recovery data is unreadable")
	}
	secret, err := transferwire.DecodeSecret(data.ActivationSecret)
	if err != nil {
		return row, err
	}
	hash := sha256.Sum256(secret)
	if hex.EncodeToString(hash[:]) != row.ActivationHash {
		return row, errors.New("transfer: source snapshot identity is invalid")
	}
	if len(row.PeerState) == 0 {
		if row.Phase == "committed" {
			return row, errors.New("transfer: retired source lost its destination offer")
		}
		return row, ErrPending
	}
	var offer transferclient.Offer
	if json.Unmarshal(row.PeerState, &offer) != nil || offer.OperationID != id || offer.BackendID != row.PeerBackendID || offer.OwnershipEpoch != row.OwnershipEpoch {
		return row, errors.New("transfer: destination offer does not match the operation")
	}
	peer, err := s.peer(offer)
	if err != nil {
		return row, err
	}
	defer peer.Close()
	if row.CancelRequested {
		return s.cancel(ctx, row, data, peer)
	}
	state, err := peer.Status(ctx)
	if err != nil {
		return row, err
	}
	if state.OwnershipEpoch != row.OwnershipEpoch {
		return row, errors.New("transfer: destination ownership epoch does not match the operation")
	}
	if state.Phase == "canceled" {
		if row.Phase == "committed" {
			return row, errors.New("transfer: destination canceled after source retirement")
		}
		// An authenticated recipient may discard an UNPREPARED offer while
		// the phone is offline. It never learned activation proof; its durable
		// canceled status is sufficient to release an unretired source.
		if _, err := s.store.RequestThreadTransferCancellation(id); err != nil {
			return row, err
		}
		return s.store.AdvanceThreadTransfer(id, "canceled", row.ManifestHash)
	}
	var archive transferwire.Upload
	row, archive, err = s.sourceArchive(ctx, row)
	if err != nil {
		return row, err
	}
	if row.CancelRequested {
		return s.cancel(ctx, row, data, peer)
	}
	if err := matchingSnapshot(state, archive, row.OwnershipEpoch); err != nil {
		return row, err
	}
	if state.Phase == "complete" {
		if row.Phase != "committed" {
			return row, errors.New("transfer: destination activated before source retirement")
		}
		return s.store.AdvanceThreadTransfer(id, "complete", archive.SHA256)
	}
	if state.Phase == "preparing" {
		state, err = s.upload(ctx, row, archive, peer, state)
		if err != nil && !errors.Is(err, errCancelRequested) {
			return row, err
		}
	}
	// Cancellation can arrive while bytes are in flight. Read its durable
	// intent again before preparation/retirement; the store's commit gate is
	// the final guard against a cancellation arriving after this read.
	row, readErr := s.store.GetThreadTransfer(id)
	if readErr != nil {
		return row, readErr
	}
	if row.CancelRequested {
		return s.cancel(ctx, row, data, peer)
	}
	if err != nil {
		return row, err
	}
	if state.Phase == "preparing" {
		state, err = peer.Prepare(ctx)
		if err != nil {
			return row, err
		}
	}
	if err := matchingSnapshot(state, archive, row.OwnershipEpoch); err != nil {
		return row, err
	}
	if state.Phase == "preparing" {
		return row, pendingDestination(state)
	}
	if state.Phase != "prepared" {
		return row, errors.New("transfer: destination is not prepared")
	}
	if row.Phase != "committed" {
		row, err = s.store.AdvanceThreadTransfer(id, "prepared", archive.SHA256)
		if err != nil {
			return row, err
		}
		row, err = s.store.AdvanceThreadTransfer(id, "committed", archive.SHA256)
		if err != nil {
			return row, err
		}
	}
	// This is the first place the source releases its activation secret.
	// The source's committed tombstone is already durable above.
	state, err = peer.Activate(ctx, data.ActivationSecret)
	if err != nil {
		return row, err
	}
	if err := matchingSnapshot(state, archive, row.OwnershipEpoch); err != nil {
		return row, err
	}
	if state.Phase != "complete" {
		return row, pendingDestination(state)
	}
	return s.store.AdvanceThreadTransfer(id, "complete", archive.SHA256)
}

func pendingDestination(state transferwire.State) error {
	if state.NeedsAttention {
		return errors.New("The destination could not finish this transfer. Check its transfer details, then retry.")
	}
	return ErrPending
}

func matchingSnapshot(state transferwire.State, archive transferwire.Upload, ownershipEpoch int64) error {
	if state.OwnershipEpoch != ownershipEpoch {
		return errors.New("transfer: destination ownership epoch does not match the operation")
	}
	if state.SHA256 != "" && (state.SHA256 != archive.SHA256 || state.Size != archive.Size) {
		return errors.New("transfer: destination is preparing different content")
	}
	if state.Phase != "preparing" && state.Phase != "canceled" && (state.SHA256 != archive.SHA256 || state.Size != archive.Size || state.Received != archive.Size) {
		return errors.New("transfer: destination has not verified the complete snapshot")
	}
	return nil
}

func (s *Source) cancel(ctx context.Context, row store.ThreadTransfer, data SourceData, peer sourcePeer) (store.ThreadTransfer, error) {
	if row.Phase == "committed" {
		return row, errors.New("transfer: a retired source cannot cancel activation")
	}
	state, err := peer.Cancel(ctx, data.ActivationSecret)
	if err != nil {
		return row, err
	}
	if state.Phase != "canceled" {
		return row, errors.New("transfer: destination did not acknowledge cancellation")
	}
	return s.store.AdvanceThreadTransfer(row.ID, "canceled", row.ManifestHash)
}

func (s *Source) upload(ctx context.Context, row store.ThreadTransfer, archive transferwire.Upload, peer sourcePeer, state transferwire.State) (transferwire.State, error) {
	var err error
	if state.SHA256 == "" {
		state, err = peer.BeginUpload(ctx, archive)
		if err != nil {
			return state, err
		}
	}
	if err := matchingSnapshot(state, archive, row.OwnershipEpoch); err != nil {
		return state, err
	}
	if state.Received == archive.Size {
		return state, nil
	}
	root, err := os.OpenRoot(s.root)
	if err != nil {
		return state, err
	}
	defer root.Close()
	file, err := root.Open(filepath.Join(row.ID, "archive.tar"))
	if err != nil {
		return state, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return state, err
	}
	if !info.Mode().IsRegular() || info.Size() != archive.Size {
		return state, errors.New("transfer: source archive is missing or changed")
	}
	buffer := make([]byte, 4<<20)
	chunkSize := len(buffer)
	for state.Received < archive.Size {
		if err := ctx.Err(); err != nil {
			return state, err
		}
		cancelRequested, err := s.store.ThreadTransferCancellationRequested(row.ID)
		if err != nil {
			return state, err
		}
		if cancelRequested {
			return state, errCancelRequested
		}
		length := min(int64(chunkSize), archive.Size-state.Received)
		chunk := buffer[:length]
		if _, err := file.ReadAt(chunk, state.Received); err != nil {
			return state, err
		}
		hash := sha256.Sum256(chunk)
		next, err := peer.Chunk(ctx, state.Received, hex.EncodeToString(hash[:]), chunk)
		if err != nil {
			// A slow link needs a smaller request, not an endless attempt to
			// push the same 4 MiB inside a two-minute deadline. Re-read the
			// checkpoint first: the timed-out request may have committed.
			if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil && chunkSize > 64<<10 {
				chunkSize /= 4
				state, err = peer.Status(ctx)
				if err != nil {
					return state, err
				}
				if err := matchingSnapshot(state, archive, row.OwnershipEpoch); err != nil {
					return state, err
				}
				continue
			}
			return state, err
		}
		if err := matchingSnapshot(next, archive, row.OwnershipEpoch); err != nil {
			return state, err
		}
		if next.Received < state.Received+length || next.Received > archive.Size {
			return state, errors.New("transfer: destination acknowledged an invalid byte range")
		}
		state = next
	}
	return state, nil
}
