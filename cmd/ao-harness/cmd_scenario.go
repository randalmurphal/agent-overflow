package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"agent-overflow/internal/harness/scenario"
	"agent-overflow/internal/harnessclient"
)

func runScenario(e *env, args []string) error {
	if len(args) == 0 {
		return usagef("scenario needs a subcommand: set, list, clear, validate")
	}
	switch args[0] {
	case "set":
		return scenarioSet(e, args[1:])
	case "list":
		return scenarioList(e, args[1:])
	case "clear":
		return scenarioClear(e, args[1:])
	case "validate":
		return scenarioValidate(e, args[1:])
	default:
		return usagef("unknown scenario subcommand %q (want set, list, clear, validate)", args[0])
	}
}

func scenarioSet(e *env, args []string) error {
	flags := e.newFlagSet("scenario set")
	name := flags.String("name", "", "library scenario name")
	file := flags.String("f", "", "inline scenario JSON file, or - for stdin")
	cwd := flags.String("cwd", "", "scope the rule to mocks spawned in this workspace")
	sessionRef := flags.String("session-ref", "", "scope the rule to one provider session (matched against the mock's --resume value; claude-only, and never a first spawn)")
	fixtureRoot := flags.String("fixture-root", "", "resolve the scenario's relative fixture paths against this root (default: the instance data root)")
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	if *name == "" && *file == "" && len(rest) == 1 {
		*name = rest[0]
		rest = nil
	}
	if len(rest) != 0 {
		return usagef("scenario set takes no positional arguments beyond a library name (got %v)", rest)
	}
	if (*name == "") == (*file == "") {
		return usagef("scenario set needs exactly one of --name <library-name> or -f <file>")
	}

	spec := map[string]any{}
	if *name != "" {
		spec["name"] = *name
	} else {
		doc, err := readJSONDocument(*file)
		if err != nil {
			return err
		}
		spec["scenario"] = doc
	}
	if *cwd != "" {
		abs, err := filepath.Abs(*cwd)
		if err != nil {
			return fmt.Errorf("resolve --cwd: %w", err)
		}
		spec["cwd"] = abs
	}
	if *sessionRef != "" {
		spec["sessionRef"] = *sessionRef
	}
	if *fixtureRoot != "" {
		abs, err := filepath.Abs(*fixtureRoot)
		if err != nil {
			return fmt.Errorf("resolve --fixture-root: %w", err)
		}
		spec["fixtureRoot"] = abs
	}

	ctx := context.Background()
	return e.withClient(ctx, func(client *harnessclient.Client, _ target, _ harnessclient.Bootstrap) error {
		result, err := client.Call(ctx, "HarnessSetScenario", spec)
		if err != nil {
			return err
		}
		return e.writeRawJSON(result)
	})
}

func scenarioList(e *env, args []string) error {
	flags := e.newFlagSet("scenario list")
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return usagef("scenario list takes no positional arguments (got %v)", rest)
	}
	ctx := context.Background()
	return e.withClient(ctx, func(client *harnessclient.Client, _ target, _ harnessclient.Bootstrap) error {
		raw, err := client.Call(ctx, "HarnessListScenarios")
		if err != nil {
			return err
		}
		if e.jsonOutput() {
			return e.writeRawJSON(raw)
		}
		var result struct {
			Library []struct {
				Name        string `json:"name"`
				Description string `json:"description"`
				Provider    string `json:"provider"`
			} `json:"library"`
			Rules []struct {
				Name       string `json:"name"`
				Provider   string `json:"provider"`
				Cwd        string `json:"cwd"`
				SessionRef string `json:"sessionRef"`
			} `json:"rules"`
		}
		if err := json.Unmarshal(raw, &result); err != nil {
			return fmt.Errorf("decode scenarios: %w", err)
		}
		e.printf("active rules:\n")
		if len(result.Rules) == 0 {
			e.printf("  (none — mocks get their provider's default scenario)\n")
		} else {
			rows := make([][]string, 0, len(result.Rules))
			for _, rule := range result.Rules {
				rows = append(rows, []string{"  " + rule.Name, rule.Provider, orDash(rule.Cwd), orDash(rule.SessionRef)})
			}
			if err := e.table([]string{"  SCENARIO", "PROVIDER", "CWD", "SESSION REF"}, rows); err != nil {
				return err
			}
		}
		e.printf("\nlibrary:\n")
		rows := make([][]string, 0, len(result.Library))
		for _, entry := range result.Library {
			rows = append(rows, []string{"  " + entry.Name, entry.Provider, truncate(entry.Description, 60)})
		}
		return e.table([]string{"  NAME", "PROVIDER", "DESCRIPTION"}, rows)
	})
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func scenarioClear(e *env, args []string) error {
	flags := e.newFlagSet("scenario clear")
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return usagef("scenario clear takes no positional arguments (got %v)", rest)
	}
	ctx := context.Background()
	return e.withClient(ctx, func(client *harnessclient.Client, _ target, _ harnessclient.Bootstrap) error {
		if _, err := client.Call(ctx, "HarnessClearScenarios"); err != nil {
			return err
		}
		if e.jsonOutput() {
			return e.writeJSON(map[string]any{"cleared": true})
		}
		e.printf("scenario rules cleared\n")
		return nil
	})
}

// scenarioValidation is one file's verdict.
type scenarioValidation struct {
	Path     string   `json:"path"`
	Name     string   `json:"name,omitempty"`
	Provider string   `json:"provider,omitempty"`
	OK       bool     `json:"ok"`
	Error    string   `json:"error,omitempty"`
	Fixtures []string `json:"fixtures,omitempty"`
	Missing  []string `json:"missing,omitempty"`
}

// scenarioValidate is the one command that needs no instance: it parses
// scenario documents with the SAME parser the mock runs and checks that
// every fixture the script references exists. Offline because a scenario
// author is usually not holding a running harness, and because a test
// wants this verdict without booting anything.
func scenarioValidate(e *env, args []string) error {
	flags := e.newFlagSet("scenario validate")
	fixtureRoot := flags.String("fixture-root", ".", "resolve relative fixture paths against this directory")
	paths, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	if len(paths) == 0 {
		return usagef("scenario validate needs at least one file")
	}
	root, err := filepath.Abs(*fixtureRoot)
	if err != nil {
		return fmt.Errorf("resolve --fixture-root: %w", err)
	}

	results := make([]scenarioValidation, 0, len(paths))
	failures := 0
	for _, path := range paths {
		results = append(results, validateScenarioFile(path, root))
		if !results[len(results)-1].OK {
			failures++
		}
	}

	if e.jsonOutput() {
		if err := e.writeJSON(results); err != nil {
			return err
		}
	} else {
		for _, result := range results {
			if !result.OK {
				e.printf("FAIL %s: %s\n", result.Path, result.Error)
				for _, missing := range result.Missing {
					e.printf("       missing fixture %s\n", missing)
				}
				continue
			}
			e.printf("ok   %s (%s, %s, %d fixture(s))\n", result.Path, result.Name, result.Provider, len(result.Fixtures))
		}
	}
	if failures > 0 {
		return fmt.Errorf("%d of %d scenario file(s) failed validation", failures, len(results))
	}
	return nil
}

func validateScenarioFile(path, fixtureRoot string) scenarioValidation {
	out := scenarioValidation{Path: path}
	data, err := os.ReadFile(path)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	parsed, err := scenario.Parse(data)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	out.Name = parsed.Name
	out.Provider = parsed.Provider
	out.Fixtures = parsed.FixturePaths()
	for _, fixture := range out.Fixtures {
		resolved := fixture
		if !filepath.IsAbs(resolved) {
			resolved = filepath.Join(fixtureRoot, fixture)
		}
		if _, err := os.Stat(resolved); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				out.Missing = append(out.Missing, fmt.Sprintf("%s (%v)", fixture, err))
				continue
			}
			out.Missing = append(out.Missing, fixture)
		}
	}
	if len(out.Missing) > 0 {
		out.Error = fmt.Sprintf("%d fixture path(s) do not exist under %s", len(out.Missing), fixtureRoot)
		return out
	}
	out.OK = true
	return out
}
