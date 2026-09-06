package threadtransfer

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"agent-overflow/internal/entityid"
	"agent-overflow/internal/keyedlock"
	"agent-overflow/internal/store"
	"agent-overflow/internal/transferfiles"
	"agent-overflow/internal/transferwire"
)

// DestinationData is immutable, private offer state. Details are selected by
// the destination app, never accepted from the archive as installation paths.
type DestinationData struct {
	GrantHash string              `json:"grantHash"`
	archive   transferwire.Upload // Derived from the journal receipt, never immutable offer JSON.
	Details   json.RawMessage     `json:"details"`
}

// DestinationInstaller is the app's existing provider/workspace boundary.
// Prepare must validate complete history and native/workspace prerequisites
// without installing runnable files. Install durably installs the prepared
// recipe, then calls CommitIncomingThreadTransfer under the thread action lock.
// Both methods are restartable; the recipe is private, bounded recovery data.
type DestinationInstaller interface {
	Prepare(context.Context, store.ThreadTransfer, string, []transferfiles.File) (json.RawMessage, error)
	Install(context.Context, store.ThreadTransfer, string, json.RawMessage, []byte) error
}

// DestinationDiscarder releases inert workspace reservations before cancellation
// is acknowledged. It must recover its own interrupted cleanup and never touch
// an activated workspace. Pure-history installers need no additional cleanup.
type DestinationDiscarder interface {
	Discard(context.Context, store.ThreadTransfer, string) error
}

type Destination struct {
	store     *store.Store
	root      string
	locks     *keyedlock.Registry
	slots     chan struct{}
	installer DestinationInstaller
	wake      func(string)
}

func NewDestination(st *store.Store, root string, installer DestinationInstaller, wake func(string)) (*Destination, error) {
	if st == nil || !filepath.IsAbs(root) || installer == nil || wake == nil {
		return nil, errors.New("transfer: destination needs a store, private root, installer and job scheduler")
	}
	return &Destination{store: st, root: root, locks: keyedlock.New(), slots: make(chan struct{}, 4), installer: installer, wake: wake}, nil
}

func (d *Destination) read(id string) (store.ThreadTransfer, DestinationData, error) {
	if !entityid.Valid(id) {
		return store.ThreadTransfer{}, DestinationData{}, transferwire.ErrInvalid
	}
	row, err := d.store.GetThreadTransfer(id)
	if err != nil {
		return row, DestinationData{}, err
	}
	var data DestinationData
	if row.Direction != "incoming" || json.Unmarshal(row.PrivateState, &data) != nil ||
		!transferwire.ValidDigest(data.GrantHash) {
		return row, data, transferwire.ErrInvalid
	}
	if row.ArchiveSize != 0 {
		bound := transferwire.Upload{SHA256: row.ManifestHash, Size: row.ArchiveSize}
		if !bound.Valid() {
			return row, data, transferwire.ErrConflict
		}
		data.archive = bound
	}
	return row, data, nil
}

func (d *Destination) Authorize(ctx context.Context, id, grant string) bool {
	if ctx.Err() != nil {
		return false
	}
	_, data, err := d.read(id)
	secret, secretErr := transferwire.DecodeSecret(grant)
	if err != nil || secretErr != nil {
		return false
	}
	hash := sha256.Sum256(secret)
	expected, _ := hex.DecodeString(data.GrantHash)
	return subtle.ConstantTimeCompare(expected, hash[:]) == 1
}

// Status never waits behind file validation/installation. SQL phase and the
// atomic upload checkpoint are durable; there is no worker-owned progress map.
func (d *Destination) Status(ctx context.Context, id string) (transferwire.State, error) {
	if err := ctx.Err(); err != nil {
		return transferwire.State{}, err
	}
	row, data, err := d.read(id)
	if err != nil {
		return transferwire.State{}, err
	}
	state := transferwire.State{Phase: row.Phase, OwnershipEpoch: row.OwnershipEpoch, NeedsAttention: row.Error != "" && row.Phase != "complete" && row.Phase != "canceled"}
	if row.Phase == "prepared" || row.Phase == "complete" {
		state.SHA256, state.Size, state.Received = data.archive.SHA256, data.archive.Size, data.archive.Size
		return state, nil
	}
	if row.Phase == "canceled" {
		return state, nil
	}
	progress, err := transferfiles.ReadUpload(filepath.Join(d.root, id))
	if errors.Is(err, os.ErrNotExist) || (row.Phase == "preparing" && errors.Is(err, transferfiles.ErrUploadCorrupt)) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	if progress.SHA256 != data.archive.SHA256 || progress.Size != data.archive.Size {
		return state, transferwire.ErrConflict
	}
	state.SHA256, state.Size, state.Received = progress.SHA256, progress.Size, progress.Received
	return state, nil
}

func (d *Destination) BeginUpload(ctx context.Context, id string, upload transferwire.Upload) error {
	unlock, err := d.locks.LockCtx(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()
	row, data, err := d.read(id)
	if err != nil {
		return err
	}
	if row.Phase != "preparing" || !upload.Valid() || (data.archive != (transferwire.Upload{}) && upload != data.archive) {
		return transferwire.ErrConflict
	}
	if _, err := d.store.BindThreadTransferArchive(id, upload); err != nil {
		return transferwire.ErrConflict
	}
	_, err = transferfiles.BeginUpload(filepath.Join(d.root, id), upload.SHA256, upload.Size)
	if errors.Is(err, transferfiles.ErrUploadCorrupt) {
		_, err = transferfiles.ResetUpload(filepath.Join(d.root, id), upload.SHA256, upload.Size)
	}
	return err
}

func (d *Destination) ReceiveChunk(ctx context.Context, id string, offset, size int64, digest string, input io.Reader) error {
	unlock, err := d.locks.LockCtx(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()
	row, _, err := d.read(id)
	if err != nil {
		return err
	}
	if row.Phase != "preparing" {
		return transferwire.ErrConflict
	}
	progress, err := transferfiles.ReceiveChunk(ctx, filepath.Join(d.root, id), offset, size, digest, input)
	if err == nil && progress.Received == progress.Size {
		d.wake(id)
	}
	return err
}

// Prepare/Activate accept work quickly; the app owns its lifetime and retries.
func (d *Destination) Prepare(ctx context.Context, id string) error {
	state, err := d.Status(ctx, id)
	if err != nil {
		return err
	}
	if state.Phase == "canceled" {
		return transferwire.ErrConflict
	}
	if state.Size == 0 || state.Received != state.Size {
		return transferwire.ErrNotReady
	}
	d.wake(id)
	return nil
}

func (d *Destination) Activate(ctx context.Context, id string, secret []byte) error {
	unlock, err := d.locks.LockCtx(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()
	row, data, err := d.read(id)
	if err != nil {
		return err
	}
	if _, err := d.store.CheckThreadTransferActivation(id, data.archive.SHA256, secret); err != nil {
		return transferwire.ErrConflict
	}
	if row.Phase == "complete" {
		return nil
	}
	// Persist proof before acknowledging. Destination restart no longer needs
	// the initiating phone, or even the source, to finish an accepted install.
	if err := writeDestinationJSON(filepath.Join(d.root, id, "activation.json"), secret); err != nil {
		return err
	}
	d.wake(id)
	return nil
}

func (d *Destination) Cancel(ctx context.Context, id string, secret []byte) error {
	unlock, err := d.locks.LockCtx(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()
	if _, _, err := d.read(id); err != nil {
		return err
	}
	row, err := d.store.CheckIncomingTransferCancellation(id, secret)
	if err != nil {
		return transferwire.ErrConflict
	}
	directory := filepath.Join(d.root, id)
	if _, err := os.Lstat(filepath.Join(directory, "activation.json")); !errors.Is(err, os.ErrNotExist) {
		return transferwire.ErrConflict
	}
	if discard, ok := d.installer.(DestinationDiscarder); ok {
		if err := discard.Discard(ctx, row, directory); err != nil {
			return err
		}
	}
	row, err = d.store.CancelIncomingThreadTransfer(id, secret)
	if err != nil {
		return transferwire.ErrConflict
	}
	defer d.wake(id)
	// Cancel retries still work from the immutable journal. No execution file
	// was published, and the discarded private upload can now be released.
	return cleanupTransfer(ctx, d.store, d.root, row)
}

// DiscardUnprepared is a destination-local, authenticated app action. It is
// deliberately NOT an HTTP grant verb. It recovers an offer orphaned before the
// frontend managed to bind it on the source, while retirement is still impossible.
func (d *Destination) DiscardUnprepared(ctx context.Context, id string) error {
	unlock, err := d.locks.LockCtx(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()
	row, _, err := d.read(id)
	if err != nil {
		return err
	}
	if row.Phase != "preparing" && row.Phase != "canceled" {
		return transferwire.ErrConflict
	}
	directory := filepath.Join(d.root, id)
	if discard, ok := d.installer.(DestinationDiscarder); ok {
		if err := discard.Discard(ctx, row, directory); err != nil {
			return err
		}
	}
	if err := d.store.DiscardUnpreparedIncomingTransfer(id); err != nil {
		return err
	}
	defer d.wake(id)
	row.Phase = "canceled"
	return cleanupTransfer(ctx, d.store, d.root, row)
}

// Run makes one durable preparation/activation pass. It takes an app-lifetime
// context. Reopening a prepared operation uses the same installation recipe;
// it never re-baselines destination files after source retirement.
func (d *Destination) Run(ctx context.Context, id string) (result store.ThreadTransfer, runErr error) {
	unlock, err := d.locks.LockCtx(ctx, id)
	if err != nil {
		return store.ThreadTransfer{}, err
	}
	defer unlock()
	select {
	case d.slots <- struct{}{}:
		defer func() { <-d.slots }()
	case <-ctx.Done():
		return store.ThreadTransfer{}, ctx.Err()
	}
	row, data, err := d.read(id)
	if err != nil {
		return row, err
	}
	defer func() {
		if runErr == nil && (result.Phase == "complete" || result.Phase == "canceled") {
			runErr = cleanupTransfer(ctx, d.store, d.root, result)
		}
	}()
	if row.Phase == "complete" || row.Phase == "canceled" {
		return row, err
	}
	directory := filepath.Join(d.root, id)
	stage := filepath.Join(directory, "extracted")
	if row.Phase == "preparing" {
		checkpoint, err := transferfiles.ReadUpload(directory)
		if errors.Is(err, os.ErrNotExist) || (err == nil && checkpoint.Received != checkpoint.Size) {
			return row, ErrPending
		}
		if err != nil {
			return row, err
		}
		archive, progress, err := transferfiles.UploadedArchive(directory)
		if errors.Is(err, os.ErrNotExist) {
			return row, ErrPending
		}
		if err != nil {
			return row, err
		}
		if progress.SHA256 != data.archive.SHA256 || progress.Size != data.archive.Size {
			return row, transferwire.ErrConflict
		}
		// No prepared checkpoint means this is inert scratch from a failed
		// validation. Reconstruct it from the verified, immutable upload.
		if err := os.RemoveAll(stage); err != nil {
			return row, err
		}
		file, err := os.Open(archive)
		if err != nil {
			return row, err
		}
		files, extractErr := transferfiles.Extract(ctx, file, data.archive.SHA256, stage)
		if extractErr != nil && ctx.Err() == nil {
			// Header corruption can make tar parsing fail before the full digest
			// check. Reset only if disk bytes differ from the sealed upload;
			// faithfully received but invalid archives remain visible refusals.
			verifyErr := transferfiles.VerifyUploadContent(ctx, file, data.archive.SHA256, data.archive.Size)
			if errors.Is(verifyErr, transferfiles.ErrUploadCorrupt) {
				_, resetErr := transferfiles.ResetUpload(directory, data.archive.SHA256, data.archive.Size)
				if resetErr == nil {
					extractErr = ErrPending
				} else {
					extractErr = resetErr
				}
			} else if verifyErr != nil {
				extractErr = verifyErr
			}
		}
		closeErr := file.Close()
		if extractErr != nil {
			return row, extractErr
		}
		if closeErr != nil {
			return row, closeErr
		}
		plan, err := d.installer.Prepare(ctx, row, stage, files)
		if err != nil {
			return row, err
		}
		if !json.Valid(plan) {
			return row, errors.New("transfer: installer returned an invalid preparation recipe")
		}
		if err := writeDestinationJSON(filepath.Join(directory, "prepared.json"), plan); err != nil {
			return row, err
		}
		row, err = d.store.AdvanceThreadTransfer(id, "prepared", data.archive.SHA256)
		if err != nil {
			return row, err
		}
	}
	if row.Phase != "prepared" {
		return row, transferwire.ErrConflict
	}
	var secret []byte
	if err := readDestinationJSON(filepath.Join(directory, "activation.json"), 128, &secret); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return row, ErrPending
		}
		return row, err
	}
	if _, err := d.store.CheckThreadTransferActivation(id, data.archive.SHA256, secret); err != nil {
		return row, err
	}
	var plan json.RawMessage
	if err := readDestinationJSON(filepath.Join(directory, "prepared.json"), maxDestinationPlanBytes, &plan); err != nil {
		return row, err
	}
	if err := d.installer.Install(ctx, row, stage, plan, secret); err != nil {
		return row, err
	}
	row, err = d.store.GetThreadTransfer(id)
	if err == nil && row.Phase != "complete" {
		err = errors.New("transfer: installer returned without committing destination ownership")
	}
	return row, err
}
