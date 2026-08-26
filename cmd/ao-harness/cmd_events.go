package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"slices"
	"strings"
	"time"

	"agent-overflow/internal/harnessclient"
)

// defaultAwaitTimeout matches the TS client's waitForEvent budget.
const defaultAwaitTimeout = 15 * time.Second

// ringReplayCursor is the per-channel cursor `tail` and `await` request:
// zero means "everything the server still holds", which is what makes a
// one-shot CLI able to see an event that fired a moment before it
// connected. A long-lived test client does not need this because it was
// already attached; a command invoked from a shell always is late.
const ringReplayCursor = 0

// stringList is a repeatable string flag: --channel here, --meter in
// `perf start`. One type because the parsing is the parsing — the flag's
// name is what says what the strings mean.
type stringList []string

func (c *stringList) String() string     { return strings.Join(*c, ",") }
func (c *stringList) Set(v string) error { *c = append(*c, v); return nil }

func runEvents(e *env, args []string) error {
	if len(args) == 0 {
		return usagef("events needs a subcommand: tail, await, count")
	}
	switch args[0] {
	case "tail":
		return eventsTail(e, args[1:])
	case "await":
		return eventsAwait(e, args[1:])
	case "count":
		return eventsCount(e, args[1:])
	default:
		return usagef("unknown events subcommand %q (want tail, await, count)", args[0])
	}
}

// eventsTail streams events until interrupted. Text output is one line
// per event; -o json is NDJSON, because a stream has no closing bracket.
func eventsTail(e *env, args []string) error {
	flags := e.newFlagSet("events tail")
	var channels stringList
	flags.Var(&channels, "channel", "only this channel (repeatable; default: every channel this peer may see)")
	history := flags.Bool("history", true, "replay the server's retained ring for the named channels before streaming")
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return usagef("events tail takes no positional arguments (got %v)", rest)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	return e.withClient(ctx, func(client *harnessclient.Client, _ target, _ harnessclient.Bootstrap) error {
		printed := make(chan error, 1)
		cancelListen := client.Listen(func(ev harnessclient.Event) {
			if err := e.printEvent(ev); err != nil {
				select {
				case printed <- err:
				default:
				}
			}
		})
		defer cancelListen()

		if err := subscribeAndReplay(ctx, client, channels, *history); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-client.Done():
			return fmt.Errorf("the instance closed the connection")
		case err := <-printed:
			return err
		}
	})
}

func (e *env) printEvent(ev harnessclient.Event) error {
	if e.jsonOutput() {
		line, err := json.Marshal(ev)
		if err != nil {
			return fmt.Errorf("encode event: %w", err)
		}
		_, err = fmt.Fprintln(e.stdout, string(line))
		return err
	}
	marker := ""
	if ev.Gap {
		marker = " [gap]"
	}
	_, err := fmt.Fprintf(e.stdout, "%-6d %s%s %s\n", ev.Seq, ev.Channel, marker, truncate(string(ev.Data), 160))
	return err
}

// eventsAwait blocks until one matching event arrives, prints it, and
// exits non-zero on timeout — the shape a shell script waits on.
func eventsAwait(e *env, args []string) error {
	flags := e.newFlagSet("events await")
	channel := flags.String("channel", "", "channel to wait on")
	where := flags.String("where", "", "match a field: dotted.path=value")
	timeout := flags.Duration("timeout", defaultAwaitTimeout, "how long to wait")
	history := flags.Bool("history", true, "consider events the server still holds in its replay ring")
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	if *channel == "" && len(rest) == 1 {
		*channel = rest[0]
		rest = nil
	}
	if len(rest) != 0 {
		return usagef("events await takes no positional arguments beyond a channel (got %v)", rest)
	}
	if *channel == "" {
		return usagef("events await needs --channel <name>")
	}
	var matcher *whereMatcher
	if *where != "" {
		parsed, err := parseWhere(*where)
		if err != nil {
			return err
		}
		matcher = &parsed
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	return e.withClient(ctx, func(client *harnessclient.Client, _ target, _ harnessclient.Bootstrap) error {
		if err := subscribeAndReplay(ctx, client, stringList{*channel}, *history); err != nil {
			return err
		}
		event, err := client.WaitForEvent(ctx, *channel, func(ev harnessclient.Event) bool {
			// A gap marker is the server saying "your cursor fell outside
			// my ring"; it is not traffic on the channel and must not
			// satisfy a wait for traffic.
			if ev.Gap {
				return false
			}
			return matcher == nil || matcher.match(ev.Data)
		})
		if err != nil {
			return err
		}
		return e.printEvent(event)
	})
}

// eventsCount reports how many events the ring holds for a channel. It
// is the absence half of an assertion: proving something did NOT happen
// cannot be done by waiting for it.
func eventsCount(e *env, args []string) error {
	flags := e.newFlagSet("events count")
	var channels stringList
	flags.Var(&channels, "channel", "count this channel (repeatable; default: every channel replayed)")
	where := flags.String("where", "", "count only events matching dotted.path=value")
	settle := flags.Duration("settle", 250*time.Millisecond, "how long to keep receiving before counting")
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return usagef("events count takes no positional arguments (got %v)", rest)
	}
	var matcher *whereMatcher
	if *where != "" {
		parsed, err := parseWhere(*where)
		if err != nil {
			return err
		}
		matcher = &parsed
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	return e.withClient(ctx, func(client *harnessclient.Client, _ target, _ harnessclient.Bootstrap) error {
		if err := subscribeAndReplay(ctx, client, channels, true); err != nil {
			return err
		}
		// The replay marker only promises the backlog is sent, not that
		// this process has parsed it; a short settle is the difference
		// between counting the ring and counting a race.
		select {
		case <-ctx.Done():
		case <-time.After(*settle):
		}

		counts := map[string]int{}
		for _, ev := range client.Events() {
			if ev.Gap {
				continue
			}
			if matcher != nil && !matcher.match(ev.Data) {
				continue
			}
			counts[ev.Channel]++
		}
		if len(channels) > 0 {
			// A named channel with no events is a zero, not an absence:
			// that is the answer the caller asked for.
			for _, channel := range channels {
				if _, ok := counts[channel]; !ok {
					counts[channel] = 0
				}
			}
		}
		if e.jsonOutput() {
			return e.writeJSON(counts)
		}
		rows := make([][]string, 0, len(counts))
		for _, channel := range sortedKeys(counts) {
			rows = append(rows, []string{channel, fmt.Sprint(counts[channel])})
		}
		if len(rows) == 0 {
			e.printf("no events\n")
			return nil
		}
		return e.table([]string{"CHANNEL", "COUNT"}, rows)
	})
}

// subscribeAndReplay narrows the connection to the named channels (when
// any were named) and pulls the server's retained ring for them.
func subscribeAndReplay(ctx context.Context, client *harnessclient.Client, channels []string, history bool) error {
	if len(channels) > 0 {
		if err := client.Subscribe(ctx, channels...); err != nil {
			return err
		}
	}
	if !history || len(channels) == 0 {
		// Replay is per-channel by cursor; with no channel named there is
		// nothing to ask for, and a live tail is what the caller wanted.
		return nil
	}
	cursors := make(map[string]uint64, len(channels))
	for _, channel := range channels {
		cursors[channel] = ringReplayCursor
	}
	return client.Replay(ctx, cursors)
}

func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
