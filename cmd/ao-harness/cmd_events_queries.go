package main

import (
	"context"
	"fmt"
	"time"

	"agent-overflow/internal/harnessclient"
)

func eventsAwait(e *env, args []string) error {
	flags := e.newFlagSet("events await --channel <name>")
	asJSON := e.bindJSONFlag(flags)
	channel := flags.String("channel", "", "channel to wait on")
	where := flags.String("where", "", "match a field: dotted.path=value")
	timeout := flags.Duration("timeout", defaultAwaitTimeout, "how long to wait")
	since := flags.String("since", sinceNow, "`now` or a server seq to consider events after")
	history := flags.Bool("history", false, "consider every event the server still holds in its replay ring")
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	if *asJSON {
		e.format = "json"
	}
	if *channel == "" && len(rest) == 1 {
		*channel, rest = rest[0], nil
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
			Newest: wantHistory, MinSeq: minSeq, SkipHistory: !wantHistory,
		}, func(ev harnessclient.Event) bool {
			return !ev.Gap && (matcher == nil || matcher.match(ev.Data))
		})
		if err != nil {
			return err
		}
		return e.printEvent(event)
	})
}

func eventsCount(e *env, args []string) error {
	flags := e.newFlagSet("events count")
	asJSON := e.bindJSONFlag(flags)
	var channels stringList
	flags.Var(&channels, "channel", "count this channel (repeatable; default: every channel replayed)")
	where := flags.String("where", "", "count only events matching dotted.path=value")
	settle := flags.Duration("settle", 250*time.Millisecond, "how long to keep receiving before counting")
	timeout := flags.Duration("timeout", 30*time.Second, "overall budget for subscribe, replay and settle")
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	if *asJSON {
		e.format = "json"
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
		select {
		case <-ctx.Done():
		case <-time.After(*settle):
		}
		counts := map[string]int{}
		for _, ev := range client.Events() {
			if ev.Gap || (matcher != nil && !matcher.match(ev.Data)) {
				continue
			}
			counts[ev.Channel]++
		}
		for _, channel := range channels {
			if _, ok := counts[channel]; !ok {
				counts[channel] = 0
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
