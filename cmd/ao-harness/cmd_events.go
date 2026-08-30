package main

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"agent-overflow/internal/harnessclient"
)

const defaultAwaitTimeout = 15 * time.Second
const defaultEventFileMaxBytes int64 = 64 << 20
const ringReplayCursor = 0
const sinceNow = "now"

// stringList is a repeatable string flag used by event and perf commands.
type stringList []string

func (c *stringList) String() string     { return strings.Join(*c, ",") }
func (c *stringList) Set(v string) error { *c = append(*c, v); return nil }

var eventsSubcommands = commandNames(eventsCommandDescriptors())

func runEvents(e *env, args []string) error {
	if done, err := groupHelp(e, "events", args, eventsSubcommands...); done {
		return err
	}
	if len(args) == 0 {
		return usagef("events needs a subcommand: %s", strings.Join(eventsSubcommands, ", "))
	}
	switch args[0] {
	case "tail":
		return eventsTail(e, args[1:])
	case "await":
		return eventsAwait(e, args[1:])
	case "count":
		return eventsCount(e, args[1:])
	case "channels":
		return eventsChannels(e, args[1:])
	default:
		return usagef("unknown events subcommand %q (want %s)", args[0], strings.Join(eventsSubcommands, ", "))
	}
}

func optionalWhere(expr string) (*whereMatcher, error) {
	if expr == "" {
		return nil, nil
	}
	parsed, err := parseWhere(expr)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}

func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// subscribeAndReplay narrows the connection and pulls the retained ring.
func subscribeAndReplay(ctx context.Context, client *harnessclient.Client, channels []string, history bool) error {
	if len(channels) > 0 {
		if err := client.Subscribe(ctx, channels...); err != nil {
			return err
		}
	}
	if !history {
		return nil
	}
	if len(channels) == 0 {
		channels = knownChannels()
	}
	cursors := make(map[string]uint64, len(channels))
	for _, channel := range channels {
		cursors[channel] = ringReplayCursor
	}
	return client.Replay(ctx, cursors)
}

func (e *env) warnUnknownChannels(channels []string) {
	known := knownChannels()
	for _, channel := range channels {
		if slices.Contains(known, channel) {
			continue
		}
		fmt.Fprintf(e.stderr,
			"ao-harness: %q is not a registered event channel; it may still carry traffic (the harness emits onto caller-named channels), but check the spelling — `ao-harness events channels %s` lists the near ones\n",
			channel, channelHint(channel))
	}
}

func channelHint(channel string) string {
	if prefix, _, ok := strings.Cut(channel, ":"); ok && prefix != "" {
		return prefix
	}
	return channel
}

func parseSince(since string, history bool) (wantHistory bool, minSeq uint64, err error) {
	since = strings.TrimSpace(since)
	if history {
		if since != "" && since != sinceNow {
			return false, 0, usagef("--history and --since %s ask for different windows; pass one", since)
		}
		return true, 0, nil
	}
	switch since {
	case "", sinceNow:
		return false, 0, nil
	}
	seq, convErr := strconv.ParseUint(since, 10, 64)
	if convErr != nil {
		return false, 0, usagef("--since takes `now` or a server seq (got %q)", since)
	}
	return true, seq, nil
}
