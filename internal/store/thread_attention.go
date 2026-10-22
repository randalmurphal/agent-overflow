package store

import (
	"database/sql"
	"errors"
	"fmt"
	"unicode"
	"unicode/utf8"
)

// A thread asks for the user in two ways that read the same: a completion
// newer than its read marker, and the attention flag its agent raised
// (migration v160). A read clears both (threadReadTargetColumns). A turn
// nobody asked for and that found nothing worth the user's time asks for
// neither: KeepThreadReadPast keeps a thread that was read, read past it.

// MaxAttentionReasonRunes bounds the reason of an attention flag: one short
// line on the thread's sidebar row.
const MaxAttentionReasonRunes = 120

// CheckAttentionReason reports why reason cannot be the reason of an
// attention flag: it is empty, longer than MaxAttentionReasonRunes, or holds
// a character that is not plain one-line text (a control or format
// character, a line or paragraph separator), which could break the row it
// shows on or make it read as something other than what is stored.
func CheckAttentionReason(reason string) error {
	switch {
	case reason == "":
		return errors.New("the reason is empty")
	case !utf8.ValidString(reason):
		return errors.New("the reason is not valid UTF-8")
	case utf8.RuneCountInString(reason) > MaxAttentionReasonRunes:
		return fmt.Errorf("the reason is longer than %d characters", MaxAttentionReasonRunes)
	}
	for _, r := range reason {
		if unicode.In(r, unicode.Cc, unicode.Cf, unicode.Cs, unicode.Zl, unicode.Zp) {
			return fmt.Errorf("the reason holds the character %U, which is not plain one-line text", r)
		}
	}
	return nil
}

// SetThreadAttention raises the attention flag of threadID: kind, one of
// AttentionKinds, the reason the agent gave (CheckAttentionReason), and at,
// when it raised it. A flag the thread held is replaced. Raising a flag is
// activity: the thread's updated_at moves to at, as MarkThreadActivity
// moves it, in the same write.
func (s *Store) SetThreadAttention(threadID, kind, reason string, at int64) (Thread, bool, error) {
	if !ValidAttentionKind(kind) {
		return Thread{}, false, fmt.Errorf("store: flag thread %s: %q is not an attention kind", threadID, kind)
	}
	if err := CheckAttentionReason(reason); err != nil {
		return Thread{}, false, fmt.Errorf("store: flag thread %s: %w", threadID, err)
	}
	return s.applyThreadRowWrite(rowWrite{
		Action:     fmt.Sprintf("store: flag thread %s for attention", threadID),
		ID:         threadID,
		Set:        "attention_kind = ?, attention_reason = ?, attention_at = ?, updated_at = MAX(updated_at, ?)",
		SetArgs:    []any{kind, reason, at, at},
		Change:     "(attention_kind IS NOT ? OR attention_reason IS NOT ? OR attention_at IS NOT ? OR updated_at < ?)",
		ChangeArgs: []any{kind, reason, at, at},
	})
}

// ClearThreadAttention lowers the attention flag of threadID. A thread with
// no flag changes nothing.
func (s *Store) ClearThreadAttention(threadID string) (Thread, bool, error) {
	return s.applyThreadRowWrite(rowWrite{
		Action: fmt.Sprintf("store: clear the attention flag of thread %s", threadID),
		ID:     threadID,
		Set:    "attention_kind = '', attention_reason = '', attention_at = NULL",
		Change: "attention_at IS NOT NULL",
	})
}

// ThreadAttentionSaid reports whether threadID's pending attention flag is
// what the thread's rest notification says: a turn ended after the flag was
// raised, which said it (notify.MapTurnRest), and no turn error came after
// the flag, which would have replaced it. A flag no turn has ended with yet
// was never said, so lowering it takes back nothing. A thread that is gone
// answers false.
func (s *Store) ThreadAttentionSaid(threadID string) (bool, error) {
	var said bool
	err := s.reader().QueryRow(`SELECT attention_at IS NOT NULL
	    AND (last_read_at IS NULL OR attention_at > last_read_at)
	    AND COALESCE(newest_turn_error_at, 0) < attention_at
	    AND EXISTS (SELECT 1 FROM turns
	          WHERE turns.thread_id = threads.id AND turns.completed_at >= threads.attention_at)
	  FROM threads WHERE id = ?`, threadID).Scan(&said)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: read whether the attention flag of thread %s was said: %w", threadID, err)
	}
	return said, nil
}

// KeepThreadReadPast moves the read marker of threadID to at, the
// completion of its turn turnID, a turn nobody needs to read, when the
// marker already covered everything else the thread had to read: every
// other completed turn, its turn errors and its attention flag. A thread
// still owed a read keeps owing it, and one never tracked (NULL) or marked
// unread (0) is left alone, so the turn changes nothing the sidebar shows.
// A turn that failed, in an earlier round of it, is owed a read itself.
// It reports whether the marker moved and reads no row back: the caller
// announces the row through the announcer, which reads it.
func (s *Store) KeepThreadReadPast(threadID, turnID string, at int64) (bool, error) {
	action := fmt.Sprintf("store: keep thread %s read past turn %s", threadID, turnID)
	result, err := s.db.Exec(`UPDATE threads SET last_read_at = ?1
		WHERE id = ?2 AND last_read_at > 0 AND last_read_at < ?1
		  AND last_read_at >= COALESCE((SELECT MAX(completed_at) FROM turns
		       WHERE turns.thread_id = threads.id AND completed_at IS NOT NULL AND turns.turn_id IS NOT ?3), 0)
		  AND last_read_at >= COALESCE(newest_turn_error_at, 0)
		  AND last_read_at >= COALESCE(attention_at, 0)
		  AND NOT EXISTS (SELECT 1 FROM turns
		       WHERE turns.thread_id = threads.id AND turns.turn_id = ?3 AND turns.stop_reason = 'error')`, at, threadID, turnID)
	if err != nil {
		return false, fmt.Errorf("%s: %w", action, err)
	}
	return rowsChanged(result, action)
}
