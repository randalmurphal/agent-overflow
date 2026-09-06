package store

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"

	"agent-overflow/internal/keyedlock"
)

// TransferSession identifies provider-native execution independently of an AO
// row. Deleting/importing an AO alias must not revive a retired native session.
type TransferSession struct {
	Provider string `json:"provider"`
	Ref      string `json:"ref"`
}

// LockNativeSessions serializes native import against transfer reservation.
// Callers with an existing AO thread take its action lock first. While holding
// these locks, never acquire another thread's action lock: an alias is refused
// by BindThreadTransferSessions, not silently stopped or absorbed into a move.
func (s *Store) LockNativeSessions(ctx context.Context, refs []TransferSession) (func(), error) {
	if len(refs) > 16_384 {
		return nil, errors.New("transfer: too many native sessions")
	}
	keys := make([]string, 0, len(refs))
	for _, ref := range refs {
		if ref.Ref == "" {
			continue
		}
		provider := ref.Provider
		if provider == "claude-tui" {
			provider = "claude"
		}
		if len(ref.Ref) > 256 || strings.ContainsAny(ref.Ref, "\x00\r\n") || (provider != "claude" && provider != "codex") {
			return nil, errors.New("transfer: invalid native session reference")
		}
		keys = append(keys, provider+"\x00"+ref.Ref)
	}
	slices.Sort(keys)
	keys = slices.Compact(keys)
	s.nativeLocksOnce.Do(func() { s.nativeLocks = keyedlock.New() })
	unlocks := make([]func(), 0, len(keys))
	release := func() {
		for i := len(unlocks) - 1; i >= 0; i-- {
			unlocks[i]()
		}
	}
	for _, key := range keys {
		unlock, err := s.nativeLocks.LockCtx(ctx, key)
		if err != nil {
			release()
			return nil, err
		}
		unlocks = append(unlocks, unlock)
	}
	return release, nil
}

// BindThreadTransferSessions reserves a complete, immutable native closure
// before preparation can be acknowledged. The caller holds native/thread action
// locks while proving every source session idle or destination session absent.
// Outgoing copy refs name the NEW snapshot sessions, never the original ones.
func (s *Store) BindThreadTransferSessions(id string, refs []TransferSession) error {
	if len(refs) == 0 || len(refs) > 16_384 {
		return errors.New("transfer: invalid native session count")
	}
	ordered := append([]TransferSession(nil), refs...)
	slices.SortFunc(ordered, func(a, b TransferSession) int {
		if n := strings.Compare(a.Provider, b.Provider); n != 0 {
			return n
		}
		return strings.Compare(a.Ref, b.Ref)
	})
	for i, ref := range ordered {
		if (ref.Provider != "claude" && ref.Provider != "codex") || ref.Ref == "" || len(ref.Ref) > 256 ||
			strings.ContainsAny(ref.Ref, "\x00\r\n") || (i > 0 && ref == ordered[i-1]) {
			return errors.New("transfer: invalid native session reference")
		}
	}
	tx, release, err := s.beginDurableTx(context.Background())
	if err != nil {
		return err
	}
	defer release()
	defer tx.Rollback()
	row, err := scanThreadTransfer(tx.QueryRow(`SELECT `+transferColumns+` FROM thread_transfers WHERE id = ?`, id))
	if err != nil {
		return err
	}
	previous, err := transferSessionsTx(tx, id)
	if err != nil {
		return err
	}
	if len(previous) > 0 {
		if !slices.Equal(previous, ordered) {
			return errors.New("transfer: native session closure is already bound")
		}
		return nil
	}
	if row.Phase != "preparing" {
		return errors.New("transfer: native sessions must be reserved before preparation")
	}
	for _, ref := range ordered {
		// Another AO alias may already own a live provider process. It has a
		// different action lock and cannot be frozen by this conversation's
		// move. Refuse before preparation; an independent native copy is safe.
		allowedID := row.ThreadID
		if row.Direction == "outgoing" && row.Kind == "copy" {
			allowedID = row.TargetThreadID
		}
		var aliases bool
		if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM threads
WHERE CASE provider WHEN 'claude-tui' THEN 'claude' ELSE provider END = ?
AND session_ref = ? AND COALESCE(pending_fork_session_ref,'') = '' AND id <> ?)`, ref.Provider, ref.Ref, allowedID).Scan(&aliases); err != nil {
			return err
		}
		if aliases {
			return errors.New("This native session is shared by another conversation on this computer. Create an independent copy before moving it.")
		}
		owner, found, err := nativeTransferOwner(tx, ref)
		if err != nil {
			return err
		}
		if found {
			// An owned incoming completion can leave again. A retired native
			// identity can return only to the SAME destination AO identity.
			leaving := row.Direction == "outgoing" && owner.Direction == "incoming" && owner.Phase == "complete" && owner.TargetThreadID == row.ThreadID
			returning := row.Direction == "incoming" && owner.Direction == "outgoing" &&
				(owner.Phase == "committed" || owner.Phase == "complete") && owner.TargetThreadID == row.TargetThreadID
			if !leaving && !returning {
				return nativeTransferError(owner)
			}
		}
		if _, err := tx.Exec(`INSERT INTO thread_transfer_sessions (transfer_id,provider,session_ref) VALUES (?,?,?)`, id, ref.Provider, ref.Ref); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func transferSessionsTx(tx *sql.Tx, id string) ([]TransferSession, error) {
	rows, err := tx.Query(`SELECT provider,session_ref FROM thread_transfer_sessions WHERE transfer_id = ? ORDER BY provider,session_ref`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var refs []TransferSession
	for rows.Next() {
		var ref TransferSession
		if err := rows.Scan(&ref.Provider, &ref.Ref); err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	return refs, rows.Err()
}

func nativeTransferOwner(q transferQuerier, ref TransferSession) (ThreadTransfer, bool, error) {
	// Execution checks need no private recovery blobs. The session index bounds
	// aliases; journal insertion order distinguishes a return from its old move.
	var row ThreadTransfer
	err := q.QueryRow(`SELECT id,thread_id,target_thread_id,peer_backend_id,direction,phase FROM thread_transfers WHERE id = (
SELECT t.id FROM thread_transfer_sessions r JOIN thread_transfers t ON t.id = r.transfer_id
WHERE r.provider = ? AND r.session_ref = ? AND t.phase <> 'canceled' ORDER BY t.rowid DESC LIMIT 1)`, ref.Provider, ref.Ref).Scan(
		&row.ID, &row.ThreadID, &row.TargetThreadID, &row.PeerBackendID, &row.Direction, &row.Phase)
	if errors.Is(err, sql.ErrNoRows) {
		return row, false, nil
	}
	return row, err == nil, err
}

func nativeTransferError(row ThreadTransfer) error {
	return &ThreadTransferError{OperationID: row.ID, BackendID: row.PeerBackendID,
		Moved: row.Direction == "outgoing" && (row.Phase == "committed" || row.Phase == "complete")}
}

func (s *Store) CheckNativeThreadTransferAccess(provider, ref string) error {
	if ref == "" {
		return nil
	}
	row, found, err := nativeTransferOwner(s.reader(), TransferSession{Provider: provider, Ref: ref})
	if err != nil || !found {
		return err
	}
	if row.Direction == "incoming" && row.Phase == "complete" {
		return nil
	}
	return nativeTransferError(row)
}

// CheckNativeSessionImport also refuses completed incoming reservations: their
// installed history already carries the canonical AO identity. An old scan or
// a deleted display row must not create an independently executable alias.
func (s *Store) CheckNativeSessionImport(provider, ref string) error {
	row, found, err := nativeTransferOwner(s.reader(), TransferSession{Provider: provider, Ref: ref})
	if err != nil || !found {
		return err
	}
	return nativeTransferError(row)
}

// CheckTransferSourceAliases runs under the source's native locks before any
// bytes are copied. Another AO alias has a different thread lock and may write
// this same native transcript even while the selected conversation is idle.
func (s *Store) CheckTransferSourceAliases(threadID string, refs []TransferSession) error {
	for _, ref := range refs {
		var exists bool
		if err := s.reader().QueryRow(`SELECT EXISTS(SELECT 1 FROM owned_threads WHERE
CASE provider WHEN 'claude-tui' THEN 'claude' ELSE provider END = ? AND session_ref = ?
AND COALESCE(pending_fork_session_ref,'') = '' AND id <> ?)`, ref.Provider, ref.Ref, threadID).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return errors.New("Another conversation on this computer uses the same native session. Close or separate that conversation before transferring it.")
		}
	}
	return nil
}

// ReturningTransferSessions names ONLY native identities previously retired by
// this computer for the same AO conversation. Their recorded files may accept
// an incoming newer snapshot after baseline validation. Unrelated native files
// remain conflicts, including files the user has never imported into AO.
func (s *Store) ReturningTransferSessions(id string) ([]TransferSession, error) {
	rows, err := s.reader().Query(`SELECT current.provider,current.session_ref FROM thread_transfer_sessions current
JOIN thread_transfers incoming ON incoming.id=current.transfer_id
JOIN thread_transfers previous ON previous.rowid=(
 SELECT MAX(t.rowid) FROM thread_transfer_sessions r JOIN thread_transfers t ON t.id=r.transfer_id
 WHERE r.provider=current.provider AND r.session_ref=current.session_ref AND t.rowid<incoming.rowid AND t.phase<>'canceled'
) WHERE incoming.id=? AND incoming.direction='incoming' AND incoming.kind='move'
AND previous.direction='outgoing' AND previous.phase IN ('committed','complete') AND previous.target_thread_id=incoming.target_thread_id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var refs []TransferSession
	for rows.Next() {
		var ref TransferSession
		if err := rows.Scan(&ref.Provider, &ref.Ref); err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	return refs, rows.Err()
}

// CheckThreadExecutionAccess checks both the AO identity and its current native
// execution identity. A pending fork references history to copy, not a session
// to resume, and must not inherit its parent's execution tombstone.
func (s *Store) CheckThreadExecutionAccess(thread Thread) error {
	if err := s.CheckThreadTransferAccess(thread.ID); err != nil {
		return err
	}
	if thread.PendingForkRef != "" {
		return nil
	}
	provider := thread.Provider
	if provider == "claude-tui" {
		provider = "claude"
	}
	return s.CheckNativeThreadTransferAccess(provider, thread.SessionRef)
}
