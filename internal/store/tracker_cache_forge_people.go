package store

import (
	"cmp"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Who a forge account is in a mirror root's tracker
// (tracker_cache_forge_people), which credit reads to count an approval
// or a comment on a pull request for a tracker person
// (tracker_cache_credit_forge.go). Its one writer is the linked tick's
// people unit (SetMirrorForgePeople), which the due listing feeds.

// mirrorForgePeoplePage bounds one ListMirrorForgePeopleDue read, and so
// the people one tick matches.
const mirrorForgePeoplePage = 25

// mirrorForgePeopleRecheck is how long a match stands before the unit
// checks it again: a person's name changes, and a second person can take
// the name.
const mirrorForgePeopleRecheck = 24 * time.Hour

// MirrorForgePerson is a forge account: its host, its login there and
// the name the forge gives it, "" for none.
type MirrorForgePerson struct {
	Host, Login, Name string
}

// MirrorForgePersonDue is a forge account whose match is due; Viewer
// reports that its token is the one the app reads its repository with.
type MirrorForgePersonDue struct {
	MirrorForgePerson
	Viewer bool
}

// MirrorForgeMatch is who a forge account is in the tracker: Person, ""
// for nobody.
type MirrorForgeMatch struct {
	MirrorForgePerson
	Person string
}

// mirrorForgeAccountsSQL is the forge accounts root ?1's credit reads, as
// a FROM item g of (host, login, name, own): the ones with an act on a
// pull request the root's credit reads (cr_src, cr_heads) that names the
// key of an issue the mirror holds, each with the forge's name for it and
// whether its token reads a source (own). It walks each source's keys by
// the source and probes the issue of each by its key, so it costs the
// keys of the connection's repositories and their acts, whatever the
// mirror holds.
const mirrorForgeAccountsSQL = `(SELECT s.host, a.login, max(a.name) AS name, max(s.viewer = a.login) AS own
	  FROM cr_src s
	  CROSS JOIN forge_history_keys k ON k.source_id = s.id
	  CROSS JOIN forge_history_acts a ON a.source_id = k.source_id AND a.number = k.number
	  WHERE EXISTS (SELECT 1 FROM tracker_cache_issues i WHERE i.root_id = CAST(?1 AS INTEGER) AND i.issue_key = k.key AND i.issue_key <> '')
	    AND (s.every OR EXISTS (SELECT 1 FROM forge_history_prs pr
	        CROSS JOIN cr_heads h ON h.source_id = pr.source_id AND h.head = pr.head_repo
	       WHERE pr.source_id = k.source_id AND pr.number = k.number))
	  GROUP BY s.host, a.login) g`

// mirrorForgePeopleDueSQL lists the forge accounts of root ?1
// (mirrorForgeAccountsSQL) whose match is due: the ones no person paired
// (tracker_forge_pairings) with no match, a match made for another name
// than the forge gives now, one checked before ?2 (a wall time), or, for
// the account whose token reads a source, a match to another person than
// ?3, the connection's own account ("" for none). Each account's match
// is read once; never-matched accounts come first, then the longest
// unchecked.
var mirrorForgePeopleDueSQL = `WITH ` + mirrorCreditSourcesSQL(mirrorCreditParams{rules: "1"}) + `
	SELECT g.host, g.login, g.name, ?3 <> '' AND g.own AS own
	FROM ` + mirrorForgeAccountsSQL + `
	LEFT JOIN tracker_cache_forge_people m ON m.root_id = CAST(?1 AS INTEGER) AND m.host = g.host AND m.login = g.login
	WHERE (m.checked_at IS NULL OR m.name IS NOT g.name OR m.checked_at < CAST(?2 AS INTEGER) OR ?3 <> '' AND g.own AND m.person IS NOT ?3)
	  AND NOT EXISTS (SELECT 1 FROM tracker_forge_pairings p WHERE p.connection_id = ` + mirrorRootConnectionSQL + ` AND p.host = g.host AND p.login = g.login)
	ORDER BY m.checked_at IS NOT NULL, m.checked_at, g.host, g.login
	LIMIT ` + strconv.Itoa(mirrorForgePeoplePage+1)

// ListMirrorForgePeopleDue lists up to mirrorForgePeoplePage forge
// accounts of rootID whose match is due at now, a wall time in Unix ms,
// viewer being the connection's own account (mirrorForgePeopleDueSQL);
// more reports that more are. A match written for an account takes it
// off the list for mirrorForgePeopleRecheck: the viewer's as that
// account, any other's as whoever it is.
func (s *Store) ListMirrorForgePeopleDue(rootID, now int64, viewer string) (due []MirrorForgePersonDue, more bool, err error) {
	label := fmt.Sprintf("list tracker mirror %d forge people due", rootID)
	due, err = mirrorReadSnapshot(s, label, func(tx sqlQueryer) ([]MirrorForgePersonDue, error) {
		var out []MirrorForgePersonDue
		err := scanMirrorRows(tx, mirrorForgePeopleDueSQL, []any{rootID, now - mirrorForgePeopleRecheck.Milliseconds(), viewer}, func(r rowScanner) error {
			var p MirrorForgePersonDue
			err := r.Scan(&p.Host, &p.Login, &p.Name, &p.Viewer)
			out = append(out, p)
			return err
		})
		return out, err
	})
	if err != nil {
		return nil, false, err
	}
	if len(due) > mirrorForgePeoplePage {
		return due[:mirrorForgePeoplePage], true, nil
	}
	return due, false, nil
}

// MirrorForgeAccountsPage bounds one ListMirrorForgeAccounts read.
const MirrorForgeAccountsPage = 500

// MirrorForgeAccount is a forge account a root's credit reads, with who
// it is: Paired when a person said (Person "" then: no person, left out
// of credit), else the name match's person once Checked, "" for nobody.
// PersonName names the person, "" when nothing does. Own reports that
// its token is the one the app reads its repository with.
type MirrorForgeAccount struct {
	MirrorForgePerson
	Own, Paired, Checked bool
	Person, PersonName   string
}

// mirrorForgeAccountsListSQL lists the forge accounts of root ?1
// (mirrorForgeAccountsSQL) with their name matches and the matched
// people's names. Its order chooses the page: the accounts a person
// paired, whose pairings are read beside it (ListMirrorForgeAccounts),
// then the ones matched to nobody, then by the forge's name.
var mirrorForgeAccountsListSQL = `WITH ` + mirrorCreditSourcesSQL(mirrorCreditParams{rules: "1"}) + `
	SELECT g.host, g.login, g.name, g.own, m.checked_at IS NOT NULL, coalesce(m.person, ''), coalesce(u.name, '')
	FROM ` + mirrorForgeAccountsSQL + `
	LEFT JOIN tracker_forge_pairings p ON p.connection_id = ` + mirrorRootConnectionSQL + ` AND p.host = g.host AND p.login = g.login
	LEFT JOIN tracker_cache_forge_people m ON m.root_id = CAST(?1 AS INTEGER) AND m.host = g.host AND m.login = g.login
	LEFT JOIN tracker_cache_users u ON u.root_id = CAST(?1 AS INTEGER) AND u.user_id = m.person AND m.person <> ''
	ORDER BY p.login IS NULL, coalesce(m.person, '') <> '', lower(iif(g.name <> '', g.name, g.login)), g.host, g.login
	LIMIT ` + strconv.Itoa(MirrorForgeAccountsPage+1)

// ListMirrorForgeAccounts lists the forge accounts connectionID's credit
// reads with who each is, and its pairings of accounts that acted on no
// pull request credit reads now, so every pairing can be seen and
// removed: the accounts that are nobody's first, then by name. It lists
// at most MirrorForgeAccountsPage of the accounts that acted; more
// reports that more did. A connection with no mirror lists its pairings.
func (s *Store) ListMirrorForgeAccounts(connectionID string) (accounts []MirrorForgeAccount, more bool, err error) {
	label := fmt.Sprintf("list the forge accounts of tracker connection %s", connectionID)
	type listed struct {
		accounts []MirrorForgeAccount
		more     bool
	}
	out, err := mirrorReadSnapshot(s, label, func(tx sqlQueryer) (listed, error) {
		var out listed
		pairings, err := listTrackerForgePairings(tx, connectionID)
		if err != nil {
			return out, err
		}
		paired := make(map[MirrorForgePerson]TrackerForgePairing, len(pairings))
		for _, p := range pairings {
			paired[MirrorForgePerson{Host: p.Host, Login: p.Login}] = p
		}
		var root sql.NullInt64
		if err := tx.QueryRow(`SELECT id FROM tracker_cache_roots WHERE connection_id = ?1`, connectionID).Scan(&root); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return out, err
		}
		if root.Valid {
			err = scanMirrorRows(tx, mirrorForgeAccountsListSQL, []any{root.Int64}, func(r rowScanner) error {
				var a MirrorForgeAccount
				if err := r.Scan(&a.Host, &a.Login, &a.Name, &a.Own, &a.Checked, &a.Person, &a.PersonName); err != nil {
					return err
				}
				key := MirrorForgePerson{Host: a.Host, Login: a.Login}
				if p, ok := paired[key]; ok {
					a.Paired, a.Person, a.PersonName = true, p.Person, p.PersonName
					delete(paired, key)
				}
				out.accounts = append(out.accounts, a)
				return nil
			})
			if err != nil {
				return out, err
			}
		}
		if len(out.accounts) > MirrorForgeAccountsPage {
			out.accounts, out.more = out.accounts[:MirrorForgeAccountsPage], true
		}
		for _, p := range pairings {
			if _, left := paired[MirrorForgePerson{Host: p.Host, Login: p.Login}]; left {
				out.accounts = append(out.accounts, MirrorForgeAccount{MirrorForgePerson: MirrorForgePerson{Host: p.Host, Login: p.Login},
					Paired: true, Person: p.Person, PersonName: p.PersonName})
			}
		}
		// The statement's order chose the page; the pairings change who is
		// nobody's and add accounts, so the list is ordered here.
		slices.SortFunc(out.accounts, func(a, b MirrorForgeAccount) int {
			named := func(a MirrorForgeAccount) string { return strings.ToLower(cmp.Or(a.Name, a.Login)) }
			return cmp.Or(cmp.Compare(boolToInt(a.Paired || a.Person != ""), boolToInt(b.Paired || b.Person != "")),
				cmp.Compare(named(a), named(b)), cmp.Compare(a.Host, b.Host), cmp.Compare(a.Login, b.Login))
		})
		return out, nil
	})
	if err != nil {
		return nil, false, err
	}
	return out.accounts, out.more, nil
}

// The match statements of root ?1, ?2 the JSON list of the matches.
const (
	mirrorForgePeopleMovedSQL = `SELECT count(*) FROM json_each(?2) e
	LEFT JOIN tracker_cache_forge_people m ON m.root_id = CAST(?1 AS INTEGER) AND m.host = e.value ->> 'Host' AND m.login = e.value ->> 'Login'
	WHERE coalesce(m.person, '') IS NOT e.value ->> 'Person'`
	mirrorForgePeopleUpsertSQL = `INSERT INTO tracker_cache_forge_people(root_id, host, login, name, person, checked_at)
	SELECT ?1, e.value ->> 'Host', e.value ->> 'Login', e.value ->> 'Name', e.value ->> 'Person', ?3 FROM json_each(?2) e WHERE true
	ON CONFLICT(root_id, host, login) DO UPDATE SET name = excluded.name, person = excluded.person, checked_at = excluded.checked_at
	WHERE name IS NOT excluded.name OR person IS NOT excluded.person OR checked_at IS NOT excluded.checked_at`
	// mirrorBoardsCreditSQL advances the totals revision of the boards of
	// root ?1 the JSON list ?2 names: credit, which the Backlog's pace
	// counts (readMirrorPaceTx), reads what changed.
	mirrorBoardsCreditSQL = `UPDATE tracker_cache_boards SET totals_revision = totals_revision + 1
	WHERE root_id = CAST(?1 AS INTEGER) AND board_id IN (SELECT value FROM json_each(?2))`
)

// SetMirrorForgePeople records matches, each checked at the wall time
// checkedAt (Unix ms), with the names of the people they name (catalog,
// stored as a history read's are, by the read that found them). changed
// reports that an account is another person's than before, no match and
// nobody being the same; it then advances the totals revision of boards,
// the root's boards that count credit, in the same transaction. A
// detached root fails with ErrTrackerCacheGone.
func (s *Store) SetMirrorForgePeople(rootID int64, read MirrorRead, matches []MirrorForgeMatch, catalog MirrorCatalog, checkedAt int64, boards []string) (changed bool, err error) {
	times, err := read.in(rootID)
	if err != nil {
		return false, err
	}
	if err := catalog.check(); err != nil {
		return false, err
	}
	seen := map[MirrorForgePerson]bool{}
	for _, m := range matches {
		key := MirrorForgePerson{Host: m.Host, Login: m.Login}
		if m.Host == "" || m.Login == "" || seen[key] || checkedAt <= 0 || times.at <= 0 {
			return false, fmt.Errorf("%w: forge account %q on %q is empty or repeated, or the match has no read or time", ErrInvalidTrackerCache, m.Login, m.Host)
		}
		seen[key] = true
	}
	if len(matches) == 0 {
		return false, nil
	}
	listed, err := encodeMirrorJSON(matches)
	if err != nil {
		return false, err
	}
	credited, err := encodeMirrorIDs(boards)
	if err != nil {
		return false, err
	}
	err = s.mirrorWrite(rootID, fmt.Sprintf("store: match tracker mirror %d forge people", rootID), func(tx *sql.Tx) error {
		changed = false
		if len(catalog.Users) > 0 {
			if _, err := upsertMirrorCatalogTx(tx, rootID, catalog, times.at); err != nil {
				return err
			}
		}
		var moved int
		if err := tx.QueryRow(mirrorForgePeopleMovedSQL, rootID, listed).Scan(&moved); err != nil {
			return fmt.Errorf("read the matches that moved: %w", err)
		}
		if _, err := tx.Exec(mirrorForgePeopleUpsertSQL, rootID, listed, checkedAt); err != nil {
			return fmt.Errorf("write the matches: %w", err)
		}
		changed = moved > 0
		if changed {
			if _, err := tx.Exec(mirrorBoardsCreditSQL, rootID, credited); err != nil {
				return fmt.Errorf("advance the totals of the boards whose credit the matches moved: %w", err)
			}
		}
		return nil
	})
	return changed, err
}

// mirrorForgeCreditIssuesSQL lists the issues of board ?2 of root ?1,
// under the regrouping ?3, whose key the JSON list ?4 names and whose
// credit can read a forge act: one done now, or a member of a closed
// sprint of the board. Each is found by its key.
var mirrorForgeCreditIssuesSQL = `SELECT i.issue_id FROM json_each(?4) k
	CROSS JOIN tracker_cache_issues i ON i.root_id = CAST(?1 AS INTEGER) AND i.issue_key = k.value AND i.issue_key <> ''
	CROSS JOIN tracker_cache_board_issues b ON b.root_id = i.root_id AND b.board_id = ?2 AND b.issue_id = i.issue_id
	WHERE i.status_id IN (` + mirrorDoneStatusesSQL("?3") + `) OR EXISTS (SELECT 1 FROM tracker_cache_issue_sprints m
	    CROSS JOIN tracker_cache_sprints sp ON sp.root_id = m.root_id AND sp.board_id = ?2 AND sp.sprint_id = m.sprint_id
	   WHERE m.root_id = i.root_id AND m.issue_id = i.issue_id AND sp.state = ` + strconv.Itoa(MirrorSprintClosed) + `)`

// AdvanceMirrorForgeCredit tells the boards of rootID that count credit
// (boards, each with its project's regrouping) that the forge history
// their credit reads changed: with all, every one of them (a source's
// coverage or failure, or which sources the connection uses); else the
// ones holding a ticket of keys whose credit can read a forge act
// (mirrorForgeCreditIssuesSQL). It advances the totals revision of each
// such board once and returns those tickets (sorted), none with all. A
// detached root fails with ErrTrackerCacheGone.
func (s *Store) AdvanceMirrorForgeCredit(rootID int64, boards []MirrorActsBoard, keys []string, all bool) (issues []string, err error) {
	if len(boards) == 0 || !all && len(keys) == 0 {
		return nil, nil
	}
	listed, err := encodeMirrorIDs(keys)
	if err != nil {
		return nil, err
	}
	err = s.mirrorWrite(rootID, fmt.Sprintf("store: advance tracker mirror %d forge credit", rootID), func(tx *sql.Tx) error {
		issues = nil
		var moved []string
		for _, b := range boards {
			if all {
				moved = append(moved, b.BoardID)
				continue
			}
			groups := b.Regroup.statuses
			if groups == "" {
				groups = "{}"
			}
			n := len(issues)
			err := scanMirrorRows(tx, mirrorForgeCreditIssuesSQL, []any{rootID, b.BoardID, groups, listed}, func(r rowScanner) error {
				var id string
				err := r.Scan(&id)
				issues = append(issues, id)
				return err
			})
			if err != nil {
				return fmt.Errorf("board %s: %w", b.BoardID, err)
			}
			if len(issues) > n {
				moved = append(moved, b.BoardID)
			}
		}
		if len(moved) == 0 {
			return nil
		}
		encoded, err := encodeMirrorIDs(moved)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(mirrorBoardsCreditSQL, rootID, encoded); err != nil {
			return fmt.Errorf("advance the totals of the boards whose credit the forge history moved: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return unionSorted(nil, issues), nil
}

// MirrorForgeFailure is a repository rootID's credit reads whose last
// read failed: its path on its host and the failure's sentence.
type MirrorForgeFailure struct {
	Repo, Failure string
}

// mirrorForgeFailuresSQL lists the sources of cr_src whose last read
// failed, by their path.
var mirrorForgeFailuresSQL = `WITH ` + mirrorCreditSourcesSQL(mirrorCreditParams{rules: "1"}) + `
	SELECT s.label, s.failure FROM cr_src s WHERE s.failure <> '' ORDER BY s.label`

// MirrorForgeFailures lists the repositories rootID's credit reads whose
// last read failed (mirrorForgeFailuresSQL): what a screen says beside
// credit that may lack their pull requests' approvals and comments.
func (s *Store) MirrorForgeFailures(rootID int64) ([]MirrorForgeFailure, error) {
	var out []MirrorForgeFailure
	err := scanMirrorRows(s.reader(), mirrorForgeFailuresSQL, []any{rootID}, func(r rowScanner) error {
		var f MirrorForgeFailure
		err := r.Scan(&f.Repo, &f.Failure)
		out = append(out, f)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("store: read the forge failures of tracker mirror %d: %w", rootID, err)
	}
	return out, nil
}
