//go:build providersmoke

// Dimension 4 of the real-provider gate: IMPORTED-BRANCH RESUME (Claude only).
//
// WHAT IS UNPROVEN WITHOUT THIS. A Claude transcript is a DAG, and session
// import makes one AO thread per branch. Only the file's ACTIVE branch can
// carry the session ref, so every other branch is materialised LAZILY: the
// first time such a thread starts a session, `materializeImportedClaudeBranch`
// (app_session_import_branch.go) cuts the source transcript at that branch's
// leaf through `sessionfork.WriteForkFileThroughUUID` and points the thread at
// the new file. Unit tests assert the file's shape and the row state. Nothing
// in the mocked suites can assert the only thing that actually matters: that a
// REAL `claude` accepts that file for `--resume` and carries on the branch's
// conversation. A mock accepts any JSONL.
//
// WHAT THIS SCENARIO DOES. It builds a genuinely multi-leaf transcript with
// the real CLI — a shared prefix turn, one turn that becomes the abandoned
// branch, then one `--resume-session-at` turn back at the prefix leaf, which
// is what makes the file branch — enumerates its branches through the
// importer's own `sessionimport.LoadSession`, cuts the NON-ACTIVE branch with
// the production fork writer, and resumes the result for one trivial turn.
//
// It subsumes the cheaper "cut the file at its second-to-last uuid" variant:
// the abandoned branch's leaf is mid-file by construction (the resume-at turn's
// rows sit after it), so this is a mid-chain cut that additionally proves the
// branch enumeration picked a leaf the CLI will take.
//
// THE ASSERTION IS CONTENT, NOT EXIT CODE. Each of the two branches is given
// its own codeword. A fork that loads but resumes the wrong conversation, or
// that loads an empty context, answers with the other branch's codeword or
// with neither — both are failures, and both are invisible to a "did the
// process start" check.
//
// COST: four real turns. Everything is deliberately trivial and tool-free, and
// the whole scenario runs under one budget so a wedged turn cannot push the
// package past the Makefile's `-timeout`.
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/claude/sessionfork"
	"agent-overflow/internal/provider/claude/sessionimport"
	"agent-overflow/internal/testutil"
)

// providerSmokeImportBranchBudget bounds the WHOLE scenario rather than each
// turn. Four trivial, tool-free turns are seconds of work each; a per-turn
// deadline generous enough for a slow one would multiply into a worst case
// that outruns `make provider-smoke`'s own `-timeout 15m` — which the two
// workflow legs' 6-minute deadlines already spend most of.
const providerSmokeImportBranchBudget = 3 * time.Minute

// providerSmokeForkTitle is the custom title stamped on the materialised fork.
// Production passes the thread's title here; the value only has to identify
// the file to a human who finds one that outlived a failed run.
const providerSmokeForkTitle = "agent-overflow provider smoke (imported branch)"

func TestProviderSmokeClaudeImportedBranchResume(t *testing.T) {
	smoke := providerSmokeClaudeCase()
	app, _ := setupE2EApp(t)
	binaryPath := preflightProviderBinary(t, app, smoke)
	preflightProviderAuth(t, app, smoke, binaryPath)

	budget, cancelBudget := context.WithTimeout(context.Background(), providerSmokeImportBranchBudget)
	defer cancelBudget()

	// A fresh temp checkout, like the workflow leg: the Claude project slug is
	// derived from the session's cwd, so every transcript this scenario writes
	// lands in a project directory of its own that teardown can empty.
	driver := newProviderSmokeClaudeDriver(t, budget, binaryPath, testutil.InitGitRepo(t), smoke.model)

	// Turn 1 — the prefix both branches share, and the row the rewind
	// anchors on.
	prefix := driver.runTurn(t, providerSmokeClaudeTurn{
		label:  "prefix turn",
		prompt: "This is a scripted client test. Reply with exactly: READY",
	})
	sessionID := prefix.sessionID
	if sessionID == "" || prefix.leafUUID == "" {
		driver.fail(t, "IMPORTED-BRANCH RESUME FAILED: the prefix turn reported session=%q leaf=%q; without both there is no transcript to branch",
			sessionID, prefix.leafUUID)
	}

	// Turn 2 — the branch that the rewind below abandons. Its codeword is the
	// thing the materialised fork must be able to recall.
	abandonedWord := providerSmokeCodeword()
	abandoned := driver.runTurn(t, providerSmokeClaudeTurn{
		label:  "abandoned-branch turn",
		resume: sessionID,
		prompt: providerSmokeCodewordPrompt(abandonedWord),
	})
	driver.requireSameSession(t, "--resume", sessionID, abandoned)

	// Turn 3 — rewind to the prefix leaf and say something else. This is what
	// makes the transcript multi-leaf: the CLI chains the new rows onto the
	// prefix leaf, leaving turn 2's leaf on a branch nothing will resume.
	onBranch, err := claude.ResumeAtOnActiveBranch(sessionID, driver.workspace, prefix.leafUUID)
	if err != nil || !onBranch {
		driver.fail(t, "IMPORTED-BRANCH RESUME FAILED: production's own resume-at validator refuses the prefix leaf %s of session %s (onBranch=%v err=%v); the rewind that builds the second branch cannot be attempted",
			prefix.leafUUID, sessionID, onBranch, err)
	}
	activeWord := providerSmokeCodeword()
	active := driver.runTurn(t, providerSmokeClaudeTurn{
		label:    "active-branch turn",
		resume:   sessionID,
		resumeAt: prefix.leafUUID,
		prompt:   providerSmokeCodewordPrompt(activeWord),
	})
	driver.requireSameSession(t, "--resume-session-at", sessionID, active)

	sourcePath, err := sessionfork.LocateSessionFile(sessionID, driver.workspace)
	if err != nil {
		driver.fail(t, "IMPORTED-BRANCH RESUME FAILED: the transcript for session %s is not where production looks for it: %v", sessionID, err)
	}

	// The importer's own reader, not a private walk: the leaf this scenario
	// forks at must be the leaf `thread_import_state` would have recorded.
	loaded, err := sessionimport.LoadSession(sourcePath)
	if err != nil {
		driver.fail(t, "IMPORTED-BRANCH RESUME FAILED: the importer cannot read the transcript it just helped write (%s): %v", sourcePath, err)
	}
	defer loaded.Close()
	branch, found := providerSmokeNonActiveBranch(loaded.Branches, abandoned.leafUUID)
	if !found {
		driver.fail(t,
			"IMPORTED-BRANCH RESUME FAILED: %s enumerated %d branch(es) %v and none of the non-active ones carries the abandoned turn's leaf %s.\n"+
				"A single branch means `--resume-session-at` no longer rewinds in place — the whole one-thread-per-branch import model rests on it (see internal/provider/claude/sessionimport).",
			sourcePath, len(loaded.Branches), providerSmokeBranchLeaves(loaded.Branches), abandoned.leafUUID)
	}
	t.Logf("provider smoke (claude): transcript %s has %d branches; forking the non-active leaf %s",
		filepath.Base(sourcePath), len(loaded.Branches), branch.LeafUUID)

	// THE production call. materializeImportedClaudeBranch does exactly this
	// with the leaf it read out of thread_import_state.
	forkID, forkPath, _, err := sessionfork.WriteForkFileThroughUUID(sourcePath, branch.LeafUUID, providerSmokeForkTitle)
	if err != nil {
		driver.fail(t, "IMPORTED-BRANCH RESUME FAILED: cutting %s at leaf %s produced no fork: %v", sourcePath, branch.LeafUUID, err)
	}
	driver.trackTranscript(forkPath)
	if forkID == sessionID {
		driver.fail(t, "IMPORTED-BRANCH RESUME FAILED: the fork reused the source session id %s; a resume of it would append to the source transcript", forkID)
	}
	// Release the source handle before the CLI opens the directory again.
	if err := loaded.Close(); err != nil {
		t.Logf("provider smoke (claude): close transcript %s: %v", sourcePath, err)
	}

	answer := driver.runTurn(t, providerSmokeClaudeTurn{
		label:  "imported-branch fork resume",
		resume: forkID,
		prompt: "What codeword did I ask you to remember? Reply with the codeword and nothing else.",
	})
	if answer.sessionID != forkID {
		driver.fail(t, "IMPORTED-BRANCH RESUME FAILED: `--resume %s` came up as session %q; the CLI did not continue the file the fork writer produced",
			forkID, answer.sessionID)
	}
	spoken := strings.ToUpper(answer.text)
	switch {
	case strings.Contains(spoken, activeWord):
		driver.fail(t,
			"IMPORTED-BRANCH RESUME FAILED: the fork resumed the WRONG branch — it answered with the active branch's codeword %s instead of the forked branch's %s.\nCLI said: %s",
			activeWord, abandonedWord, providerSmokeTruncate(answer.text))
	case !strings.Contains(spoken, abandonedWord):
		driver.fail(t,
			"IMPORTED-BRANCH RESUME FAILED: the fork resumed but did not carry its branch's conversation — the codeword %s set on that branch is absent from the answer.\nCLI said: %s",
			abandonedWord, providerSmokeTruncate(answer.text))
	}
	t.Logf("provider smoke (claude): imported-branch resume ok — source=%s fork=%s leaf=%s answer=%q",
		sessionID, forkID, branch.LeafUUID, providerSmokeTruncate(answer.text))
}

// providerSmokeClaudeTurn is one real turn's inputs. Resume / ResumeAt mean
// exactly what they mean on provider.SessionOptions.
type providerSmokeClaudeTurn struct {
	// label names the turn in failures; a red gate has four of them.
	label    string
	prompt   string
	resume   string
	resumeAt string
}

// providerSmokeClaudeTurnResult is what one turn tells the scenario: the
// session the CLI reported on `system/init`, the settled leaf its own live
// tracker picked (the same value production feeds `--resume-session-at`), and
// the assistant text.
type providerSmokeClaudeTurnResult struct {
	sessionID string
	leafUUID  string
	text      string
}

// providerSmokeClaudeDriver runs trivial turns against the real CLI through the
// production provider package — `claude.ConfigFromOptions` + `claude.NewSession`
// — rather than hand-built argv, so the flags under test (`--resume`,
// `--resume-session-at`) are the ones production emits.
//
// It also owns teardown. Every transcript it causes lands in the developer's
// REAL `~/.claude/projects/<slug>/`, and a leaked one is not inert: it shows up
// in the user's own `claude --resume` picker forever. So each session id the
// CLI reports is tracked as it appears — including one we did not ask for —
// and the fork's path is tracked explicitly.
type providerSmokeClaudeDriver struct {
	budget     context.Context
	binaryPath string
	workspace  string
	model      string
	collector  *providerSmokeCollector

	mu         sync.Mutex
	sessionIDs []string
	paths      []string
}

func newProviderSmokeClaudeDriver(
	t *testing.T, budget context.Context, binaryPath, workspace, model string,
) *providerSmokeClaudeDriver {
	t.Helper()
	driver := &providerSmokeClaudeDriver{
		budget:     budget,
		binaryPath: binaryPath,
		workspace:  workspace,
		model:      model,
		collector:  &providerSmokeCollector{},
	}
	t.Cleanup(func() { driver.removeTranscripts(t) })
	return driver
}

// runTurn spawns one real session, sends one message, and waits for the turn to
// complete. Read-only is deliberate: it is the unattended tier — anything that
// would otherwise prompt is DENIED rather than asked — so no turn here can park
// on an approval with nobody to answer it.
func (d *providerSmokeClaudeDriver) runTurn(t *testing.T, turn providerSmokeClaudeTurn) providerSmokeClaudeTurnResult {
	t.Helper()

	cfg := claude.ConfigFromOptions(provider.SessionOptions{
		Provider:    string(provider.Claude),
		Model:       d.model,
		WorkDir:     d.workspace,
		Resume:      turn.resume,
		ResumeAt:    turn.resumeAt,
		Mode:        provider.ModeChat,
		RuntimeMode: provider.RuntimeReadOnly,
	})
	cfg.Binary = d.binaryPath

	var (
		mu    sync.Mutex
		text  strings.Builder
		fatal string
	)
	settled := make(chan struct{})
	var settleOnce sync.Once
	settle := func() { settleOnce.Do(func() { close(settled) }) }

	onEvent := func(event provider.ProviderEvent) {
		// The collector keeps the bounded, filtered diagnostic log the whole
		// gate is read from; it selects the events that explain a failure.
		d.collector.observeProviderEvent(event)
		switch event.Kind {
		case provider.EventTextDelta:
			mu.Lock()
			text.WriteString(event.Content)
			mu.Unlock()
		case provider.EventTurnComplete:
			settle()
		case provider.EventSessionStatus:
			// A process that dies (a refused `--resume`, an auth failure) is
			// reported here with its stderr tail on Meta. Only a death is
			// terminal — every other status is live-state noise.
			if strings.TrimSpace(event.Content) != "error" {
				return
			}
			mu.Lock()
			if fatal == "" {
				fatal = providerSmokeTruncate(string(event.Meta))
			}
			mu.Unlock()
			settle()
		}
	}

	// The session's own context is NOT the budget: a blown budget should reach
	// the CLI as Close's staged stdin-close / SIGTERM / SIGKILL, not as an
	// immediate process-group kill on a binary that may be mid token refresh.
	sessionCtx, cancelSession := context.WithCancel(context.Background())
	sess, err := claude.NewSession(sessionCtx, "provider-smoke-import-branch", cfg, onEvent)
	if err != nil {
		cancelSession()
		d.fail(t, "IMPORTED-BRANCH RESUME FAILED (%s): spawning %s failed: %v", turn.label, d.binaryPath, err)
	}
	var closeOnce sync.Once
	closeSession := func() {
		closeOnce.Do(func() {
			if closeErr := sess.Close(); closeErr != nil {
				t.Logf("provider smoke (claude): close %s session: %v", turn.label, closeErr)
			}
			cancelSession()
		})
	}
	defer closeSession()

	if err := sess.Send(d.budget, turn.prompt, provider.SendOptions{InteractionMode: provider.ModeChat}); err != nil {
		closeSession()
		if d.budget.Err() != nil {
			d.fail(t, "IMPORTED-BRANCH RESUME FAILED (%s): the scenario's %s budget was already spent when this turn tried to send",
				turn.label, providerSmokeImportBranchBudget)
		}
		d.fail(t, "IMPORTED-BRANCH RESUME FAILED (%s): sending the prompt failed: %v", turn.label, err)
	}

	select {
	case <-settled:
	case <-d.budget.Done():
		closeSession()
		d.fail(t, "IMPORTED-BRANCH RESUME FAILED (%s): the scenario's %s budget expired with the turn still running",
			turn.label, providerSmokeImportBranchBudget)
	}

	// Close before reading the transcript back: the exited process is what
	// guarantees every row of this turn is on disk.
	closeSession()

	mu.Lock()
	result := providerSmokeClaudeTurnResult{
		sessionID: sess.SessionID(),
		leafUUID:  sess.CanonicalLeafUUID(),
		text:      text.String(),
	}
	failure := fatal
	mu.Unlock()

	if failure != "" {
		d.fail(t, "IMPORTED-BRANCH RESUME FAILED (%s): the CLI process died before the turn completed.\nCLI said: %s", turn.label, failure)
	}
	if result.sessionID == "" {
		d.fail(t, "IMPORTED-BRANCH RESUME FAILED (%s): the CLI never reported a session id, so no transcript can be located", turn.label)
	}
	d.trackSession(result.sessionID)
	t.Logf("provider smoke (claude): %s ok — session=%s leaf=%s", turn.label, result.sessionID, result.leafUUID)
	return result
}

// requireSameSession fails when a resume minted a new session id. Production
// tolerates the change (triage adopts whatever `system/init` reports), but the
// imported-branch model does not: it assumes a resume APPENDS to the transcript
// it resumed, which is what lets one file hold several branches at all.
func (d *providerSmokeClaudeDriver) requireSameSession(
	t *testing.T, flag, want string, result providerSmokeClaudeTurnResult,
) {
	t.Helper()
	if result.sessionID == want {
		return
	}
	d.fail(t,
		"IMPORTED-BRANCH RESUME FAILED: `%s` moved the conversation from session %s to %s instead of appending to it.\n"+
			"One transcript per resume means a file can no longer hold several branches, which is the premise of both the importer's branch enumeration and materializeImportedClaudeBranch.",
		flag, want, result.sessionID)
}

func (d *providerSmokeClaudeDriver) trackSession(sessionID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sessionIDs = append(d.sessionIDs, sessionID)
}

func (d *providerSmokeClaudeDriver) trackTranscript(path string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.paths = append(d.paths, path)
}

// removeTranscripts deletes every transcript this scenario caused from the
// developer's real Claude home, then the project directory itself once it is
// empty. Failures are logged rather than fatal — teardown runs after a verdict
// is already recorded, and a leftover file is a mess, not a wrong answer.
func (d *providerSmokeClaudeDriver) removeTranscripts(t *testing.T) {
	t.Helper()
	d.mu.Lock()
	sessionIDs := append([]string(nil), d.sessionIDs...)
	paths := append([]string(nil), d.paths...)
	d.mu.Unlock()

	for _, sessionID := range sessionIDs {
		path, err := sessionfork.LocateSessionFile(sessionID, d.workspace)
		if err != nil {
			t.Logf("provider smoke (claude): locate transcript for session %s to clean up: %v", sessionID, err)
			continue
		}
		paths = append(paths, path)
	}

	removed := make(map[string]struct{}, len(paths))
	dirs := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		if _, done := removed[path]; done {
			continue
		}
		removed[path] = struct{}{}
		dirs[filepath.Dir(path)] = struct{}{}
		// RemoveSessionTranscript is production's own deleter: it takes the
		// `<sessionID>/` sidecar with the JSONL and refuses a path that is not
		// a session file.
		if err := sessionfork.RemoveSessionTranscript(path); err != nil {
			t.Logf("provider smoke (claude): remove transcript %s: %v", path, err)
		}
	}
	for dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) > 0 {
			continue
		}
		if err := os.Remove(dir); err != nil {
			t.Logf("provider smoke (claude): remove emptied project dir %s: %v", dir, err)
		}
	}
}

// fail dumps the collected provider diagnostics and then fails, naming an
// unauthenticated CLI as itself rather than reporting it as a fork defect —
// the same posture (and the same marker table) as failProviderSmokeRun.
func (d *providerSmokeClaudeDriver) fail(t *testing.T, format string, args ...any) {
	t.Helper()
	entries, dropped := d.collector.snapshot()
	if len(entries) == 0 {
		t.Log("DIAGNOSTICS provider events: none recorded")
	}
	for _, entry := range entries {
		t.Logf("DIAGNOSTICS %s", entry)
	}
	if dropped > 0 {
		t.Logf("DIAGNOSTICS: %d further provider diagnostics dropped at the retention cap", dropped)
	}
	if entry, marker, unauthenticated := providerSmokeMatches(entries, providerSmokeAuthMarkers); unauthenticated {
		t.Fatalf(
			"AUTHENTICATION FAILED (claude): the claude binary is installed but not logged in (matched %q).\nCLI said: %s\nfix: run `claude login`",
			marker, entry,
		)
	}
	t.Fatalf(format, args...)
}

// providerSmokeNonActiveBranch returns the branch carrying leafUUID from among
// the ones that are NOT the file's active branch — i.e. exactly the population
// import materialises lazily. sessionimport orders branches by leaf file
// position with the active branch last, and the importer's own resume-ref rule
// reads that same ordering.
//
// The match is "leafUUID appears on the chain" rather than "is the chain's
// leaf": the live leaf tracker's settled leaf and the DAG's leaf are the same
// row on a healthy file, but a trailing row the tracker never settled on would
// make an equality check fail for a fork that is perfectly resumable.
func providerSmokeNonActiveBranch(branches []sessionimport.Branch, leafUUID string) (sessionimport.Branch, bool) {
	if len(branches) < 2 || leafUUID == "" {
		return sessionimport.Branch{}, false
	}
	for _, branch := range branches[:len(branches)-1] {
		for _, row := range branch.Chain {
			if row.UUID == leafUUID {
				return branch, true
			}
		}
	}
	return sessionimport.Branch{}, false
}

func providerSmokeBranchLeaves(branches []sessionimport.Branch) []string {
	leaves := make([]string, 0, len(branches))
	for _, branch := range branches {
		leaves = append(leaves, branch.LeafUUID)
	}
	return leaves
}

// providerSmokeCodeword mints a token that cannot have been seen before, so a
// coherent-sounding answer carried over from an earlier run (or invented) can
// never satisfy the assertion. Uppercase hex only: no separator for a model to
// reformat away.
func providerSmokeCodeword() string {
	return "SMOKE" + strings.ToUpper(strings.ReplaceAll(uuid.NewString(), "-", ""))[:8]
}

func providerSmokeCodewordPrompt(codeword string) string {
	return fmt.Sprintf(
		"Remember this codeword for later: %s. Do not use any tools. Reply with exactly: STORED",
		codeword)
}
