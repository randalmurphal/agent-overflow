package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"agent-overflow/internal/entityid"
	"github.com/google/uuid"
)

// ThreadTransfer is the durable coordination record, not a copy of provider
// runtime state. PrivateState is never serialized onto an ordinary status RPC:
// it may hold a one-operation destination grant or the source activation secret.
type ThreadTransfer struct {
	ID               string          `json:"id"`
	ThreadID         string          `json:"threadId"`
	TargetThreadID   string          `json:"targetThreadId"`
	PeerBackendID    string          `json:"peerBackendId"`
	ProjectID        string          `json:"-"` // Destination reservation; never inferred from private app JSON.
	Kind             string          `json:"kind"`
	Direction        string          `json:"direction"`
	Phase            string          `json:"phase"`
	ManifestHash     string          `json:"manifestHash,omitempty"`
	ArchiveSize      int64           `json:"archiveSize,omitempty"`
	ActivationHash   string          `json:"-"`
	PrivateState     json.RawMessage `json:"-"`
	PeerState        json.RawMessage `json:"-"`
	CancelRequested  bool            `json:"cancelRequested,omitempty"`
	NeedsDestination bool            `json:"needsDestination,omitempty"`
	OwnershipEpoch   int64           `json:"ownershipEpoch"`
	Error            string          `json:"error,omitempty"`
	CreatedAt        int64           `json:"createdAt"`
	UpdatedAt        int64           `json:"updatedAt"`
}

const transferColumns = `id, thread_id, target_thread_id, peer_backend_id, kind, direction, phase,
manifest_hash, archive_size, activation_hash, private_state, peer_state, cancel_requested, ownership_epoch, error, created_at, updated_at, project_id`

func scanThreadTransfer(scanner interface{ Scan(...any) error }) (ThreadTransfer, error) {
	var row ThreadTransfer
	var peer []byte
	err := scanner.Scan(&row.ID, &row.ThreadID, &row.TargetThreadID, &row.PeerBackendID, &row.Kind, &row.Direction, &row.Phase,
		&row.ManifestHash, &row.ArchiveSize, &row.ActivationHash, &row.PrivateState, &peer, &row.CancelRequested, &row.OwnershipEpoch, &row.Error, &row.CreatedAt, &row.UpdatedAt, &row.ProjectID)
	row.PeerState = peer
	row.NeedsDestination = row.Direction == "outgoing" && row.Phase == "preparing" && len(peer) == 0
	return row, err
}

func validTransferDigest(value string) bool {
	b, err := hex.DecodeString(value)
	return err == nil && len(b) == 32 && hex.EncodeToString(b) == value
}

// CreateThreadTransfer is idempotent only for the SAME immutable request. A
// reused operation ID must not authorize another host, conversation or secret.
// The caller holds the thread action lock while proving it is quiescent.
func (s *Store) CreateThreadTransfer(request ThreadTransfer) (ThreadTransfer, error) {
	if !entityid.Valid(request.ID) || !entityid.Valid(request.PeerBackendID) || request.ThreadID == "" || len(request.ThreadID) > 128 ||
		len(request.ProjectID) > 128 || (request.Direction != "incoming" && request.ProjectID != "") ||
		(request.Kind != "move" && request.Kind != "copy") || (request.Direction != "incoming" && request.Direction != "outgoing") ||
		!validTransferDigest(request.ActivationHash) || !json.Valid(request.PrivateState) || len(request.PrivateState) > 1<<20 {
		return ThreadTransfer{}, errors.New("transfer: invalid request")
	}
	if request.TargetThreadID == "" {
		request.TargetThreadID = request.ThreadID
		if request.Direction == "outgoing" && request.Kind == "copy" {
			request.TargetThreadID = uuid.NewSHA1(uuid.NameSpaceURL, []byte("agent-overflow/transfer/thread/"+request.ID)).String()
		}
	}
	if len(request.TargetThreadID) > 128 ||
		((request.Direction == "incoming" || request.Kind == "move") && request.TargetThreadID != request.ThreadID) ||
		(request.Direction == "outgoing" && request.Kind == "copy" && request.TargetThreadID == request.ThreadID) {
		return ThreadTransfer{}, errors.New("transfer: invalid destination conversation identity")
	}
	tx, release, err := s.beginDurableTx(context.Background())
	if err != nil {
		return ThreadTransfer{}, err
	}
	defer release()
	defer tx.Rollback()
	previous, err := scanThreadTransfer(tx.QueryRow(`SELECT `+transferColumns+` FROM thread_transfers WHERE id = ?`, request.ID))
	if err == nil {
		if previous.ThreadID != request.ThreadID || previous.TargetThreadID != request.TargetThreadID || previous.PeerBackendID != request.PeerBackendID || previous.ProjectID != request.ProjectID || previous.Kind != request.Kind || previous.Direction != request.Direction || previous.ActivationHash != request.ActivationHash || !bytes.Equal(previous.PrivateState, request.PrivateState) {
			return ThreadTransfer{}, errors.New("transfer: operation ID already belongs to a different request")
		}
		if (request.Direction == "incoming" || request.OwnershipEpoch != 0) && request.OwnershipEpoch != previous.OwnershipEpoch {
			return ThreadTransfer{}, errors.New("transfer: operation ID already names another ownership epoch")
		}
		return previous, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return ThreadTransfer{}, err
	}
	if request.ProjectID != "" {
		var exists bool
		if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM projects WHERE id = ?)`, request.ProjectID).Scan(&exists); err != nil {
			return ThreadTransfer{}, err
		}
		if !exists {
			return ThreadTransfer{}, errors.New("The destination project is no longer available. Add it again before transferring.")
		}
	}
	accessErr := checkThreadTransferAccess(tx, request.ThreadID)
	if accessErr != nil {
		// An explicitly authorized incoming move may return an old thread,
		// including A → B → C → A. The source must commit its own retirement
		// before destination activation. A copy has a new thread identity;
		// it cannot supersede an ownership tombstone.
		var moved *ThreadTransferError
		if request.Direction != "incoming" || request.Kind != "move" || !errors.As(accessErr, &moved) || !moved.Moved {
			return ThreadTransfer{}, accessErr
		}
	}
	if request.Direction == "incoming" && accessErr == nil {
		var exists bool
		if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM threads WHERE id = ?)`, request.ThreadID).Scan(&exists); err != nil {
			return ThreadTransfer{}, err
		}
		if exists {
			return ThreadTransfer{}, errors.New("transfer: destination conversation already exists")
		}
	}
	if err := assignTransferEpoch(tx, &request); err != nil {
		return ThreadTransfer{}, err
	}
	request.Phase, request.ManifestHash, request.Error = "preparing", "", ""
	request.ArchiveSize = 0
	request.PeerState = nil
	request.CancelRequested = false
	request.NeedsDestination = request.Direction == "outgoing"
	request.CreatedAt = time.Now().UnixMilli()
	request.UpdatedAt = request.CreatedAt
	_, err = tx.Exec(`INSERT INTO thread_transfers (`+transferColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		request.ID, request.ThreadID, request.TargetThreadID, request.PeerBackendID, request.Kind, request.Direction, request.Phase,
		request.ManifestHash, request.ArchiveSize, request.ActivationHash, []byte(request.PrivateState), nil, false, request.OwnershipEpoch, "", request.CreatedAt, request.UpdatedAt, request.ProjectID)
	if err != nil {
		return ThreadTransfer{}, fmt.Errorf("transfer: begin: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return ThreadTransfer{}, err
	}
	return request, nil
}

func (s *Store) GetThreadTransfer(id string) (ThreadTransfer, error) {
	return scanThreadTransfer(s.reader().QueryRow(`SELECT `+transferColumns+` FROM thread_transfers WHERE id = ?`, id))
}

// ListRecentThreadTransfers contains no private state and bounds both row count
// and query payload. Pending operations sort first so failures remain visible.
func (s *Store) ListRecentThreadTransfers() ([]ThreadTransfer, error) {
	rows, err := s.reader().Query(`SELECT id,thread_id,target_thread_id,peer_backend_id,kind,direction,phase,manifest_hash,archive_size,'',x'',CASE WHEN peer_state IS NULL THEN NULL ELSE x'7b7d' END,cancel_requested,ownership_epoch,error,created_at,updated_at,''
FROM thread_transfers ORDER BY phase NOT IN ('complete','canceled') DESC,updated_at DESC,id LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]ThreadTransfer, 0)
	for rows.Next() {
		row, err := scanThreadTransfer(rows)
		if err != nil {
			return nil, err
		}
		row.PeerState = nil // The query carries only presence, never the offer.
		result = append(result, row)
	}
	return result, rows.Err()
}

// BindThreadTransferPeer records the destination's one-operation offer after
// the frontend has authorized both ends. The source's immutable request and
// activation secret are already durable. Binding is once-only and idempotent,
// so a lost Begin reply cannot redirect an accepted move to another recipient.
func (s *Store) BindThreadTransferPeer(id string, peer json.RawMessage) (ThreadTransfer, error) {
	if !json.Valid(peer) || len(peer) > 1<<20 {
		return ThreadTransfer{}, errors.New("transfer: invalid peer offer")
	}
	tx, release, err := s.beginDurableTx(context.Background())
	if err != nil {
		return ThreadTransfer{}, err
	}
	defer release()
	defer tx.Rollback()
	row, err := scanThreadTransfer(tx.QueryRow(`SELECT `+transferColumns+` FROM thread_transfers WHERE id = ?`, id))
	if err != nil {
		return ThreadTransfer{}, err
	}
	if len(row.PeerState) != 0 {
		if !bytes.Equal(row.PeerState, peer) {
			return ThreadTransfer{}, errors.New("transfer: destination offer is already bound")
		}
		return row, nil
	}
	if row.Direction != "outgoing" || row.Phase != "preparing" {
		return ThreadTransfer{}, errors.New("transfer: this operation cannot accept a destination offer")
	}
	row.PeerState = append(json.RawMessage(nil), peer...)
	row.NeedsDestination = false
	row.UpdatedAt = time.Now().UnixMilli()
	if _, err := tx.Exec(`UPDATE thread_transfers SET peer_state = ?,updated_at = ? WHERE id = ?`, []byte(peer), row.UpdatedAt, id); err != nil {
		return ThreadTransfer{}, err
	}
	if err := tx.Commit(); err != nil {
		return ThreadTransfer{}, err
	}
	return row, nil
}

// AdvanceThreadTransfer commits a monotonic phase transition. Retries may
// restate the current phase and digest; they cannot erase a commit or change
// the prepared content. Only pre-commit work can be canceled unilaterally.
func (s *Store) AdvanceThreadTransfer(id, phase, manifestHash string) (ThreadTransfer, error) {
	tx, release, err := s.beginDurableTx(context.Background())
	if err != nil {
		return ThreadTransfer{}, err
	}
	defer release()
	defer tx.Rollback()
	row, err := scanThreadTransfer(tx.QueryRow(`SELECT `+transferColumns+` FROM thread_transfers WHERE id = ?`, id))
	if err != nil {
		return ThreadTransfer{}, err
	}
	if row.ManifestHash != "" && manifestHash != row.ManifestHash {
		return ThreadTransfer{}, errors.New("transfer: prepared content cannot change")
	}
	if row.Direction == "incoming" && (phase == "committed" || phase == "complete" || phase == "canceled") {
		return ThreadTransfer{}, errors.New("transfer: destination completion requires activation and installed history")
	}
	if phase == "committed" && row.CancelRequested {
		return ThreadTransfer{}, errors.New("transfer: cancellation was requested before retirement")
	}
	if phase != "canceled" && !validTransferDigest(manifestHash) {
		return ThreadTransfer{}, errors.New("transfer: missing content digest")
	}
	if row.Phase == phase {
		return row, nil
	}
	allowed := (row.Phase == "preparing" && phase == "prepared") || (row.Phase == "prepared" && phase == "committed") ||
		(row.Phase == "committed" && phase == "complete") || ((row.Phase == "preparing" || row.Phase == "prepared") && phase == "canceled")
	if !allowed {
		return ThreadTransfer{}, fmt.Errorf("transfer: cannot change %s to %s", row.Phase, phase)
	}
	row.Phase, row.ManifestHash, row.Error, row.UpdatedAt = phase, manifestHash, "", time.Now().UnixMilli()
	row.NeedsDestination = phase == "preparing" && row.Direction == "outgoing" && len(row.PeerState) == 0
	_, err = tx.Exec(`UPDATE thread_transfers SET phase = ?, manifest_hash = ?, error = '', updated_at = ? WHERE id = ?`, phase, manifestHash, row.UpdatedAt, id)
	if err != nil {
		return ThreadTransfer{}, err
	}
	if err := tx.Commit(); err != nil {
		return ThreadTransfer{}, err
	}
	return row, nil
}

// SetThreadTransferError preserves the recovery phase. A failed network request
// is neither permission to repeat activation nor permission to restore source
// execution. The coordinator publishes a sanitized user-facing error here.
func (s *Store) SetThreadTransferError(id, message string) error {
	if len(message) > 4096 {
		return errors.New("transfer: error message too long")
	}
	_, err := s.db.Exec(`UPDATE thread_transfers SET error = ?, updated_at = ? WHERE id = ? AND phase NOT IN ('complete', 'canceled')`, message, time.Now().UnixMilli(), id)
	return err
}

// ListPendingThreadTransfers is restart recovery, not an in-memory read model.
func (s *Store) ListPendingThreadTransfers() ([]ThreadTransfer, error) {
	rows, err := s.reader().Query(`SELECT ` + transferColumns + ` FROM thread_transfers WHERE phase NOT IN ('complete', 'canceled') ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []ThreadTransfer
	for rows.Next() {
		row, err := scanThreadTransfer(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

// ThreadTransferError carries enough identity for a stale client to find the
// operation/new owner. No credential or source-local path belongs in this error.
type ThreadTransferError struct {
	OperationID, BackendID string
	Moved                  bool
}

// ThreadTransferRef is the transport-neutral error boundary. The dispatcher
// supplies fixed user-facing prose instead of exposing wrapped internal errors.
func (e *ThreadTransferError) ThreadTransferRef() (operationID, backendID string, moved bool) {
	return e.OperationID, e.BackendID, e.Moved
}

func (e *ThreadTransferError) Error() string {
	if e.Moved {
		return fmt.Sprintf("This conversation moved to another computer (%s). Transfer: %s", e.BackendID, e.OperationID)
	}
	return fmt.Sprintf("This conversation has a pending transfer (%s). Resume or cancel the transfer before continuing.", e.OperationID)
}

// CheckThreadTransferAccess gates execution under the caller's action lock.
// Move tombstones survive both completion and source history deletion.
func (s *Store) CheckThreadTransferAccess(threadID string) error {
	return checkThreadTransferAccess(s.reader(), threadID)
}

type transferQuerier interface{ QueryRow(string, ...any) *sql.Row }

func checkThreadTransferAccess(q transferQuerier, threadID string) error {
	var id, backend, direction, kind, phase string
	var archiveSize int64
	err := q.QueryRow(`SELECT id, peer_backend_id, direction, kind, phase, archive_size FROM thread_transfers
WHERE thread_id = ? AND phase <> 'canceled'
ORDER BY rowid DESC LIMIT 1`, threadID).Scan(&id, &backend, &direction, &kind, &phase, &archiveSize)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if phase == "complete" && (direction != "outgoing" || kind != "move") {
		return nil
	}
	// A sealed copy has private native identities and immutable archive bytes.
	// Its upload/recovery no longer needs to hold the original conversation.
	if direction == "outgoing" && kind == "copy" && archiveSize > 0 {
		return nil
	}
	return &ThreadTransferError{OperationID: id, BackendID: backend, Moved: direction == "outgoing" && kind == "move" && (phase == "committed" || phase == "complete")}
}
