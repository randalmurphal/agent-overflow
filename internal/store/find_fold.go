package store

import (
	"database/sql/driver"
	"fmt"

	"modernc.org/sqlite"

	"agent-overflow/internal/stringsx"
)

// find_fold(text) is text folded by the find rule (stringsx.FoldForFind),
// so a pane's thread find matches what its bar paints; SQLite's LOWER and
// LIKE fold ASCII only. It is a Go call per row, so only the thread find
// uses it, on rows an index already narrowed to one thread
// (threadFindSelection); the cross-thread search keeps LOWER and LIKE.
// Registered on the driver before any connection opens; NULL stays NULL.
//
// VolatileArgs is what keeps a TEXT argument's byte length: without it the
// driver reads TEXT as a C string (libc.GoString), cut at the first NUL.
// The arguments are then views of SQLite's memory, valid only during the
// call; the result may be one (FoldForFind returns unchanged text as it
// is), which the driver copies (sqlite3_result_text, SQLITE_TRANSIENT)
// before the call returns to SQLite.
func init() {
	sqlite.MustRegisterFunction("find_fold", &sqlite.FunctionImpl{
		NArgs:         1,
		Deterministic: true,
		VolatileArgs:  true,
		Scalar: func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
			switch v := args[0].(type) {
			case nil:
				return nil, nil
			case string:
				return stringsx.FoldForFind(v), nil
			case []byte:
				return stringsx.FoldForFind(string(v)), nil
			case int64, float64:
				return v, nil
			default:
				return nil, fmt.Errorf("find_fold: %T is not text", v)
			}
		},
	})
}

// findMatchSQL is the condition that column holds the needle a findPattern
// gives, by the find rule.
func findMatchSQL(column string) string {
	return "instr(find_fold(" + column + "), " + boundText + ") > 0"
}

// findPattern is the trimmed query folded by the find rule, the bound
// value of findMatchSQL.
func findPattern(trimmed string) string {
	return stringsx.FoldForFind(trimmed)
}
