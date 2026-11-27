package store

import (
	"database/sql"
	"fmt"
	"strconv"
)

// The sources of an issue's acts (tracker_cache_issue_acts.source).
const (
	// MirrorActComment is a comment: when it was made, by whom.
	MirrorActComment = 1
	// MirrorActWorklog is a worklog: when the work started, by whom, and
	// the seconds spent.
	MirrorActWorklog = 2
)

// MirrorAct is one comment or worklog of an issue as credit counts it:
// its ID within the issue and source, its author's account (Person), when
// it was made (At, Unix ms: a comment's creation, a worklog's start) and a
// worklog's seconds (zero for a comment).
type MirrorAct struct {
	ID      string
	Person  string
	At      int64
	Seconds int64
}

// MirrorIssueActs is the whole of an issue's acts of one source as one
// read found them, and the issue's update time that read returned
// (ReadFor), which its receipt records.
type MirrorIssueActs struct {
	IssueID string
	Source  int
	ReadFor int64
	Acts    []MirrorAct
}

// MirrorActsResult is what ReplaceMirrorActs did with one MirrorIssueActs:
// stored it, found it Superseded by a read that began later, or neither
// (the mirror does not keep the issue).
type MirrorActsResult struct {
	IssueID    string
	Source     int
	Stored     bool
	Superseded bool
}

// mirrorActsKeyRow and mirrorActRow are a MirrorIssueActs and a MirrorAct
// as the acts statements read them from json_each.
type mirrorActsKeyRow struct {
	Issue   string `json:"i"`
	Source  int    `json:"s"`
	ReadFor int64  `json:"f"`
}

type mirrorActRow struct {
	Issue   string `json:"i"`
	Source  int    `json:"s"`
	ID      string `json:"a"`
	Person  string `json:"p"`
	At      int64  `json:"t"`
	Seconds int64  `json:"x"`
}

// The acts statements, of ?1 root, ?2 the read's order time, ?3 the JSON
// list of the issues and sources replaced (mirrorActsKeyRow), ?4 the JSON
// list of their acts (mirrorActRow). Each finds an issue's rows by the
// primary key's prefix.
var (
	// mirrorActsStateSQL reads, for each listed issue and source, whether
	// the mirror keeps the issue and the read order of its receipt (0 for
	// none).
	mirrorActsStateSQL = `SELECT e.value ->> 'i', e.value ->> 's',
	    iif(i.issue_id IS NULL, 0, ` + mirrorIssueKept("i.root_id", "i.issue_id") + `), coalesce(r.read_at, 0)
	  FROM json_each(?3) e
	  LEFT JOIN tracker_cache_issues i ON i.root_id = CAST(?1 AS INTEGER) AND i.issue_id = e.value ->> 'i'
	  LEFT JOIN tracker_cache_issue_act_reads r ON r.root_id = CAST(?1 AS INTEGER) AND r.issue_id = e.value ->> 'i'
	    AND r.source = e.value ->> 's'`
	mirrorActsRemoveSQL = `DELETE FROM tracker_cache_issue_acts
	  WHERE root_id = CAST(?1 AS INTEGER) AND (issue_id, source) IN (SELECT e.value ->> 'i', e.value ->> 's' FROM json_each(?3) e)
	    AND (issue_id, source, act_id) NOT IN (SELECT a.value ->> 'i', a.value ->> 's', a.value ->> 'a' FROM json_each(?4) a)`
	mirrorActsUpsertSQL = `INSERT INTO tracker_cache_issue_acts(root_id, issue_id, source, act_id, person, at, seconds)
	  SELECT ?1, a.value ->> 'i', a.value ->> 's', a.value ->> 'a', a.value ->> 'p', a.value ->> 't', a.value ->> 'x' FROM json_each(?4) a WHERE true
	  ON CONFLICT(root_id, issue_id, source, act_id) DO UPDATE SET person = excluded.person, at = excluded.at, seconds = excluded.seconds
	  WHERE person IS NOT excluded.person OR at IS NOT excluded.at OR seconds IS NOT excluded.seconds`
	mirrorActReadsSQL = `INSERT INTO tracker_cache_issue_act_reads(root_id, issue_id, source, read_for, read_at)
	  SELECT ?1, e.value ->> 'i', e.value ->> 's', e.value ->> 'f', ?2 FROM json_each(?3) e WHERE true
	  ON CONFLICT(root_id, issue_id, source) DO UPDATE SET read_for = excluded.read_for, read_at = excluded.read_at`
)

// ReplaceMirrorActs makes each of issues the whole of its issue's acts of
// its source, with a receipt of the read (begun at read) that found them,
// and returns each one's outcome in input order. An issue the mirror does
// not keep stores nothing; one whose receipt is from a read that began at
// or after read is superseded and changes nothing. The authors' names
// (catalog) are stored as a history read's are. sourceGen is the time
// source generation of the root's connection the read began under
// (TrackerConnection.TimeSourceGen): a call that replaces worklogs under
// another is refused with ErrTrackerCacheSourceChanged, since the change
// deleted the root's worklog acts (migration v177).
//
// It takes at most 2 × mirrorHistoryBatch issues and sources, each once.
// They are written in transactions of at most mirrorMaxBatchChanges acts,
// an issue's acts of a source never split, the catalog in the first; a
// call that fails after a transaction committed returns the outcomes of
// those before it with its error.
func (s *Store) ReplaceMirrorActs(rootID, sourceGen int64, read MirrorRead, issues []MirrorIssueActs, catalog MirrorCatalog) (results []MirrorActsResult, catalogWrite MirrorCatalogWrite, err error) {
	times, err := read.in(rootID)
	if err != nil {
		return nil, MirrorCatalogWrite{}, err
	}
	readAt := times.at
	if readAt <= 0 || len(issues) > 2*mirrorHistoryBatch {
		return nil, MirrorCatalogWrite{}, fmt.Errorf("%w: %d issues' acts, over %d, or no read", ErrInvalidTrackerCache, len(issues), 2*mirrorHistoryBatch)
	}
	if err := catalog.check(); err != nil {
		return nil, MirrorCatalogWrite{}, err
	}
	if err := checkMirrorIssueActs(issues); err != nil {
		return nil, MirrorCatalogWrite{}, err
	}
	results = make([]MirrorActsResult, len(issues))
	for i, in := range issues {
		results[i] = MirrorActsResult{IssueID: in.IssueID, Source: in.Source}
	}
	action := fmt.Sprintf("store: replace tracker mirror %d issue acts", rootID)
	for start, first := 0, true; start < len(issues) || first; first = false {
		end, acts := start, 0
		for end < len(issues) && (end == start || acts+len(issues[end].Acts) <= mirrorMaxBatchChanges) {
			acts += len(issues[end].Acts)
			end++
		}
		committed := results[:start]
		var changed MirrorCatalogWrite
		err = s.mirrorWrite(rootID, action, func(tx *sql.Tx) error {
			var err error
			if first && len(catalog.Users)+len(catalog.Statuses) > 0 {
				if changed, err = upsertMirrorCatalogTx(tx, rootID, catalog, readAt); err != nil {
					return err
				}
			}
			return replaceMirrorActsTx(tx, rootID, sourceGen, readAt, issues[start:end], results[start:end])
		})
		if err != nil {
			return committed, catalogWrite, err
		}
		catalogWrite.Changed = catalogWrite.Changed || changed.Changed
		catalogWrite.Threads = unionSorted(catalogWrite.Threads, changed.Threads)
		start = end
	}
	return results, catalogWrite, nil
}

// checkMirrorIssueActs refuses an input with no issue, a source that is
// neither, no update time, an issue and source twice, or an act with no
// ID or author, no time, negative seconds, a comment with seconds, or an
// ID twice within its issue and source.
func checkMirrorIssueActs(issues []MirrorIssueActs) error {
	type key struct {
		issue  string
		source int
	}
	seen := make(map[key]bool, len(issues))
	for _, in := range issues {
		k := key{in.IssueID, in.Source}
		switch {
		case in.IssueID == "" || in.Source != MirrorActComment && in.Source != MirrorActWorklog || in.ReadFor <= 0:
			return fmt.Errorf("%w: issue %q source %d read for %d", ErrInvalidTrackerCache, in.IssueID, in.Source, in.ReadFor)
		case seen[k]:
			return fmt.Errorf("%w: issue %s source %d twice", ErrInvalidTrackerCache, in.IssueID, in.Source)
		}
		seen[k] = true
		ids := make(map[string]bool, len(in.Acts))
		for _, a := range in.Acts {
			if a.ID == "" || ids[a.ID] || a.Person == "" || a.At <= 0 || a.Seconds < 0 || in.Source == MirrorActComment && a.Seconds != 0 {
				return fmt.Errorf("%w: act %q of issue %s source %d: empty, repeated, or with no author, time or valid seconds",
					ErrInvalidTrackerCache, a.ID, in.IssueID, in.Source)
			}
			ids[a.ID] = true
		}
	}
	return nil
}

// replaceMirrorActsTx writes the admitted of issues in tx and records each
// outcome in results, issues' own.
func replaceMirrorActsTx(tx *sql.Tx, rootID, sourceGen, readAt int64, issues []MirrorIssueActs, results []MirrorActsResult) error {
	if len(issues) == 0 {
		return nil
	}
	index := make(map[string]int, len(issues))
	keys := make([]mirrorActsKeyRow, len(issues))
	worklogs := false
	for i, in := range issues {
		keys[i] = mirrorActsKeyRow{Issue: in.IssueID, Source: in.Source, ReadFor: in.ReadFor}
		index[in.IssueID+"\x00"+strconv.Itoa(in.Source)] = i
		worklogs = worklogs || in.Source == MirrorActWorklog
	}
	if worklogs {
		var current int64
		if err := tx.QueryRow(mirrorWorklogSourceGenSQL, rootID).Scan(&current); err != nil {
			return fmt.Errorf("read the time source generation: %w", err)
		}
		if current != sourceGen {
			return fmt.Errorf("worklogs read under time source generation %d, now %d: %w", sourceGen, current, ErrTrackerCacheSourceChanged)
		}
	}
	listed, err := encodeMirrorJSON(keys)
	if err != nil {
		return err
	}
	admitted := make([]bool, len(issues))
	err = scanMirrorRows(tx, mirrorActsStateSQL, []any{rootID, readAt, listed}, func(r rowScanner) error {
		var issue string
		var source int
		var kept bool
		var storedAt int64
		if err := r.Scan(&issue, &source, &kept, &storedAt); err != nil {
			return err
		}
		i := index[issue+"\x00"+strconv.Itoa(source)]
		switch {
		case !kept:
		case storedAt >= readAt:
			results[i].Superseded = true
		default:
			admitted[i] = true
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("read the issues' acts state: %w", err)
	}
	keys = keys[:0]
	acts := []mirrorActRow{}
	for i, in := range issues {
		if !admitted[i] {
			continue
		}
		keys = append(keys, mirrorActsKeyRow{Issue: in.IssueID, Source: in.Source, ReadFor: in.ReadFor})
		for _, a := range in.Acts {
			acts = append(acts, mirrorActRow{Issue: in.IssueID, Source: in.Source, ID: a.ID, Person: a.Person, At: a.At, Seconds: a.Seconds})
		}
	}
	if len(keys) == 0 {
		return nil
	}
	if listed, err = encodeMirrorJSON(keys); err != nil {
		return err
	}
	encodedActs, err := encodeMirrorJSON(acts)
	if err != nil {
		return err
	}
	args := []any{rootID, readAt, listed, encodedActs}
	for _, step := range []struct {
		what, query string
		args        []any
	}{
		{"remove acts", mirrorActsRemoveSQL, args},
		{"write acts", mirrorActsUpsertSQL, args},
		{"record the reads", mirrorActReadsSQL, args[:3]},
	} {
		if _, err := tx.Exec(step.query, step.args...); err != nil {
			return fmt.Errorf("%s: %w", step.what, err)
		}
	}
	for i := range issues {
		results[i].Stored = admitted[i]
	}
	return nil
}
