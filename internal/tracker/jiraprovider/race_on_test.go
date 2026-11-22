//go:build race

package jiraprovider

// raceOn reports a build with the race detector, whose instrumentation
// changes what escapes and so what allocates.
const raceOn = true
