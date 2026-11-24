package store

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// The issue record is the one record of an issue: its row in
// tracker_cache_issues holds the card every read of the issue shares, the
// time its status began, and the bookkeeping of the ticket body it holds
// in tracker_cache_issue_bodies: the rest of what an open ticket shows
// (its description, fields, comments, links, attachments, history and
// moves). A ticket's read brings the card, the body and the evidence of
// its status time, and stores all three in one transaction
// (StoreMirrorTickets); a card read by ID stores the card alone through
// the same card path (UpsertMirrorCards).

// mirrorBodyCap is how many bodies a root keeps: those of the issues kept
// for an open ticket, then the most recently opened.
const mirrorBodyCap = 200

// mirrorBodyDigest is the digest body_digest holds of a body: the first
// 16 bytes of its SHA-256. A read that finds the same body compares the
// digest and writes no body bytes.
func mirrorBodyDigest(data []byte) []byte {
	sum := sha256.Sum256(data)
	return sum[:16]
}

// MirrorBody is the body the record of an issue holds: the update time
// of the read that stored it (UpdatedAt, the version it shows, which can
// be older than the record's card), when that read began (ReadAt, an
// order time, and Wall, the wall time a person is shown and its age is
// measured from), when the issue was last opened (OpenedAt, an order
// time) and the body, with the record's card as ListMirrorBodies read it.
type MirrorBody struct {
	IssueID      string
	UpdatedAt    int64
	ReadAt, Wall int64
	OpenedAt     int64
	Data         []byte
	Card         MirrorCard
}

// MirrorTicket is one ticket as a read found it: its card, its body (nil
// for a read that brought none, a check by card), the evidence of its
// status time (nil for none), and the bind of the open ticket whose read
// it is (KeepMirrorTicket, of the card's issue), zero for a read no open
// ticket made.
type MirrorTicket struct {
	Card   MirrorCard
	Body   []byte
	Status *MirrorStatusEvidence
	Bind   MirrorBind
}

// MirrorBodyOutcome is what a ticket write did with the body it brought.
type MirrorBodyOutcome int

const (
	// MirrorBodyNone is a write that brought no body.
	MirrorBodyNone MirrorBodyOutcome = iota
	// MirrorBodyStored is a body the record holds: this read's, new or the
	// same as the one it held.
	MirrorBodyStored
	// MirrorBodySuperseded is a body the record holds one of a later
	// version, or of the same version from a read that began later, in
	// place of: the caller shows the record's.
	MirrorBodySuperseded
	// MirrorBodyNotKept is a body the record keeps none of: the mirror does
	// not keep the issue, the read is of an open ticket's bind that is not
	// the issue's current one (ActivateMirrorBind), a read no open ticket made is not a person's
	// opening and the record holds no body, or the cap dropped it in the
	// same write.
	MirrorBodyNotKept
)

// MirrorTicketWrite is what a ticket write did for one ticket, as its
// transaction committed. Kept reports that the mirror keeps the issue,
// and so stored or ordered its card. CardChanged reports a new or changed
// card, StatusChanged a record's status time the write moved (with its
// card or without), BodyChanged a body stored new or changed, or the held
// one moved to another version (the same bytes read of a later update).
// UpdatedAt is the record's card's update time after the write, Held the
// body it holds (zero UpdatedAt: none), and StatusFinal whether a final
// read decided the record's status time for its update.
type MirrorTicketWrite struct {
	Kept                                    bool
	CardChanged, StatusChanged, BodyChanged bool
	Body                                    MirrorBodyOutcome
	UpdatedAt                               int64
	Held                                    MirrorBodyState
	StatusFinal                             bool
}

// MirrorBodyState is a body's bookkeeping: the update time of the read
// that stored it, when that read began (an order time) and its wall time.
type MirrorBodyState struct {
	UpdatedAt, ReadAt, Wall int64
}

// MirrorTicketsWrite is what StoreMirrorTickets did, as its transaction
// committed: each ticket's outcome, in the order of its tickets, what the
// card path changed (Cards, Superseded, Unkept and Catalog of
// MirrorChanges), and the issues whose bodies it dropped to keep the
// mirrorBodyCap, sorted: only bodies held before the write, as a body
// the same write started and dropped was never held (its outcome says
// so).
type MirrorTicketsWrite struct {
	Tickets []MirrorTicketWrite
	Changes MirrorChanges
	Dropped []string
}

// mirrorCardState is a card's issue as a write of the card reads it before
// it writes: whether the mirror keeps it, holds its row (present),
// refuses the card (mirrorCardRefusedSQL) or admits it from an earlier
// read (mirrorCardAdvancesSQL), and whether the card names other sprints
// (mirrorCardSprintsMovedSQL); the record's update time and status time
// with its finality; and its body's bookkeeping.
type mirrorCardState struct {
	kept, present, refused, advances bool
	sprintsMoved                     bool
	updatedAt, since, finalFor       int64
	body                             MirrorBodyState
	bodyOpened                       int64
	digest                           []byte
}

// mirrorCardStatesSQL reads, for each card of the JSON list ?2 read from
// ?3 by its place in the list, its issue's state (mirrorCardState), the
// list driving the join.
var mirrorCardStatesSQL = `SELECT c.key, ` + mirrorIssueKept("?1", "c.value ->> 'i'") + `, i.rowid IS NOT NULL,
	    coalesce(` + mirrorCardRefusedSQL("?3") + `, 0), coalesce(` + mirrorCardAdvancesSQL("?3") + `, 0), ` + mirrorCardSprintsMovedSQL + `,
	    coalesce(i.updated_at, 0), coalesce(i.status_since, 0), coalesce(i.status_final_for, 0),
	    coalesce(i.body_updated_at, 0), coalesce(i.body_read_at, 0), coalesce(i.body_wall, 0), coalesce(i.body_opened_at, 0),
	    coalesce(i.body_digest, x'')
	FROM json_each(?2) c LEFT JOIN tracker_cache_issues i ON i.root_id = ?1 AND i.issue_id = c.value ->> 'i'`

// readMirrorCardStatesTx reads the state of each of n encoded cards read
// from readAt in one statement, in the cards' order: the one state read
// of every write of cards with no board (UpsertMirrorCards,
// StoreMirrorTickets).
func readMirrorCardStatesTx(tx *sql.Tx, rootID int64, encoded string, n int, readAt int64) ([]mirrorCardState, error) {
	states := make([]mirrorCardState, n)
	err := scanMirrorRows(tx, mirrorCardStatesSQL, []any{rootID, encoded, readAt}, func(r rowScanner) error {
		var at int
		var st mirrorCardState
		if err := r.Scan(&at, &st.kept, &st.present, &st.refused, &st.advances, &st.sprintsMoved, &st.updatedAt, &st.since, &st.finalFor,
			&st.body.UpdatedAt, &st.body.ReadAt, &st.body.Wall, &st.bodyOpened, &st.digest); err != nil {
			return err
		}
		if at < 0 || at >= n {
			return fmt.Errorf("a state of card %d of %d", at, n)
		}
		states[at] = st
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read cards: %w", err)
	}
	return states, nil
}

// mirrorTicketMove is a ticket's row of mirrorTicketMoveSQL: its issue,
// whether its card is admitted from an earlier read and not replaced
// (Advance, which moves read_at), the body bookkeeping it writes when
// Body is set (the version U, the digest D), the opening it records on
// the body it holds or starts (O, an order time, zero for none), and its
// status result (X, a mirrorStatusResult, zero for none, giving S, for
// version V of a card in status T).
type mirrorTicketMove struct {
	ID      string `json:"i"`
	Advance bool   `json:"a,omitempty"`
	Body    bool   `json:"b,omitempty"`
	U       int64  `json:"u"`
	D       string `json:"d,omitempty"`
	O       int64  `json:"o"`
	X       int    `json:"x"`
	S       int64  `json:"s"`
	V       int64  `json:"v"`
	T       string `json:"t,omitempty"`
}

// mirrorTicketMoveSQL writes, for each kept ticket of the JSON list ?2 of
// mirrorTicketMove read from ?3 at wall time ?4, the card's read, the
// body's bookkeeping and the status result, as the issue row holds them
// when it runs (after the card upsert), in one statement that joins the
// list, parsed once, to the issue rows, each found by key from its entry,
// and returns each row it changed as it left it: a row the move leaves as
// it was is not written (an identical read again). The list names an
// issue once (StoreMirrorTickets refuses a ticket read twice), so each
// row joins one entry. The list is materialized, so the join is driven
// from it and finds each row by its primary key; joined as json_each, the
// planner searches every row of the root and scans the list for each
// (TestTheTicketMoveGrowsLinearly).
// A stored body's bookkeeping is the admitted body's own (bodyOutcome
// admits only a body of a later (update time, read) pair than the held
// one): its version, its read, its wall time and its digest.
// mirrorTicketMoveSQL.full assigns too the body version and opening,
// which idx_tracker_cache_issues_body_opened holds; SQLite rewrites an
// index whose column a statement assigns, changed or not, so a write in
// which neither changes for any ticket (an idle round) runs
// mirrorTicketMoveSQL.reads, which leaves them and the index alone.
var mirrorTicketMoveSQL = struct{ full, reads string }{mirrorTicketMoveOf(true), mirrorTicketMoveOf(false)}

func mirrorTicketMoveOf(full bool) string {
	since, finalFor, statusRead := mirrorStatusWriteSQL("(h.value ->> 'x')", "(h.value ->> 's')", "(h.value ->> 'v')", "(h.value ->> 't')", "?3")
	i := "tracker_cache_issues."
	columns, versions := `(read_at, body_read_at, body_wall, body_digest, status_since, status_final_for, status_read_at`, ``
	if full {
		columns += `, body_updated_at, body_opened_at`
		versions = `,
	         iif(h.value ->> 'b', h.value ->> 'u', ` + i + `body_updated_at),
	         iif(h.value ->> 'b' OR ` + i + `body_read_at > 0, max(` + i + `body_opened_at, h.value ->> 'o'), ` + i + `body_opened_at)`
	}
	columns += `)`
	after := `(iif(h.value ->> 'a', max(` + i + `read_at, ?3), ` + i + `read_at),
	         iif(h.value ->> 'b', ?3, ` + i + `body_read_at),
	         iif(h.value ->> 'b', ?4, ` + i + `body_wall),
	         iif(h.value ->> 'b', unhex(h.value ->> 'd'), ` + i + `body_digest),
	         ` + since + `, ` + finalFor + `, ` + statusRead + versions + `)`
	return `WITH h AS MATERIALIZED (SELECT value FROM json_each(?2))
	UPDATE tracker_cache_issues SET ` + columns + ` = ` + after + `
	FROM h
	WHERE root_id = ?1 AND issue_id = h.value ->> 'i' AND ` + columns + ` IS NOT ` + after + `
	RETURNING ` + mirrorRecordAfterColumns
}

// mirrorBodyWriteSQL stores body ?3 of issue ?2, whose bookkeeping the
// same transaction writes (mirrorTicketMoveSQL).
const mirrorBodyWriteSQL = `INSERT INTO tracker_cache_issue_bodies(root_id, issue_id, data) VALUES (?1, ?2, ?3)
	ON CONFLICT(root_id, issue_id) DO UPDATE SET data = excluded.data`

// mirrorBodyEvictSQL clears the bookkeeping of the bodies of root ?1
// beyond the mirrorBodyCap kept first: those of an issue an open ticket
// keeps, then the most recently opened, walking the bodies' index, whose
// entries are the bodies held (at most the cap and one write's tickets),
// and sorting them; and
// mirrorBodyDropSQL deletes the bodies of the issues of the JSON list ?2
// it returned.
var (
	mirrorBodyEvictSQL = `UPDATE tracker_cache_issues SET body_updated_at = 0, body_read_at = 0, body_wall = 0, body_opened_at = 0,
	    body_digest = x''
	WHERE root_id = ?1 AND issue_id IN (
	  SELECT b.issue_id FROM tracker_cache_issues b INDEXED BY idx_tracker_cache_issues_body_opened
	   WHERE b.root_id = ?1 AND b.body_updated_at > 0
	   ORDER BY EXISTS (SELECT 1 FROM tracker_cache_open_tickets o WHERE o.root_id = ?1 AND o.issue_id = b.issue_id) DESC,
	            b.body_opened_at DESC, b.issue_id DESC LIMIT -1 OFFSET ` + strconv.Itoa(mirrorBodyCap) + `)
	RETURNING issue_id`
	mirrorBodyDropSQL = `DELETE FROM tracker_cache_issue_bodies WHERE root_id = ?1 AND issue_id IN (SELECT value FROM json_each(?2))`
)

// evictMirrorBodiesTx drops the bodies of rootID beyond the mirrorBodyCap
// (mirrorBodyEvictSQL): the one writer that removes a body from a live
// root, its bookkeeping and its bytes in one transaction. It returns the
// issues dropped.
func evictMirrorBodiesTx(tx *sql.Tx, rootID int64) ([]string, error) {
	dropped, err := queryIDs(tx, mirrorBodyEvictSQL, rootID)
	if err != nil || len(dropped) == 0 {
		return nil, err
	}
	list, err := encodeMirrorIDs(dropped)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(mirrorBodyDropSQL, rootID, list); err != nil {
		return nil, err
	}
	return dropped, nil
}

// bodyOutcome is the rule for a body brought by a ticket of version
// updatedAt read from readAt, admitted as a (mirrorBinds.admit), with
// openedAt the person's opening it records (zero for none), against the
// issue's state st: not kept when the mirror does not keep the issue, when
// an open ticket's read is not of the issue's current bind, or when the
// record holds no body and a read no open ticket made is not the opening;
// superseded by a held body of a later (update time, read) pair; the same
// body when the digest is the held one's; and else stored, starting a body
// when the record held none.
func (st mirrorCardState) bodyOutcome(updatedAt, readAt, openedAt int64, a mirrorAdmission, digest []byte) (out MirrorBodyOutcome, write, start bool) {
	held := st.body.ReadAt > 0
	switch {
	case !st.kept, !a.admitted:
		return MirrorBodyNotKept, false, false
	case !held && a.unbound && (openedAt <= 0 || readAt < openedAt):
		return MirrorBodyNotKept, false, false
	case held && (updatedAt < st.body.UpdatedAt || updatedAt == st.body.UpdatedAt && readAt < st.body.ReadAt):
		return MirrorBodySuperseded, false, false
	case held && bytes.Equal(digest, st.digest):
		return MirrorBodyStored, false, false
	}
	return MirrorBodyStored, true, !held
}

// StoreMirrorTickets stores, in one transaction, what one read begun at
// read found of tickets (each issue at most once, at most
// mirrorMaxPageRows): each card through the one card path, ordered,
// fenced, named from catalog, with its sprints and the marks of what it
// names that the mirror lacks, as UpsertMirrorCards stores a card read
// by ID; each body by the record's order of bodies (bodyOutcome), its
// bytes written only when its digest differs from the held body's; and
// each status result (MirrorStatusSince of the ticket's evidence) for
// the version it was read for (mirrorStatusWriteSQL). opened is a
// person's opening or refresh the read serves (the zero read for none),
// which orders the bodies the cap keeps. A ticket an open ticket read
// (MirrorTicket.Bind) writes its body, its opening and the order the cap
// keeps by only while its bind is the issue's current one, looked up once
// in the transaction (mirrorBinds.admit), with a body or as a check by
// card alike, its opening the later of opened and the bind's; its card
// and status evidence are ordered as any read's. A read no open ticket
// made is ordered by its (update time, read) alone and records opened
// alone: its body is the record's like any, which a later view of the
// issue may show. sweep is the root's sweep
// captured before the read (ErrTrackerCacheSwept). The outcomes are of the
// committed state: a body the cap dropped, started by this write or held
// before, is not held.
//
// Statements: the root's check, one per kind the catalog names, the
// cards' state, the upsert, one move of every kept ticket's read, body
// bookkeeping and status result, the marks, one per body whose bytes
// changed, and two to keep the cap when a body was started.
func (s *Store) StoreMirrorTickets(rootID, sweep int64, tickets []MirrorTicket, catalog MirrorCatalog, opened, read MirrorRead) (out MirrorTicketsWrite, err error) {
	opening, err := opened.in(rootID)
	if err != nil {
		return MirrorTicketsWrite{}, err
	}
	times, err := read.in(rootID)
	if err != nil {
		return MirrorTicketsWrite{}, err
	}
	readAt := times.at
	if len(tickets) == 0 || len(tickets) > mirrorMaxPageRows || readAt <= 0 || sweep <= 0 {
		return MirrorTicketsWrite{}, fmt.Errorf("%w: %d tickets, none or over %d, or no read time or sweep", ErrInvalidTrackerCache,
			len(tickets), mirrorMaxPageRows)
	}
	rows := make([]mirrorCardRow, len(tickets))
	digests := make([][]byte, len(tickets))
	for i, t := range tickets {
		if err := t.Card.check(); err != nil {
			return MirrorTicketsWrite{}, err
		}
		if slices.ContainsFunc(tickets[:i], func(o MirrorTicket) bool { return o.Card.IssueID == t.Card.IssueID }) {
			return MirrorTicketsWrite{}, fmt.Errorf("%w: issue %s is read twice", ErrInvalidTrackerCache, t.Card.IssueID)
		}
		// A bind of another root (a run that began before a restore made the
		// pump's new root) is refused as an ended one is; one of another
		// issue is no read of this ticket.
		if !t.Bind.IsZero() && t.Bind.issue != t.Card.IssueID {
			return MirrorTicketsWrite{}, fmt.Errorf("%w: issue %s is read under a bind of issue %s", ErrInvalidTrackerCache, t.Card.IssueID, t.Bind.issue)
		}
		if e := t.Status; e != nil && (e.UpdatedAt != t.Card.UpdatedAt || e.Status != t.Card.StatusID) {
			return MirrorTicketsWrite{}, fmt.Errorf("%w: the status evidence of issue %s is not of its card", ErrInvalidTrackerCache, t.Card.IssueID)
		}
		rows[i] = newMirrorCardRow(t.Card)
		if t.Body != nil {
			digests[i] = mirrorBodyDigest(t.Body)
		}
	}
	if err := catalog.check(); err != nil {
		return MirrorTicketsWrite{}, err
	}
	encoded, err := encodeMirrorJSON(rows)
	if err != nil {
		return MirrorTicketsWrite{}, err
	}
	action := fmt.Sprintf("store: store tracker mirror %d tickets of %d issues", rootID, len(tickets))
	err = s.mirrorSweptWrite(rootID, sweep, action, func(tx *sql.Tx) error {
		out = MirrorTicketsWrite{Tickets: make([]MirrorTicketWrite, len(tickets))}
		var err error
		if out.Changes.Catalog, err = upsertMirrorCatalogTx(tx, rootID, catalog, readAt); err != nil {
			return err
		}
		states, err := readMirrorCardStatesTx(tx, rootID, encoded, len(rows), readAt)
		if err != nil {
			return err
		}
		admitted := s.mirrorBinds.admit(rootID, tickets)
		moves := make([]mirrorTicketMove, 0, len(tickets))
		// Whether a ticket moves the body version or the opening the record
		// holds (mirrorTicketMoveSQL.full).
		versions := false
		var bodies []int
		var moved []string
		started := false
		for i, t := range tickets {
			st, w := states[i], &out.Tickets[i]
			id := t.Card.IssueID
			w.Kept = st.kept
			if st.sprintsMoved {
				moved = append(moved, id)
			}
			if !st.kept {
				out.Changes.Unkept = append(out.Changes.Unkept, id)
				if t.Body != nil {
					w.Body = MirrorBodyNotKept
				}
				continue
			}
			if st.refused {
				out.Changes.Superseded = append(out.Changes.Superseded, id)
			}
			m := mirrorTicketMove{ID: id, Advance: st.advances, U: t.Card.UpdatedAt}
			if a := admitted[i]; a.admitted {
				// The opening the record orders its bodies by: this read's, or
				// its bind's when later.
				m.O = max(opening.at, a.opened)
			}

			if t.Body != nil {
				var write, start bool
				w.Body, write, start = st.bodyOutcome(t.Card.UpdatedAt, readAt, opening.at, admitted[i], digests[i])
				m.Body = w.Body == MirrorBodyStored
				if m.Body {
					m.D = hex.EncodeToString(digests[i])
					versions = versions || st.body.UpdatedAt != m.U
				}
				if write {
					w.BodyChanged, started = true, started || start
					bodies = append(bodies, i)
				}
				// The same bytes read of another version move what the body is
				// of, which a view of it shows (the body behind the record).
				w.BodyChanged = w.BodyChanged || m.Body && st.body.UpdatedAt != t.Card.UpdatedAt
			}
			// A refused card's evidence is of a card the record does not
			// hold: it gives no result.
			versions = versions || m.O > st.bodyOpened && (m.Body || st.body.ReadAt > 0)
			if t.Status != nil && !st.refused {
				var r mirrorStatusResult
				r, m.S = mirrorStatusSince(*t.Status)
				m.X, m.V, m.T = int(r), t.Card.UpdatedAt, t.Status.Status
			}
			moves = append(moves, m)
		}
		slices.Sort(out.Changes.Unkept)
		slices.Sort(out.Changes.Superseded)
		var after map[string]mirrorRecordAfter
		move := func(stored []string) error {
			if len(moves) == 0 {
				return nil
			}
			for k := range moves {
				// A card the upsert replaced carries this read already.
				if _, found := slices.BinarySearch(stored, moves[k].ID); found {
					moves[k].Advance = false
				}
			}
			list, err := encodeMirrorJSON(moves)
			if err != nil {
				return err
			}
			after = map[string]mirrorRecordAfter{}
			query := mirrorTicketMoveSQL.reads
			if versions {
				query = mirrorTicketMoveSQL.full
			}
			if _, err := scanMirrorRecordsAfter(tx, query, after, rootID, list, readAt, times.wall); err != nil {
				return fmt.Errorf("record the tickets' read: %w", err)
			}
			return nil
		}
		slices.Sort(moved)
		stored, err := storeMirrorCardsTx(tx, mirrorLinkedCards, rootID, rows, encoded, readAt, moved, move)
		if err != nil {
			return err
		}
		out.Changes.Cards = stored.changed
		for _, i := range bodies {
			if _, err := tx.Exec(mirrorBodyWriteSQL, rootID, tickets[i].Card.IssueID, mirrorData(tickets[i].Body)); err != nil {
				return fmt.Errorf("write the body of issue %s: %w", tickets[i].Card.IssueID, err)
			}
		}
		var dropped []string
		if started {
			if dropped, err = evictMirrorBodiesTx(tx, rootID); err != nil {
				return fmt.Errorf("drop bodies: %w", err)
			}
		}
		for i, t := range tickets {
			w := &out.Tickets[i]
			if !w.Kept {
				continue
			}
			id := t.Card.IssueID
			_, w.CardChanged = slices.BinarySearch(stored.changed, id)
			a, ok := after[id]
			switch {
			case ok:
			case stored.after[id] != (mirrorRecordAfter{}):
				a = stored.after[id]
			default:
				// Neither the upsert nor the move wrote the row: it is as the
				// state read found it.
				st := states[i]
				a = mirrorRecordAfter{updatedAt: st.updatedAt, statusSince: st.since, statusFinal: st.finalFor >= st.updatedAt,
					bodyUpdatedAt: st.body.UpdatedAt, bodyRead: st.body.ReadAt, bodyWall: st.body.Wall}
			}
			w.UpdatedAt, w.StatusFinal = a.updatedAt, a.statusFinal
			w.StatusChanged = a.statusSince != states[i].since
			w.Held = MirrorBodyState{UpdatedAt: a.bodyUpdatedAt, ReadAt: a.bodyRead, Wall: a.bodyWall}
			if slices.Contains(dropped, id) {
				w.Held, w.BodyChanged = MirrorBodyState{}, false
				if w.Body != MirrorBodyNone {
					w.Body = MirrorBodyNotKept
				}
				if states[i].body.ReadAt == 0 {
					// Started and dropped by this write: never held.
					dropped = slices.DeleteFunc(dropped, func(d string) bool { return d == id })
				}
			}
		}
		if len(dropped) > 0 {
			slices.Sort(dropped)
			out.Dropped = dropped
		}
		return nil
	})
	if err != nil {
		return MirrorTicketsWrite{}, err
	}
	return out, nil
}

// MirrorMaxBodies bounds the bodies one ListMirrorBodies read returns: a
// body is up to the transport's answer limit, so a read of every open
// ticket's body at once is not one statement's work.
const MirrorMaxBodies = 16

// mirrorBodiesSQL reads the bodies the records of the kept issues among a
// JSON list of IDs hold, each with its record's card, probing each by key.
var mirrorBodiesSQL = `SELECT ` + mirrorCardColumns + `, i.body_updated_at, i.body_read_at, i.body_wall, i.body_opened_at, d.data
	FROM json_each(?2) l
	CROSS JOIN tracker_cache_issues i ON i.root_id = ?1 AND i.issue_id = l.value
	JOIN tracker_cache_issue_bodies d ON d.root_id = i.root_id AND d.issue_id = i.issue_id
	WHERE i.body_read_at > 0 AND ` + mirrorIssueKept("i.root_id", "i.issue_id")

// ListMirrorBodies reads the bodies the records of issueIDs (at most
// MirrorMaxBodies; a repeated ID is read once) hold, with each record's card, sorted by
// issue ID, in one statement: an issue the mirror does not keep, or whose
// record holds no body, is left out. A body read is of an issue kept in
// the same statement, so the cleanup's sweep, which removes only unkept
// issues, has removed none of what it returns.
func (s *Store) ListMirrorBodies(rootID int64, issueIDs []string) ([]MirrorBody, error) {
	if len(issueIDs) == 0 {
		return nil, nil
	}
	if len(issueIDs) > MirrorMaxBodies {
		return nil, fmt.Errorf("%w: %d bodies asked, over %d", ErrInvalidTrackerCache, len(issueIDs), MirrorMaxBodies)
	}
	encoded, err := encodeMirrorIDs(issueIDs)
	if err != nil {
		return nil, err
	}
	out := make([]MirrorBody, 0, len(issueIDs))
	err = scanMirrorRows(s.reader(), mirrorBodiesSQL, []any{rootID, encoded}, func(r rowScanner) error {
		var b MirrorBody
		card, err := scanMirrorCard(r, &b.UpdatedAt, &b.ReadAt, &b.Wall, &b.OpenedAt, &b.Data)
		b.IssueID, b.Card = card.IssueID, card
		out = append(out, b)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("store: read tracker mirror %d bodies of %d issues: %w", rootID, len(issueIDs), err)
	}
	slices.SortFunc(out, func(a, b MirrorBody) int { return strings.Compare(a.IssueID, b.IssueID) })
	return out, nil
}
