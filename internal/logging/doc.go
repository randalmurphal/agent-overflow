// Package logging provides structured NDJSON file logging with size-based rotation.
//
// Each log entry is written as a single JSON line (newline-delimited JSON).
// When the log file exceeds maxBytes, it is rotated with up to 3 backups
// (.1, .2, .3). Rotation is: delete .3, rename .2->.3, .1->.2, current->.1,
// create new current.
package logging
