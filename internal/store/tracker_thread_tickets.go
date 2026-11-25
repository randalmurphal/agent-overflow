package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"agent-overflow/internal/threadmode"
)

// How a thread came to be linked to its ticket (thread_tickets.origin).
const (
	// TicketOriginStartWork: Start work on the ticket created the thread.
	TicketOriginStartWork = "start_work"
	// TicketOriginAsk: Ask about the ticket created the thread.
	TicketOriginAsk = "ask"
	// TicketOriginManual: the user linked an existing thread.
	TicketOriginManual = "manual"
	// TicketOriginAgent: the thread's agent linked it through a tool.
	TicketOriginAgent = "agent"
)

// ErrInvalidThreadTicket is returned by SetThreadTicket for a value the
// table refuses, and for a thread that is a fork holder.
var ErrInvalidThreadTicket = errors.New("store: invalid thread ticket")

// TicketRef names one issue: the tracker kind (jira or linear), the site
// as tracker_connections.site holds it for that kind, and the tracker's own
// issue ID.
type TicketRef struct {
	Kind    string
	Site    string
	IssueID string
}

// ThreadTicket is the one ticket a thread is linked to (migrations v162
// and v166). IssueKey, IssueTitle and the status are as last seen;
// SeenRoot is the mirror root whose card they equal, 0 for none, and
// Missing says that root's last read of the issue did not return it.
// LinkedAt is unix milliseconds.
type ThreadTicket struct {
	ThreadID    string
	Ticket      TicketRef
	IssueKey    string
	IssueTitle  string
	StatusID    string
	StatusName  string
	StatusGroup int
	Origin      string
	LinkedAt    int64
	SeenRoot    int64
	Missing     bool
}

const threadTicketColumns = `thread_id, kind, site, issue_id, issue_key, issue_title, status_id, status_name, status_group,
	origin, linked_at, seen_root, missing`

func scanThreadTicket(scan func(...any) error) (ThreadTicket, error) {
	var t ThreadTicket
	err := scan(&t.ThreadID, &t.Ticket.Kind, &t.Ticket.Site, &t.Ticket.IssueID, &t.IssueKey, &t.IssueTitle,
		&t.StatusID, &t.StatusName, &t.StatusGroup, &t.Origin, &t.LinkedAt, &t.SeenRoot, &t.Missing)
	return t, err
}

// What a thread row says of its link's copy (ThreadTicketLink.State),
// derived on the computer that reads the row.
const (
	// TicketLinkCurrent: the copy equals the card of a live mirror here.
	TicketLinkCurrent = "current"
	// TicketLinkNotFound: the last read of the issue by a live mirror here
	// did not return it: deleted, or hidden from the token's user.
	TicketLinkNotFound = "not_found"
	// TicketLinkNotChecked: a connection to the site is on here, and none
	// of its reads has stored the issue since it started.
	TicketLinkNotChecked = "not_checked"
	// TicketLinkTurnedOff: every connection to the site here is off.
	TicketLinkTurnedOff = "turned_off"
	// TicketLinkNoConnection: this computer has no connection to the site.
	TicketLinkNoConnection = "no_connection"
)

// ThreadTicketLink is a thread's link as its row shows it: the ticket,
// its key, title and status as last seen, and how the link came to be.
// State and ConnectionID are derived when the row is read: State as the
// constants above say, ConnectionID the connection here that reads the
// ticket ("" when none is on).
type ThreadTicketLink struct {
	Kind         string `json:"kind"`
	Site         string `json:"site"`
	IssueID      string `json:"issueId"`
	Key          string `json:"key"`
	Title        string `json:"title"`
	StatusID     string `json:"statusId"`
	StatusName   string `json:"statusName"`
	StatusGroup  int    `json:"statusGroup"`
	Origin       string `json:"origin"`
	LinkedAt     int64  `json:"linkedAt"`
	State        string `json:"state"`
	ConnectionID string `json:"connectionId"`
}

// threadTicketLinkColumn is threadColumns' column of a thread's link, a
// ThreadTicketLink as JSON or NULL: one probe of thread_tickets' key per
// thread, and for a linked thread one of its root and at most two of the
// connections of its site. The copy is current while seen_root names a
// root that is attached; a root's ID is never reused, so a detached,
// removed or emptied mirror's copy is not.
const threadTicketLinkColumn = `CASE WHEN EXISTS (SELECT 1 FROM thread_tickets) THEN (SELECT json_object(
        'kind', tt.kind, 'site', tt.site, 'issueId', tt.issue_id, 'key', tt.issue_key, 'title', tt.issue_title,
        'statusId', tt.status_id, 'statusName', tt.status_name, 'statusGroup', tt.status_group,
        'origin', tt.origin, 'linkedAt', tt.linked_at,
        'state', CASE WHEN seen.connection_id IS NOT NULL THEN iif(tt.missing, 'not_found', 'current')
          ELSE ` + ticketSiteStateSQL + ` END,
        'connectionId', COALESCE(seen.connection_id, ` + ticketSiteConnectionSQL + `, ''))
      FROM thread_tickets tt LEFT JOIN tracker_cache_roots seen ON seen.id = tt.seen_root
     WHERE tt.thread_id = threads.id) END`

// ticketSiteStateSQL is the state of a link to a ticket of tracker
// tt.kind's site tt.site that no live mirror here has read, and
// ticketSiteConnectionSQL the connection here that reads that site: the
// lowest ID of those turned on, NULL when none is. The thread row's link
// and TicketConnection both read them, so a link and the call that makes
// one name the same connection.
const (
	ticketSiteStateSQL = `CASE (SELECT MAX(c.enabled) FROM tracker_connections c
            WHERE c.kind = tt.kind AND c.site = tt.site AND c.parent_id IS NULL)
          WHEN 1 THEN 'not_checked' WHEN 0 THEN 'turned_off' ELSE 'no_connection' END`
	ticketSiteConnectionSQL = `(SELECT MIN(c.id) FROM tracker_connections c
          WHERE c.kind = tt.kind AND c.site = tt.site AND c.parent_id IS NULL AND c.enabled = 1)`
)

// TicketConnection is the connection of this computer that reads ticket,
// "" when none of its tracker and site is turned on, and the state a link
// to it shows before a read: TicketLinkNotChecked when one is on,
// TicketLinkTurnedOff when every one is off, TicketLinkNoConnection when
// there is none.
func (s *Store) TicketConnection(ticket TicketRef) (id, state string, err error) {
	if err := ticket.validate(); err != nil {
		return "", "", err
	}
	err = s.reader().QueryRow(`SELECT COALESCE(`+ticketSiteConnectionSQL+`, ''), `+ticketSiteStateSQL+`
		FROM (SELECT ? AS kind, ? AS site) AS tt`, ticket.Kind, ticket.Site).Scan(&id, &state)
	if err != nil {
		return "", "", fmt.Errorf("store: read the %s connection of %s: %w", ticket.Kind, ticket.Site, err)
	}
	return id, state, nil
}

func (r TicketRef) validate() error {
	switch {
	case r.Kind != TrackerKindJira && r.Kind != TrackerKindLinear:
		return fmt.Errorf("%w: unknown ticket kind %q", ErrInvalidThreadTicket, r.Kind)
	case r.Site == "" || r.IssueID == "":
		return fmt.Errorf("%w: a ticket needs its site and issue ID", ErrInvalidThreadTicket)
	}
	return nil
}

func (t ThreadTicket) validate() error {
	if err := t.Ticket.validate(); err != nil {
		return err
	}
	switch t.Origin {
	case TicketOriginStartWork, TicketOriginAsk, TicketOriginManual, TicketOriginAgent:
	default:
		return fmt.Errorf("%w: unknown origin %q", ErrInvalidThreadTicket, t.Origin)
	}
	switch {
	case t.ThreadID == "":
		return fmt.Errorf("%w: a ticket link needs its thread", ErrInvalidThreadTicket)
	case t.IssueKey == "":
		return fmt.Errorf("%w: a ticket link needs the issue key", ErrInvalidThreadTicket)
	case t.LinkedAt <= 0:
		return fmt.Errorf("%w: a ticket link needs its time", ErrInvalidThreadTicket)
	}
	return nil
}

// ErrThreadTicketCardMoved refuses a link copied from the mirror's card
// (ThreadTicketWrite with no Read) when the mirror holds another version
// of the card by the time the link is written: the caller reads the card
// again.
var ErrThreadTicketCardMoved = errors.New("store: the mirror's card of the ticket changed while it was being linked")

// ThreadTicketWrite links a thread to the issue of Card, as mirror root
// Root's connection names its tracker and site. With Read set, Card and
// Catalog are what that read of the tracker returned, stored in the
// mirror (UpsertMirrorCards' rule, at Sweep) in the link's transaction,
// since the link is what makes the mirror keep the issue. With no Read,
// Card is the mirror's own card of the issue, which the mirror must still
// hold at Card.UpdatedAt (ErrThreadTicketCardMoved). Card.Title is the
// title the link shows.
type ThreadTicketWrite struct {
	ThreadID string
	Origin   string
	LinkedAt int64
	Root     int64
	Card     MirrorCard
	Read     MirrorRead
	Catalog  MirrorCatalog
	Sweep    int64
}

// ThreadTicketSet is what SetThreadTicket did: whether the link as a
// thread row shows it changed, the ticket it replaced (nil when the
// thread had none or had this one), and what storing a read card changed,
// whose Threads are the other threads whose link the card moved.
type ThreadTicketSet struct {
	Changed  bool
	Replaced *TicketRef
	Cards    MirrorChanges
}

// SetThreadTicket links w.ThreadID to w.Card's issue in one transaction:
// the link, its copy of the card under w.Root (key, title, status, the
// status's name and group from the root's catalog), and, for a read, the
// card in the mirror. Linking another ticket replaces the link; setting
// the ticket the thread has keeps the link's origin and time. A missing
// thread fails with sql.ErrNoRows, a thread a delete claimed or committed
// with ErrThreadDeleting, a thread with an outgoing move or an unsealed
// transfer with *ThreadTransferError, a holder or an invalid value with
// ErrInvalidThreadTicket, a detached root with ErrTrackerCacheGone, and a
// sweep since w.Sweep with ErrTrackerCacheSwept; a refusal changes
// nothing.
func (s *Store) SetThreadTicket(w ThreadTicketWrite) (set ThreadTicketSet, err error) {
	switch w.Origin {
	case TicketOriginStartWork, TicketOriginAsk, TicketOriginManual, TicketOriginAgent:
	default:
		return ThreadTicketSet{}, fmt.Errorf("%w: unknown origin %q", ErrInvalidThreadTicket, w.Origin)
	}
	switch {
	case w.ThreadID == "":
		return ThreadTicketSet{}, fmt.Errorf("%w: a ticket link needs its thread", ErrInvalidThreadTicket)
	case w.LinkedAt <= 0:
		return ThreadTicketSet{}, fmt.Errorf("%w: a ticket link needs its time", ErrInvalidThreadTicket)
	case w.Root <= 0:
		return ThreadTicketSet{}, fmt.Errorf("%w: a ticket link needs the mirror its card is of", ErrInvalidThreadTicket)
	}
	if err := w.Card.check(); err != nil {
		return ThreadTicketSet{}, err
	}
	times, err := w.Read.in(w.Root)
	if err != nil {
		return ThreadTicketSet{}, err
	}
	var rows []mirrorCardRow
	var encoded string
	switch {
	case times.at > 0 && w.Sweep <= 0:
		return ThreadTicketSet{}, fmt.Errorf("%w: a card read for a link needs its sweep", ErrInvalidTrackerCache)
	case times.at > 0:
		if rows, encoded, err = prepareMirrorCards([]MirrorCard{w.Card}, w.Catalog); err != nil {
			return ThreadTicketSet{}, err
		}
	case w.Sweep != 0:
		return ThreadTicketSet{}, fmt.Errorf("%w: a sweep with no read", ErrInvalidTrackerCache)
	}
	action := fmt.Sprintf("store: link thread %s to issue %s of tracker mirror %d", w.ThreadID, w.Card.IssueID, w.Root)
	err = s.mirrorSweptWrite(w.Root, w.Sweep, action, func(tx *sql.Tx) error {
		if err := checkTicketThreadWritableTx(tx, w.ThreadID); err != nil {
			return err
		}
		var mode string
		if err := tx.QueryRow(`SELECT mode FROM threads WHERE id = ?`, w.ThreadID).Scan(&mode); err != nil {
			return fmt.Errorf("read thread: %w", err)
		}
		if mode == threadmode.ModeHolder {
			return fmt.Errorf("%w: thread %s is a fork holder", ErrInvalidThreadTicket, w.ThreadID)
		}
		ref := TicketRef{IssueID: w.Card.IssueID}
		if err := tx.QueryRow(`SELECT c.kind, c.site FROM tracker_cache_roots r JOIN tracker_connections c ON c.id = r.connection_id
			WHERE r.id = ?`, w.Root).Scan(&ref.Kind, &ref.Site); err != nil {
			return fmt.Errorf("read the mirror's connection: %w", err)
		}
		if times.at == 0 {
			var held bool
			if err := tx.QueryRow(`SELECT EXISTS (SELECT 1 FROM tracker_cache_issues WHERE root_id = ? AND issue_id = ? AND updated_at = ?)`,
				w.Root, w.Card.IssueID, w.Card.UpdatedAt).Scan(&held); err != nil {
				return fmt.Errorf("read the mirror's card: %w", err)
			}
			if !held {
				return ErrThreadTicketCardMoved
			}
		}
		before, had, err := readThreadTicketTx(tx, w.ThreadID)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO thread_tickets(`+threadTicketColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, '', 0, ?, ?, ?, 0)
			ON CONFLICT(thread_id) DO UPDATE SET
			  kind = excluded.kind, site = excluded.site, issue_id = excluded.issue_id,
			  issue_key = excluded.issue_key, issue_title = excluded.issue_title, status_id = excluded.status_id,
			  seen_root = excluded.seen_root, missing = 0,
			  origin = CASE WHEN `+sameTicketSQL+` THEN origin ELSE excluded.origin END,
			  linked_at = CASE WHEN `+sameTicketSQL+` THEN linked_at ELSE excluded.linked_at END`,
			w.ThreadID, ref.Kind, ref.Site, ref.IssueID, w.Card.Key, w.Card.Title, w.Card.StatusID, w.Origin, w.LinkedAt, w.Root); err != nil {
			return fmt.Errorf("write the link: %w", err)
		}
		if times.at > 0 {
			if set.Cards, err = upsertMirrorCardsTx(tx, w.Root, rows, encoded, w.Catalog, times.at); err != nil {
				return err
			}
			// The thread's own change is Changed, which its caller announces.
			set.Cards.Threads = slices.DeleteFunc(set.Cards.Threads, func(id string) bool { return id == w.ThreadID })
		}
		if _, err := tx.Exec(`UPDATE thread_tickets SET
			  status_name = COALESCE((SELECT name FROM tracker_cache_statuses WHERE root_id = ?2 AND status_id = thread_tickets.status_id), ''),
			  status_group = COALESCE((SELECT status_group FROM tracker_cache_statuses WHERE root_id = ?2 AND status_id = thread_tickets.status_id), 0)
			WHERE thread_id = ?1`, w.ThreadID, w.Root); err != nil {
			return fmt.Errorf("name the link's status: %w", err)
		}
		after, _, err := readThreadTicketTx(tx, w.ThreadID)
		if err != nil {
			return err
		}
		set.Changed = !had || before != after
		if had && before.Ticket != ref {
			replaced := before.Ticket
			set.Replaced = &replaced
		}
		return nil
	})
	if err != nil {
		return ThreadTicketSet{}, err
	}
	return set, nil
}

// readThreadTicketTx reads threadID's link in tx, false when it has none.
func readThreadTicketTx(tx *sql.Tx, threadID string) (ThreadTicket, bool, error) {
	t, err := scanThreadTicket(tx.QueryRow(`SELECT `+threadTicketColumns+` FROM thread_tickets WHERE thread_id = ?`, threadID).Scan)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return ThreadTicket{}, false, nil
	case err != nil:
		return ThreadTicket{}, false, fmt.Errorf("read thread %s ticket: %w", threadID, err)
	}
	return t, true, nil
}

// sameTicketSQL is true in SetThreadTicket's upsert when the stored row
// names the ticket being set.
const sameTicketSQL = `(kind = excluded.kind AND site = excluded.site AND issue_id = excluded.issue_id)`

// mirrorLinkCopySQL is the card writers' copy of their cards to the links
// that name the issues (storeMirrorCardsTx): ?1 the root, ?2 the cards as
// the card statements read them. Each link of the root's connection's
// tracker and site whose issue the write's card is of takes the card the
// mirror now holds, when that card is of the same update time as the
// write's (an older card the upsert superseded copies nothing; the write
// that stored the newer one copied it), and names the root: key, title,
// status and the status's name and group in the root's catalog, not
// missing. It writes a link only where one of those differs, so a card
// that changes nothing a link shows writes nothing and returns no thread,
// and only a link SetThreadTicket could write. With no link at all it
// reads no card (the CTE's constant guard).
var mirrorLinkCopySQL = `WITH c AS MATERIALIZED (
	  SELECT value ->> 'i' AS i, value ->> 'k' AS k, value ->> 'x' AS x, value ->> 's' AS s, value ->> 'u' AS u
	    FROM json_each(?2) WHERE EXISTS (SELECT 1 FROM thread_tickets))
	UPDATE thread_tickets SET issue_key = c.k, issue_title = c.x, status_id = c.s,
	  status_name = COALESCE(st.name, ''), status_group = COALESCE(st.status_group, 0), seen_root = ?1, missing = 0
	FROM c
	  JOIN tracker_cache_issues i ON i.root_id = ?1 AND i.issue_id = c.i AND i.updated_at = c.u
	  JOIN tracker_cache_roots r ON r.id = ?1
	  JOIN tracker_connections conn ON conn.id = r.connection_id
	  LEFT JOIN tracker_cache_statuses st ON st.root_id = ?1 AND st.status_id = c.s
	WHERE thread_tickets.kind = conn.kind AND thread_tickets.site = conn.site AND thread_tickets.issue_id = c.i
	  AND ` + ticketThreadWritableSQL + `
	  AND (thread_tickets.issue_key IS NOT c.k OR thread_tickets.issue_title IS NOT c.x OR thread_tickets.status_id IS NOT c.s
	    OR thread_tickets.status_name IS NOT COALESCE(st.name, '') OR thread_tickets.status_group IS NOT COALESCE(st.status_group, 0)
	    OR thread_tickets.seen_root IS NOT ?1 OR thread_tickets.missing <> 0)
	RETURNING thread_tickets.thread_id`

// MarkMirrorLinksMissing records that read, a read of root's tracker
// that began at read, did not return the issues issueIDs names: each link
// of the root's tracker and site to one of them names the root and is
// missing, unless the mirror holds a card of the issue from a read that
// began after read. It returns the threads whose link changed, sorted. A
// detached root fails with ErrTrackerCacheGone.
func (s *Store) MarkMirrorLinksMissing(rootID int64, read MirrorRead, issueIDs []string) ([]string, error) {
	times, err := read.in(rootID)
	if err != nil {
		return nil, err
	}
	if times.at <= 0 {
		return nil, fmt.Errorf("%w: a missing issue is of a read", ErrInvalidTrackerCache)
	}
	list, err := encodeMirrorIDs(issueIDs)
	if err != nil {
		return nil, err
	}
	var threads []string
	action := fmt.Sprintf("store: mark the links to %d issues tracker mirror %d did not return", len(issueIDs), rootID)
	err = s.mirrorWrite(rootID, action, func(tx *sql.Tx) error {
		var err error
		threads, err = queryIDs(tx, `UPDATE thread_tickets SET missing = 1, seen_root = ?1
			FROM tracker_cache_roots r JOIN tracker_connections conn ON conn.id = r.connection_id
			WHERE r.id = ?1 AND thread_tickets.kind = conn.kind AND thread_tickets.site = conn.site
			  AND thread_tickets.issue_id IN (SELECT value FROM json_each(?2))
			  AND NOT EXISTS (SELECT 1 FROM tracker_cache_issues i WHERE i.root_id = ?1 AND i.issue_id = thread_tickets.issue_id AND i.read_at > ?3)
			  AND `+ticketThreadWritableSQL+`
			  AND (thread_tickets.missing = 0 OR thread_tickets.seen_root IS NOT ?1)
			RETURNING thread_tickets.thread_id`, rootID, list, times.at)
		return err
	})
	if err != nil {
		return nil, err
	}
	slices.Sort(threads)
	return threads, nil
}

// checkTicketThreadWritableTx refuses a link write to a thread this
// computer may not change: one with an outgoing move or an unsealed
// transfer (*ThreadTransferError), and one a delete claimed or committed
// (ErrThreadDeleting). Both are read in the write's transaction, which
// serializes the write with a transfer reservation and a delete claim.
func checkTicketThreadWritableTx(tx *sql.Tx, threadID string) error {
	if err := checkThreadTransferAccess(tx, threadID); err != nil {
		return err
	}
	return checkThreadNotDeleting(tx, threadID)
}

// ticketThreadWritableSQL is checkTicketThreadWritableTx and the holder
// refusal as a predicate on thread_tickets.thread_id, for a write to many
// links at once.
var ticketThreadWritableSQL = `EXISTS (SELECT 1 FROM threads
	WHERE threads.id = thread_tickets.thread_id AND threads.mode <> ` + sqlTextLiteral(threadmode.ModeHolder) + `
	  AND threads.deleting = 0 AND threads.delete_claim = '' AND ` + threadTransferReadableExpr("threads.") + `)`

// ticketThreadOwnedSQL keeps the links of the threads this computer owns
// (owned_threads), as the sidebar and the public thread read do: a thread
// moved away whose cache stays here reads as having no link.
const ticketThreadOwnedSQL = `EXISTS (SELECT 1 FROM owned_threads WHERE owned_threads.id = thread_tickets.thread_id)`

// ClearThreadTicket removes threadID's ticket link and returns the ticket
// it named, false when it had none. It refuses a thread SetThreadTicket
// refuses for its transfer or its delete; a missing thread or a holder
// has no link to clear.
func (s *Store) ClearThreadTicket(threadID string) (cleared TicketRef, had bool, err error) {
	action := fmt.Sprintf("store: clear thread %s ticket", threadID)
	tx, err := s.db.Begin()
	if err != nil {
		return TicketRef{}, false, fmt.Errorf("%s: begin: %w", action, err)
	}
	defer rollbackTx(tx, action, &err)
	if err := checkTicketThreadWritableTx(tx, threadID); err != nil {
		return TicketRef{}, false, err
	}
	err = tx.QueryRow(`DELETE FROM thread_tickets WHERE thread_id = ? RETURNING kind, site, issue_id`, threadID).
		Scan(&cleared.Kind, &cleared.Site, &cleared.IssueID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		err = nil
	case err != nil:
		return TicketRef{}, false, fmt.Errorf("%s: %w", action, err)
	default:
		had = true
	}
	if err := tx.Commit(); err != nil {
		return TicketRef{}, false, fmt.Errorf("%s: commit: %w", action, err)
	}
	return cleared, had, nil
}

// copyThreadTicketTx gives a fork the link of its source, as it stands,
// in the fork's transaction: the same ticket, copy, root, origin and
// time, since both are threads of this computer.
func copyThreadTicketTx(tx *sql.Tx, forkID, sourceID string) error {
	if _, err := tx.Exec(`INSERT INTO thread_tickets(`+threadTicketColumns+`)
		SELECT ?, kind, site, issue_id, issue_key, issue_title, status_id, status_name, status_group, origin, linked_at, seen_root, missing
		  FROM thread_tickets WHERE thread_id = ?`, forkID, sourceID); err != nil {
		return fmt.Errorf("store: copy the ticket link of %s to its fork %s: %w", sourceID, forkID, err)
	}
	return nil
}

// replaceThreadTicketTx makes link, as another computer's thread row
// carried it, threadID's link in tx, or removes threadID's link when link
// is nil. Only the ticket, its key, title and status as last seen, the
// origin and the time are taken: a carried state, connection or root is
// the sender's, so the link names no root here and reads as not checked
// until a read here stores its card.
func replaceThreadTicketTx(tx *sql.Tx, threadID string, link *ThreadTicketLink) error {
	if _, err := tx.Exec(`DELETE FROM thread_tickets WHERE thread_id = ?`, threadID); err != nil {
		return fmt.Errorf("store: remove the ticket link %s held: %w", threadID, err)
	}
	if link == nil {
		return nil
	}
	t := ThreadTicket{ThreadID: threadID, Ticket: TicketRef{Kind: link.Kind, Site: link.Site, IssueID: link.IssueID},
		IssueKey: link.Key, IssueTitle: link.Title, Origin: link.Origin, LinkedAt: link.LinkedAt}
	if err := t.validate(); err != nil {
		return fmt.Errorf("store: the carried ticket link of %s: %w", threadID, err)
	}
	if link.StatusGroup < 0 || link.StatusGroup > 5 {
		return fmt.Errorf("store: the carried ticket link of %s: %w: status group %d", threadID, ErrInvalidThreadTicket, link.StatusGroup)
	}
	if _, err := tx.Exec(`INSERT INTO thread_tickets(`+threadTicketColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, 0)`,
		threadID, t.Ticket.Kind, t.Ticket.Site, t.Ticket.IssueID, t.IssueKey, t.IssueTitle,
		link.StatusID, link.StatusName, link.StatusGroup, t.Origin, t.LinkedAt); err != nil {
		return fmt.Errorf("store: write the carried ticket link of %s: %w", threadID, err)
	}
	return nil
}

// GetThreadTicket reads threadID's ticket link; false when it has none or
// this computer does not own the thread (owned_threads).
func (s *Store) GetThreadTicket(threadID string) (ThreadTicket, bool, error) {
	t, err := scanThreadTicket(s.reader().QueryRow(`SELECT `+threadTicketColumns+` FROM thread_tickets
		WHERE thread_id = ? AND `+ticketThreadOwnedSQL, threadID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return ThreadTicket{}, false, nil
	}
	if err != nil {
		return ThreadTicket{}, false, fmt.Errorf("store: read thread %s ticket: %w", threadID, err)
	}
	return t, true, nil
}

// ListThreadTickets reads the ticket links of the threads threadIDs names,
// keyed by thread; a thread with no link, or one this computer does not
// own, has no entry.
func (s *Store) ListThreadTickets(threadIDs []string) (map[string]ThreadTicket, error) {
	encoded, err := json.Marshal(uniqueNonEmptyStrings(threadIDs))
	if err != nil {
		return nil, fmt.Errorf("store: list thread tickets: encode: %w", err)
	}
	links, err := s.queryThreadTickets(`SELECT `+threadTicketColumns+` FROM thread_tickets
		WHERE thread_id IN (SELECT value FROM json_each(?)) AND `+ticketThreadOwnedSQL, string(encoded))
	if err != nil {
		return nil, fmt.Errorf("store: list thread tickets: %w", err)
	}
	out := make(map[string]ThreadTicket, len(links))
	for _, link := range links {
		out[link.ThreadID] = link
	}
	return out, nil
}

// ListThreadsForTicket reads the rows of the threads this computer owns
// that are linked to ticket, newest first, an ask's hidden copy left out:
// one statement on idx_thread_tickets_issue, whose cost grows with the
// ticket's links and not with the threads.
func (s *Store) ListThreadsForTicket(ticket TicketRef) ([]Thread, error) {
	if err := ticket.validate(); err != nil {
		return nil, err
	}
	rows, err := s.reader().Query(`SELECT `+threadColumns+` FROM owned_threads AS threads
		WHERE threads.id IN (SELECT thread_id FROM thread_tickets WHERE kind = ? AND site = ? AND issue_id = ?)
		  AND NOT (`+askCopySQL+`)
		ORDER BY updated_at DESC`, ticket.Kind, ticket.Site, ticket.IssueID)
	if err != nil {
		return nil, fmt.Errorf("store: list threads of %s ticket %s: %w", ticket.Kind, ticket.IssueID, err)
	}
	var out []Thread
	for rows.Next() {
		t, err := scanThread(rows)
		if err != nil {
			return nil, errors.Join(fmt.Errorf("store: list threads of %s ticket %s: %w", ticket.Kind, ticket.IssueID, err), rows.Close())
		}
		out = append(out, t)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("store: list threads of %s ticket %s: %w", ticket.Kind, ticket.IssueID, err)
	}
	return out, nil
}

// ListThreadsLinkedToSite lists the threads this computer owns that are
// linked to a ticket of tracker kind's site, sorted: those whose link's
// state a change to a connection of that site moves. One statement on the
// prefix of idx_thread_tickets_issue.
func (s *Store) ListThreadsLinkedToSite(kind, site string) ([]string, error) {
	ids, err := queryIDs(s.reader(), `SELECT thread_id FROM thread_tickets WHERE kind = ? AND site = ? AND `+ticketThreadOwnedSQL+`
		ORDER BY thread_id`, kind, site)
	if err != nil {
		return nil, fmt.Errorf("store: list the threads linked to %s site %s: %w", kind, site, err)
	}
	return ids, nil
}

// MirrorIssueRef is one issue of one mirror root, with the connection the
// root belongs to.
type MirrorIssueRef struct {
	ConnectionID string
	Root         int64
	IssueID      string
}

// ListUnkeptLinkedIssues reads, among tickets, the issues that an
// attached mirror of their tracker and site holds a record of and keeps
// no longer (mirrorIssueKept): those a link that ended was the last
// thing to keep. One statement, ordered by connection and issue.
func (s *Store) ListUnkeptLinkedIssues(tickets []TicketRef) ([]MirrorIssueRef, error) {
	if len(tickets) == 0 {
		return nil, nil
	}
	type ref struct {
		Kind  string `json:"k"`
		Site  string `json:"s"`
		Issue string `json:"i"`
	}
	refs := make([]ref, len(tickets))
	for i, t := range tickets {
		refs[i] = ref{t.Kind, t.Site, t.IssueID}
	}
	encoded, err := encodeMirrorJSON(refs)
	if err != nil {
		return nil, err
	}
	rows, err := s.reader().Query(`WITH t AS MATERIALIZED (SELECT DISTINCT value ->> 'k' AS k, value ->> 's' AS s, value ->> 'i' AS i FROM json_each(?1))
		SELECT conn.id, r.id, t.i FROM t
		  JOIN tracker_connections conn ON conn.kind = t.k AND conn.site = t.s AND conn.parent_id IS NULL
		  JOIN tracker_cache_roots r ON r.connection_id = conn.id
		 WHERE EXISTS (SELECT 1 FROM tracker_cache_issues i WHERE i.root_id = r.id AND i.issue_id = t.i)
		   AND NOT `+mirrorIssueKept("r.id", "t.i")+`
		 ORDER BY conn.id, t.i`, encoded)
	if err != nil {
		return nil, fmt.Errorf("store: read the issues no link keeps: %w", err)
	}
	var out []MirrorIssueRef
	for rows.Next() {
		var r MirrorIssueRef
		if err := rows.Scan(&r.ConnectionID, &r.Root, &r.IssueID); err != nil {
			return nil, errors.Join(fmt.Errorf("store: read the issues no link keeps: %w", err), rows.Close())
		}
		out = append(out, r)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("store: read the issues no link keeps: %w", err)
	}
	return out, nil
}

func (s *Store) queryThreadTickets(query string, args ...any) ([]ThreadTicket, error) {
	rows, err := s.reader().QueryContext(context.Background(), query, args...)
	if err != nil {
		return nil, err
	}
	var out []ThreadTicket
	for rows.Next() {
		link, err := scanThreadTicket(rows.Scan)
		if err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		out = append(out, link)
	}
	return out, errors.Join(rows.Err(), rows.Close())
}
