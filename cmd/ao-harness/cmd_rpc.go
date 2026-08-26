package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"agent-overflow/internal/harnessclient"
)

func runRPC(e *env, args []string) error {
	flags := e.newFlagSet("rpc")
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		return usagef("rpc needs a method name, e.g. `rpc HarnessListMocks`")
	}
	method := rest[0]
	params := make([]json.RawMessage, 0, len(rest)-1)
	for i, arg := range rest[1:] {
		if !json.Valid([]byte(arg)) {
			// A bare word is the mistake everyone makes first. Say what a
			// JSON value looks like rather than letting the server answer
			// bad_params.
			return usagef("argument %d (%q) is not a JSON value; strings need quotes, e.g. '\"thread-id\"'", i+1, arg)
		}
		params = append(params, json.RawMessage(arg))
	}
	ctx := context.Background()
	return e.withClient(ctx, func(client *harnessclient.Client, _ target, _ harnessclient.Bootstrap) error {
		result, err := client.CallRaw(ctx, method, params)
		if err != nil {
			return err
		}
		return e.writeRawJSON(result)
	})
}

func runSeed(e *env, args []string) error {
	flags := e.newFlagSet("seed")
	file := flags.String("f", "", "seed spec JSON file, or - for stdin")
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	if len(rest) > 1 {
		return usagef("seed takes at most one spec file (got %v)", rest)
	}
	source := *file
	if source == "" && len(rest) == 1 {
		source = rest[0]
	}
	if source == "" {
		return usagef("seed needs a spec: `seed -f spec.json` or `seed -f -` to read stdin")
	}
	spec, err := readJSONDocument(source)
	if err != nil {
		return err
	}
	ctx := context.Background()
	return e.withClient(ctx, func(client *harnessclient.Client, _ target, _ harnessclient.Bootstrap) error {
		result, err := client.CallRaw(ctx, "HarnessSeed", []json.RawMessage{spec})
		if err != nil {
			return err
		}
		return e.writeRawJSON(result)
	})
}

func runReset(e *env, args []string) error {
	flags := e.newFlagSet("reset")
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return usagef("reset takes no positional arguments (got %v)", rest)
	}
	ctx := context.Background()
	return e.withClient(ctx, func(client *harnessclient.Client, _ target, _ harnessclient.Bootstrap) error {
		if _, err := client.Call(ctx, "HarnessReset"); err != nil {
			return err
		}
		if e.jsonOutput() {
			return e.writeJSON(map[string]any{"reset": true})
		}
		e.printf("reset\n")
		return nil
	})
}

// threadRow is the subset of store.Thread this CLI prints. Declared
// locally so the binary does not link the store package (and its SQLite
// driver) to render five columns; -o json passes the server's own bytes
// through untouched, so nothing is lost by not typing the rest.
type threadRow struct {
	ID            string `json:"id"`
	Title         string `json:"title"`
	Provider      string `json:"provider"`
	Model         string `json:"model"`
	WorkspacePath string `json:"workspacePath"`
}

func runThreads(e *env, args []string) error {
	flags := e.newFlagSet("threads")
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return usagef("threads takes no positional arguments (got %v)", rest)
	}
	ctx := context.Background()
	return e.withClient(ctx, func(client *harnessclient.Client, _ target, _ harnessclient.Bootstrap) error {
		// The harness escape hatch, not App.ListThreads: a draft row with
		// no items is invisible to the production read, and "was a row
		// created" is exactly the question this command gets asked.
		raw, err := client.Call(ctx, "HarnessListThreadRows")
		if err != nil {
			return err
		}
		if e.jsonOutput() {
			return e.writeRawJSON(raw)
		}
		var rows []threadRow
		if err := json.Unmarshal(raw, &rows); err != nil {
			return fmt.Errorf("decode thread rows: %w", err)
		}
		if len(rows) == 0 {
			e.printf("no threads\n")
			return nil
		}
		table := make([][]string, 0, len(rows))
		for _, row := range rows {
			table = append(table, []string{row.ID, row.Provider, truncate(row.Title, 40), truncate(row.WorkspacePath, 48)})
		}
		return e.table([]string{"ID", "PROVIDER", "TITLE", "WORKSPACE"}, table)
	})
}

// itemRow is the printed subset of store.Item, for the same reason
// threadRow is.
type itemRow struct {
	ID        string `json:"id"`
	TurnIndex int    `json:"turnIndex"`
	ItemIndex int    `json:"itemIndex"`
	Kind      string `json:"kind"`
	Role      string `json:"role"`
	Status    string `json:"status"`
	Summary   string `json:"summary"`
}

func runItems(e *env, args []string) error {
	flags := e.newFlagSet("items")
	thread := flags.String("thread", "", "thread id")
	turn := flags.Int("turn", -1, "only items in this turn index")
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	if *thread == "" && len(rest) == 1 {
		*thread = rest[0]
		rest = nil
	}
	if len(rest) != 0 {
		return usagef("items takes only --thread (got %v)", rest)
	}
	if *thread == "" {
		return usagef("items needs --thread <id>")
	}
	ctx := context.Background()
	return e.withClient(ctx, func(client *harnessclient.Client, _ target, _ harnessclient.Bootstrap) error {
		raw, err := client.Call(ctx, "ListItems", *thread)
		if err != nil {
			return err
		}
		var rows []itemRow
		if err := json.Unmarshal(raw, &rows); err != nil {
			return fmt.Errorf("decode items: %w", err)
		}
		if *turn >= 0 {
			filtered := rows[:0]
			for _, row := range rows {
				if row.TurnIndex == *turn {
					filtered = append(filtered, row)
				}
			}
			rows = filtered
		}
		if e.jsonOutput() {
			// A turn filter changes the answer, so json follows the filtered
			// set rather than replaying the server's bytes.
			if *turn >= 0 {
				return e.writeJSON(rows)
			}
			return e.writeRawJSON(raw)
		}
		if len(rows) == 0 {
			e.printf("no items\n")
			return nil
		}
		table := make([][]string, 0, len(rows))
		for _, row := range rows {
			table = append(table, []string{
				fmt.Sprintf("%d.%d", row.TurnIndex, row.ItemIndex),
				row.Kind, row.Role, row.Status, truncate(row.Summary, 60),
			})
		}
		return e.table([]string{"TURN.ITEM", "KIND", "ROLE", "STATUS", "SUMMARY"}, table)
	})
}

func runSend(e *env, args []string) error {
	flags := e.newFlagSet("send")
	thread := flags.String("thread", "", "thread id")
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	if *thread == "" {
		return usagef("send needs --thread <id>")
	}
	text := strings.TrimSpace(strings.Join(rest, " "))
	if text == "" {
		return usagef("send needs message text")
	}
	ctx := context.Background()
	return e.withClient(ctx, func(client *harnessclient.Client, _ target, _ harnessclient.Bootstrap) error {
		// nil attachments: SendMessage's third parameter is a []string the
		// server treats as absent when null.
		if _, err := client.Call(ctx, "SendMessage", *thread, text, nil); err != nil {
			return err
		}
		if e.jsonOutput() {
			return e.writeJSON(map[string]any{"threadId": *thread, "sent": true})
		}
		e.printf("sent to %s\n", *thread)
		return nil
	})
}

// readJSONDocument loads a JSON document from a path or stdin ("-"),
// refusing anything that will not parse. Failing here beats a server
// bad_params: the caller still has the file open.
func readJSONDocument(source string) (json.RawMessage, error) {
	var data []byte
	var err error
	if source == "-" {
		data, err = io.ReadAll(os.Stdin)
		if err != nil {
			return nil, fmt.Errorf("read stdin: %w", err)
		}
	} else {
		data, err = os.ReadFile(source)
		if err != nil {
			return nil, err
		}
	}
	if !json.Valid(data) {
		return nil, fmt.Errorf("%s does not contain valid JSON", sourceName(source))
	}
	return json.RawMessage(data), nil
}

func sourceName(source string) string {
	if source == "-" {
		return "stdin"
	}
	return source
}
