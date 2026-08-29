package main

import (
	"fmt"
	"os"
	"strings"

	"agent-overflow/internal/harnessrun"
)

var artifactsSubcommands = commandNames(artifactsCommandDescriptors())

func runArtifacts(e *env, args []string) error {
	if done, err := groupHelp(e, "artifacts", args, artifactsSubcommands...); done {
		return err
	}
	if len(args) == 0 {
		return usagef("artifacts needs a subcommand: %s", strings.Join(artifactsSubcommands, ", "))
	}
	switch args[0] {
	case "list":
		return artifactsList(e, args[1:])
	case "pin":
		return artifactsPin(e, args[1:], true)
	case "unpin":
		return artifactsPin(e, args[1:], false)
	case "clean":
		return artifactsClean(e, args[1:])
	default:
		return usagef("unknown artifacts subcommand %q (want %s)", args[0], strings.Join(artifactsSubcommands, ", "))
	}
}

func artifactsList(e *env, args []string) error {
	flags := e.newFlagSet("artifacts list")
	dir := flags.String("artifact-registry-dir", os.Getenv("AO_HARNESS_ARTIFACT_REGISTRY_DIR"), "override the host-global artifact registry directory")
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return usagef("artifacts list takes no positional arguments (got %v)", rest)
	}
	registry, err := harnessrun.OpenArtifactRegistry(harnessrun.RegistryOptions{Directory: *dir})
	if err != nil {
		return err
	}
	entries, err := registry.List()
	if err != nil {
		return err
	}
	if e.jsonOutput() {
		return e.writeJSON(struct {
			Policy  harnessrun.RetentionPolicy `json:"policy"`
			Entries []harnessrun.ArtifactEntry `json:"entries"`
		}{registry.Policy(), entries})
	}
	if len(entries) == 0 {
		e.printf("no retained artifacts\n")
		return nil
	}
	rows := make([][]string, 0, len(entries))
	for _, entry := range entries {
		pinned := "no"
		if entry.Pinned {
			pinned = "yes"
		}
		rows = append(rows, []string{entry.RunID, fmt.Sprint(entry.Bytes), pinned, string(entry.State), entry.Root})
	}
	return e.table([]string{"RUN", "BYTES", "PINNED", "STATE", "ROOT"}, rows)
}

func artifactsPin(e *env, args []string, pinned bool) error {
	flags := e.newFlagSet("artifacts pin|unpin <run-id>")
	dir := flags.String("artifact-registry-dir", os.Getenv("AO_HARNESS_ARTIFACT_REGISTRY_DIR"), "override the host-global artifact registry directory")
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	if len(rest) != 1 || strings.TrimSpace(rest[0]) == "" {
		return usagef("artifacts %s needs exactly one run id", map[bool]string{true: "pin", false: "unpin"}[pinned])
	}
	registry, err := harnessrun.OpenArtifactRegistry(harnessrun.RegistryOptions{Directory: *dir})
	if err != nil {
		return err
	}
	if pinned {
		err = registry.Pin(rest[0])
	} else {
		err = registry.Unpin(rest[0])
	}
	if err != nil {
		return err
	}
	if e.jsonOutput() {
		return e.writeJSON(struct {
			RunID  string `json:"runId"`
			Pinned bool   `json:"pinned"`
		}{rest[0], pinned})
	}
	state := "unpinned"
	if pinned {
		state = "pinned"
	}
	e.printf("%s %s\n", state, rest[0])
	return nil
}

func artifactsClean(e *env, args []string) error {
	flags := e.newFlagSet("artifacts clean")
	dir := flags.String("artifact-registry-dir", os.Getenv("AO_HARNESS_ARTIFACT_REGISTRY_DIR"), "override the host-global artifact registry directory")
	dryRun := flags.Bool("dry-run", false, "report candidates without deleting roots or registry entries")
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return usagef("artifacts clean takes no positional arguments (got %v)", rest)
	}
	registry, err := harnessrun.OpenArtifactRegistry(harnessrun.RegistryOptions{Directory: *dir})
	if err != nil {
		return err
	}
	result, err := registry.Clean(harnessrun.CleanOptions{DryRun: *dryRun})
	if err != nil {
		return err
	}
	if e.jsonOutput() {
		return e.writeJSON(result)
	}
	e.printf("cleaned %d run(s), %d remaining, %d bytes\n", len(result.Pruned), result.AfterRuns, result.AfterBytes)
	for _, entry := range result.Skipped {
		e.printf("skipped %s\n", entry.RunID)
	}
	return nil
}
