//go:build providersmoke

// The real-provider smoke gate (`make provider-smoke`).
//
// Everything else in this repo's Go suite runs against scripted or mocked
// provider binaries, which accept any structured-output schema. The real
// `claude` and `codex` CLIs do not: they validate the generated envelope schema
// in strict mode and refuse the phase before it starts. That gap is how five
// envelope-schema defects survived a fully green harness, and this file is the
// live check that closes it.
//
// It is behind the `providersmoke` build tag, so `make go-test` and `make
// verify` never compile it and stay hermetic. Run it manually before a release
// and after upgrading either provider CLI. It spends real model tokens — one
// trivial turn per provider here, plus the four the Claude imported-branch
// scenario costs (providersmoke_importbranch_test.go) — which is the price of
// the only assertions a mock cannot make.
//
// This file's workflow gate measures three things, in the order a failure
// should be read:
//
//  1. SCHEMA ACCEPTANCE — the CLI accepted the generated envelope schema.
//  2. ENVELOPE ROUND-TRIP — the run reached done through a real envelope
//     carrying its declared output.
//  3. BRANCH-RULE CONFORMANCE — the writing phase ran in the run's isolated
//     worktree on the run's own branch, and the project checkout was untouched.
//
// A fourth dimension lives in its own file because it drives raw provider
// sessions rather than a workflow run:
//
//  4. IMPORTED-BRANCH RESUME (Claude only) — the real CLI accepts a transcript
//     the session importer's lazy branch materialisation cut, and continues
//     THAT branch's conversation. See providersmoke_importbranch_test.go.
//
// Nothing here overrides `claudeBinaryPath` / `codexBinaryPath`: the point is to
// exercise the exact default binary resolution production uses. There is no
// t.Skip anywhere in this file either — a manual gate that quietly does nothing
// is worse than no gate.
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/providerstatus"
	"agent-overflow/internal/settings"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/workflow/engine"
)

const (
	// providerSmokeRunDeadline bounds one provider's leg. Real turns take
	// minutes, not the milliseconds the mocked suites wait; two legs plus
	// process overhead must still fit the Makefile's -timeout.
	providerSmokeRunDeadline = 6 * time.Minute
	// providerSmokeWatchdog is the profile inactivity watchdog. It sits under
	// the run deadline so a silently wedged turn parks itself as `stalled`
	// instead of being reported as a deadline overrun.
	providerSmokeWatchdog = 5 * time.Minute
	// providerSmokePollInterval is how often the terminal-state waiter reads
	// the run row. A real turn does not need a tight loop.
	providerSmokePollInterval = 500 * time.Millisecond
	// providerSmokeProbeTimeout bounds the token-free preflight account probe.
	providerSmokeProbeTimeout = 30 * time.Second
)

// providerSmokeCase is one provider's leg of the gate.
type providerSmokeCase struct {
	// providerName is the workflow `provider:` value and the key production
	// resolves a binary path under.
	providerName string
	// model is a current, cheap model id for that provider. Never a Haiku model:
	// this gate must exercise a model the product actually routes work to.
	model string
	// installHint and loginHint are the operator instructions a failed preflight
	// prints. A gate that fails without saying what to install or log into just
	// moves the investigation.
	installHint string
	loginHint   string
	// probeAccount is the provider's own token-free account probe, used as the
	// authentication preflight.
	probeAccount func(*App) (provider.AccountInfo, error)
	// unauthenticated decides whether a successful probe nonetheless means "not
	// logged in". Nil when the provider has no reliable pre-turn signal, in
	// which case an auth failure is classified from the run's own error text.
	unauthenticated func(provider.AccountInfo) bool
}

func TestProviderSmokeClaude(t *testing.T) {
	runProviderSmoke(t, providerSmokeClaudeCase())
}

// providerSmokeClaudeCase is the Claude leg's case. It is shared with the
// imported-branch scenario so both preflight the same default binary, run the
// same model, and print the same operator instructions; a private second copy
// would drift the moment either moved.
func providerSmokeClaudeCase() providerSmokeCase {
	return providerSmokeCase{
		providerName: string(provider.Claude),
		// Sonnet 4.6 is the cheapest non-Haiku model in the Claude catalog and is
		// accepted by Claude Code 2.1.219.
		model:        "claude-sonnet-4-6",
		installHint:  "install Claude Code (https://docs.claude.com/en/docs/claude-code/setup) and put `claude` on PATH",
		loginHint:    "run `claude login`",
		probeAccount: (*App).ProbeClaudeAccount,
		// Production's own logged-out predicate, called directly rather than
		// mirrored: a private copy here would silently drift from the shipped
		// rule and fail the gate on hosts the product considers authenticated
		// (Bedrock/Vertex accounts surface only apiProvider, firstParty
		// profile logins only email).
		unauthenticated: providerstatus.ClaudeUnauthenticated,
	}
}

func TestProviderSmokeCodex(t *testing.T) {
	runProviderSmoke(t, providerSmokeCase{
		providerName: string(provider.Codex),
		// Codex's own catalog describes gpt-5.6-luna as the fast, affordable
		// coding model, and unlike gpt-5.4-mini it carries no upgrade/deprecation
		// pointer.
		model:        "gpt-5.6-luna",
		installHint:  "install the Codex CLI (https://github.com/openai/codex#installation) and put `codex` on PATH",
		loginHint:    "run `codex login`",
		probeAccount: (*App).ProbeCodexAccount,
		// Deliberately nil: a zero-value Codex AccountInfo is documented as
		// ambiguous (signed in, but the rate-limit backend has seen no activity),
		// so treating it as logged-out would fail authenticated hosts. Codex auth
		// failures are classified from the run's error text instead.
		unauthenticated: nil,
	})
}

// runProviderSmoke drives one trivial workflow through one real provider CLI and
// asserts the gate's three dimensions.
func runProviderSmoke(t *testing.T, smoke providerSmokeCase) {
	app, _ := setupE2EApp(t)
	collector := &providerSmokeCollector{}
	// The router hook is the one seam that sees every wire event with its Meta
	// intact. setupE2EApp installs a capture hook of its own; this gate does not
	// use that bus, and its collector keeps a bounded, filtered log instead.
	app.triage.SetEventHook(collector.observeProviderEvent)

	binaryPath := preflightProviderBinary(t, app, smoke)
	preflightProviderAuth(t, app, smoke, binaryPath)

	repo := testutil.InitGitRepo(t)
	projectRow := mustReloadProject(t, app.store, testutil.EnsureProject(t, app.store, repo).ID)
	configRoot := t.TempDir()
	writeProviderSmokeWorkflow(t, configRoot, smoke)
	writeProviderSmokeProfile(t, configRoot, projectRow.Slug)
	startWorkflowEngineForTest(t, app, configRoot)

	goal := "provider smoke gate: " + smoke.providerName
	started := time.Now()
	item, err := app.WorkflowStartRun(
		projectRow.ID, providerSmokeWorkflowID, "shared", goal,
		json.RawMessage(`{}`), nil, "", false,
	)
	if err != nil {
		t.Fatalf("provider smoke (%s): start run: %v", smoke.providerName, err)
	}
	t.Cleanup(func() { removeProviderSmokeWorktree(t, app, repo, item.ID) })

	final, settled := waitForProviderSmokeTerminal(t, app, item.ID, providerSmokeRunDeadline)
	elapsed := time.Since(started)
	t.Logf("provider smoke (%s): model=%s wall=%s state=%s reason=%q",
		smoke.providerName, smoke.model, elapsed.Round(time.Second), final.State, final.Reason)
	if !settled {
		failProviderSmokeRun(t, app, smoke, final, collector,
			"RUN DID NOT SETTLE (%s): still %s after %s (phase %q); the inactivity watchdog is %s, so a turn that outran this deadline was still emitting",
			smoke.providerName, final.State, providerSmokeRunDeadline, final.CurrentPhaseID, providerSmokeWatchdog,
		)
	}

	assertProviderSmokeSchemaAccepted(t, app, smoke, final, collector)
	envelopeOutput := assertProviderSmokeEnvelopeRoundTrip(t, app, smoke, final, collector)
	assertProviderSmokeBranchRules(t, app, smoke, repo, final, envelopeOutput, collector)
}

// preflightProviderBinary resolves the provider binary exactly as production
// does — settings value (never overridden here) through
// provider.DetectProvider's PATH lookup and version check — and refuses to run
// the gate on anything but a ready binary.
func preflightProviderBinary(t *testing.T, app *App, smoke providerSmokeCase) string {
	t.Helper()
	configured := app.providerBinaryPath(smoke.providerName)
	expected := defaultProviderBinaryPath(t, smoke.providerName)
	if configured != expected {
		t.Fatalf(
			"provider smoke (%s): binary path setting is %q, want the default %q — this gate must exercise default binary resolution, never an override",
			smoke.providerName, configured, expected,
		)
	}
	status := provider.DetectProvider(smoke.providerName, configured)
	if status.Status != "ready" {
		t.Fatalf(
			"provider smoke (%s): %q is not usable (status=%s, path=%q, version=%q): %s\nfix: %s",
			smoke.providerName, configured, status.Status, status.BinaryPath, status.Version,
			status.Message, smoke.installHint,
		)
	}
	t.Logf("provider smoke (%s): resolved %q -> %s (%s)",
		smoke.providerName, configured, status.BinaryPath, status.Version)
	return status.BinaryPath
}

// defaultProviderBinaryPath is the shipped default for a provider — what
// production resolves when the user has configured nothing.
func defaultProviderBinaryPath(t *testing.T, providerName string) string {
	t.Helper()
	switch providerName {
	case string(provider.Claude):
		return settings.DefaultSettings.ClaudeBinaryPath
	case string(provider.Codex):
		return settings.DefaultSettings.CodexBinaryPath
	default:
		t.Fatalf("provider smoke: no default binary path for provider %q", providerName)
		return ""
	}
}

// preflightProviderAuth runs the provider's token-free account probe so an
// unauthenticated host fails before a turn is attempted, with the sentence that
// names the actual problem.
func preflightProviderAuth(t *testing.T, app *App, smoke providerSmokeCase, binaryPath string) {
	t.Helper()
	type probeResult struct {
		info provider.AccountInfo
		err  error
	}
	done := make(chan probeResult, 1)
	go func() {
		info, err := smoke.probeAccount(app)
		done <- probeResult{info: info, err: err}
	}()
	select {
	case result := <-done:
		if result.err != nil {
			t.Fatalf(
				"provider smoke (%s): %s is installed but its account probe failed: %v\nthis usually means the CLI is not authenticated — %s",
				smoke.providerName, binaryPath, result.err, smoke.loginHint,
			)
		}
		if smoke.unauthenticated != nil && smoke.unauthenticated(result.info) {
			t.Fatalf(
				"provider smoke (%s): %s is installed but not logged in (its account probe reported no account identity at all)\nfix: %s",
				smoke.providerName, binaryPath, smoke.loginHint,
			)
		}
		t.Logf("provider smoke (%s): account probe ok (subscription=%q apiProvider=%q)",
			smoke.providerName, result.info.SubscriptionType, result.info.APIProvider)
	case <-time.After(providerSmokeProbeTimeout):
		t.Fatalf(
			"provider smoke (%s): %s account probe did not answer within %s\nthis usually means the CLI is not authenticated or is blocked on a prompt — %s",
			smoke.providerName, binaryPath, providerSmokeProbeTimeout, smoke.loginHint,
		)
	}
}

// assertProviderSmokeSchemaAccepted is dimension 1 and runs first: a schema
// rejection kills the phase before any work happens, and every downstream
// assertion would then fail for a reason that has nothing to do with it.
func assertProviderSmokeSchemaAccepted(
	t *testing.T, app *App, smoke providerSmokeCase, item store.WorkItem, collector *providerSmokeCollector,
) {
	t.Helper()
	entries, _ := collector.snapshot()
	entry, marker, rejected := providerSmokeMatches(entries, providerSmokeSchemaRejectionMarkers)
	if !rejected {
		return
	}
	verdict := "could not regenerate the phase's schema to explain the rejection"
	if schema, err := providerSmokePhaseSchema(item); err == nil {
		verdict = providerSmokeSchemaVerdict(schema)
		t.Logf("provider smoke (%s): schema handed to the CLI: %s", smoke.providerName, schema)
	} else {
		t.Logf("provider smoke (%s): %v", smoke.providerName, err)
	}
	dumpProviderSmokeDiagnostics(t, app, item.ID, collector)
	t.Fatalf(
		"SCHEMA ACCEPTANCE FAILED (%s): the CLI refused the generated envelope schema (matched %q).\nCLI said: %s\nVerdict: %s",
		smoke.providerName, marker, entry, verdict,
	)
}

// assertProviderSmokeEnvelopeRoundTrip is dimension 2: the run reached done
// through a real envelope, and that envelope carried the phase's declared
// output. It returns the declared output so the branch assertions can follow it.
func assertProviderSmokeEnvelopeRoundTrip(
	t *testing.T, app *App, smoke providerSmokeCase, item store.WorkItem, collector *providerSmokeCollector,
) string {
	t.Helper()
	if engine.State(item.State) != engine.StateDone {
		failProviderSmokeRun(t, app, smoke, item, collector,
			"ENVELOPE ROUND-TRIP FAILED (%s): run ended %s/%q, want done",
			smoke.providerName, item.State, item.Reason,
		)
	}
	if item.Reason != "" {
		failProviderSmokeRun(t, app, smoke, item, collector,
			"ENVELOPE ROUND-TRIP FAILED (%s): a done run carries typed reason %q",
			smoke.providerName, item.Reason,
		)
	}
	phases, err := app.store.ListWorkItemPhases(item.ID)
	if err != nil {
		t.Fatalf("provider smoke (%s): list phase attempts: %v", smoke.providerName, err)
	}
	if len(phases) == 0 {
		failProviderSmokeRun(t, app, smoke, item, collector,
			"ENVELOPE ROUND-TRIP FAILED (%s): a done run persisted no phase attempt", smoke.providerName)
	}
	completed := phases[len(phases)-1]
	if completed.PhaseID != providerSmokePhaseID || completed.Status != "completed" {
		failProviderSmokeRun(t, app, smoke, item, collector,
			"ENVELOPE ROUND-TRIP FAILED (%s): final attempt is phase %q status %q, want %q completed",
			smoke.providerName, completed.PhaseID, completed.Status, providerSmokePhaseID,
		)
	}
	if len(completed.OutputEnvelope) == 0 {
		failProviderSmokeRun(t, app, smoke, item, collector,
			"ENVELOPE ROUND-TRIP FAILED (%s): completed phase %q recorded no envelope",
			smoke.providerName, completed.PhaseID,
		)
	}
	declared, err := providerSmokeDeclaredOutput(completed.OutputEnvelope)
	if err != nil {
		failProviderSmokeRun(t, app, smoke, item, collector,
			"ENVELOPE ROUND-TRIP FAILED (%s): %v\nenvelope: %s",
			smoke.providerName, err, completed.OutputEnvelope,
		)
	}

	// The run record's declared deliverable must agree with the phase envelope
	// it is sourced from; a run whose outputs never surface is not a round-trip.
	detail, err := app.WorkflowGetItem(item.ID)
	if err != nil {
		t.Fatalf("provider smoke (%s): read run detail: %v", smoke.providerName, err)
	}
	runOutput, ok := detail.Outputs[providerSmokeOutput].(string)
	if !ok || strings.TrimSpace(runOutput) != strings.TrimSpace(declared) {
		failProviderSmokeRun(t, app, smoke, item, collector,
			"ENVELOPE ROUND-TRIP FAILED (%s): run output %q = %#v, want the phase envelope's %q",
			smoke.providerName, providerSmokeOutput, detail.Outputs[providerSmokeOutput], declared,
		)
	}
	t.Logf("provider smoke (%s): envelope round-trip ok — attempts=%d outputs.%s=%q",
		smoke.providerName, len(phases), providerSmokeOutput, declared)
	return declared
}

// assertProviderSmokeBranchRules is dimension 3: spec §9's workspace rules held
// for a real run. A writing phase gets its own worktree on its own branch cut
// from the profile's base branch, its work lands there, and the project checkout
// is untouched.
func assertProviderSmokeBranchRules(
	t *testing.T, app *App, smoke providerSmokeCase, repo string,
	item store.WorkItem, declaredOutput string, collector *providerSmokeCollector,
) {
	t.Helper()
	if item.WorktreePath == "" || item.Branch == "" {
		failProviderSmokeRun(t, app, smoke, item, collector,
			"BRANCH-RULE CONFORMANCE FAILED (%s): writing run recorded no isolated workspace (worktree=%q branch=%q)",
			smoke.providerName, item.WorktreePath, item.Branch,
		)
	}
	if gitops.SameFilesystemPath(item.WorktreePath, repo) {
		failProviderSmokeRun(t, app, smoke, item, collector,
			"BRANCH-RULE CONFORMANCE FAILED (%s): writing run executed in the project checkout %q",
			smoke.providerName, repo,
		)
	}
	if !strings.Contains(item.Branch, "workflow-"+providerSmokeWorkflowID+"-") {
		failProviderSmokeRun(t, app, smoke, item, collector,
			"BRANCH-RULE CONFORMANCE FAILED (%s): branch %q does not carry the run's workflow prefix",
			smoke.providerName, item.Branch,
		)
	}
	if item.BaseBranch != "main" {
		failProviderSmokeRun(t, app, smoke, item, collector,
			"BRANCH-RULE CONFORMANCE FAILED (%s): worktree was cut from %q, want the profile's base branch main",
			smoke.providerName, item.BaseBranch,
		)
	}
	if head := providerSmokeGitOutput(t, item.WorktreePath, "rev-parse", "--abbrev-ref", "HEAD"); head != item.Branch {
		failProviderSmokeRun(t, app, smoke, item, collector,
			"BRANCH-RULE CONFORMANCE FAILED (%s): worktree HEAD is %q, want the run's branch %q",
			smoke.providerName, head, item.Branch,
		)
	}

	// The phase thread is the record of where the provider session actually ran.
	phases, err := app.store.ListWorkItemPhases(item.ID)
	if err != nil {
		t.Fatalf("provider smoke (%s): list phase attempts: %v", smoke.providerName, err)
	}
	thread, err := app.store.GetThread(phases[len(phases)-1].ThreadID)
	if err != nil {
		t.Fatalf("provider smoke (%s): read phase thread: %v", smoke.providerName, err)
	}
	if !gitops.SameFilesystemPath(thread.WorkspacePath, item.WorktreePath) ||
		!gitops.SameFilesystemPath(thread.WorktreePath, item.WorktreePath) ||
		thread.Branch != item.Branch {
		failProviderSmokeRun(t, app, smoke, item, collector,
			"BRANCH-RULE CONFORMANCE FAILED (%s): phase thread ran in workspace=%q worktree=%q branch=%q, want the run's %q / %q",
			smoke.providerName, thread.WorkspacePath, thread.WorktreePath, thread.Branch,
			item.WorktreePath, item.Branch,
		)
	}

	// The declared output is a path the phase says it created; it must exist in
	// the worktree and nowhere near the project checkout.
	created := declaredOutput
	if !filepath.IsAbs(created) {
		created = filepath.Join(item.WorktreePath, created)
	}
	// Existence is checked before containment on purpose: canonicalising a path
	// that does not exist silently falls back to the uncleaned form, which would
	// then report a missing file as "outside the worktree".
	body, err := os.ReadFile(created)
	if err != nil {
		failProviderSmokeRun(t, app, smoke, item, collector,
			"BRANCH-RULE CONFORMANCE FAILED (%s): declared output %q is not readable at %q: %v",
			smoke.providerName, declaredOutput, created, err,
		)
	}
	if strings.TrimSpace(string(body)) == "" {
		failProviderSmokeRun(t, app, smoke, item, collector,
			"BRANCH-RULE CONFORMANCE FAILED (%s): declared output %q at %q is empty",
			smoke.providerName, declaredOutput, created,
		)
	}
	// Both sides are canonicalised: the worktree sits under a temp root that can
	// itself be a symlink, and a comparison that ignored that would reject a file
	// sitting exactly where it belongs.
	worktreeRoot := gitops.CanonicalPath(item.WorktreePath) + string(filepath.Separator)
	if !strings.HasPrefix(gitops.CanonicalPath(created)+string(filepath.Separator), worktreeRoot) {
		failProviderSmokeRun(t, app, smoke, item, collector,
			"BRANCH-RULE CONFORMANCE FAILED (%s): declared output %q resolves to %q, outside the run's worktree %q",
			smoke.providerName, declaredOutput, created, item.WorktreePath,
		)
	}

	// The project checkout must be exactly as the fixture left it: no new file,
	// no modification, still on its own branch.
	if dirty := providerSmokeGitOutput(t, repo, "status", "--porcelain"); dirty != "" {
		failProviderSmokeRun(t, app, smoke, item, collector,
			"BRANCH-RULE CONFORMANCE FAILED (%s): the project checkout %q was written to:\n%s",
			smoke.providerName, repo, dirty,
		)
	}
	if head := providerSmokeGitOutput(t, repo, "rev-parse", "--abbrev-ref", "HEAD"); head != "main" {
		failProviderSmokeRun(t, app, smoke, item, collector,
			"BRANCH-RULE CONFORMANCE FAILED (%s): the project checkout %q is on branch %q, want main",
			smoke.providerName, repo, head,
		)
	}
	t.Logf("provider smoke (%s): branch rules ok — worktree=%q branch=%q base=%q file=%q",
		smoke.providerName, item.WorktreePath, item.Branch, item.BaseBranch, created)
}

// failProviderSmokeRun dumps the run's diagnostics and then fails with the
// caller's message. Every gate failure goes through it, so a red gate is always
// diagnosable from the log alone — and an environmental cause (an unauthenticated
// CLI whose probe nonetheless answered) is named as itself rather than reported
// as a schema or envelope defect.
func failProviderSmokeRun(
	t *testing.T, app *App, smoke providerSmokeCase, item store.WorkItem,
	collector *providerSmokeCollector, format string, args ...any,
) {
	t.Helper()
	dumpProviderSmokeDiagnostics(t, app, item.ID, collector)
	entries, _ := collector.snapshot()
	if entry, marker, unauthenticated := providerSmokeMatches(entries, providerSmokeAuthMarkers); unauthenticated {
		t.Fatalf(
			"AUTHENTICATION FAILED (%s): the %s binary is installed but not logged in (matched %q).\nCLI said: %s\nfix: %s",
			smoke.providerName, smoke.providerName, marker, entry, smoke.loginHint,
		)
	}
	t.Fatalf(format, args...)
}

// removeProviderSmokeWorktree tears down the worktree the run provisioned.
// Cleanup policy keeps unlanded worktrees for a human (spec §9), which is right
// for the product and wrong for a test fixture, so the gate removes its own.
func removeProviderSmokeWorktree(t *testing.T, app *App, repo, itemID string) {
	t.Helper()
	item, err := app.store.GetWorkItem(itemID)
	if err != nil {
		t.Logf("provider smoke: read run %s for worktree cleanup: %v", itemID, err)
		return
	}
	if item.WorktreePath == "" {
		return
	}
	if err := app.gitCore().RemoveWorktreeForce(repo, item.WorktreePath, true); err != nil {
		t.Logf("provider smoke: remove worktree %q: %v", item.WorktreePath, err)
	}
}
