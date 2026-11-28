package store

import "strconv"

// Credit from the forge: the one place a mirror root reads the forge pull
// request history (forge_history.go), as review actions of the credit
// fragment (mirrorCreditRowsSQL). What must hold:
//   - A root's credit reads the sources its connection uses
//     (forge_history_uses) whole, and of the parent of each that is a
//     fork, the pull requests whose head is that fork (cr_src, cr_heads),
//     as a ticket's Work does (ForgeHistoryForWork).
//   - A pull request is a ticket's when it names the ticket's key
//     (forge_history_keys). Each approval on it is an "approved" and each
//     comment a "commented" of the person the forge account is: the one
//     a person paired it with (tracker_forge_pairings), else the one it
//     is matched to by name (tracker_cache_forge_people), at the act's
//     time. An account that is nobody's is one review row of its own,
//     listed by the forge's name for it (unmatched) and counted nothing;
//     one paired as no person has no row.
//   - A rule that counts neither approvals nor comments reads no forge
//     act, and no source keeps its tickets unread.
//   - Under a rule that counts either, a ticket's reviews are read only
//     when every source covers it (forgeCreditCoversSQL); else the ticket
//     has one review row not counted, naming the first such repository
//     and why (forgeCreditUnreadSQL).

// mirrorCreditSourcesSQL is the CTEs cr_heads and cr_src of the forge
// sources root ?1's credit reads, none without credit rules: cr_heads
// gives, for each fork the connection uses, its parent's source and the
// fork's own head, which names its pull requests there; cr_src is each
// source once with what coverage reads of it, every set when the
// connection uses it itself, so each of its pull requests counts.
func mirrorCreditSourcesSQL(p mirrorCreditParams) string {
	uses := `tracker_cache_roots r
	      CROSS JOIN forge_history_uses u ON u.connection_id = r.connection_id`
	return `cr_heads AS MATERIALIZED (
	  SELECT par.id AS source_id, o.own_head AS head FROM ` + uses + `
	      CROSS JOIN forge_history_sources o ON o.id = u.source_id AND o.parent_key <> ''
	      CROSS JOIN forge_history_sources par ON par.repo_key = o.parent_key
	   WHERE r.id = CAST(?1 AS INTEGER) AND ` + p.rules + ` IS NOT NULL),
	  cr_src AS MATERIALIZED (
	  SELECT s.id, s.host, s.namespace || '/' || s.repo AS label, s.reach, s.acts_owed, s.failure, s.good_at, s.viewer, max(x.every) AS every
	  FROM (SELECT u.source_id AS id, 1 AS every FROM ` + uses + ` WHERE r.id = CAST(?1 AS INTEGER) AND ` + p.rules + ` IS NOT NULL
	    UNION ALL SELECT h.source_id, 0 FROM cr_heads h) x
	  CROSS JOIN forge_history_sources s ON s.id = x.id
	  GROUP BY s.id)`
}

// forgeCreditCoversSQL holds for a source (alias s: a row of cr_src, or
// of forge_history_sources) that has read every act credit counts of a
// ticket created at created (the tracker's Unix ms) whose acts count up
// to hi, of a sprint that is closed or not:
//   - it has read every update back to ForgeHistoryClockMargin before the
//     ticket's creation, or to the start of the window it keeps (reach 0):
//     a pull request names a key only once the ticket exists, and what an
//     older pull request held before the window is not kept;
//   - its last read left no act of a pull request unread (acts_owed);
//   - and its last read landed, or, for a closed sprint, one landed
//     ForgeHistoryClockMargin after the sprint's end: while a source
//     fails, a sprint that ended since its last good read may lack acts.
//     A sprint still open, and a week, count what the source holds; the
//     sentence beside the number names the failure (MirrorForgeFailures).
func forgeCreditCoversSQL(s, created, hi, closed string) string {
	margin := strconv.FormatInt(ForgeHistoryClockMargin.Milliseconds(), 10)
	return `(` + s + `.reach IS NOT NULL AND ` + s + `.acts_owed = 0 AND (` + s + `.reach = 0 OR ` + s + `.reach <= ` + created + ` - ` + margin + `)
	      AND (` + s + `.failure = '' OR NOT ` + closed + ` OR ` + s + `.good_at >= ` + hi + ` + ` + margin + `))`
}

// forgeCreditUnreadSQL is why the reviews of a ticket (created, hi and
// closed as forgeCreditCoversSQL takes them) are not read from the forge:
// the first source of cr_src, by its path, that does not cover it, how
// many more do not, and its failure, or that it is not read yet; NULL
// when every source covers it.
func forgeCreditUnreadSQL(created, hi, closed string) string {
	return `(SELECT 'pull requests of ' || x.label || CASE WHEN x.n = 2 THEN ' and 1 more repository' WHEN x.n > 2 THEN ' and ' || (x.n - 1) || ' more repositories' ELSE '' END
	        || iif(x.failure <> '', ' could not be read: ' || x.failure, ' are not read yet')
	      FROM (SELECT s.label, s.failure, count(*) OVER () AS n FROM cr_src s
	         WHERE NOT ` + forgeCreditCoversSQL("s", created, hi, closed) + ` ORDER BY s.label LIMIT 1) x)`
}

// mirrorRootConnectionSQL is the connection root ?1 mirrors, NULL for a
// detached root: read once a statement.
const mirrorRootConnectionSQL = `(SELECT r.connection_id FROM tracker_cache_roots r WHERE r.id = CAST(?1 AS INTEGER))`

// mirrorCreditForgeActsSQL is the FROM of the forge acts of finished
// ticket f (a row of cr_f): each act a, at or before f's end, of a pull
// request that names f's key in a source s of cr_src, one of a parent
// only when its head is a fork the connection uses, with who its account
// is: the person's pairing of it as p (tracker_forge_pairings), which
// leaves out the acts of an account paired as no person, and the name
// match as m (tracker_cache_forge_people). mirrorCreditForgePersonSQL
// reads the person of the two.
const mirrorCreditForgeActsSQL = `forge_history_keys k
	    CROSS JOIN cr_src s ON s.id = k.source_id
	    CROSS JOIN forge_history_acts a ON a.source_id = k.source_id AND a.number = k.number
	    LEFT JOIN tracker_forge_pairings p ON p.connection_id = ` + mirrorRootConnectionSQL + ` AND p.host = s.host AND p.login = a.login
	    LEFT JOIN tracker_cache_forge_people m ON m.root_id = CAST(?1 AS INTEGER) AND m.host = s.host AND m.login = a.login
	    WHERE k.key = f.issue_key AND a.at <= f.hi AND p.person IS NOT '' AND (s.every OR EXISTS (SELECT 1 FROM forge_history_prs pr
	        CROSS JOIN cr_heads h ON h.source_id = pr.source_id AND h.head = pr.head_repo
	       WHERE pr.source_id = k.source_id AND pr.number = k.number))`

// mirrorCreditForgePersonSQL is the tracker person a forge act of
// mirrorCreditForgeActsSQL counts for, "" for nobody: the pairing's, else
// the name match's.
const mirrorCreditForgePersonSQL = `coalesce(p.person, m.person, '')`

// mirrorCreditUnmatchedSQL names a forge account (login on host, with the
// forge's name for it, "" for none) no tracker person is matched to, as
// a credit row's unmatched lists it: the name and its login there, or the
// login alone.
func mirrorCreditUnmatchedSQL(host, login, name string) string {
	return `iif(` + name + ` <> '', ` + name + ` || ' (' || ` + login + ` || ' on ' || ` + host + ` || ')', ` + login + ` || ' on ' || ` + host + `)`
}

// mirrorCreditUnmatchedNote is the note of a review row of a forge
// account no tracker person is matched to.
const mirrorCreditUnmatchedNote = `no person in the tracker is matched to this forge account`
