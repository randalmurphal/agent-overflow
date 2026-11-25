package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// The pane's reads of the mirror. Each is bounded: by a literal LIMIT with
// a more flag, by the bound of what the writer accepts, or by refusing a
// longer list of IDs than mirrorMaxLookup. A detached root, a board the
// mirror does not show and an issue it does not keep read as absent.

const (
	// mirrorSectionPageSize is how many rows one section page holds.
	mirrorSectionPageSize = 100
	// mirrorMaxLookup bounds the IDs one read by ID takes.
	mirrorMaxLookup = 500
	// mirrorIssueRowsPage bounds one ListMirrorIssueRows read.
	mirrorIssueRowsPage = 100
	// mirrorLinkedPage bounds one ListMirrorLinkedIssues read.
	mirrorLinkedPage = 200
)

// mirrorCardColumns are a card's columns, of tracker_cache_issues aliased
// i, in the order scanMirrorCard reads them.
const mirrorCardColumns = `i.issue_id, i.issue_key, i.project_id, i.type_id, i.type_level, i.status_id, i.assignee_id,
	i.priority_id, i.parent_id, i.created_at, i.updated_at, i.resolved_at, i.status_since, i.status_final_for >= i.updated_at, i.title, i.data,
	(SELECT json_group_array(m.sprint_id) FROM tracker_cache_issue_sprints m WHERE m.root_id = i.root_id AND m.issue_id = i.issue_id)`

func scanMirrorCard(row rowScanner, extra ...any) (MirrorCard, error) {
	var c MirrorCard
	var sprints []byte
	dest := append([]any{&c.IssueID, &c.Key, &c.ProjectID, &c.TypeID, &c.TypeLevel, &c.StatusID, &c.AssigneeID,
		&c.PriorityID, &c.ParentID, &c.CreatedAt, &c.UpdatedAt, &c.ResolvedAt, &c.StatusSince, &c.StatusFinal, &c.Title, &c.Data, &sprints}, extra...)
	if err := row.Scan(dest...); err != nil {
		return MirrorCard{}, err
	}
	if len(sprints) > 2 {
		if err := json.Unmarshal(sprints, &c.Sprints); err != nil {
			return MirrorCard{}, fmt.Errorf("decode card %s sprints: %w", c.IssueID, err)
		}
		// Sorted here, not by the aggregate, which would sort in a
		// temporary b-tree per card.
		slices.Sort(c.Sprints)
	}
	return c, nil
}

// GetMirrorBoard reads the structure of a board the mirror shows: the
// board, its columns with their statuses, its sprints and its lifetime.
// The catalog is read by ID (GetMirrorCatalog). Its statements are bounded
// by the structure's limits, and it reads in one snapshot like every
// mirror read of more than one statement (mirrorReadSnapshot).
func (s *Store) GetMirrorBoard(rootID int64, boardID string) (st MirrorStructure, ok bool, err error) {
	label := fmt.Sprintf("read tracker mirror %d board %s", rootID, boardID)
	st, err = mirrorReadSnapshot(s, label, func(tx sqlQueryer) (MirrorStructure, error) {
		var out MirrorStructure
		b := &out.Board
		err := tx.QueryRow(`SELECT b.board_id, b.project_id, b.name, b.plans_sprints, b.estimate_unit, b.estimate_field, b.data, b.lifetime
			FROM tracker_cache_boards b WHERE b.root_id = CAST(?1 AS INTEGER) AND b.board_id = ?2 AND `+mirrorBoardShown(intParam("?1"), "?2"), rootID, boardID).Scan(
			&b.ID, &b.ProjectID, &b.Name, &b.PlansSprints, &b.EstimateUnit, &b.EstimateField, &b.Data, &out.Lifetime)
		if errors.Is(err, sql.ErrNoRows) {
			return MirrorStructure{}, nil
		}
		if err != nil {
			return MirrorStructure{}, err
		}
		ok = true
		if out.Columns, err = readMirrorColumnsTx(tx, rootID, boardID); err != nil {
			return MirrorStructure{}, err
		}
		out.Sprints, err = readMirrorSprintsTx(tx, rootID, boardID)
		return out, err
	})
	if err != nil {
		return MirrorStructure{}, false, err
	}
	return st, ok, nil
}

func readMirrorColumnsTx(tx sqlQueryer, rootID int64, boardID string) ([]MirrorColumn, error) {
	names, err := queryIDs(tx, `SELECT name FROM tracker_cache_board_columns WHERE root_id = CAST(? AS INTEGER) AND board_id = ?
		ORDER BY position LIMIT `+strconv.Itoa(mirrorMaxColumns), rootID, boardID)
	if err != nil {
		return nil, fmt.Errorf("read columns: %w", err)
	}
	columns := make([]MirrorColumn, len(names))
	for i, name := range names {
		columns[i].Name = name
	}
	err = scanMirrorRows(tx, `SELECT position, status_id FROM tracker_cache_board_column_statuses WHERE root_id = CAST(? AS INTEGER) AND board_id = ?
		ORDER BY position, rank LIMIT `+strconv.Itoa(mirrorMaxColumnStatuses), []any{rootID, boardID}, func(r rowScanner) error {
		var position int
		var status string
		if err := r.Scan(&position, &status); err != nil {
			return err
		}
		if position < len(columns) {
			columns[position].Statuses = append(columns[position].Statuses, status)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read column statuses: %w", err)
	}
	return columns, nil
}

func readMirrorSprintsTx(tx sqlQueryer, rootID int64, boardID string) ([]MirrorSprint, error) {
	var sprints []MirrorSprint
	err := scanMirrorRows(tx, `SELECT sprint_id, origin_board_id, name, goal, state, start_at, end_at, completed_at
		FROM tracker_cache_sprints WHERE root_id = CAST(? AS INTEGER) AND board_id = ?
		ORDER BY position, sprint_id LIMIT `+strconv.Itoa(mirrorMaxSprints), []any{rootID, boardID}, func(r rowScanner) error {
		var sp MirrorSprint
		err := r.Scan(&sp.ID, &sp.OriginBoardID, &sp.Name, &sp.Goal, &sp.State, &sp.StartAt, &sp.EndAt, &sp.CompletedAt)
		sprints = append(sprints, sp)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("read sprints: %w", err)
	}
	return sprints, nil
}

// GetMirrorCatalog reads the users and statuses of rootID named by ID, in
// one statement, each sorted by ID and read once; an ID the mirror does
// not hold is left out.
func (s *Store) GetMirrorCatalog(rootID int64, users, statuses []string) (catalog MirrorCatalog, err error) {
	if len(users) > mirrorMaxLookup || len(statuses) > mirrorMaxLookup {
		return MirrorCatalog{}, fmt.Errorf("%w: %d users and %d statuses, over %d", ErrInvalidTrackerCache, len(users), len(statuses), mirrorMaxLookup)
	}
	if err := readMirrorCatalogTx(s.reader(), rootID, users, statuses, &catalog); err != nil {
		return MirrorCatalog{}, fmt.Errorf("store: read tracker mirror %d catalog: %w", rootID, err)
	}
	sortMirrorCatalog(&catalog)
	return catalog, nil
}

// mirrorCatalogByIDSQL reads users ?2 and statuses ?3 (JSON lists of IDs)
// of root ?1, each list driving its arm by key: a user's group is NULL, a
// status's data.
var mirrorCatalogByIDSQL = `SELECT u.user_id, u.name, u.data, NULL FROM json_each(?2) l
	CROSS JOIN tracker_cache_users u ON u.root_id = CAST(?1 AS INTEGER) AND u.user_id = l.value
	WHERE ` + mirrorRootLiveSQL + `
	UNION ALL
	SELECT st.status_id, st.name, NULL, st.status_group FROM json_each(?3) l
	CROSS JOIN tracker_cache_statuses st ON st.root_id = CAST(?1 AS INTEGER) AND st.status_id = l.value
	WHERE ` + mirrorRootLiveSQL

// readMirrorCatalogTx appends to catalog the users and statuses of rootID
// named by ID, at most mirrorMaxLookup of each, in one statement.
func readMirrorCatalogTx(tx sqlQueryer, rootID int64, users, statuses []string, catalog *MirrorCatalog) error {
	if len(users)+len(statuses) == 0 {
		return nil
	}
	userList, err := encodeMirrorIDs(users)
	if err != nil {
		return err
	}
	statusList, err := encodeMirrorIDs(statuses)
	if err != nil {
		return err
	}
	err = scanMirrorRows(tx, mirrorCatalogByIDSQL, []any{rootID, userList, statusList}, func(r rowScanner) error {
		var id, name string
		var data []byte
		var group sql.NullInt64
		if err := r.Scan(&id, &name, &data, &group); err != nil {
			return err
		}
		if group.Valid {
			catalog.Statuses = append(catalog.Statuses, MirrorStatus{ID: id, Name: name, Group: int(group.Int64)})
		} else {
			catalog.Users = append(catalog.Users, MirrorUser{ID: id, Name: name, Data: data})
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("users and statuses: %w", err)
	}
	return nil
}

// sortMirrorCatalog sorts a catalog's users and statuses by ID, here
// rather than in the statements: the ID list drives each, so an ORDER BY
// would sort in a temporary b-tree.
func sortMirrorCatalog(catalog *MirrorCatalog) {
	slices.SortFunc(catalog.Users, func(a, b MirrorUser) int { return strings.Compare(a.ID, b.ID) })
	slices.SortFunc(catalog.Statuses, func(a, b MirrorStatus) int { return strings.Compare(a.ID, b.ID) })
}

// MirrorNames are what a set of cards name, read in their snapshot: the
// users and statuses (Catalog, sorted by ID) and the cards of the parents
// the mirror keeps (sorted by issue ID). An ID the mirror does not hold is
// left out.
type MirrorNames struct {
	Catalog MirrorCatalog
	Parents []MirrorCard
}

// readMirrorNamesTx reads what the cards of rows name, and the statuses
// also, in tx's snapshot, mirrorMaxLookup IDs of a kind per statement: a
// board view's pages and columns name at most a few, a page one each.
func readMirrorNamesTx(tx sqlQueryer, rootID int64, rows [][]MirrorRow, also []string) (MirrorNames, error) {
	var names MirrorNames
	users, statuses, parents := map[string]bool{}, map[string]bool{}, map[string]bool{}
	var userIDs, statusIDs, parentIDs []string
	add := func(seen map[string]bool, ids *[]string, id string) {
		if id != "" && !seen[id] {
			seen[id] = true
			*ids = append(*ids, id)
		}
	}
	for _, id := range also {
		add(statuses, &statusIDs, id)
	}
	for _, page := range rows {
		for _, row := range page {
			add(users, &userIDs, row.Card.AssigneeID)
			add(statuses, &statusIDs, row.Card.StatusID)
			add(parents, &parentIDs, row.Card.ParentID)
		}
	}
	for len(userIDs)+len(statusIDs) > 0 {
		u, st := userIDs[:min(len(userIDs), mirrorMaxLookup)], statusIDs[:min(len(statusIDs), mirrorMaxLookup)]
		userIDs, statusIDs = userIDs[len(u):], statusIDs[len(st):]
		if err := readMirrorCatalogTx(tx, rootID, u, st, &names.Catalog); err != nil {
			return MirrorNames{}, err
		}
	}
	sortMirrorCatalog(&names.Catalog)
	for batch := range slices.Chunk(parentIDs, mirrorMaxLookup) {
		cards, err := readMirrorCardsTx(tx, rootID, batch)
		if err != nil {
			return MirrorNames{}, fmt.Errorf("parents: %w", err)
		}
		names.Parents = append(names.Parents, cards...)
	}
	slices.SortFunc(names.Parents, func(a, b MirrorCard) int { return strings.Compare(a.IssueID, b.IssueID) })
	return names, nil
}

// mirrorReadSnapshot runs a mirror read of more than one statement, or of
// one whose cost grows with a board or a root, in one snapshot through
// historyReadSnapshot: it waits for a history read slot before it takes a
// read connection, so mirror reads in flight never hold the connection
// the pool keeps for single-statement lookups. A read's error is reported
// with label. The mirror's reads take no caller context: none of their
// callers holds one, and the wait ends when a read holding a slot ends.
// Every read transaction of the mirror opens here
// (TestMirrorReadsTakeAHistoryReadSlot).
func mirrorReadSnapshot[T any](s *Store, label string, read func(sqlQueryer) (T, error)) (T, error) {
	return historyReadSnapshot(context.Background(), s, label, func(q sqlQueryer) (T, error) {
		value, err := read(q)
		if err != nil {
			return value, fmt.Errorf("store: %s: %w", label, err)
		}
		return value, nil
	})
}

// scanMirrorRows runs query and calls scan for each row.
func scanMirrorRows(q sqlQueryer, query string, args []any, scan func(rowScanner) error) error {
	rows, err := q.Query(query, args...)
	if err != nil {
		return err
	}
	for rows.Next() {
		if err := scan(rows); err != nil {
			return errors.Join(err, rows.Close())
		}
	}
	return errors.Join(rows.Err(), rows.Close())
}

// mirrorCardsSQL reads the cards of the kept issues among a JSON list of
// IDs, probing each by key.
var mirrorCardsSQL = `SELECT ` + mirrorCardColumns + ` FROM json_each(?2) l
	CROSS JOIN tracker_cache_issues i ON i.root_id = CAST(?1 AS INTEGER) AND i.issue_id = l.value
	WHERE ` + mirrorIssueKept("i.root_id", "i.issue_id")

// ListMirrorCards reads the cards of issueIDs the mirror keeps, sorted by
// issue ID; any other issue is left out.
func (s *Store) ListMirrorCards(rootID int64, issueIDs []string) ([]MirrorCard, error) {
	if len(issueIDs) == 0 {
		return nil, nil
	}
	if len(issueIDs) > mirrorMaxLookup {
		return nil, fmt.Errorf("%w: %d IDs, over %d", ErrInvalidTrackerCache, len(issueIDs), mirrorMaxLookup)
	}
	cards, err := readMirrorCardsTx(s.reader(), rootID, issueIDs)
	if err != nil {
		return nil, fmt.Errorf("store: read tracker mirror %d cards: %w", rootID, err)
	}
	slices.SortFunc(cards, func(a, b MirrorCard) int { return strings.Compare(a.IssueID, b.IssueID) })
	return cards, nil
}

// readMirrorCardsTx reads the cards of the kept issues among issueIDs, in
// no order, in one statement.
func readMirrorCardsTx(q sqlQueryer, rootID int64, issueIDs []string) ([]MirrorCard, error) {
	encoded, err := encodeMirrorIDs(issueIDs)
	if err != nil {
		return nil, err
	}
	cards := make([]MirrorCard, 0, len(issueIDs))
	err = scanMirrorRows(q, mirrorCardsSQL, []any{rootID, encoded}, func(r rowScanner) error {
		card, err := scanMirrorCard(r)
		cards = append(cards, card)
		return err
	})
	return cards, err
}

// MirrorIssueRow is an issue's row on one board, without its card.
type MirrorIssueRow struct {
	BoardID, OrderKey string
	Estimate          *float64
}

// mirrorIssueRowsSQL reads a page of an issue's rows on the boards the
// mirror shows, by board ID after ?3, from the primary key; a row read no
// later than its board's estimate definition last changed has no estimate.
var mirrorIssueRowsSQL = `SELECT b.board_id, b.order_key, ` + mirrorRowEstimate(`(SELECT r.estimate_reset_at FROM tracker_cache_boards r
	  WHERE r.root_id = b.root_id AND r.board_id = b.board_id)`) + ` FROM tracker_cache_board_issues b
	WHERE b.root_id = CAST(?1 AS INTEGER) AND b.issue_id = ?2 AND b.board_id > ?3 AND ` + mirrorBoardShown("b.root_id", "b.board_id") + `
	ORDER BY b.board_id LIMIT ` + strconv.Itoa(mirrorIssueRowsPage+1)

// ListMirrorIssueRows reads up to mirrorIssueRowsPage of one issue's rows
// on the boards of rootID the mirror shows, by board ID after afterBoard
// (empty for the first); more reports that rows follow the last.
func (s *Store) ListMirrorIssueRows(rootID int64, issueID, afterBoard string) (rows []MirrorIssueRow, more bool, err error) {
	err = scanMirrorRows(s.reader(), mirrorIssueRowsSQL, []any{rootID, issueID, afterBoard}, func(r rowScanner) error {
		var row MirrorIssueRow
		err := r.Scan(&row.BoardID, &row.OrderKey, &row.Estimate)
		rows = append(rows, row)
		return err
	})
	if err != nil {
		return nil, false, fmt.Errorf("store: read tracker mirror %d issue %s rows: %w", rootID, issueID, err)
	}
	if len(rows) > mirrorIssueRowsPage {
		return rows[:mirrorIssueRowsPage], true, nil
	}
	return rows, false, nil
}

// MirrorTicketRow is an issue's row on a board the mirror shows, with the
// board's estimate unit (MirrorEstimateNone, Points or Time): what an
// open ticket shows of its estimates.
type MirrorTicketRow struct {
	IssueID, BoardID string
	Estimate         *float64
	EstimateUnit     int
}

// mirrorTicketRowsSQL reads the rows of the issues of the JSON list ?2 on
// the boards the mirror shows, by issue and then board: each issue a seek
// of the primary key, each row its board by its unique key. A row read no
// later than its board's estimate definition last changed has no estimate.
var mirrorTicketRowsSQL = `SELECT b.issue_id, b.board_id, ` + mirrorRowEstimate("r.estimate_reset_at") + `, r.estimate_unit
	FROM tracker_cache_board_issues b
	JOIN tracker_cache_boards r ON r.root_id = b.root_id AND r.board_id = b.board_id
	WHERE b.root_id = ` + intParam("?1") + ` AND b.issue_id IN (SELECT value FROM json_each(?2)) AND ` + mirrorBoardShown("b.root_id", "b.board_id") + `
	ORDER BY b.issue_id, b.board_id`

// ListMirrorTicketRows reads, in one statement, the rows of up to
// MirrorMaxBodies issues of rootID on the boards the mirror shows, by
// issue and then board: what the views of that many open tickets, built
// together, show of their estimates. An issue holds at most one row per
// board some project lists.
func (s *Store) ListMirrorTicketRows(rootID int64, issueIDs []string) ([]MirrorTicketRow, error) {
	if len(issueIDs) == 0 {
		return nil, nil
	}
	if len(issueIDs) > MirrorMaxBodies {
		return nil, fmt.Errorf("%w: %d issues, over %d", ErrInvalidTrackerCache, len(issueIDs), MirrorMaxBodies)
	}
	encoded, err := encodeMirrorIDs(issueIDs)
	if err != nil {
		return nil, err
	}
	var rows []MirrorTicketRow
	err = scanMirrorRows(s.reader(), mirrorTicketRowsSQL, []any{rootID, encoded}, func(r rowScanner) error {
		var row MirrorTicketRow
		err := r.Scan(&row.IssueID, &row.BoardID, &row.Estimate, &row.EstimateUnit)
		rows = append(rows, row)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("store: read tracker mirror %d rows of %d tickets: %w", rootID, len(issueIDs), err)
	}
	return rows, nil
}

// mirrorOffBoardLinkedSQL reads a page of the issues the links of a root
// name (mirrorLinksSQL, the keep rule's own) that have no row on a board
// of the JSON list ?3 the mirror shows, by issue ID after ?2: the
// tickets' issue index in order, and per ticket a walk of the issue's own
// rows, each tested against the list (built once; the unary plus keeps
// the planner from probing the issue's key once per listed board).
var mirrorOffBoardLinkedSQL = `SELECT DISTINCT link_ticket.issue_id FROM ` + mirrorLinksSQL(intParam("?1")) + `
	AND link_ticket.issue_id > ?2
	AND NOT EXISTS (SELECT 1 FROM tracker_cache_board_issues on_board
	                 WHERE on_board.root_id = CAST(?1 AS INTEGER) AND on_board.issue_id = link_ticket.issue_id
	                   AND +on_board.board_id IN (SELECT value FROM json_each(?3))
	                   AND ` + mirrorBoardShown("on_board.root_id", "on_board.board_id") + `)
	ORDER BY link_ticket.issue_id LIMIT ` + strconv.Itoa(mirrorLinkedPage+1)

// ListMirrorOffBoardLinkedIssues reads up to mirrorLinkedPage of the
// distinct issues that threads this computer owns link through rootID's
// connection and that no board of polled the mirror shows has a row of:
// polled are the boards whose reads run now and keep their rows' cards
// fresh, so the caller reads the cards of the rest. Sorted, after after
// (empty for the first); more reports that issues follow the last. A
// detached root has none. A page reads the links in issue order until it
// has a page, so it reads at most every link of the connection, each with
// its issue's rows on the boards, whatever the number of polled boards,
// and it waits for a history read slot (mirrorReadSnapshot).
func (s *Store) ListMirrorOffBoardLinkedIssues(rootID int64, after string, polled []string) (issueIDs []string, more bool, err error) {
	boards, err := encodeMirrorIDs(polled)
	if err != nil {
		return nil, false, err
	}
	issueIDs, err = mirrorReadSnapshot(s, fmt.Sprintf("list tracker mirror %d linked issues", rootID), func(q sqlQueryer) ([]string, error) {
		return queryIDs(q, mirrorOffBoardLinkedSQL, rootID, after, boards)
	})
	if err != nil {
		return nil, false, err
	}
	if len(issueIDs) > mirrorLinkedPage {
		return issueIDs[:mirrorLinkedPage], true, nil
	}
	return issueIDs, false, nil
}
