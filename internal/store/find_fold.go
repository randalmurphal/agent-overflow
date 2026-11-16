package store

import (
	"database/sql/driver"
	"fmt"

	"modernc.org/sqlite"
)

// The SQL functions of thread find's candidate filter (find_candidate.go).
// find_candidate(kind, summary, meta, tool_name, payload) is a row's
// candidate text, payload the part of its payload the row shows or NULL;
// the v150 triggers write it and the backfill fills it. find_loose(text) is
// the loose form a query and a streamed reasoning chunk are matched and
// appended in. A pane's thread find matches a query's loose form in the
// stored candidate text with instr, and the frontend decides which
// candidates hold a match by the find rule; the cross-thread search keeps
// SQLite's LOWER and LIKE. Both are Go calls per row, made when a row is
// written, never per row of a read. Registered on the driver before any
// connection opens, so a trigger that calls them runs on every connection
// of the process; NULL text stays NULL.
//
// VolatileArgs is what keeps a TEXT argument's byte length: without it the
// driver reads TEXT as a C string (libc.GoString), cut at the first NUL.
// The arguments are then views of SQLite's memory, valid only during the
// call; findLoose may return one (it returns unchanged text as it is),
// which the driver copies (sqlite3_result_text, SQLITE_TRANSIENT) before
// the call returns to SQLite.
func init() {
	sqlite.MustRegisterFunction("find_loose", &sqlite.FunctionImpl{
		NArgs:         1,
		Deterministic: true,
		VolatileArgs:  true,
		Scalar: func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
			text, ok, err := findTextArg("find_loose", args[0])
			if err != nil || !ok {
				return nil, err
			}
			return findLoose(text), nil
		},
	})
	sqlite.MustRegisterFunction("find_candidate", &sqlite.FunctionImpl{
		NArgs:         5,
		Deterministic: true,
		VolatileArgs:  true,
		Scalar: func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
			var text [4]string
			for i := range text {
				value, _, err := findTextArg("find_candidate", args[i])
				if err != nil {
					return nil, err
				}
				text[i] = value
			}
			payload, ok, err := findTextArg("find_candidate", args[4])
			if err != nil {
				return nil, err
			}
			var held *string
			if ok {
				held = &payload
			}
			return findCandidateText(text[0], text[1], text[2], text[3], held), nil
		},
	})
}

// findTextArg reads a text or blob argument; ok is false for NULL.
func findTextArg(name string, value driver.Value) (string, bool, error) {
	switch v := value.(type) {
	case nil:
		return "", false, nil
	case string:
		return v, true, nil
	case []byte:
		return string(v), true, nil
	case int64, float64:
		return fmt.Sprint(v), true, nil
	default:
		return "", false, fmt.Errorf("%s: %T is not text", name, v)
	}
}

// findMatchSQL is the condition that a row's candidate text holds the
// loose needle bound at needle, or is findAnyCandidate. A row the backfill
// has not reached yet computes its candidate text from its own row; one
// among them whose candidate text reads its payload, which the read does
// not load, is taken as a candidate.
func findMatchSQL(alias, needle string) string {
	candidate := `find_candidate(` + alias + `kind, ` + alias + `summary, ` + alias + `meta, ` + alias + `tool_name, NULL)`
	return `(CASE WHEN ` + alias + `find_text IS NOT NULL THEN ` + findHoldsSQL(alias+`find_text`, needle) + `
	      WHEN ` + alias + `payload_id IS NOT NULL
	       AND ` + alias + `kind IN ('thinking', 'compaction_reasoning', 'tool_call', 'tool_completion') THEN 1
	      ELSE ` + findHoldsSQL(candidate, needle) + ` END)`
}

// findHoldsSQL is the condition that the candidate text expression text
// holds needle or is findAnyCandidate.
func findHoldsSQL(text, needle string) string {
	return `(instr(` + text + `, ` + needle + `) > 0 OR ` + text + ` = ` + sqlTextLiteral(findAnyCandidate) + `)`
}

// findNeedle is the trimmed query's loose form, the bound value of
// findMatchSQL; empty when the query holds no letter or digit, which no
// candidate text can narrow.
func findNeedle(trimmed string) string {
	return findLoose(trimmed)
}
