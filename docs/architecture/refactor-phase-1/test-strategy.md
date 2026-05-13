# Phase 1 test-verification protocol

Operational guide for moving logic out of root-level `app_*.go` into
`internal/` packages without weakening or silently dropping behavior.
Written for an agent working in a worktree on one migration at a time.

The hard constraint that makes this cheap: the transport invariant
(`AGENTS.md`) freezes every `*App` method name + signature. Existing
`app_*_test.go` files that call those methods MUST keep compiling and
passing unchanged. A migration that "rewrites the test against the new
package" instead of preserving the App-level test is breaking the
constraint — refuse it.

## 1. Current test landscape

Numbers below were captured on `main` at the start of Phase 1 planning
(2026-05-12, Go 1.25, `make go-test`).

### 1.1 Top-line counts

| Surface | Source LOC | Test LOC | Test count | Notes |
|---|---:|---:|---:|---|
| `package main` (`app_*.go`) | 13,488 | 28,996 | 699 | 65 source + 75 test files; ~157s wall clock |
| `internal/*` (all packages) | n/a | n/a | 2,273 | 34 packages |
| Total binding-exposed methods | n/a | n/a | n/a | 171 wire-exposed methods; 351 total `(a *App)` receivers including unexported helpers |

`package main` carries the bulk of the integration coverage: real
SQLite, real provider mocks (NDJSON / JSON-RPC fixtures from
`internal/testutil/app.go`), real triage router, real settings. The
trade-off in the migration: shrink that file count + shift unit-level
logic into `internal/` while keeping the integration suite mostly
intact at the App boundary.

### 1.2 App-method unit tests — signal quality sample

Sampled four files of varying sizes to spot-check the existing bar.

- **`app_attachment_test.go`** (232 LOC, 9 tests). Strong signal. Tests
  the four binding methods (`UploadAttachment`, `ListAttachments`,
  `DeleteAttachment`, `GetAttachmentData`, `GetAttachmentThumbnail`)
  through the public surface, exercises cross-thread rejection, init
  guard, empty-slice non-nil contract, and the thumbnail-cache
  identity round-trip. The thumbnail test uses a real decodable PNG —
  exactly the kind of detail that gets lost in a sloppy migration.
- **`app_send_test.go`** (2,089 LOC, 33 tests). Mixed signal.
  Combines App-binding tests, lazy-session-start coordination
  semantics, queue interaction, and event-emit-order assertions.
  Several tests reach into `app.sendMessageFn`, `app.startSessionFn`,
  `app.testEmitHook` — these injection seams must survive any
  refactor that lifts code out of `app_send.go`. If `sendMessage`
  moves to a helper package, the helper must still be reachable via
  the same function-pointer hook OR the test must move with it
  (preferable: keep the hook on App, have App call into the helper).
- **`app_concurrent_test.go`** (518 LOC, 8 tests). Race-condition
  coverage: 50-goroutine creates, 10-goroutine racing
  `UpdateThreadModel`, concurrent shutdown. These tests rely on the
  shared `setupE2EApp` builder, real store, and real triage. They
  would NOT detect a deadlock introduced by changing a mutex's scope
  unless they're rerun under `-race` — see `make test-race` for the
  scoped race-detector run.
- **`app_bindings_test.go`** (970 LOC, 29 tests). Cross-cutting
  binding sanity: settings round-trip, model catalog, thread CRUD,
  generic options validation. Houses `newTestAppWithStore`
  (the lightweight builder most non-E2E tests use), `testThread`,
  and `defaultTestProjectID`. Anything that touches App field
  initialization (the struct literal at lines 926-933) is load-bearing
  for the whole test suite.

### 1.3 App-method integration / E2E tests

These exercise the full provider + triage + store pipeline against
shell-script mocks from `internal/testutil/app.go`. They are slow but
catch wire-shape regressions a unit test cannot:

- **`app_e2e_lifecycle_test.go`** (1,498 LOC, 24 tests).
  `setupE2EApp` + `capturedEventBus` is the gold-standard fixture:
  real `store.Store`, real `triage.NewRouter`, captured emissions
  through `bus.emit`, `bus.observeRouterEvent` hook on the router for
  pipeline syncpoints. Tests use `bus.nextProviderEventOfKind(...)`
  to block on routed events, then assert persisted items. **This
  fixture is the single most important asset for the migration.**
  Any change to App field initialization order must keep this builder
  green or you break dozens of tests at once.
- **`app_e2e_background_codex_test.go`** (621 LOC, 16 tests) and
  **`app_e2e_background_claude_test.go`**. End-to-end coverage of the
  Codex unifiedExec + spawn-agent yields and the Claude
  background-task lifecycle (invariant 25). These tests are tightly
  coupled to triage routing and the wire-typed signals from
  `provider/codex/protocol.go`. If a migration touches those signals,
  this suite is the canary.
- **`app_composer_integration_test.go`** (764 LOC). Composer +
  pre-send + queue interactions. Owns `app.testEmitHook` chains
  similar to `app_send_test.go`.
- **`app_discussion_integration_test.go`** (594 LOC). Multi-agent
  deliberation across two provider sessions. Hard to replace with
  package-local unit tests — it spans `discussion`, `triage`,
  `store`, and the App's deliberation map.
- **`app_pr_thread_integration_test.go`**, **`app_ship_integration_test.go`**,
  **`app_flush_queue_integration_test.go`**,
  **`app_transport_integration_test.go`**,
  **`app_mode_integration_test.go`**. Cross-cutting integration suites
  whose existence is itself test signal — these run the broadest
  wiring on each commit.

### 1.4 Internal package test signal

Quick coverage scan via `go test -count=1 -cover ./internal/...`:

| Package | Coverage | Notes |
|---|---:|---|
| `internal/stringsx` | 100.0% | Pure helpers. |
| `internal/terminal` | 91.5% | PTY ring buffer + manager. |
| `internal/workspacefiles` | 88.4% | File-search index. |
| `internal/diffsummary` | 87.7% | Pure formatter. |
| `internal/git` | 87.5% | Real git repos via `testutil`. |
| `internal/shellenv` | 88.2% | Shell-launcher PATH probe. |
| `internal/editor` | 86.8% | Editor catalog. |
| `internal/transport` | 86.4% | Includes the `methods_gen_test.go` integrity gate. |
| `internal/clientmode` | 86.2% | `--connect` bootstrap. |
| `internal/gitwatch` | 86.3% | FS watcher + polling fallback. |
| `internal/logging` | 86.2% | NDJSON logger. |
| `internal/discussion` | 85.5% | Deliberation registry. |
| `internal/settings` | 85.4% | Persistent JSON service. |
| `internal/provider/claude` | 85.3% | Parser + session. |
| `internal/uikeys` | 85.0% | Browser-style keybindings. |
| `internal/observability/replay` | 81.6% | NDJSON replay writer. |
| `internal/provider/codex` | 80.9% | App-server protocol. |
| `internal/wsllauncher` | 80.2% | WSL launcher pinning. |
| `internal/triage` | 78.9% | The big router. Heavy-fixture tests. |
| `internal/provider` | 76.2% | Shared interfaces. |
| `internal/provider/claude/sessionfork` | 71.1% | Fork-by-replay. |
| `internal/attachment` | 70.9% | Disk layout + thumbnailer. |
| `internal/wsldistro` | 70.0% | Cross-process config. |
| `internal/design` | 70.0% | Workdir manager + MCP server. |
| `internal/store` | 69.3% | SQLite. Wide schema. |
| `internal/checkpoint` | 63.4% | Git-ref message checkpoints. |
| `internal/observability/otel` | 61.4% | Optional tracing. |
| `internal/screenshot` | 58.0% | Headless Chrome. |
| `internal/platform` | 55.6% | WSL detection. |
| `internal/testutil` | 54.0% | Mock-binary writers; lower expected. |
| `internal/externalurl` | 46.3% | URL validator. |
| `internal/observability` | (no statements) | Top-level meta-package. |
| `internal/pathlinks` | 92.1% but **1 failing test** | New, in-progress; failing test `TestExtractAndValidate/missing_workspacePath_drops_relative_paths_but_keeps_absolute` is unrelated to Phase 1 but must be resolved before any baseline can be trusted. |

Repo-root `agent-overflow` (the `package main`): 70.5% coverage with
the 699 tests.

**Action item:** The `internal/pathlinks` failing test must be fixed
or quarantined before the migration starts; otherwise every
"baseline check" is contaminated by a flaky baseline. The package is
brand new (`?? internal/pathlinks/` in git status) so the failure
likely belongs to its author, not to Phase 1.

### 1.5 Shared test helpers

- **`internal/testutil/`** — mock provider scripts
  (`WriteMockClaudeSession`, `WriteMockClaudeScript`,
  `MockClaudeStreamedText`, `MockClaudeStreamedThinking`,
  `WriteMockCodexSession`, `WriteMockGhCLI`), git repo seed
  (`InitGitRepo`, `InitGitRepoWithCommits`, `RunGit`), store seed
  (`EnsureProject`). Importable by every package — `*App` helpers
  cannot live here because App is in `package main`.
- **`app_test_helpers_test.go`** — `defaultTestProjectID`,
  `ensureDefaultTestProject`, `createTestThread`,
  `projectPathForThread`, `setThreadProject`. Used by every
  App-level test. **Must stay in `package main`** and continue
  serving the `*App` literal-struct construction pattern used by
  `newTestAppWithStore` (line 914 of `app_bindings_test.go`),
  `newTestApp` (line 528 of `app_checkpoint_test.go`),
  `newAppWithStore` (line 183 of `app_codex_reconcile_test.go`),
  `newTestAppWithDesign` (line 235 of `app_design_test.go`),
  `newAppWithTerminals` (line 16 of `app_terminal_test.go`),
  `setupE2EApp` (line 238 of `app_e2e_lifecycle_test.go`).
- **`app_test_event_helpers_test.go`** — pairs with the event-bus
  capture machinery in `app_e2e_lifecycle_test.go`.

### 1.6 Transport-level safety net (already in place)

This is the strongest free guard the codebase already gives the
migration:

- `internal/transport/methods_gen.go` is regenerated from a Go-AST
  walk of all `func (a *App) <Name>(...)` declarations in
  `*.go` at the repo root (see `internal/transport/methodgen/`).
- `internal/transport/methods_gen_test.go::TestMethodsGen_InSync`
  re-runs the generator into a tempfile and bytes-diffs against the
  committed output. **Any rename, signature change, deletion, or
  visibility change to an exported `(a *App)` method fails this test.**
- `TestLocalOnlyMethods_AllExist` guards the LAN-bind allow-list
  against typos — every name in `LocalOnlyMethods` must correspond
  to a real entry in `GeneratedMethods`.
- `frontend/bindings/agent-overflow/app.ts` (2,100 LOC, 171 exported
  functions, generated by Wails). `pnpm run check` (which the
  `make check` target depends on) runs TypeScript across the whole
  frontend, so a signature drift visible to TS surfaces here too —
  though only if bindings get regenerated. The migration MUST
  regenerate bindings on every method-touching commit; see §3.

The migration plan: lift bodies, not signatures. These guards make
that mechanically enforceable.

### 1.7 Known structural gaps

- **`exitCode != 0` regressions are caught by `go test`, race-window
  regressions are not.** `make go-test` is single-threaded per
  package, doesn't run `-race`. Use `make test-race` for the scoped
  race-detector run; it's currently scoped to `./internal/transport/...`,
  `./internal/wsllauncher/...`, `./internal/clientmode/...`,
  `./internal/editor/...`, and `.` (the root). Anything moved out of
  `package main` into a not-listed `internal/` package loses race-
  detector coverage unless `test-race` is updated to include it.
- **Logging side-effects are untested.** `log.Printf` calls scattered
  through `app_approval.go`, `app_emit.go`, `app_errors.go`,
  `app_runtime_mode.go`, `app_codex_reconcile.go`,
  `app_checkpoint.go`, `app_chat_bar.go`, etc. carry diagnostic
  context. No test grep asserts these stay; a migration that drops a
  `log.Printf` will pass all tests.
- **`a.emit(...)` event names** are checked by tests that read them
  via `capturedEventBus`, but new event names introduced or old ones
  renamed do NOT fail any structural test — the frontend listener
  would simply fall silent at runtime. The TypeScript bindings cover
  method names + types but not event-name strings.

## 2. Per-migration test protocol

This is the operational checklist. An agent doing a Phase 1 migration
in a worktree runs through these items in order. Items are written so
they can be copy-pasted into the work log; each is a concrete command
+ a concrete assertion. **Failure of any step aborts the migration.**

The checklist assumes one migration = one PR = one logical unit of
code (e.g. all of `app_attachment.go`, OR a self-contained helper
extracted from `app_send.go`, but not "lift everything in app_send.go
+ rewrite the queue subsystem"). Each migration is therefore expected
to pass these checks in ≤ 30 minutes of CI wall clock; longer than
that means the migration is too big and should be split.

### 2.1 Pre-flight (before touching code)

1. **Worktree is clean and at the agreed base commit.**
   ```
   git rev-parse HEAD                # record as $BASE
   git status --short                # must be empty
   ```
2. **Baseline `make test` exit status + test count.**
   ```
   make go-test 2>&1 | tee /tmp/baseline-go.txt
   grep -E '^ok|^FAIL|^---' /tmp/baseline-go.txt | tee /tmp/baseline-go-summary.txt
   ```
   Expected: zero `FAIL` lines, zero `--- FAIL:` lines.

   If `internal/pathlinks` is still failing at this point, abort and
   coordinate with the package author before proceeding.
3. **Baseline frontend check.**
   ```
   (cd frontend && pnpm run check 2>&1 | tee /tmp/baseline-fe.txt)
   ```
4. **Baseline per-package coverage for moved-from and moved-to packages.**
   ```
   go test -count=1 -coverprofile=/tmp/cov-main-before.out .
   go test -count=1 -coverprofile=/tmp/cov-target-before.out ./internal/<target>/...
   go tool cover -func=/tmp/cov-main-before.out | tail -5
   go tool cover -func=/tmp/cov-target-before.out | tail -5
   ```
   Record the `total:` line numbers from each. These are the regression
   guardrails: post-migration, neither may drop more than 1 percentage
   point absolute without an explicit, documented reason (test fixture
   moved with the code; a previously-dead branch is now compile-time
   unreachable; etc.).
5. **Snapshot the methods_gen.go file.**
   ```
   cp internal/transport/methods_gen.go /tmp/methods-before.go
   ```
   This is your transport-invariant baseline. After the migration this
   file must be byte-for-byte identical unless a method's name OR
   signature was deliberately and explicitly changed (which would
   itself be a breaking change requiring a separate scope conversation).

### 2.2 Choose-what-to-move (planning step)

6. **List every function and exported symbol in the source file(s).**
   ```
   grep -nE '^func ' app_<X>.go > /tmp/symbols-before.txt
   ```
   Each entry has a destination decision: stays on `*App` (binding),
   moves to `internal/<pkg>` (helper), or stays in `package main` as
   a non-method (rare; usually means refactor incomplete).

7. **For every test in the matching `app_<X>_test.go`, decide its
   post-migration home.** This is the **behavior-contract diff** the
   user named in the brief.

   For each test function, fill in the table below in the PR
   description (or `notes.md` in the worktree):

   | Test name | Asserts | New home | Notes |
   |---|---|---|---|
   | `TestUploadAttachmentBindingRoundTrip` | Upload+List round-trip via App method | stays in `app_attachment_test.go` (still hits binding) | unchanged |
   | `TestUploadAttachmentRequiresInitialisedStore` | Init guard returns `not initialized` | stays in `app_attachment_test.go` | binding-level guard |
   | `TestGetAttachmentThumbnailReturnsThumb` | Cached thumbnail round-trips identically | could move to `internal/attachment` as a public `Store.Thumbnail` test | only if a focused unit test in `internal/attachment` already proves the same property; if not, keep in app-level until the unit test lands |

   Rules:
   - **No assertion may disappear.** If a test moves, the new test
     must assert exactly what the old test asserted, against the new
     public API.
   - **If a test cannot move cleanly, keep it.** App-level tests are
     not waste — they cover the integration seam. A weaker reason
     than "the test is exercising provider + triage + store and can't
     be run against just the new package" must clear a higher bar.
   - **Logging side-effects:** for every `log.Printf` line removed
     during the migration, verify the new package emits an equivalent
     line (e.g. via `slog` or a `log.Printf`) OR justify the removal
     in the table. Default is "preserve the log line." Use
     ```
     grep -n 'log\.Printf\|log\.Print\b\|log\.Println' <moved files>
     ```
     to enumerate.

### 2.3 During the migration

8. **Keep `(*App).<Method>` signatures identical.** The body changes;
   the signature does not.
9. **Regenerate bindings after every commit that changes an `*App`
   method body (even if no signature change).** A signature drift you
   didn't intend is the migration's biggest risk.
   ```
   wails3 task generate:bindings
   git diff --stat frontend/bindings/agent-overflow/
   ```
   `app.ts` may legitimately re-order or re-format lines but the
   exported function list (`grep -c '^export function' frontend/bindings/agent-overflow/app.ts`)
   must equal the baseline.
10. **Run the methods_gen integrity test in a tight loop while
    iterating.** This is the cheapest signal — runs in <1 second:
    ```
    go test -count=1 -run TestMethodsGen_InSync ./internal/transport/
    ```
    If it fails, regenerate immediately:
    ```
    go run ./internal/transport/methodgen
    ```
    Then re-run the test. If after regeneration the diff vs
    `/tmp/methods-before.go` is non-empty, you've changed a method
    signature without intending to — investigate before continuing.

### 2.4 Post-migration verification

11. **`make go-test` must pass with the same test count as baseline.**
    ```
    make go-test 2>&1 | tee /tmp/after-go.txt
    diff /tmp/baseline-go-summary.txt <(grep -E '^ok|^FAIL|^---' /tmp/after-go.txt)
    ```
    Allowed diffs: per-package execution time changes, new package
    coverage lines appearing (you added an `internal/<X>` test file).
    **Not allowed:** any package goes from `ok` to `FAIL`, any
    package disappears, any new `--- FAIL:` line.

12. **`make go-test` test count is ≥ baseline, no new `t.Skip`.**
    ```
    go test -count=1 -list '.*' ./... 2>&1 | grep -c '^Test\|^Example\|^Benchmark'   # compare vs baseline
    grep -rn 't\.Skip\b' . | grep -v _test.go.orig
    ```
    Migration may NOT introduce a new `t.Skip` call. If a test is
    genuinely platform-gated, the gate must already exist on
    baseline.

13. **`pnpm run check` passes.**
    ```
    (cd frontend && pnpm run check) 2>&1 | tee /tmp/after-fe.txt
    ```
    TypeScript will fail if a binding signature drifted in a way the
    frontend caller can detect — that's the integration seam against
    drift.

14. **Bindings re-generation produces no diff vs the head of branch.**
    ```
    wails3 task generate:bindings
    git diff --exit-code frontend/bindings/   # must exit 0
    ```
    If this fails after a method body-only change, the change wasn't
    body-only.

15. **`methods_gen.go` unchanged from baseline.**
    ```
    diff /tmp/methods-before.go internal/transport/methods_gen.go   # must be empty
    ```
    If non-empty: a method's exported name changed. This is by
    definition a breaking change to the wire surface — escalate.

16. **Per-package coverage delta within tolerance.**
    ```
    go test -count=1 -coverprofile=/tmp/cov-main-after.out .
    go test -count=1 -coverprofile=/tmp/cov-target-after.out ./internal/<target>/...
    diff <(go tool cover -func=/tmp/cov-main-before.out | tail -1) <(go tool cover -func=/tmp/cov-main-after.out | tail -1)
    diff <(go tool cover -func=/tmp/cov-target-before.out | tail -1) <(go tool cover -func=/tmp/cov-target-after.out | tail -1)
    ```
    Allowed: `package main` total drops because some statements moved
    out (offset by `internal/<target>` rising correspondingly).
    Forbidden: both drop. Forbidden: `internal/<target>` rises but
    `package main` drops by more than the moved statement count.

    A rough sanity check on "moved statement count" is enough — count
    the lines deleted from `app_*.go` files and verify the sum of
    `before(main) - after(main) - after(target) + before(target)` is
    within ±50 LOC.

17. **For every public function introduced in `internal/<target>`,
    there is at least one test in `internal/<target>/*_test.go`
    exercising it.**
    ```
    grep -nE '^func [A-Z]' internal/<target>/*.go | grep -v _test.go > /tmp/new-exports.txt
    for fn in $(awk -F'func ' '{print $2}' /tmp/new-exports.txt | awk -F'(' '{print $1}'); do
      grep -l "$fn" internal/<target>/*_test.go || echo "MISSING TEST: $fn"
    done
    ```
    A "test" here means more than a compile-only call: the test must
    make at least one assertion about the function's return value,
    side-effect, or error.

18. **Logging side-effects preserved.**
    Run the inventory of `log.Printf` calls in the moved files (from
    step 7 of §2.2) and grep for equivalent log lines in the new
    location:
    ```
    git diff $BASE -- app_<X>.go | grep '^-.*log\.Printf' | sort > /tmp/log-removed.txt
    git diff $BASE -- internal/<target>/ | grep '^+.*log\.Printf' | sort > /tmp/log-added.txt
    diff /tmp/log-removed.txt /tmp/log-added.txt   # ideally identical message strings
    ```
    Differences must be deliberate and documented in the PR.

19. **For any test that previously injected a fake via `app.<X>Fn`
    function-pointer hook, the seam still exists.**
    ```
    grep -nE 'app\.(sendMessageFn|startSessionFn|testEmitHook|sendMessageWithOptionsFn|reconcileCodexFn|claudeProbeFn|codexProbeFn|generateThreadTitleFn|generateCommitMessageFn)' app_*_test.go | wc -l
    ```
    Count must equal baseline. These hooks are how the unit tests
    avoid spawning real subprocesses; removing one without rewriting
    the calling test silently weakens coverage.

20. **`-race` on the affected scope.** If the moved code touches
    goroutines, channels, mutexes, atomics, or `sync.WaitGroup`:
    ```
    make test-race                                    # repo's curated scope
    go test -count=1 -race -timeout 600s ./internal/<target>/...
    ```
    If `internal/<target>` is not in the curated `test-race` set in
    the Makefile and the moved code is concurrent, update `test-race`
    in the same PR.

21. **`make check` is green.**
    ```
    make check
    ```
    This is the gate `make` users hit before commit; it has to pass
    for the migration to be done.

### 2.5 Per-migration deliverables (PR description)

In addition to the code diff, the PR must include:

- The **assertion-mapping table** from step 7 (every old assertion
  → its post-migration home).
- The **coverage delta** numbers from step 16 (one-line summary:
  `main: 70.5% → 69.8% (-0.7); target: 70.9% → 73.1% (+2.2)`).
- The **logging-side-effects diff** outcome from step 18.
- Confirmation that **`methods_gen.go` is byte-identical** to
  baseline (one-line: `methods_gen.go unchanged ✔`).

These are short, mechanical, and a reviewer can verify them in
under five minutes. Without them, reviewing the migration requires
re-deriving the same facts, which is the slow path.

## 3. Tooling proposals

Each item: what it does, where it lives, rough build cost, verdict.

### 3.1 `scripts/baseline-test-state.sh` — capture pre-migration state

**What.** One-shot script that runs `make go-test`, `pnpm run check`,
captures `methods_gen.go`, captures per-package coverage for a caller-
specified `<pkg>`, and writes everything to `.refactor-phase-1/<sha>/`.
The agent runs this once at the start of a migration and once at the
end; a follow-up diff script then compares the two snapshots.

**Where.** `scripts/baseline-test-state.sh` (new). Bash; no
dependencies beyond what `make test` already requires.

**Cost.** Small — ~50 lines of shell. The expensive parts (running
the tests) are not new work; the script just orchestrates the existing
make targets and tee's outputs.

**Verdict.** Worth building. Manual capture is error-prone (forgetting
to record one of the four artifacts is the failure mode that lets
silent regressions slip through). A one-script wrapper turns "did
the agent run the protocol" into "is the output dir present and
hash-stable" — much easier to enforce in CI.

### 3.2 `scripts/diff-test-state.sh` — compare before/after

**What.** Diffs two snapshot dirs produced by `baseline-test-state.sh`
and prints a single PASS/FAIL summary with the specific check that
failed: test count delta, methods_gen diff, coverage delta, bindings
diff.

**Where.** `scripts/diff-test-state.sh`.

**Cost.** Small — ~80 lines of shell.

**Verdict.** Worth building. Pairs with §3.1; the user cares about
this output being a single grep-able report.

### 3.3 Test-count diff (subsumed by §3.2)

A standalone test-count diff is redundant with §3.2 — the diff script
covers it. **Skip.**

### 3.4 Behavior-preservation golden-output comparison

**What.** Capture the `capturedEventBus.allEvents()` output from a
small set of representative E2E tests (the happy-path Claude session,
the Codex unifiedExec yield, the approval round-trip) as a checked-in
golden NDJSON. Re-run after each migration; diff vs golden. Drift is
either intentional (update the golden) or a regression (revert).

**Where.** New: `app_e2e_golden_test.go` + `testdata/golden/*.ndjson`.

**Cost.** Medium — ~200-300 LOC for the test harness, plus authoring
the golden inputs. The big risk is that the goldens become noisy
(timestamps, IDs, ordering jitter from goroutines) and developers
start to update them reflexively without reading the diff. A small
normalizer (zero out timestamps, sort by deterministic keys) is
necessary and adds another 50-100 LOC.

**Verdict.** **Skip for Phase 1.** The existing E2E tests already
encode the same behavioral contracts inline, and the `methods_gen`
integrity gate + bindings regeneration already catch wire-shape
drift. A golden-output suite would be load-bearing for a Phase 2
where the migration *changes* internal types — at that point the
goldens prove the wire shape didn't move. Phase 1 doesn't need it
because Phase 1's stated contract is "same code, different package."
Revisit if and when Phase 1 lands and Phase 2 begins.

### 3.5 Pre-commit / pre-push hook

**What.** `git` hook that runs `make check` and the methods_gen
integrity test before allowing a commit (or push) of an `app_*.go`
change.

**Where.** `.githooks/pre-push` + `git config core.hooksPath`.

**Cost.** Small — ~30 lines.

**Verdict.** **Hold.** Hooks are easy to bypass and easy to silently
fail. The same checks belong in CI where they cannot be bypassed.
Don't ship as part of Phase 1 protocol; let the worktree agent run
them explicitly via §3.2.

### 3.6 CI gate addition

**What.** Existing CI (whatever the repo uses; `make check` + `make test`
+ `make test-race` are the canonical commands) already runs the
methods_gen integrity test and the binding generation check. The
additions needed for Phase 1:
- `make test-race` is currently scoped to a few packages — add any
  `internal/<target>` package that receives concurrent code during
  Phase 1 to the test-race scope in the same migration PR.
- A CI step that runs `wails3 task generate:bindings` then
  `git diff --exit-code frontend/bindings/` to fail builds where the
  generator output drifts from the committed copy.

**Where.** CI config (location not in repo at root; the Makefile is
the source of truth).

**Cost.** Small.

**Verdict.** Worth doing once. The binding-drift check is the most
valuable single addition; it catches the "I added a method but forgot
to regenerate bindings" failure mode that no other check covers
(unless `pnpm run check` happens to surface it via a TS error, which
is not guaranteed).

### 3.7 Coverage-diff script

**What.** Wraps `go tool cover -func` for two profiles and prints a
side-by-side table per-package, flagging packages whose coverage
dropped by > 1pp absolute.

**Where.** `scripts/coverage-diff.sh`.

**Cost.** Small — ~40 lines.

**Verdict.** Worth building, fold into §3.2.

### 3.8 Log-line preservation grep

**What.** A small helper that, given a base commit and the moved
files, lists every `log.*` line removed and asserts each appears in
the new home (by message-string match, allowing the format-args to
differ).

**Where.** `scripts/log-preserved.sh`.

**Cost.** Small — ~30 lines.

**Verdict.** Worth building, fold into §3.2.

## 4. Known gaps

### 4.1 Goroutine race windows not covered by `-race` runs

**Severity:** Medium. Several `app_*.go` files run goroutines
(session start/stop, watcher pumps, reconciler loops). `make
test-race` covers only `./internal/transport/...`,
`./internal/wsllauncher/...`, `./internal/clientmode/...`,
`./internal/editor/...`, and `.` — when code moves out of `.` into a
new `internal/<X>` not on the list, race coverage silently drops.

**Mitigation:** For every Phase 1 migration that moves goroutine code,
add the new package to the `test-race` scope in the same PR. This is
covered by step 20 of the protocol; the gap is that the protocol
must be followed — there is no automated enforcement.

### 4.2 Frontend / event-name drift

**Severity:** Medium. Event names emitted via `a.emit(name, data)`
(e.g. `"provider:item_event"`, `"provider:turn_started"`) are
strings. The frontend listens for the same strings. Neither the
binding generator nor `methods_gen_test.go` checks event-name
consistency.

**Mitigation:** Phase 1 should not change any event name. The
protocol's behavior-contract diff already calls out the assertion
that emissions stay identical, but it would benefit from a search-
and-list step: `grep -hE 'a\.emit\("' app_*.go internal/*/*.go` to
produce a baseline event-name inventory at the start of Phase 1, then
re-run at the end of each PR. **Accept the gap for Phase 1.** A
typed event-name registry is a Phase-2-class change.

### 4.3 Logging side-effects

**Severity:** Low-Medium. `log.Printf` lines carry diagnostic context
(thread IDs, error messages). No test asserts they exist. A mechanical
migration that drops a `log.Printf` will pass all tests.

**Mitigation:** Step 18 of the protocol. Build §3.8 to make it cheap
to run.

### 4.4 Provider-specific edge cases without explicit test coverage

**Severity:** Variable. Each provider package has well-tested wire
shapes (the `parse_*_test.go` files), but some edge cases live only
in `app_*.go` and have no direct unit test — e.g. behavior when a
provider session is closed mid-turn, or when an interrupt arrives
before the first user message. These are typically integration tests
in `app_e2e_*_test.go` and would survive a migration intact, but if
the migration also restructures the integration test fixtures,
coverage can quietly fall.

**Mitigation:** Step 7 of the protocol forces an explicit
assertion-mapping table per test. If a test moves, the table makes
the move auditable; if a test is dropped, the table forces a
justification.

### 4.5 Wails binding name-collision

**Severity:** Low. `frontend/bindings/agent-overflow/` already has
subdirs for `internal/git`, `internal/store`, `internal/settings`,
`internal/terminal`, etc. — these are types exported as wire-visible
data shapes. If a Phase 1 migration moves an App method's return-type
struct into `internal/<X>`, the binding generator emits a new
`internal/<X>/models.ts` file. That's fine; what's NOT fine is moving
the struct under a different name and breaking frontend imports.

**Mitigation:** When a struct moves out of `package main`, do NOT
rename it. If a rename is wanted, it's a separate PR after the move
has landed and the bindings have re-stabilized.

### 4.6 The currently-failing `internal/pathlinks` test

**Severity:** High (procedural). Until this is green, no Phase 1
migration can claim a clean baseline.

**Mitigation:** Fix or revert before Phase 1 starts. Out of scope
for the migration agent — coordinate with the package's author.

## 5. Walkthrough: applying the protocol to `app_claude_stop.go`

This is a small, self-contained migration with a clear destination
(`internal/provider/claude/`). Used here as a worked example.

Source: `app_claude_stop.go` (56 LOC, 1 binding method
`StopClaudeTask`).
Test: `app_claude_stop_test.go` (118 LOC, 4 tests:
`_SessionMissing`, `_ProviderMismatch`, `_RoundTripSucceeds`,
`_ShuttingDown`).

### Step 2.1 — Pre-flight

```
git rev-parse HEAD              # → a3f4...
git status --short              # empty
make go-test 2>&1 | tee /tmp/baseline-go.txt
# All 699 + N internal tests pass.
go test -count=1 -coverprofile=/tmp/cov-main-before.out .
# → 70.5% of statements
go test -count=1 -coverprofile=/tmp/cov-claude-before.out ./internal/provider/claude/...
# → 85.3% of statements
cp internal/transport/methods_gen.go /tmp/methods-before.go
```

### Step 2.2 — Choose what to move

`grep -nE '^func ' app_claude_stop.go`:
```
1:package main
38:func (a *App) StopClaudeTask(threadID, taskID string) error {
```

There's one binding method and one constant
(`stopClaudeTaskTimeout`). The binding cannot move (transport
invariant). The body is mostly session lookup + a delegate call to
`sess.claude.StopTask(ctx, taskID)`.

**Migration choice:** the `app_claude_stop.go` file is already a
thin wrapper. The body is 18 lines and pure glue. **Verdict:** this
file is not a good Phase 1 migration target — there's no shared logic
to lift into `internal/`. Leave it.

Reframing as a walkthrough exercise: if there *were* a helper to
lift (e.g., a multi-thread "stop everything claude-side" routine),
the assertion-mapping table would look like:

| Test name | Asserts | New home |
|---|---|---|
| `TestStopClaudeTask_SessionMissing` | App binding returns `"no active session"` for unknown thread | stays in `app_claude_stop_test.go` (binding-level guard) |
| `TestStopClaudeTask_ProviderMismatch` | App binding returns `"is not a Claude thread"` for a Codex session entry | stays (binding-level guard, asserts the App.sessions map contract) |
| `TestStopClaudeTask_RoundTripSucceeds` | End-to-end success: fake CLI ack → no error | stays (exercises the full glue including ctx + timeout) |
| `TestStopClaudeTask_ShuttingDown` | App binding short-circuits with `ErrShuttingDown` | stays (App-level lifecycle gate) |

None of these tests can usefully move to `internal/provider/claude`
because all four assertions are about the **App binding**: the App
sessions map, the App shuttingDown atomic, and the App-side error
text. The `internal/provider/claude/session_test.go::TestSession_StopTask_*`
tests already cover the wire-level success/failure round-trip.

### Step 2.3 / 2.4 — During and post-migration

Skipped because we chose not to migrate. If a fictional helper
*had* been extracted into `internal/provider/claude/stop_helper.go`:

- `wails3 task generate:bindings` produces zero diff (no method
  changed signature).
- `go test -count=1 -run TestMethodsGen_InSync ./internal/transport/`
  passes — no rename happened.
- `diff /tmp/methods-before.go internal/transport/methods_gen.go` is
  empty.
- A new test in `internal/provider/claude/stop_helper_test.go` would
  cover the new exported helper function with at least one assertion
  per public symbol.
- `log.Printf` audit: `app_claude_stop.go` has no `log.Printf` calls,
  so step 18 is a no-op (diff `log-removed.txt` is empty).
- Coverage: main drops by ~0.05pp (one tiny function moved out),
  `internal/provider/claude` rises by ~0.2pp. Within tolerance.

### Step 2.5 — Deliverables

Empty assertion-mapping table is unusual; the PR description would
note "no test moved — assertions all live at the binding seam." This
is fine — the protocol doesn't require movement, it requires
auditability of *what changed*.

### Outcome on this example

The walkthrough surfaces a useful general principle: many of the
~140 `app_*.go` files are already thin glue with nothing to lift.
Phase 1 will yield the biggest wins where files contain meaningful
private logic (e.g. `app_send.go` at 2089 LOC of tests against a
file with non-trivial business logic, `app_checkpoint.go` at 18K LOC
of source). The protocol scales down trivially for thin-glue files
(steps complete in minutes, mostly producing "no-op" results).

## 6. Cross-references — files a migration will likely touch

### 6.1 Always

- `app_<X>.go` — source being moved or partially lifted.
- `app_<X>_test.go` — test file mirroring the source.
- `internal/<target>/<file>.go` — new home for lifted helpers.
- `internal/<target>/<file>_test.go` — new tests for lifted helpers.
- `internal/transport/methods_gen.go` — regenerate if any `(a *App)`
  method's name OR signature changed (Phase 1 should produce no
  diff).
- `frontend/bindings/agent-overflow/app.ts` (generated) — regenerate
  to verify no diff.

### 6.2 If logging moves

- The new home must keep equivalent `log.Printf` (or `slog.*`) calls.
  See `internal/logging/` for the structured logger pattern when
  upgrading from `log.Printf` is appropriate.

### 6.3 If goroutines / concurrency moves

- `Makefile::test-race` — add the new package to the curated scope.

### 6.4 If a shared test helper grows

- `internal/testutil/` — only for mock-binary writers, git repo seeds,
  store seeds. NOT for `*App` builders (App is in `package main`).
- `app_test_helpers_test.go` — for `*App`-constructing helpers and
  the default-project fixture.
- `app_test_event_helpers_test.go` — for `capturedEventBus`-related
  fixtures.

### 6.5 If a struct returned by a binding moves

- `frontend/bindings/agent-overflow/models.ts` (generated) — verify
  the type re-appears under the new package's namespace and the
  frontend caller continues to import correctly. **Do not rename
  the struct** as part of the move.

### 6.6 If an `internal/<target>` AGENTS.md exists

- Update the AGENTS.md (and its `CLAUDE.md` symlink) to reflect new
  responsibilities. The map at `internal/AGENTS.md` may also need an
  edit if a wholly new responsibility lands.

### 6.7 If the migration touches the LocalOnlyMethods set

- `internal/transport/internalmethods.go` — every `*App` method that
  touches FS, spawns processes, controls provider sessions, mutates
  settings, or writes attachments belongs in `LocalOnlyMethods`. A
  migration that preserves a method name + signature does NOT need to
  touch this file. A migration that introduces a *new* binding method
  in addition to lifting code (rare in Phase 1) MUST add it.

---

## Appendix A: Quick-reference per-migration checklist (5-item summary)

The minimum operational checklist an agent must run in every Phase 1
worktree. The longer protocol in §2 expands each of these:

1. **Baseline before, baseline after.** `make test` (both Go and pnpm)
   passes at the same count and exit status at the start and end of
   the migration. No new `t.Skip`.
2. **Transport surface unchanged.** `internal/transport/methods_gen.go`
   byte-identical to baseline; `wails3 task generate:bindings`
   produces no diff.
3. **Assertion-mapping table in PR.** For every test in the moved
   file, the table records: stays at App level / moves to new
   package / removed (with justification).
4. **New public function in `internal/<target>` has a real test
   asserting its behavior.** Compile-only coverage doesn't count.
5. **Coverage delta within tolerance.** `package main` may drop only
   by the amount that moved out; `internal/<target>` rises
   correspondingly. Neither drops by >1pp without a documented
   reason.

If `make test-race` scope needs updating (concurrent code moved) or
logging is touched (`log.Printf` lines removed), those become items 6
and 7 for that particular migration.
