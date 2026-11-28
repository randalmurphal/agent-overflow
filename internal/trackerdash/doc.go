// Package trackerdash evaluates a board's dashboard: metrics, each one
// SELECT over the board's export (store.ExportMirrorBoard), reduced by the
// app into the numbers, bars, lines, tables and ticket lists a dashboard
// draws, and the rows behind each of them.
//
// A metric's SELECT returns contribution rows: one per ticket counted, at
// least the ticket, with named columns from a fixed set (Column*). The app
// owns everything after the rows: the range, its previous range and its
// trend, narrowing to a person or an epic, the reduction, and the drill.
// So a number and the rows it opens are one evaluation's (Drill evaluates
// the metric again and answers the cell's number with its rows), and How
// this is counted is rendered from the metric's definition.
//
// The evaluation is a sandbox for SQL an agent writes: the metrics of the
// widgets a project keeps and its agents propose (Own, Try) run in it as
// the app's own (Builtins) do. What must hold:
//
//   - The connection's only database is the board's export. It is a new
//     private in-memory database on this package's own driver, filled by
//     the store with the app file attached read-only, which the store
//     detaches before it returns. Before any metric runs, the gate
//     (gateConnection) proves the connection lists no database but main
//     and temp, sets the attached-database limit to 0, turns query_only
//     on, lowers the limits and installs the authorizer; any failure
//     closes the connection and fails the evaluation.
//   - The authorizer allows a SELECT, reads of the export's relations in
//     main, and a list of deterministic functions (allowedFunctions); it
//     denies everything else, recursion, pragmas, attach and the catalog
//     included, and records what it denied for the widget's sentence.
//   - What a count asks the copy for, and what How this is counted names,
//     is what the plan of the statement it runs reads (plan.go): the
//     wrapped statement is planned on an empty session of the copy's own
//     schema, and its program's cursors and column reads are the
//     relations and the columns filled only when asked
//     (store.MirrorExportColumns) it reads, the columns of a USING or
//     NATURAL join included, and nothing a join SQLite drops, a shadowing
//     CTE or an unread * names. The reading is exact for the program
//     SQLite emits, and that program can hold reads it never runs (a
//     grouped join whose only use SQLite removed, a CASE WHEN 0 branch),
//     which the count asks for too. Every count reads tickets, which the
//     wrapper looks each row's ticket up in. A program the reader cannot
//     attribute refuses the widget.
//   - A metric's SQL is one statement (oneStatement): it cannot close the
//     wrapper's parenthesis or add a statement. The wrapper produces the
//     whole result in SQLite's first step, which a canceled context
//     interrupts, and bounded_rows charges each row's encoding against the
//     byte budget; a row cap sits inside the wrapper.
//   - Time is bound by the host (:now, and :since, where the range's
//     periods begin, so a metric of the range reads only rows it can
//     place) and the app's functions (days,
//     working_days, day); SQLite's date functions and random are denied,
//     so a metric evaluated twice on the same rows answers the same rows.
//   - Nothing is retained: every evaluation and drill opens, fills, runs
//     and closes its own connection.
package trackerdash
