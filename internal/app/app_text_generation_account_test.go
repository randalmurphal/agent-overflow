package app

import (
	"context"
	"slices"
	"testing"
	"time"

	"agent-overflow/internal/claudecatalog"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provideraccounts"
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
	app.textGenerationExecutor = func(_ context.Context, spec textgen.CLISpec) (textgen.CLIResult, error) {
		if !switched {
			switched = true
			// The gate: the model is chosen and the CLI has not started.
			if _, err := app.SwitchProviderAccount(claudeName, "second"); err != nil {
				t.Errorf("SwitchProviderAccount: %v", err)
			}
		}
		release, err := spec.Admit()
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

// A switch asked for while a text-generation CLI is starting lands once the
// start has returned: it never lands between the admission and the start,
// and it does not wait for the run.
func TestAnAccountSwitchWaitsForATextGenerationStartAndNotItsRun(t *testing.T) {
	app := twoAccountTitleApp(t)
	claudeName := string(provider.Claude)
	landedWhileStarting := false
	var switchErr error
	app.textGenerationExecutor = func(_ context.Context, spec textgen.CLISpec) (textgen.CLIResult, error) {
		release, err := spec.Admit()
		if err != nil {
			return textgen.CLIResult{}, err
		}
		switched := make(chan error, 1)
		go func() {
			_, err := app.SwitchProviderAccount(claudeName, "second")
			switched <- err
		}()
		// A slow machine only makes this check weaker: it cannot fail it.
		select {
		case switchErr = <-switched:
			landedWhileStarting = true
		case <-time.After(200 * time.Millisecond):
		}
		release()
		// The run: the switch lands while it goes on.
		if !landedWhileStarting {
			switchErr = <-switched
		}
		return textgen.CLIResult{Stdout: `{"structured_output":{"title":"From Claude"}}`}, nil
	}

	if _, err := generateTitle(t, app); err != nil {
		t.Fatalf("generate title: %v", err)
	}
	if switchErr != nil {
		t.Fatalf("SwitchProviderAccount: %v", switchErr)
	}
	if landedWhileStarting {
		t.Fatal("the account switch landed while the text-generation CLI was starting")
	}
	if inUse := app.captureProviderAccountSelection(claudeName).AccountID; inUse != "second" {
		t.Fatalf("account in use = %q, want second", inUse)
	}
}
