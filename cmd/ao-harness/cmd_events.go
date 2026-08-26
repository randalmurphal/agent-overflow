package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"time"

	"agent-overflow/internal/harnessclient"
)

// defaultAwaitTimeout matches the TS client's waitForEvent budget.
const defaultAwaitTimeout = 15 * time.Second

// ringReplayCursor is the per-channel cursor a history-carrying read
// requests: zero means "everything the server still holds", which is what
// makes a one-shot CLI able to see an event that fired a moment before it
// connected. A long-lived test client does not need this because it was
// already attached; a command invoked from a shell always is late.
const ringReplayCursor = 0

// sinceNow is the `--since` value that means "nothing that already
// happened" — the default for `await`, and the reason it is a default is
// a guaranteed false positive it replaced. `await` used to pull the whole
// replay ring and settle on the OLDEST match in it. Every invocation is a
// fresh connection, so the client-side "a wait consumes its match" rule
// never carried across one: `events await --channel provider:turn_completed`
// returned a turn that finished ten minutes ago, instantly, forever.
const sinceNow = "now"

// stringList is a repeatable string flag: --channel here, --meter in
// `perf start`. One type because the parsing is the parsing — the flag's
// name is what says what the strings mean.
type stringList []string

func (c *stringList) String() string     { return strings.Join(*c, ",") }
func (c *stringList) Set(v string) error { *c = append(*c, v); return nil }

var eventsSubcommands = []string{"tail", "await", "count", "channels"}

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

// eventsTail streams events until interrupted or until --timeout. Text
// output is two lines per event; -o json is NDJSON, because a stream has
// no closing bracket.
func eventsTail(e *env, args []string) error {
	flags := e.newFlagSet("events tail")
	var channels stringList
	flags.Var(&channels, "channel", "only this channel (repeatable; default: every channel this peer may see)")
	where := flags.String("where", "", "print only events matching dotted.path=value")
	timeout := flags.Duration("timeout", 0, "stop after this long (0 = until interrupted)")
	history := flags.Bool("history", true, "replay the server's retained ring for the named channels before streaming")
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return usagef("events tail takes no positional arguments (got %v)", rest)
	}
	matcher, err := optionalWhere(*where)
	if err != nil {
		return err
	}
	e.warnUnknownChannels(channels)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if *timeout > 0 {
		timed, cancel := context.WithTimeout(ctx, *timeout)
		defer cancel()
		ctx = timed
	}

	return e.withClient(ctx, func(client *harnessclient.Client, _ target, _ harnessclient.Bootstrap) error {
		printed := make(chan error, 1)
		cancelListen := client.Listen(func(ev harnessclient.Event) {
			if matcher != nil && (ev.Gap || !matcher.match(ev.Data)) {
				return
			}
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

// printEvent is the text form: `seq channel` on one line and the payload
// indented on the next. One line held both, which meant a fixed-width
// prefix ate the terminal and the PAYLOAD — the only varying half, and
// the reason anyone is reading — was what got truncated.
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
	if _, err := fmt.Fprintf(e.stdout, "%d %s%s\n", ev.Seq, ev.Channel, marker); err != nil {
		return err
	}
	if len(ev.Data) == 0 {
		return nil
	}
	_, err := fmt.Fprintf(e.stdout, "  %s\n", truncate(string(ev.Data), 400))
	return err
}

// eventsAwait blocks until one matching event arrives, prints it, and
// exits non-zero on timeout — the shape a shell script waits on.
//
// The `--since` default is the whole correctness of this command. See
// sinceNow: a history-first await over a replay ring answers with
// something that happened before the caller existed, which is not a wait
// at all. When history IS asked for, the scan runs NEWEST-first, because
// a caller reaching into the ring on purpose wants the latest occurrence,
// not the oldest one still retained.
func eventsAwait(e *env, args []string) error {
	flags := e.newFlagSet("events await --channel <name>")
	channel := flags.String("channel", "", "channel to wait on")
	where := flags.String("where", "", "match a field: dotted.path=value")
	timeout := flags.Duration("timeout", defaultAwaitTimeout, "how long to wait")
	since := flags.String("since", sinceNow, "`now` (only events pushed after this command subscribes) or a server seq to consider events after")
	history := flags.Bool("history", false, "consider every event the server still holds in its replay ring (equivalent to --since 0)")
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
	matcher, err := optionalWhere(*where)
	if err != nil {
		return err
	}
	wantHistory, minSeq, err := parseSince(*since, *history)
	if err != nil {
		return err
	}
	e.warnUnknownChannels(stringList{*channel})

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	return e.withClient(ctx, func(client *harnessclient.Client, _ target, _ harnessclient.Bootstrap) error {
		if err := subscribeAndReplay(ctx, client, stringList{*channel}, wantHistory); err != nil {
			return err
		}
		event, err := client.WaitForEventOpts(ctx, *channel, harnessclient.WaitOptions{
			Newest:      wantHistory,
			MinSeq:      minSeq,
			SkipHistory: !wantHistory,
		}, func(ev harnessclient.Event) bool {
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

// parseSince turns --since / --history into "do we pull the ring" plus a
// sequence floor. They are one question asked two ways, and answering it
// once here is what keeps the two flags from disagreeing.
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
	// A seq floor is a statement about the ring, so the ring is what gets
	// pulled — asking for "after seq 40" while refusing history would only
	// ever match live traffic, which is what --since now already means.
	return true, seq, nil
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
	timeout := flags.Duration("timeout", 30*time.Second, "overall budget for the subscribe, replay and settle")
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return usagef("events count takes no positional arguments (got %v)", rest)
	}
	matcher, err := optionalWhere(*where)
	if err != nil {
		return err
	}
	e.warnUnknownChannels(channels)

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
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

// eventsChannels lists what there is to subscribe to. It needs no
// instance: the registry is a compile-time table (internal/eventchan),
// so the answer is the same for every instance this binary was built
// alongside.
func eventsChannels(e *env, args []string) error {
	flags := e.newFlagSet("events channels [pattern]")
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	if len(rest) > 1 {
		return usagef("events channels takes at most one pattern (got %v)", rest)
	}
	names := knownChannels()
	if len(rest) == 1 {
		names = filterContains(names, rest[0])
	}
	if e.jsonOutput() {
		return e.writeJSON(names)
	}
	if len(names) == 0 {
		e.printf("no channel matches %q\n", rest[0])
		return nil
	}
	for _, name := range names {
		e.printf("%s\n", name)
	}
	return nil
}

// warnUnknownChannels says so when a caller names a channel the registry
// has never heard of — the typo that otherwise produces a perfectly
// successful wait for nothing.
//
// A WARNING, not a refusal, and that is a decision rather than caution.
// The harness publishes onto caller-named channels through an explicit
// escape hatch (`HarnessEmit`, the replayer), so the registry is a list
// of what the BACKEND emits, not of every name that can legitimately
// carry traffic — `harness:mock` is in it, a bundle's own replay channel
// need not be. Refusing would break the escape hatch to catch a typo.
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

// channelHint is the prefix worth grepping for: the part before the
// colon, which is how every registered name is grouped.
func channelHint(channel string) string {
	if prefix, _, ok := strings.Cut(channel, ":"); ok && prefix != "" {
		return prefix
	}
	return channel
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
