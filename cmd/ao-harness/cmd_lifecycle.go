package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"agent-overflow/internal/externalurl"
	"agent-overflow/internal/harness/instanceinfo"
	"agent-overflow/internal/harnessclient"
)

// stopGrace is how long a SIGTERM gets before the kill. The backend's
// graceful shutdown settles in-flight turns and withdraws its discovery
// files; five seconds is the same budget e2e/src/harness.ts allows.
const stopGrace = 5 * time.Second

func runDown(e *env, args []string) error {
	flags := e.newFlagSet("down")
	all := flags.Bool("all", false, "stop every live instance in the registry")
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return usagef("down takes no positional arguments (got %v)", rest)
	}

	type victim struct {
		id       string
		pid      int
		dataRoot string
	}
	var victims []victim

	if *all {
		rows, err := e.listInstances()
		if err != nil {
			return err
		}
		for _, row := range rows {
			if row.Stale {
				continue
			}
			victims = append(victims, victim{row.ID, row.PID, row.DataRoot})
		}
		if len(victims) == 0 {
			if e.jsonOutput() {
				return e.writeJSON([]any{})
			}
			e.printf("no live instances\n")
			return nil
		}
	} else {
		t, err := e.resolveTarget()
		if err != nil {
			return err
		}
		pid, err := pidFor(t)
		if err != nil {
			return err
		}
		victims = append(victims, victim{t.ID, pid, t.DataRoot})
	}

	results := make([]map[string]any, 0, len(victims))
	var failures []error
	for _, v := range victims {
		err := harnessclient.TerminateProcess(context.Background(), v.pid, stopGrace)
		entry := map[string]any{"id": v.id, "pid": v.pid, "dataRoot": v.dataRoot, "stopped": err == nil}
		if err != nil {
			entry["error"] = err.Error()
			failures = append(failures, fmt.Errorf("instance %s (pid %d): %w", v.id, v.pid, err))
		}
		results = append(results, entry)
	}

	if e.jsonOutput() {
		if err := e.writeJSON(results); err != nil {
			return err
		}
	} else {
		for _, entry := range results {
			if entry["stopped"] == true {
				e.printf("stopped %v (pid %v)\n", entry["id"], entry["pid"])
			} else {
				e.printf("failed  %v (pid %v): %v\n", entry["id"], entry["pid"], entry["error"])
			}
		}
	}
	return errors.Join(failures...)
}

// pidFor answers "which process is this instance": the registry row when
// there is one, otherwise the data-dir instance file. A target with
// neither is not running, which is what the error says.
func pidFor(t target) (int, error) {
	if t.Row != nil && t.Row.PID > 0 && !t.Row.Stale {
		return t.Row.PID, nil
	}
	bs, err := harnessclient.ReadInstanceFile(t.DataDir)
	if err != nil {
		return 0, fmt.Errorf("no live instance at %s: %w", t.DataRoot, err)
	}
	if !instanceinfo.ProcessAlive(bs.PID) {
		return 0, fmt.Errorf("instance %s names pid %d, which is not running", t.ID, bs.PID)
	}
	return bs.PID, nil
}

func runList(e *env, args []string) error {
	flags := e.newFlagSet("list")
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return usagef("list takes no positional arguments (got %v)", rest)
	}

	dir, err := e.registry()
	if err != nil {
		return err
	}
	rows, err := instanceinfo.ListIn(dir, nil)
	if err != nil {
		return err
	}

	live := make([]instanceinfo.Instance, 0, len(rows))
	pruned := make([]string, 0)
	for _, row := range rows {
		if !row.Stale {
			live = append(live, row)
			continue
		}
		if !safeToPrune(row) {
			// Stale by pid, but the data dir names somebody else. Report it
			// rather than deleting a row we cannot explain.
			live = append(live, row)
			continue
		}
		if err := instanceinfo.RemoveIn(dir, row.ID); err != nil {
			fmt.Fprintf(e.stderr, "ao-harness: prune %s: %v\n", row.ID, err)
			live = append(live, row)
			continue
		}
		pruned = append(pruned, row.ID)
	}

	if e.jsonOutput() {
		return e.writeJSON(map[string]any{"instances": live, "pruned": pruned})
	}
	if len(live) == 0 {
		e.printf("no instances\n")
	} else {
		tableRows := make([][]string, 0, len(live))
		for _, row := range live {
			state := "live"
			if row.Stale {
				state = "stale"
			}
			tableRows = append(tableRows, []string{
				row.ID, string(row.Mode), state, fmt.Sprint(row.PID), fmt.Sprint(row.Port),
				truncate(row.Worktree, 48), row.DataRoot,
			})
		}
		if err := e.table([]string{"ID", "MODE", "STATE", "PID", "PORT", "WORKTREE", "DATA ROOT"}, tableRows); err != nil {
			return err
		}
	}
	for _, id := range pruned {
		e.printf("pruned stale row %s\n", id)
	}
	return nil
}

// safeToPrune decides whether a stale registry row may be deleted on
// sight. A dead pid is necessary but not sufficient: the row is
// discovery state about a DATA ROOT, and the authoritative statement
// about that root is its own instance file. Delete only when that file
// is gone, or when it names the very pid we just found dead — anything
// else means a second process is involved and the row is not ours to
// remove.
func safeToPrune(row instanceinfo.Instance) bool {
	bs, err := harnessclient.ReadInstanceFile(row.DataDir)
	if err != nil {
		// Unreadable for any reason (missing, truncated, permissions):
		// nothing there claims the root.
		return true
	}
	return bs.PID == row.PID
}

func runInfo(e *env, args []string) error {
	flags := e.newFlagSet("info")
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return usagef("info takes no positional arguments (got %v)", rest)
	}
	ctx := context.Background()
	return e.withClient(ctx, func(client *harnessclient.Client, t target, bs harnessclient.Bootstrap) error {
		info, err := client.Info(ctx)
		if err != nil {
			return err
		}
		stderrPath := filepath.Join(info.DataDir, logDirName, backendStderrLog)
		if e.jsonOutput() {
			return e.writeJSON(map[string]any{
				"id": t.ID, "mode": bs.Mode, "window": bs.Window, "worktree": bs.Worktree,
				"startedAt": bs.StartedAt, "url": bs.URL, "port": bs.Port,
				"info": info, "backendStderr": stderrPath,
			})
		}
		e.printf("instance %s (%s%s)\n", t.ID, orUnknown(string(bs.Mode)), windowSuffix(bs.Window))
		e.printf("  version   %s\n", info.Version)
		e.printf("  pid       %d\n", info.PID)
		e.printf("  url       %s\n", bs.URL)
		e.printf("  worktree  %s\n", orUnknown(bs.Worktree))
		e.printf("  started   %s\n", orUnknown(bs.StartedAt))
		e.printf("  data root %s\n", info.DataRoot)
		e.printf("  data dir  %s\n", info.DataDir)
		e.printf("  home      %s\n", orUnknown(info.HomeDir))
		e.printf("  mock      %s\n", info.MockProvider)
		e.printf("  db        %s\n", info.DBPath)
		e.printf("  events    %s\n", info.EventLogDir)
		e.printf("  ui trace  %s\n", info.UITracePath)
		e.printf("  fe errors %s\n", info.FrontendErrorsPath)
		e.printf("  backend   %s\n", stderrPath)
		return nil
	})
}

func orUnknown(s string) string {
	if s == "" {
		return "(unknown)"
	}
	return s
}

func runOpen(e *env, args []string) error {
	flags := e.newFlagSet("open")
	browser := flags.Bool("browser", false, "open the URL in the host browser instead of only printing it")
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return usagef("open takes no positional arguments (got %v)", rest)
	}
	t, err := e.resolveTarget()
	if err != nil {
		return err
	}
	bs, err := harnessclient.ReadInstanceFile(t.DataDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("no live instance at %s", t.DataRoot)
		}
		return err
	}
	if e.jsonOutput() {
		if err := e.writeJSON(map[string]any{"id": t.ID, "url": bs.URL}); err != nil {
			return err
		}
	} else {
		e.printf("%s\n", bs.URL)
	}
	if !*browser {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return externalurl.Open(ctx, bs.URL)
}
