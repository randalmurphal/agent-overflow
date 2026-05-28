package orphanreaper

import (
	"fmt"
	"strconv"
	"strings"
)

// The control protocol is newline-delimited text on the pipe: one
// `watch <pgid>` or `release <pgid>` command per line. Lines stay whole
// because the parent is the pipe's sole writer and serializes every send
// under a mutex (see Client.send); being far under PIPE_BUF, they
// wouldn't tear even if that weren't so.

type cmdKind int

const (
	cmdWatch cmdKind = iota
	cmdRelease
)

type command struct {
	kind cmdKind
	pgid int
}

func formatWatch(pgid int) string   { return fmt.Sprintf("watch %d\n", pgid) }
func formatRelease(pgid int) string { return fmt.Sprintf("release %d\n", pgid) }

// parseCommand decodes a single control line. It rejects malformed input
// (wrong arity, non-numeric or non-positive pgid, unknown verb) so a
// garbled line is logged and skipped rather than silently mutating the
// watched set.
func parseCommand(line string) (command, error) {
	fields := strings.Fields(line)
	if len(fields) != 2 {
		return command{}, fmt.Errorf("orphanreaper: malformed control line %q", line)
	}
	pgid, err := strconv.Atoi(fields[1])
	if err != nil {
		return command{}, fmt.Errorf("orphanreaper: non-numeric pgid in %q", line)
	}
	if pgid <= 1 {
		// pgid 0 targets the caller's own group and 1 is init — neither is
		// ever a legitimate provider group to watch.
		return command{}, fmt.Errorf("orphanreaper: refusing unsafe pgid %d", pgid)
	}
	switch fields[0] {
	case "watch":
		return command{kind: cmdWatch, pgid: pgid}, nil
	case "release":
		return command{kind: cmdRelease, pgid: pgid}, nil
	default:
		return command{}, fmt.Errorf("orphanreaper: unknown control verb %q", fields[0])
	}
}
