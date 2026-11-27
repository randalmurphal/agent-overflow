package trackerdash

// What a board's tiles (trackerapp/board_tiles.go) read of a board's
// widgets without an evaluation: the built-in dashboard's widgets, and
// the unit a widget's numbers are in, which a limit is set in.

// Builtin is the built-in dashboard of a board of kind k and its metrics
// by name.
func Builtin(k Kind) (Dashboard, map[string]Metric) {
	return builtin(k)
}

// ValueUnit is the unit m's numbers are in on a board whose estimates are
// estimateUnit (store.MirrorEstimate*): tickets, days, points or seconds,
// as an answer names it (Tried.Unit).
func ValueUnit(m Metric, estimateUnit int) string {
	return valueUnit(m, estimatesOf(estimateUnit))
}
