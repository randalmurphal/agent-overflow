package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"agent-overflow/internal/compare"
)

var compareSubcommands = commandNames(compareCommandDescriptors())

func runCompare(e *env, args []string) error {
	return runCompareContext(context.Background(), e, args)
}

func runCompareContext(ctx context.Context, e *env, args []string) error {
	if done, err := groupHelp(e, "compare", args, compareSubcommands...); done {
		return err
	}
	if len(args) == 0 {
		return usagef("compare needs a subcommand: %s", strings.Join(compareSubcommands, ", "))
	}
	switch args[0] {
	case "prepare":
		return comparePrepare(e, args[1:])
	case "run":
		return compareRunContext(ctx, e, args[1:])
	default:
		return usagef("unknown compare subcommand %q (want %s)", args[0], strings.Join(compareSubcommands, ", "))
	}
}

func comparePrepare(e *env, args []string) error {
	flags := e.newFlagSet("compare prepare")
	from := flags.String("from", "", "offline copy or harness-owned data root")
	out := flags.String("out", "", "capsule directory to create")
	force := flags.Bool("force", false, "replace an existing capsule")
	assetDigest := flags.String("asset-digest", "", "frontend asset digest to require during run")
	buildDigest := flags.String("build-digest", "", "application build digest to require during run")
	workloadName := flags.String("workload", "replay", "workload shape name")
	var required stringList
	flags.Var(&required, "require-capability", "required runner capability (repeatable)")
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return usagef("compare prepare takes no positional arguments (got %v)", rest)
	}
	capsule, err := compare.Prepare(compare.PrepareOptions{Source: *from, Output: *out, Force: *force, AssetDigest: *assetDigest, BuildDigest: *buildDigest, Workload: compare.WorkloadShape{Name: *workloadName, RequiredCapabilities: []string(required)}})
	if err != nil {
		return err
	}
	manifest := filepath.Join(*out, "manifest.json")
	if e.jsonOutput() {
		return e.writeJSON(map[string]any{"capsule": *out, "manifest": manifest, "version": capsule.Version, "capsuleSha256": capsule.CapsuleSHA256, "events": capsule.Events.Count, "panes": len(capsule.Panes), "attachments": len(capsule.Attachments), "fixtures": len(capsule.Fixtures)})
	}
	e.printf("prepared compare capsule %s\n", *out)
	e.printf("  manifest  %s\n", manifest)
	e.printf("  digest    %s\n", capsule.CapsuleSHA256)
	e.printf("  workload  %s\n", capsule.Workload.Name)
	e.printf("  panes     %d\n", len(capsule.Panes))
	e.printf("  events    %d\n", capsule.Events.Count)
	return nil
}

func compareRun(e *env, args []string) error {
	return compareRunContext(context.Background(), e, args)
}

func compareRunContext(ctx context.Context, e *env, args []string) error {
	flags := e.newFlagSet("compare run")
	capsule := flags.String("capsule", "", "capsule manifest or capsule directory")
	binary := flags.String("binary", "", "agent-overflow backend binary (default: harness binary resolution)")
	mockProvider := flags.String("mock-provider", "", "mock provider binary (default: backend resolution)")
	window := flags.Bool("window", true, "launch a real webview window for each leg")
	cdp := flags.String("cdp", defaultCDPSpec(), "optional Chromium DevTools endpoint to record exact page ownership")
	instrument := flags.String("instrument", "perf", "leg instrument: perf or none")
	sampleMs := flags.Int("sample-ms", 0, "frontend/backend perf sampling interval")
	pairs := flags.Int("pairs", 1, "complete A/B pairs")
	out := flags.String("out", "", "partial report path (default: beside the immutable capsule)")
	base := flags.String("base-dir", "", "directory for disposable leg roots (default: the OS temporary directory)")
	keep := flags.Bool("keep-roots", false, "retain disposable roots for inspection")
	asset := flags.String("asset-digest", "", "additional expected asset digest")
	build := flags.String("build-digest", "", "additional expected build digest")
	bootstrap := flags.Int("bootstrap", 0, "bootstrap resamples (default 10000, only with enough pairs)")
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return usagef("compare run takes no positional arguments (got %v)", rest)
	}
	if strings.TrimSpace(*capsule) == "" {
		return usagef("compare run needs --capsule <manifest or directory>")
	}
	if *instrument != "perf" && *instrument != "none" {
		return usagef("compare run --instrument must be perf or none (got %q)", *instrument)
	}
	if *sampleMs < 0 {
		return usagef("compare run --sample-ms must not be negative")
	}
	if !*window {
		return usagef("compare run requires --window: semantic comparison needs a real frontend page")
	}
	manifest := *capsule
	if info, statErr := os.Stat(manifest); statErr == nil && info.IsDir() {
		manifest = filepath.Join(manifest, "manifest.json")
	}
	if *out == "" {
		// The capsule is immutable and its directory is read-only. Keep the
		// evolving report beside it rather than attempting to write into it.
		capsuleDir := filepath.Dir(manifest)
		*out = filepath.Join(filepath.Dir(capsuleDir), filepath.Base(capsuleDir)+"-report.json")
	}
	backend, err := resolveBackendBinary(*binary)
	if err != nil {
		return err
	}
	runner := compare.NewBrowserRunner(compare.BrowserRunnerOptions{Binary: backend, MockProvider: *mockProvider, Window: *window, PageID: strings.TrimSpace(e.pageID), CDP: *cdp, SampleMs: *sampleMs, AssetDigest: *asset, BuildDigest: *build, Instrument: *instrument})
	report, runErr := compare.Run(ctx, compare.RunOptions{Capsule: manifest, BaseDir: *base, Pairs: *pairs, Bootstrap: *bootstrap, KeepRoots: *keep, ReportPath: *out, ExpectedAsset: *asset, ExpectedBuild: *build, Instrument: *instrument}, runner, nil)
	if e.jsonOutput() {
		if err := e.writeJSON(report); err != nil {
			return err
		}
	} else {
		e.printf("compare report %s\n", *out)
		e.printf("  legs      %d\n", len(report.Legs))
		e.printf("  pairs     %d\n", len(report.Pairs))
		e.printf("  complete  %v\n", report.Complete)
		if report.Error != "" {
			e.printf("  error     %s\n", report.Error)
		}
	}
	return runErr
}
