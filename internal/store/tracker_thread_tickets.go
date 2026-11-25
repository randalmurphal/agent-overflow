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
// and v167). IssueKey, IssueTitle and the status are the link's last-seen
// copy: the card of the site's link reader's mirror as a write last stored
// it (linkCopySQL), or what a transfer carried. A thread row shows the
// copy only while that mirror holds no card of the issue
// (threadTicketLinkColumn). LinkedAt is unix milliseconds.
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
}

const threadTicketColumns = `thread_id, kind, site, issue_id, issue_key, issue_title, status_id, status_name, status_group,
	origin, linked_at`

func scanThreadTicket(scan func(...any) error) (ThreadTicket, error) {
	var t ThreadTicket
	err := scan(&t.ThreadID, &t.Ticket.Kind, &t.Ticket.Site, &t.Ticket.IssueID, &t.IssueKey, &t.IssueTitle,
		&t.StatusID, &t.StatusName, &t.StatusGroup, &t.Origin, &t.LinkedAt)
	return t, err
}

// What a thread row says of its ticket (ThreadTicketLink.State), derived
// on the computer that reads the row from the mirror of the site's link
// reader (ticketSiteReaderSQL).
const (
	// TicketLinkCurrent: that mirror holds a card of the issue, and no read
	// of it that began later missed the issue (mirrorIssueHeldSQL).
	TicketLinkCurrent = "current"
	// TicketLinkNotFound: a read of that mirror did not return the issue,
	// and no card it holds is from a read that began later: deleted, or
	// hidden from the token's user.
	TicketLinkNotFound = "not_found"
	// TicketLinkNotChecked: a connection to the site is on here, and its
	// mirror has neither a card nor an absence of the issue.
	TicketLinkNotChecked = "not_checked"
	// TicketLinkTurnedOff: every connection to the site here is off.
	TicketLinkTurnedOff = "turned_off"
	// TicketLinkNoConnection: this computer has no connection to the site.
	TicketLinkNoConnection = "no_connection"
)

// ThreadTicketLink is a thread's link as its row shows it: the ticket,
// its key, title and status, and how the link came to be. The key, title
// and status are the link reader's card while its mirror holds one, and
// the link's last-seen copy otherwise. State and ConnectionID are derived
// when the row is read: State as the constants above say, ConnectionID the
// connection here that reads the ticket ("" when none is on).
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

// mirrorIssueHeldJoinSQL joins, for the issue issue of mirror root root
// (SQL expressions), its card as i and its absence as m, either NULL for
// none: what mirrorIssueHeldSQL and mirrorIssueGoneSQL read.
func mirrorIssueHeldJoinSQL(root, issue string) string {
	return `LEFT JOIN tracker_cache_issues i ON i.root_id = ` + root + ` AND i.issue_id = ` + issue + `
	    LEFT JOIN tracker_cache_issue_missing m ON m.root_id = ` + root + ` AND m.issue_id = ` + issue
}

// mirrorIssueHeldSQL is the one rule for whether a mirror holds an issue,
// over mirrorIssueHeldJoinSQL's i and m: it holds a card of it, and no
// read that missed the issue began after the card's latest read.
// mirrorIssueGoneSQL is the rule for an absence in force: a read missed
// the issue no earlier than any card the mirror holds was read. The thread
// row's link and SetThreadTicket read them; the card writers delete the
// absences a later card supersedes (mirrorAbsencesClearSQL) and the missing
// mark records one only when no card is from a later read, which keeps
// the absences to those in force.
const (
	mirrorIssueHeldSQL = `(i.root_id IS NOT NULL AND (m.root_id IS NULL OR m.read_at < i.read_at))`
	mirrorIssueGoneSQL = `(m.root_id IS NOT NULL AND m.read_at >= COALESCE(i.read_at, 0))`
)

// threadTicketLinkJSON and threadTicketLinkFrom are the one projection of
// a link as its row shows it (ThreadTicketLink) from thread_tickets tt:
// the key, title and status of the card the site's link reader's mirror
// holds (rr, i), the status's name and group in that root's catalog (st),
// else the link's copy; the state by mirrorIssueHeldSQL and
// mirrorIssueGoneSQL, else by the site's connections; the connection the
// link reader. Each link costs a probe of its site's connections, its
// reader's root, card, absence and status, all by key.
var (
	threadTicketLinkJSON = `json_object(
	    'kind', tt.kind, 'site', tt.site, 'issueId', tt.issue_id,
	    'key', COALESCE(i.issue_key, tt.issue_key), 'title', COALESCE(i.title, tt.issue_title),
	    'statusId', COALESCE(i.status_id, tt.status_id),
	    'statusName', iif(i.root_id IS NULL, tt.status_name, COALESCE(st.name, '')),
	    'statusGroup', iif(i.root_id IS NULL, tt.status_group, COALESCE(st.status_group, 0)),
	    'origin', tt.origin, 'linkedAt', tt.linked_at,
	    'state', CASE WHEN ` + mirrorIssueHeldSQL + ` THEN 'current' WHEN ` + mirrorIssueGoneSQL + ` THEN 'not_found'
	      ELSE ` + ticketSiteStateSQL + ` END,
	    'connectionId', COALESCE(rr.connection_id, ` + ticketSiteConnectionSQL + `, ''))`
	threadTicketLinkFrom = `thread_tickets tt
	    LEFT JOIN tracker_cache_roots rr ON rr.connection_id = ` + ticketSiteConnectionSQL + `
	    ` + mirrorIssueHeldJoinSQL("rr.id", "tt.issue_id") + `
	    LEFT JOIN tracker_cache_statuses st ON st.root_id = rr.id AND st.status_id = i.status_id`
)

// threadTicketLinkColumn is threadColumns' column of a thread's link, a
// ThreadTicketLink as JSON or NULL: one probe of thread_tickets' key per
// thread, and the projection's probes for a linked thread. A read of many
// threads computes each link once instead (threadListSelect). The JSON is
// only scanned: a scalar subquery's result carries no JSON subtype into
// another JSON function.
var threadTicketLinkColumn = `CASE WHEN EXISTS (SELECT 1 FROM thread_tickets) THEN (SELECT ` + threadTicketLinkJSON + `
	  FROM ` + threadTicketLinkFrom + ` WHERE tt.thread_id = threads.id) END`

// threadTicketLinksSQL is the projection of every link, by thread
// (thread_id, link): the links CTE of threadListSelect.
var threadTicketLinksSQL = `SELECT tt.thread_id AS thread_id, ` + threadTicketLinkJSON + ` AS link FROM ` + threadTicketLinkFrom

// ticketSiteReaderSQL is the one rule for which connection here reads and
// writes the links of tracker kind's site site, both SQL expressions: the
// site's link reader, its lowest connection ID turned on, NULL when none
// is. The thread row's link reads that connection's mirror alone,
// TicketConnection names it for a new link, SetThreadTicket links only
// through it, its mirror alone keeps the site's links (mirrorLinksSQL)
// and writes their copy (linkCopySQL), so a link and the call that makes
// one name the same connection.
func ticketSiteReaderSQL(kind, site string) string {
	return `(SELECT MIN(reader.id) FROM tracker_connections reader
	      WHERE reader.kind = ` + kind + ` AND reader.site = ` + site + ` AND reader.parent_id IS NULL AND reader.enabled = 1)`
}

// ticketSiteStateSQL is the state of a link to a ticket of tracker
// tt.kind's site tt.site that the link reader's mirror holds nothing of,
// and ticketSiteConnectionSQL that site's link reader.
const ticketSiteStateSQL = `CASE (SELECT MAX(c.enabled) FROM tracker_connections c
	        WHERE c.kind = tt.kind AND c.site = tt.site AND c.parent_id IS NULL)
	      WHEN 1 THEN 'not_checked' WHEN 0 THEN 'turned_off' ELSE 'no_connection' END`

var ticketSiteConnectionSQL = ticketSiteReaderSQL("tt.kind", "tt.site")

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

// ErrThreadTicketNotHeld refuses a link when the mirror it reads through
// holds no card of the issue that no later read missed
// (mirrorIssueHeldSQL): with no Read, the caller reads the card from the
// tracker. A refusal changes nothing.
var ErrThreadTicketNotHeld = errors.New("store: the mirror holds no current card of the ticket")

// ErrThreadTicketNotReader refuses a link through a mirror whose
// connection is not its site's link reader (ticketSiteReaderSQL), as when
// a connection of the site with a lower ID was turned on since the caller
// chose it. A refusal changes nothing.
var ErrThreadTicketNotReader = errors.New("store: the mirror's connection no longer reads its site's ticket links")

// ThreadTicketWrite links thread ThreadID to issue IssueID of the tracker
// and site of mirror root Root's connection. With Read set, Card and
// Catalog are what that read of the tracker returned, the card of
// IssueID, stored in the mirror (UpsertMirrorCards' rule, at Sweep) in the
// link's transaction, since the link is what makes the mirror keep the
// issue; with no Read, the link is of the card the mirror holds, and Card
// is empty.
type ThreadTicketWrite struct {
	ThreadID string
	Origin   string
	LinkedAt int64
	Root     int64
	IssueID  string
	Card     MirrorCard
	Read     MirrorRead
	Catalog  MirrorCatalog
	Sweep    int64
}

// ThreadTicketSet is what SetThreadTicket did: whether the link as a
// thread row shows it changed, its state included, the ticket it replaced (nil when the
// thread had none or had this one), and what storing a read card changed,
// whose Threads are the other threads whose row the card moved.
type ThreadTicketSet struct {
	Changed  bool
	Replaced *TicketRef
	Cards    MirrorChanges
}

// SetThreadTicket links w.ThreadID to w.IssueID in one transaction: for a
// read, the card in the mirror; then the link, and its copy from the card
// the mirror holds (linkCopySQL). Linking another ticket replaces the
// link; setting the ticket the thread has keeps the link's origin and
// time. The transaction checks that w.Root's connection is its site's link
// reader (ErrThreadTicketNotReader) and that the mirror holds the issue
// once the read card is stored (ErrThreadTicketNotHeld): a read card a
// later read superseded links the later card. A missing thread fails with
// sql.ErrNoRows, a thread a delete claimed or committed with
// ErrThreadDeleting, a thread with an outgoing move or an unsealed
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
		return ThreadTicketSet{}, fmt.Errorf("%w: a ticket link needs the mirror it reads through", ErrInvalidThreadTicket)
	case w.IssueID == "":
		return ThreadTicketSet{}, fmt.Errorf("%w: a ticket link needs its issue", ErrInvalidThreadTicket)
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
	case times.at > 0 && w.Card.IssueID != w.IssueID:
		return ThreadTicketSet{}, fmt.Errorf("%w: a link to issue %s with a card of issue %q", ErrInvalidThreadTicket, w.IssueID, w.Card.IssueID)
	case times.at > 0:
		if err := w.Card.check(); err != nil {
			return ThreadTicketSet{}, err
		}
		if rows, encoded, err = prepareMirrorCards([]MirrorCard{w.Card}, w.Catalog); err != nil {
			return ThreadTicketSet{}, err
		}
	case w.Sweep != 0:
		return ThreadTicketSet{}, fmt.Errorf("%w: a sweep with no read", ErrInvalidTrackerCache)
	case w.Card.IssueID != "":
		return ThreadTicketSet{}, fmt.Errorf("%w: a link to the mirror's card with a card of its own", ErrInvalidThreadTicket)
	}
	action := fmt.Sprintf("store: link thread %s to issue %s of tracker mirror %d", w.ThreadID, w.IssueID, w.Root)
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
		ref := TicketRef{IssueID: w.IssueID}
		var reader bool
		if err := tx.QueryRow(`SELECT c.kind, c.site, c.id IS `+ticketSiteReaderSQL("c.kind", "c.site")+`
			FROM tracker_cache_roots r JOIN tracker_connections c ON c.id = r.connection_id
			WHERE r.id = `+boundInt, w.Root).Scan(&ref.Kind, &ref.Site, &reader); err != nil {
			return fmt.Errorf("read the mirror's connection: %w", err)
		}
		if !reader {
			return ErrThreadTicketNotReader
		}
		before, replaced, had, err := readThreadLinkTx(tx, w.ThreadID)
		if err != nil {
			return err
		}
		// The link's first copy is the read card, or the mirror's: the
		// copy writer below makes it the card the mirror holds.
		seen := ThreadTicket{IssueKey: w.Card.Key, IssueTitle: w.Card.Title, StatusID: w.Card.StatusID}
		if times.at == 0 {
			held, err := readMirrorIssueHeldTx(tx, w.Root, w.IssueID, &seen)
			if err != nil {
				return err
			}
			if !held {
				return ErrThreadTicketNotHeld
			}
		}
		if _, err := tx.Exec(`INSERT INTO thread_tickets(`+threadTicketColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, '', 0, ?, ?)
			ON CONFLICT(thread_id) DO UPDATE SET
			  kind = excluded.kind, site = excluded.site, issue_id = excluded.issue_id,
			  issue_key = excluded.issue_key, issue_title = excluded.issue_title, status_id = excluded.status_id,
			  origin = CASE WHEN `+sameTicketSQL+` THEN origin ELSE excluded.origin END,
			  linked_at = CASE WHEN `+sameTicketSQL+` THEN linked_at ELSE excluded.linked_at END`,
			w.ThreadID, ref.Kind, ref.Site, ref.IssueID, seen.IssueKey, seen.IssueTitle, seen.StatusID, w.Origin, w.LinkedAt); err != nil {
			return fmt.Errorf("write the link: %w", err)
		}
		if times.at > 0 {
			if set.Cards, err = upsertMirrorCardsTx(tx, w.Root, rows, encoded, w.Catalog, times.at); err != nil {
				return err
			}
			held, err := readMirrorIssueHeldTx(tx, w.Root, w.IssueID, nil)
			if err != nil {
				return err
			}
			if !held {
				return ErrThreadTicketNotHeld
			}
			// The thread's own row is Changed's, which its caller announces.
			set.Cards.Threads = slices.DeleteFunc(set.Cards.Threads, func(id string) bool { return id == w.ThreadID })
		}
		copied, err := queryIDs(tx, threadLinkCopySQL, w.Root, w.ThreadID)
		if err != nil {
			return fmt.Errorf("copy the mirror's card to the link: %w", err)
		}
		if len(copied) != 1 {
			return fmt.Errorf("copy the mirror's card to the link: %d links copied, want the thread's", len(copied))
		}
		after, _, _, err := readThreadLinkTx(tx, w.ThreadID)
		if err != nil {
			return err
		}
		set.Changed = before != after
		if had && replaced != ref {
			set.Replaced = &replaced
		}
		return nil
	})
	if err != nil {
		return ThreadTicketSet{}, err
	}
	return set, nil
}

// readMirrorIssueHeldTx reads whether root's mirror holds issue
// (mirrorIssueHeldSQL) and, into seen when it is not nil, the key, title
// and status of the card it holds.
func readMirrorIssueHeldTx(tx *sql.Tx, root int64, issue string, seen *ThreadTicket) (bool, error) {
	var held bool
	var key, title, status sql.NullString
	err := tx.QueryRow(`SELECT `+mirrorIssueHeldSQL+`, i.issue_key, i.title, i.status_id FROM tracker_cache_roots r
		`+mirrorIssueHeldJoinSQL("r.id", "?2")+` WHERE r.id = CAST(?1 AS INTEGER)`, root, issue).Scan(&held, &key, &title, &status)
	if err != nil {
		return false, fmt.Errorf("read the mirror's card of issue %s: %w", issue, err)
	}
	if seen != nil {
		seen.IssueKey, seen.IssueTitle, seen.StatusID = key.String, title.String, status.String
	}
	return held, nil
}

// readThreadLinkTx reads threadID's link in tx as its row shows it
// (threadTicketLinkJSON), state included, and the ticket it names; had
// is false, and link "", when it has none. SetThreadTicket's Changed
// compares two of its reads, so a set that changes only the state, such
// as a read that clears an absence, is announced.
func readThreadLinkTx(tx *sql.Tx, threadID string) (link string, ticket TicketRef, had bool, err error) {
	err = tx.QueryRow(`SELECT tt.kind, tt.site, tt.issue_id, `+threadTicketLinkJSON+` FROM `+threadTicketLinkFrom+`
		WHERE tt.thread_id = ?`, threadID).Scan(&ticket.Kind, &ticket.Site, &ticket.IssueID, &link)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", TicketRef{}, false, nil
	case err != nil:
		return "", TicketRef{}, false, fmt.Errorf("read thread %s ticket link: %w", threadID, err)
	}
	return link, ticket, true, nil
}

// sameTicketSQL is true in SetThreadTicket's upsert when the stored row
// names the ticket being set.
const sameTicketSQL = `(kind = excluded.kind AND site = excluded.site AND issue_id = excluded.issue_id)`

// mirrorReaderIssuesSQL is the bound list of issues ?2 (IDs) when mirror
// root ?1's connection is its site's link reader and the site has a link,
// and an empty list otherwise, so a write of another account or of a site
// with no link parses no bound list: json_each reads the list once, so
// the guard is in its argument; a WHERE term of the list's rows runs once
// a row.
var mirrorReaderIssuesSQL = `json_each(CASE WHEN EXISTS (SELECT 1 FROM tracker_cache_roots gr
	  JOIN tracker_connections gc ON gc.id = gr.connection_id
	  JOIN thread_tickets gt ON gt.kind = gc.kind AND gt.site = gc.site
	 WHERE gr.id = CAST(?1 AS INTEGER) AND gc.id = ` + ticketSiteReaderSQL("gc.kind", "gc.site") + `) THEN ?2 ELSE '[]' END)`

// linkCopySQL is the one writer of the links' last-seen copy: for root ?1
// of its site's link reader, each link of that site to an issue the
// mirror holds a card of, that on (a term of the link tt, after the FROM
// items from) and where (a term of the card i) name, takes the card's key,
// title and status and the status's name and group in the root's catalog.
// It writes every link it names, and returns each, so the threads whose
// card moved are announced whatever of the card changed. A thread this
// computer may not write is left alone.
func linkCopySQL(with, from, on, where string) string {
	return with + `UPDATE thread_tickets SET (issue_key, issue_title, status_id, status_name, status_group) =
	  (SELECT i.issue_key, i.title, i.status_id, COALESCE(st.name, ''), COALESCE(st.status_group, 0)
	     FROM tracker_cache_issues i LEFT JOIN tracker_cache_statuses st ON st.root_id = i.root_id AND st.status_id = i.status_id
	    WHERE i.root_id = CAST(?1 AS INTEGER) AND i.issue_id = thread_tickets.issue_id)
	WHERE thread_tickets.rowid IN (SELECT tt.rowid FROM ` + from + `tracker_cache_roots r
	    JOIN tracker_connections conn ON conn.id = r.connection_id
	    CROSS JOIN thread_tickets tt ON tt.kind = conn.kind AND tt.site = conn.site` + on + `
	    CROSS JOIN tracker_cache_issues i ON i.root_id = r.id AND i.issue_id = tt.issue_id
	   WHERE r.id = CAST(?1 AS INTEGER) AND conn.id = ` + ticketSiteReaderSQL("conn.kind", "conn.site") + where + `)
	  AND ` + ticketThreadWritableSQL + `
	RETURNING thread_tickets.thread_id`
}

var (
	// threadLinkCopySQL copies to thread ?2's link (SetThreadTicket).
	threadLinkCopySQL = linkCopySQL(``, ``, ` AND tt.thread_id = ?2`, ``)
	// mirrorLinkCopySQL copies to the links of the issues ?2 (IDs) a card
	// write stored or cleared an absence of (storeMirrorCardsTx): the list
	// drives the lookups by tracker, site and issue.
	mirrorLinkCopySQL = linkCopySQL(`WITH c AS MATERIALIZED (SELECT value FROM `+mirrorReaderIssuesSQL+`) `,
		`c CROSS JOIN `, ` AND tt.issue_id = c.value`, ``)
	// mirrorStatusLinkCopySQL copies to the site's links whose card has a
	// status of ?2 (IDs) the catalog renamed or regrouped
	// (upsertMirrorCatalogTx): a walk of the site's links, on a catalog
	// change, which is rare.
	mirrorStatusLinkCopySQL = linkCopySQL(``, ``, ``, ` AND i.status_id IN (SELECT value FROM json_each(?2))`)
)

// mirrorAbsencesClearSQL deletes, of the issues ?2 (IDs) a card write
// admitted, the absences in root ?1 that the card's read supersedes, and
// returns their issues: those whose link the write makes current again.
// A root with no absence reads no bound list.
const mirrorAbsencesClearSQL = `WITH c AS MATERIALIZED (SELECT value FROM json_each(?2)
	  WHERE EXISTS (SELECT 1 FROM tracker_cache_issue_missing g WHERE g.root_id = CAST(?1 AS INTEGER)))
	DELETE FROM tracker_cache_issue_missing WHERE root_id = CAST(?1 AS INTEGER) AND issue_id IN (SELECT value FROM c)
	  AND read_at < (SELECT i.read_at FROM tracker_cache_issues i WHERE i.root_id = CAST(?1 AS INTEGER) AND i.issue_id = tracker_cache_issue_missing.issue_id)
	RETURNING issue_id`

// The statements of MarkMirrorIssuesMissing, of ?1 root, ?2 the issues
// (IDs) a read begun at ?3 did not return. mirrorMissingLinksSQL reads,
// when the root is its site's link reader's, the threads this computer
// owns linked to an issue the mark makes not found: one the root does not
// hold with an absence in force, and holds no card of from a later read.
// mirrorMissingMarkSQL records the absences, each at its latest read.
var (
	mirrorMissingLinksSQL = `WITH c AS MATERIALIZED (SELECT value FROM ` + mirrorReaderIssuesSQL + `)
	SELECT tt.thread_id FROM c CROSS JOIN tracker_cache_roots r
	  JOIN tracker_connections conn ON conn.id = r.connection_id
	  CROSS JOIN thread_tickets tt ON tt.kind = conn.kind AND tt.site = conn.site AND tt.issue_id = c.value
	  ` + mirrorIssueHeldJoinSQL("r.id", "c.value") + `
	 WHERE r.id = CAST(?1 AS INTEGER) AND EXISTS (SELECT 1 FROM owned_threads o WHERE o.id = tt.thread_id)
	   AND NOT ` + mirrorIssueGoneSQL + ` AND COALESCE(i.read_at, 0) <= ?3`
	mirrorMissingMarkSQL = `INSERT INTO tracker_cache_issue_missing(root_id, issue_id, read_at)
	SELECT ?1, c.value, ?3 FROM json_each(?2) c
	 WHERE NOT EXISTS (SELECT 1 FROM tracker_cache_issues i WHERE i.root_id = CAST(?1 AS INTEGER) AND i.issue_id = c.value AND i.read_at > ?3)
	ON CONFLICT(root_id, issue_id) DO UPDATE SET read_at = excluded.read_at WHERE excluded.read_at > read_at`
)

// MarkMirrorIssuesMissing records that a read of root's tracker that
// began at read did not return the issues issueIDs names: an absence of
// each one the mirror holds no card of from a later read, which a card
// from a later read clears (mirrorIssueGoneSQL). It returns, sorted, the
// threads whose link it made not found, which happens only on the root of
// its site's link reader. A detached root fails with ErrTrackerCacheGone.
func (s *Store) MarkMirrorIssuesMissing(rootID int64, read MirrorRead, issueIDs []string) ([]string, error) {
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
	action := fmt.Sprintf("store: mark %d issues tracker mirror %d did not return", len(issueIDs), rootID)
	err = s.mirrorWrite(rootID, action, func(tx *sql.Tx) error {
		var err error
		if threads, err = queryIDs(tx, mirrorMissingLinksSQL, rootID, list, times.at); err != nil {
			return fmt.Errorf("read the links the absences move: %w", err)
		}
		if _, err := tx.Exec(mirrorMissingMarkSQL, rootID, list, times.at); err != nil {
			return fmt.Errorf("record the absences: %w", err)
		}
		return nil
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
// in the fork's transaction: the same ticket, copy, origin and time, and
// so the same row, since both are threads of this computer.
func copyThreadTicketTx(tx *sql.Tx, forkID, sourceID string) error {
	if _, err := tx.Exec(`INSERT INTO thread_tickets(`+threadTicketColumns+`)
		SELECT ?, kind, site, issue_id, issue_key, issue_title, status_id, status_name, status_group, origin, linked_at
		  FROM thread_tickets WHERE thread_id = ?`, forkID, sourceID); err != nil {
		return fmt.Errorf("store: copy the ticket link of %s to its fork %s: %w", sourceID, forkID, err)
	}
	return nil
}

// replaceThreadTicketTx makes link, as another computer's thread row
// carried it, threadID's link in tx, or removes threadID's link when link
// is nil. The ticket, the origin and the time are taken, and the key,
// title and status the sender's row showed become the link's copy: the
// carried state and connection are the sender's, so the row here derives
// its own, from the link reader's mirror once it holds the card.
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
	if _, err := tx.Exec(`INSERT INTO thread_tickets(`+threadTicketColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
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
// that are linked to ticket, newest first, a hidden mode left out as
// search leaves it out (an ask's copy, a side chat before Keep, which
// carries its source's link and lists once kept): one statement on
// idx_thread_tickets_issue, whose cost grows with the ticket's links and
// not with the threads.
func (s *Store) ListThreadsForTicket(ticket TicketRef) ([]Thread, error) {
	if err := ticket.validate(); err != nil {
		return nil, err
	}
	hidden, args := hiddenThreadModesClause("threads.mode")
	rows, err := s.reader().Query(`SELECT `+threadColumns+` FROM owned_threads AS threads
		WHERE threads.id IN (SELECT thread_id FROM thread_tickets WHERE kind = ? AND site = ? AND issue_id = ?)
		  AND `+hidden+`
		ORDER BY updated_at DESC`, append([]any{ticket.Kind, ticket.Site, ticket.IssueID}, args...)...)
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
