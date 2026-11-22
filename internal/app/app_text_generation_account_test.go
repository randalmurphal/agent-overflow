package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/claudecatalog"
	"agent-overflow/internal/procutil"
	"agent-overflow/internal/procutil/procutiltest"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provideraccounts"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/textgen"
	"agent-overflow/internal/threadtitle"
)

// twoAccountTitleApp is claudeTitleApp with two saved Claude accounts whose
// CLIs list different Haikus: "first" (selected) lists claude-haiku-6 and
// "second" lists claude-haiku-5-5.
func twoAccountTitleApp(t *testing.T) *App {
	t.Helper()
	app, _ := claudeTitleApp(t, time.Nanosecond)
	claudeName := string(provider.Claude)
	installIdentityTestAccount(t, app, claudeName, "first", "first@example.com",
		[]byte(`{"claudeAiOauth":{"accessToken":"first"}}`))
	if err := providerCredentialsForTest(t, app).WriteAccountCredential(claudeName, "second",
		[]byte(`{"claudeAiOauth":{"accessToken":"second"}}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := app.providerAccounts.MetadataStoreForTest().UpsertAndActivate(provideraccounts.Account{
		ID: "second", Provider: claudeName, Email: "second@example.com",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := app.providerAccounts.MetadataStoreForTest().Activate(claudeName, "first"); err != nil {
		t.Fatal(err)
	}
	app.switchedAccountProbeFn = func() error { return nil }
	binary := app.providerBinaryPath(claudeName)
	for accountID, haiku := range map[string]string{"first": "claude-haiku-6", "second": "claude-haiku-5-5"} {
		capture := claudecatalog.ModelCapture{}
		capture.Capture([]claude.WireModel{{Value: "haiku", ResolvedModel: haiku}}, nil)
		capture.Store(app.providerProbeCacheKeyForAccount(claudeName, binary, accountID))
	}
	return app
}

// generateTitle runs one title generation through the shared text-generation
// path and the real Claude runner, which hands the config's admission to the
// app's executor.
func generateTitle(t *testing.T, app *App) (string, error) {
	t.Helper()
	workspace := t.TempDir()
	return runTextGenWithFallback(context.Background(), app, time.Minute, func(cfg textgen.Config, deadline time.Time) (string, error) {
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		defer cancel()
		stdout, err := textgen.RunClaude(ctx, cfg, workspace, threadtitle.ClaudeSchemaJSON, nil, "prompt", time.Minute)
		if err != nil {
			return "", err
		}
		return threadtitle.DecodeClaude(stdout)
	})
}

// A switch that lands after text generation chose its model and before its
// CLI starts makes it choose again, so the CLI starts with the model the
// account it starts under lists. Not parallel: the Claude catalog is
// process-wide.
func TestTextGenerationChoosesAgainWhenTheAccountSwitchesBeforeItsCLIStarts(t *testing.T) {
	app := twoAccountTitleApp(t)
	claudeName := string(provider.Claude)
	var started []string
	switched := false
	app.textGenerationExecutor = func(ctx context.Context, spec textgen.CLISpec) (textgen.CLIResult, error) {
		if !switched {
			switched = true
			// The gate: the model is chosen and the CLI has not started.
			if _, err := app.SwitchProviderAccount(claudeName, "second"); err != nil {
				t.Errorf("SwitchProviderAccount: %v", err)
			}
		}
		release, err := spec.Admit(ctx, func(error) {})
		if err != nil {
			return textgen.CLIResult{}, err
		}
		defer release()
		started = append(started, nextArgAfter(spec.Args, "--model"))
		return textgen.CLIResult{Stdout: `{"structured_output":{"title":"From Claude"}}`}, nil
	}

	title, err := generateTitle(t, app)
	if err != nil || title != "From Claude" {
		t.Fatalf("title = %q, %v", title, err)
	}
	if inUse := app.captureProviderAccountSelection(claudeName).AccountID; inUse != "second" {
		t.Fatalf("account in use = %q, want second", inUse)
	}
	if !slices.Equal(started, []string{"claude-haiku-5-5"}) {
		t.Fatalf("CLIs started with %v, want one start with second's claude-haiku-5-5", started)
	}
}

// A switch ends a text-generation CLI that is running on the account it
// leaves, and lands once that process has exited; the generation then runs
// again with the model the new account lists. The CLI is a shell standing in
// for Claude, run through the real executor. Not parallel: the Claude catalog
// is process-wide.
func TestAnAccountSwitchEndsARunningTextGenerationAndItRunsAgain(t *testing.T) {
	app := twoAccountTitleApp(t)
	claudeName := string(provider.Claude)
	dir := t.TempDir()
	started, hold := filepath.Join(dir, "started"), filepath.Join(dir, "hold")
	if err := os.WriteFile(hold, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// A CLI the switch did not end finishes once hold is gone, so a failing
	// run does not wait out its budget.
	t.Cleanup(func() { _ = os.Remove(hold) })
	// The first model's CLI runs until it is stopped; any other prints a title.
	const script = `if [ "$1" = claude-haiku-6 ]; then
	: > "$0/started"
	while [ -e "$0/hold" ]; do sleep 0.05; done
	exit 1
fi
printf '%s\n' '{"structured_output":{"title":"From Claude"}}'`
	var mu sync.Mutex
	var ran []string
	firstExited := make(chan struct{})
	app.textGenerationExecutor = func(ctx context.Context, spec textgen.CLISpec) (textgen.CLIResult, error) {
		model := nextArgAfter(spec.Args, "--model")
		admit := spec.Admit
		spec.Admit = func(ctx context.Context, stop func(error)) (func(), error) {
			release, err := admit(ctx, stop)
			if err != nil {
				return nil, err
			}
			mu.Lock()
			ran = append(ran, model)
			mu.Unlock()
			// The process has exited when this runs. It is marked before the
			// release, which is what lets the switch return.
			return func() {
				if model == "claude-haiku-6" {
					close(firstExited)
				}
				release()
			}, nil
		}
		spec.Binary = "/bin/sh"
		spec.Args = []string{"-c", script, dir, model}
		return textgen.ExecCLI(ctx, procutiltest.Reaper(), spec)
	}

	type outcome struct {
		title string
		err   error
	}
	generated := make(chan outcome, 1)
	go func() {
		title, err := generateTitle(t, app)
		generated <- outcome{title, err}
	}()
	waitForFile(t, started)
	if _, err := app.SwitchProviderAccount(claudeName, "second"); err != nil {
		t.Fatalf("SwitchProviderAccount: %v", err)
	}
	select {
	case <-firstExited:
	default:
		t.Fatal("the switch landed while the text-generation CLI of the account it left was running")
	}
	got := <-generated
	if got.err != nil || got.title != "From Claude" {
		t.Fatalf("title = %q, %v", got.title, got.err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(ran, []string{"claude-haiku-6", "claude-haiku-5-5"}) {
		t.Fatalf("CLIs ran with %v, want first's claude-haiku-6 ended and then second's claude-haiku-5-5", ran)
	}
}

// A run that an account change ended is run again, so its error is dropped;
// a CLI group that outlived its stop is logged once, naming the provider and
// the CLI, so the process still running is seen. Not parallel: the standard
// logger and the Claude catalog are process-wide.
func TestATextGenerationCLIThatOutlivesItsStopIsLoggedOnce(t *testing.T) {
	app := twoAccountTitleApp(t)
	logs := testutil.CaptureLogOutput(t)
	runs := 0
	got, err := runTextGenWithFallback(context.Background(), app, time.Minute, func(cfg textgen.Config, _ time.Time) (string, error) {
		runs++
		if runs == 1 {
			return "", errors.Join(errTextGenerationSelectionMoved, procutil.ErrGroupSurvived)
		}
		return "title", nil
	})
	if err != nil || got != "title" || runs != 2 {
		t.Fatalf("runTextGenWithFallback = %q, %v after %d runs; want the second run's title", got, err, runs)
	}
	var lines []string
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "could not be stopped and is still running") {
			lines = append(lines, line)
		}
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "a claude text-generation CLI") {
		t.Fatalf("logged %q, want one line naming the claude CLI", lines)
	}
}
