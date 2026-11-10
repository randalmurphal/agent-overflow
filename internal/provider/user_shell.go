package provider

import "time"

// UserShellCommand is a command the person ran from the composer, as a
// provider's model is told about it. Each provider package renders it in the
// form its own CLI records such a command, so the model reads it as something
// the person ran rather than as prose.
type UserShellCommand struct {
	Command string
	// Output is the combined stdout and stderr, already bounded and free of
	// terminal control sequences.
	Output string
	// ExitCode is the command's own, or -1 when a signal or the timeout
	// ended it.
	ExitCode int
	Duration time.Duration
	TimedOut bool
}
