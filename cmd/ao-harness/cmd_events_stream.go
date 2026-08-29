package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"

	"agent-overflow/internal/harnessclient"
)

type eventOutputLimitError struct{ limit int64 }

func (e eventOutputLimitError) Error() string {
	return fmt.Sprintf("event output reached --max-bytes %d; use a larger --max-bytes or stop earlier with --timeout", e.limit)
}

type eventOutputWriter struct {
	written int64
	limit   int64
	w       io.Writer
}

func (w *eventOutputWriter) Write(p []byte) (int, error) {
	if w.limit > 0 && w.written+int64(len(p)) > w.limit {
		return 0, eventOutputLimitError{limit: w.limit}
	}
	n, err := w.w.Write(p)
	w.written += int64(n)
	return n, err
}

// eventsTail streams events until interrupted, timeout, or its output limit.
func eventsTail(e *env, args []string) error {
	flags := e.newFlagSet("events tail")
	asJSON := e.bindJSONFlag(flags)
	var channels stringList
	flags.Var(&channels, "channel", "only this channel (repeatable; default: every channel this peer may see)")
	where := flags.String("where", "", "print only events matching dotted.path=value")
	timeout := flags.Duration("timeout", 0, "stop after this long (0 = until interrupted)")
	history := flags.Bool("history", true, "replay the server's retained ring before streaming")
	maxEvents := flags.Int("max-events", 1000, "stop after this many matching events (0 = no limit; --full is an alias)")
	full := flags.Bool("full", false, "stream every matching event without an output budget")
	file := flags.String("file", "", "write the complete event stream here instead of stdout")
	maxBytes := flags.Int64("max-bytes", defaultEventFileMaxBytes, "with --file, stop before writing beyond this many bytes")
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	if *asJSON {
		e.format = "json"
	}
	if len(rest) != 0 {
		return usagef("events tail takes no positional arguments (got %v)", rest)
	}
	if *maxEvents < 0 || *maxBytes < 1 {
		return usagef("--max-events must not be negative and --max-bytes must be positive")
	}
	if *full || *file != "" {
		*maxEvents = 0
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
		output, boundedOutput, outputFile, err := eventOutput(e, *file, *maxBytes)
		if err != nil {
			return err
		}
		if outputFile != nil {
			defer outputFile.Close()
		}
		seen := 0
		printed := make(chan error, 1)
		cancelListen := client.Listen(func(ev harnessclient.Event) {
			if matcher != nil && (ev.Gap || !matcher.match(ev.Data)) {
				return
			}
			if err := e.printEventToWithLimit(output, ev, *file != ""); err != nil {
				select {
				case printed <- err:
				default:
				}
			}
			if !ev.Gap {
				seen++
				if *maxEvents > 0 && seen >= *maxEvents {
					stop()
				}
			}
		})
		defer cancelListen()
		if err := subscribeAndReplay(ctx, client, channels, *history); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
		case <-client.Done():
			return fmt.Errorf("the instance closed the connection")
		case err := <-printed:
			return err
		}
		if outputFile == nil {
			return nil
		}
		if err := outputFile.Sync(); err != nil {
			return fmt.Errorf("flush event output %s: %w", *file, err)
		}
		complete := *timeout == 0
		if e.jsonOutput() {
			return e.writeJSON(map[string]any{"file": *file, "events": seen, "bytes": boundedOutput.written, "full": complete, "complete": complete})
		}
		state := "complete"
		if !complete {
			state = "bounded"
		}
		e.printf("event stream written to %s (%d event(s), %d bytes, %s)\n", *file, seen, boundedOutput.written, state)
		return nil
	})
}

func eventOutput(e *env, path string, maxBytes int64) (io.Writer, *eventOutputWriter, *os.File, error) {
	if path == "" {
		return e.stdout, nil, nil, nil
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("open event output %s: %w", path, err)
	}
	bounded := &eventOutputWriter{limit: maxBytes, w: file}
	return bounded, bounded, file, nil
}

func (e *env) printEvent(ev harnessclient.Event) error { return e.printEventTo(e.stdout, ev) }

func (e *env) printEventTo(output io.Writer, ev harnessclient.Event) error {
	return e.printEventToWithLimit(output, ev, false)
}

func (e *env) printEventToWithLimit(output io.Writer, ev harnessclient.Event, completePayload bool) error {
	if e.jsonOutput() {
		line, err := json.Marshal(ev)
		if err != nil {
			return fmt.Errorf("encode event: %w", err)
		}
		_, err = fmt.Fprintln(output, string(line))
		return err
	}
	marker := ""
	if ev.Gap {
		marker = " [gap]"
	}
	if _, err := fmt.Fprintf(output, "%d %s%s\n", ev.Seq, ev.Channel, marker); err != nil {
		return err
	}
	if len(ev.Data) == 0 {
		return nil
	}
	payload := string(ev.Data)
	if !completePayload {
		payload = truncate(payload, 400)
	}
	_, err := fmt.Fprintf(output, "  %s\n", payload)
	return err
}
