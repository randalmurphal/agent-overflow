package main

import (
	"fmt"

	"agent-overflow/internal/harness/instanceinfo"
	"agent-overflow/internal/harnessclient"
)

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
	kept := make([]instanceinfo.Instance, 0, len(rows))
	pruned := make([]string, 0)
	for _, row := range rows {
		if !row.Stale {
			kept = append(kept, row)
			continue
		}
		if !safeToPrune(row) {
			kept = append(kept, row)
			continue
		}
		if err := instanceinfo.RemoveIn(dir, row.ID); err != nil {
			fmt.Fprintf(e.stderr, "ao-harness: prune %s: %v\n", row.ID, err)
			kept = append(kept, row)
			continue
		}
		pruned = append(pruned, row.ID)
	}
	if e.jsonOutput() {
		return e.writeJSON(map[string]any{"instances": kept, "pruned": pruned})
	}
	if len(kept) == 0 {
		e.printf("no instances\n")
	} else {
		tableRows := make([][]string, 0, len(kept))
		for _, row := range kept {
			state := "live"
			if row.Stale {
				state = "stale"
			}
			tableRows = append(tableRows, []string{row.ID, string(row.Mode), state, fmt.Sprint(row.PID), fmt.Sprint(row.Port), truncate(row.Worktree, 48), row.DataRoot})
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

func safeToPrune(row instanceinfo.Instance) bool {
	if err := validateTargetPaths(row.DataRoot, row.DataDir); err != nil {
		return false
	}
	bs, err := harnessclient.ReadInstanceFile(row.DataDir)
	if err != nil {
		return true
	}
	if row.PIDNamespace != "" && bs.PIDNamespace != row.PIDNamespace {
		return false
	}
	if bs.PIDNamespace != "" && bs.PIDNamespace != instanceinfo.CurrentPIDNamespace() {
		return false
	}
	if row.PIDNamespace != "" && row.PIDNamespace != instanceinfo.CurrentPIDNamespace() {
		return false
	}
	return bs.PID == row.PID
}
