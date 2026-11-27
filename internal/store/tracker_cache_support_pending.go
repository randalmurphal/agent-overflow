package store

// This file stands in for the support rule of Credit change 1, which has
// not landed: supportArg and mirrorSupportSQL with change 1's names and
// signatures, naming no row support. Change 1 defines both, so a merge
// with it does not build until this file is deleted.

// supportArg is the support rule as a statement binds it: NULL for none.
func (r MirrorRegroup) supportArg() any { return nil }

// mirrorSupportSQL holds for a row b of board, with its issue i, that is a
// support ticket by the rule the JSON object rule binds (NULL for none).
func mirrorSupportSQL(rule, board, estimate string) string {
	return `(` + estimate + ` IS NULL AND ` + rule + ` IS NOT NULL AND ` + board + ` IS NULL)`
}
