//go:build race

package transport

// raceEnabled reports a -race build, whose instrumentation allocates on
// paths that otherwise do not, so allocation counts do not apply.
const raceEnabled = true
