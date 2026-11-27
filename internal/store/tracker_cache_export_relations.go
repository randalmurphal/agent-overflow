package store

import "strconv"

// The export's thread links and the viewer's logged time (threads,
// worklogs, worklog_days).
//
// What must hold:
//   - threads is the threads of this computer linked to one of the board's
//     tickets, by the board's site: the links of the site
//     (idx_thread_tickets_issue) whose issue the copy's tickets hold, of
//     the threads the Ticket screen lists (owned_threads, the hidden modes
//     left out as ListThreadsForTicket leaves them out). A thread has no
//     title in the export.
//   - worklogs and worklog_days are the logged time and the read days of
//     one person, the board's connection's verified account, the account
//     the dashboard's Mine reads (MirrorExport.Viewer, read in the same
//     snapshot). worklogs holds
//     the time on the board's tickets alone; worklog_days holds every day
//     of that person's the mirror read, whatever it logged. Both start a
//     day before the history window (MirrorExport.Window), so they hold
//     the days the Dashboards screen reads, in any time zone, and no day
//     it does not: a count of them finds a day before the window not read
//     rather than holding a part of a sprint that began before it.
//     The mirror holds the worklogs of the connection's time source alone
//     (its time-source triggers), so neither needs to name the source.
//   - Each fill seeks by its keys: the site's links, the person's days.

// mirrorExportFirstDaySQL is the first day the export's logged time holds.
var mirrorExportFirstDaySQL = `date(CAST(?` + strconv.Itoa(mirrorExportLoggedArg) + ` AS INTEGER) / 1000, 'unixepoch', '-1 day')`

// mirrorExportViewerSQL is the verified account of root ?1's connection.
const mirrorExportViewerSQL = `(SELECT c.verified_account_id FROM tracker_cache_roots r
	  CROSS JOIN tracker_connections c ON c.id = r.connection_id WHERE r.id = CAST(?1 AS INTEGER))`

// mirrorExportRelationFills are the fills of threads, worklogs and
// worklog_days (mirrorExportFills). The export has a relation named
// threads, so these name the file's threads only through its
// owned_threads view, whose body reads its own schema's tables.
func mirrorExportRelationFills() []mirrorExportFill {
	return []mirrorExportFill{
		{relation: "threads", name: "threads", needs: []string{"tickets"}, sql: `INSERT INTO main.threads(id, ticket, origin, linked_at, created_at, archived, last_turn_at)
		  SELECT th.id, tt.issue_id, tt.origin, tt.linked_at, th.created_at, th.archived,
		    ` + newestTurnSQL("th.id", "nt.started_at") + `
		  FROM tracker_cache_roots r
		  CROSS JOIN tracker_connections c ON c.id = r.connection_id
		  CROSS JOIN thread_tickets tt ON tt.kind = c.kind AND tt.site = c.site
		  CROSS JOIN main.tickets k ON k.id = tt.issue_id
		  CROSS JOIN owned_threads th ON th.id = tt.thread_id
		  WHERE r.id = CAST(?1 AS INTEGER) AND ` + hiddenThreadModesSQL("th.mode")},
		{relation: "worklogs", name: "worklogs", needs: []string{"tickets"}, sql: `INSERT INTO main.worklogs(id, ticket, day, seconds)
		  SELECT w.worklog_id, w.issue_id, w.day, w.spent_seconds
		  FROM tracker_cache_worklogs w
		  CROSS JOIN main.tickets k ON k.id = w.issue_id
		  WHERE w.root_id = CAST(?1 AS INTEGER) AND w.author_id = ` + mirrorExportViewerSQL + ` AND w.day >= ` + mirrorExportFirstDaySQL},
		{relation: "worklog_days", name: "worklog_days", sql: `INSERT INTO main.worklog_days(day)
		  SELECT d.day FROM tracker_cache_worklog_days d
		  WHERE d.root_id = CAST(?1 AS INTEGER) AND d.author_id = ` + mirrorExportViewerSQL + ` AND d.day >= ` + mirrorExportFirstDaySQL},
	}
}
