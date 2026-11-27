package store

import "agent-overflow/internal/slicesx"

// This file stands in for the support rule of Credit change 1, which has
// not landed: MirrorSupportRule, WithSupport, supportArg and
// mirrorSupportSQL with change 1's names and signatures. Its rule names a
// row support by board or issue type; change 1's labels column is not on
// this branch. Change 1 defines each, so a merge with it does not build
// until this file is deleted.

// MirrorSupportRule names a project's support tickets of one tracker
// site: those of an issue type (by ID), with a label (by name) or on a
// board (by ID) it lists.
type MirrorSupportRule struct {
	Types, Labels, Boards []string
}

// mirrorSupport is the JSON object mirrorSupportSQL reads.
type mirrorSupport struct {
	Boards []string `json:"boards"`
	Types  []string `json:"types"`
	Labels []string `json:"labels"`
}

// WithSupport is r with rule as its support rule; a rule that names
// nothing names no row.
func (r MirrorRegroup) WithSupport(rule MirrorSupportRule) (MirrorRegroup, error) {
	r.support = ""
	if len(rule.Types)+len(rule.Labels)+len(rule.Boards) == 0 {
		return r, nil
	}
	encoded, err := encodeMirrorJSON(mirrorSupport{Boards: slicesx.OrEmpty(rule.Boards), Types: slicesx.OrEmpty(rule.Types),
		Labels: slicesx.OrEmpty(rule.Labels)})
	if err != nil {
		return MirrorRegroup{}, err
	}
	r.support = encoded
	return r, nil
}

// supportArg is the support rule as a statement binds it: NULL for none.
func (r MirrorRegroup) supportArg() any {
	if r.support == "" {
		return nil
	}
	return r.support
}

// mirrorSupportSQL holds for a row b of board, with its issue i, that is a
// support ticket by the rule the JSON object rule binds (NULL for none):
// it has no estimate and the rule lists its board or its type.
func mirrorSupportSQL(rule, board, estimate string) string {
	return `(` + estimate + ` IS NULL AND ` + rule + ` IS NOT NULL AND (` +
		board + ` IN (SELECT value FROM json_each(` + rule + `, '$.boards'))
		 OR i.type_id IN (SELECT value FROM json_each(` + rule + `, '$.types'))))`
}
