//go:build race

package linear

// raceOn reports a build with the race detector, whose instrumentation
// changes what escapes and so what allocates.
const raceOn = true
