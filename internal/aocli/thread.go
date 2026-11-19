package aocli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

// threadCommand is `agent-overflow thread send`: a script outside the app
// messaging a thread on this computer. Its caller is not a session, so it
// reaches the running backend through the owner's local control file rather
// than a session credential. The backend badges the message as an outside
// send and quotes it as data (SendExternalThreadMessage).
func threadCommand(args []string, root string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		if len(args) == 0 {
			_ = writeOutput(stderr, threadUsage)
			return exitError
		}
		if err := writeOutput(stdout, threadUsage); err != nil {
			return operationalError(stderr, err)
		}
		return exitOK
	}
	if args[0] != "send" {
		_ = writeOutput(stderr, fmt.Sprintf("agent-overflow thread: unknown command %q\n", args[0])+threadUsage)
		return exitError
	}
	flags := flag.NewFlagSet("agent-overflow thread send", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configRoot := flags.String("config-root", root, "override the app data root")
	wait := flags.Duration("wait", 0, "wait for backend startup (e.g. 30s)")
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			if writeErr := writeOutput(stdout, threadUsage); writeErr != nil {
				return operationalError(stderr, writeErr)
			}
			return exitOK
		}
		_ = writeOutput(stderr, fmt.Sprintf("%s: %v\n", flags.Name(), err)+threadUsage)
		return exitError
	}
	if flags.NArg() != 2 {
		_ = writeOutput(stderr, fmt.Sprintf("%s: want a thread id and the text, got %d arguments\n", flags.Name(), flags.NArg())+threadUsage)
		return exitError
	}
	threadID, text := strings.TrimSpace(flags.Arg(0)), flags.Arg(1)
	if threadID == "" || strings.TrimSpace(text) == "" {
		return operationalError(stderr, errors.New("the thread id and the text must not be empty"))
	}
	if *wait < 0 || *wait > 2*time.Minute {
		return operationalError(stderr, errors.New("wait must be between 0s and 2m"))
	}
	if testing.Testing() && *configRoot == "" {
		return operationalError(stderr, errors.New("tests must supply an isolated config root for local control"))
	}
	resolvedRoot, err := resolveConfigRoot(*configRoot)
	if err != nil {
		return operationalError(stderr, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second+*wait)
	defer cancel()
	client, err := connectLocalControl(ctx, resolvedRoot, *wait)
	if err != nil {
		return operationalError(stderr, err)
	}
	defer client.Close()
	var sendID string
	if err := client.Call(ctx, "SendExternalThreadMessage", &sendID, threadID, text); err != nil {
		return operationalError(stderr, err)
	}
	if err := writeOutput(stdout, fmt.Sprintf("Queued for thread %s as %s.\n", threadID, sendID)); err != nil {
		return operationalError(stderr, err)
	}
	return exitOK
}
