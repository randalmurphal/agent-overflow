package store

import (
	"database/sql/driver"
	"fmt"

	"modernc.org/sqlite"

	"agent-overflow/internal/stringsx"
)

// find_fold(text) is text folded by the find rule (stringsx.FoldForFind),
// so a search's SQL matches what the find bar paints. SQLite's LOWER and
// LIKE fold ASCII only. Registered on the driver before any connection
// opens; NULL stays NULL.
func init() {
	sqlite.MustRegisterDeterministicScalarFunction("find_fold", 1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
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
