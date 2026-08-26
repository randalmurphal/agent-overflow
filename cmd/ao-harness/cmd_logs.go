package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"

	"agent-overflow/internal/harnessclient"
	"agent-overflow/internal/uitrace"
)

// defaultLogLines is what `logs <stream>` prints without -n. Enough to
// see the last thing that happened, short enough to read.
const defaultLogLines = 50

// logStreams are the three files worth tailing from a harness run.
// backend is the detached process's console; the other two are the
// frontend's diagnostic channels.
var logStreams = []string{"backend", "frontend-errors", "ui-trace"}

func runLogs(e *env, args []string) error {
	flags := e.newFlagSet("logs")
	follow := flags.Bool("f", false, "keep printing lines as they are appended")
	lines := flags.Int("n", defaultLogLines, "print this many trailing lines first (0 for none)")
	path := flags.Bool("path", false, "print the file path instead of its contents")
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return usagef("logs needs exactly one stream: %s", strings.Join(logStreams, ", "))
	}
	stream := rest[0]
	if !slices.Contains(logStreams, stream) {
		return usagef("unknown log stream %q (want %s)", stream, strings.Join(logStreams, ", "))
	}
	if *lines < 0 {
		return usagef("-n must not be negative")
	}

	file, err := e.logPath(stream)
	if err != nil {
		return err
	}
	if *path {
		e.printf("%s\n", file)
		return nil
	}

	if *lines > 0 {
		tail, err := harnessclient.TailFile(file, *lines)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) && *follow {
				// Following a file that does not exist yet is legitimate:
				// ui-trace only appears once the frontend traces something.
				e.printf("(%s does not exist yet)\n", file)
			} else {
				return err
			}
		}
		for _, line := range tail {
			e.printf("%s\n", line)
		}
	}
	if !*follow {
		return nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return harnessclient.FollowFile(ctx, file, func(line string) {
		e.printf("%s\n", line)
	})
}

// logPath resolves a stream to a file. The running instance is asked
// first, because it is the only thing that knows for certain what it
// opened; a stopped instance falls back to the on-disk layout, since a
// log is most interesting exactly when the process that wrote it is
// gone.
func (e *env) logPath(stream string) (string, error) {
	var info harnessclient.HarnessInfo
	dataDir := ""
	ctx := context.Background()
	client, t, _, err := e.attach(ctx)
	if err == nil {
		defer client.Close()
		info, err = client.Info(ctx)
		if err != nil {
			return "", err
		}
		dataDir = info.DataDir
	} else {
		if t.DataDir == "" {
			return "", err
		}
		dataDir = t.DataDir
	}

	switch stream {
	case "backend":
		// `up` redirects the detached backend's stderr here. An instance
		// started some other way (make harness, a terminal) writes to its
		// own console instead, so this file may not exist.
		return filepath.Join(dataDir, logDirName, backendStderrLog), nil
	case "frontend-errors":
		if info.FrontendErrorsPath != "" {
			return info.FrontendErrorsPath, nil
		}
		return filepath.Join(dataDir, uitrace.DirName, uitrace.ErrorFileName), nil
	case "ui-trace":
		if info.UITracePath != "" {
			return info.UITracePath, nil
		}
		return filepath.Join(dataDir, uitrace.DirName, uitrace.FileName), nil
	default:
		return "", fmt.Errorf("unknown log stream %q", stream)
	}
}
