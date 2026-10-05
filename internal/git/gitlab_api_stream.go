package git

import (
	"context"
	"errors"
	"io"
	"time"
)

// gitlabAPIStderrLimit bounds the diagnostics kept from one streamed
// `glab api` call. glab writes a one-line error there; anything near this
// size is not a message worth keeping.
const gitlabAPIStderrLimit = 64 << 10

// gitlabAPIQuietEnv keeps glab's own chatter out of a streamed call.
// GLAB_CHECK_UPDATE=false skips the post-command release check, whose
// network-failure path prints to stdout, after the response body. glab
// parses both values with strconv.ParseBool and prints a warning to
// stdout for a value it cannot parse, so they must stay "false".
// GLAB_DEBUG_HTTP=false keeps a user's request dump out of the stderr a
// caller may show.
var gitlabAPIQuietEnv = []string{"GLAB_CHECK_UPDATE=false", "GLAB_DEBUG_HTTP=false"}

// StreamGitLabAPI runs `glab api ARGS...` with the response body streamed
// into dst and returns glab's exit code and bounded stderr.
//
// The call fails once more than limit bytes arrive; an error dst returns
// is wrapped, so errors.Is sees it. A missing glab wraps
// exec.ErrNotFound. A non-zero exit is not an error: the caller reads
// the exit code and stderr, and on an HTTP error glab has already copied
// the forge's error body into dst. ctx's deadline bounds the command;
// without one the package's interactive default applies. The isolated
// boot's forge CLI policy decides what runs for glab.
func (c *Core) StreamGitLabAPI(ctx context.Context, args []string, dst io.Writer, limit int64) (exitCode int, stderr string, err error) {
	if ctx == nil {
		return 0, "", errors.New("glab api: nil context")
	}
	if dst == nil || limit <= 0 {
		return 0, "", errors.New("glab api: a stream needs a destination and a positive limit")
	}
	var timeout time.Duration
	if deadline, ok := ctx.Deadline(); ok {
		timeout = time.Until(deadline)
		if timeout <= 0 {
			return 0, "", context.DeadlineExceeded
		}
	}
	result, err := c.runSpec(commandSpec{
		binary:      "glab",
		ctx:         ctx,
		args:        append([]string{"api"}, args...),
		timeout:     timeout,
		output:      dst,
		outputLimit: limit,
		maxBytes:    gitlabAPIStderrLimit,
		extraEnv:    gitlabAPIQuietEnv,
	})
	return result.exitCode, result.stderr, err
}
