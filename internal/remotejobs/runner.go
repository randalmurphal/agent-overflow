package remotejobs

import (
	"context"
	"errors"
	"io"

	"agent-overflow/internal/procutil"
)

// Outcome is what a runner learned from one command. Leftovers reports that
// processes remained in the command's group after the command itself exited:
// they were stopped, and the receipt says so, but the exit code stays the
// command's own.
type Outcome = procutil.GroupOutcome

type Run func(context.Context, string, []string, io.Writer) (Outcome, error)

// ProcessRunner runs each command through procutil.RunGroup, which owns the
// process group and the output pipe. Environment belongs to the destination; a
// requesting frontend or agent never supplies credentials or environment
// overrides.
func ProcessRunner(environment func() []string) Run {
	return func(ctx context.Context, cwd string, argv []string, output io.Writer) (Outcome, error) {
		outcome, err := procutil.RunGroup(ctx, procutil.GroupCommand{
			Dir: cwd, Env: environment(), Argv: argv, Output: output,
		})
		return outcome, errors.Join(err, outcome.PipeErr)
	}
}
