# Phase 1 Go-side duplication audit

Read-only research for the Phase 1 migration of root-level `app_*.go`
into `internal/`. The brief: surface high-leverage consolidation
opportunities that are adjacent to the migration so the refactor lands
once instead of twice.

This document is for planning — it does not modify code. Frontend,
`_test.go` files, and `t3-code/` are out of scope.

---

## 1. Methodology

### Hard constraints respected

- **Transport invariant** (`AGENTS.md`): `*App` method names and
  signatures are immutable. Every binding-bearing function listed below
  keeps its current signature; the proposed extractions are private
  helpers behind the public method.
- **Core principle #6** (`CLAUDE.md`): no unified Claude/Codex
  abstraction. Where a finding crosses provider boundaries
  (F3 probes, F5 approval dedup) the proposal is to consolidate the
  *provider-agnostic plumbing* — caches, dedup tables, file-system
  scaffolding — not the wire-shape layer. Parallel structure in
  `provider/claude/*` and `provider/codex/*` whose differences are
  load-bearing (e.g. `buildApprovalResponse` vs
  `buildApprovalResponseResult`, the NDJSON vs JSON-RPC probe shells)
  is treated as a non-finding.
- **Match-language-idioms**: every finding is judged against "if
  behavior X needs to change, must all copies change together to stay
  correct?" Findings answer yes; non-findings answer no.

### How candidates were found

1. Walked `app_*.go` at the repo root (≈140 files) and grouped by
   semantic neighborhood — send/steer/flush, probe/recheck,
   text-generation tasks, checkpoint readers, mode toggles, idle
   reaper.
2. For each cluster, identified the shared "prologue" and "epilogue"
   versus the load-bearing differences.
3. For `internal/provider/{claude,codex}/` walked symbol-by-symbol
   looking for byte-identical bodies and structurally-parallel
   bodies. The former are F-class candidates; the latter are
   non-findings unless the differences are accidental.
4. Cross-checked dead-code candidates with
   `grep -rn '<symbol>' --include='*.go'` across both production
   and test trees before flagging.
5. For each high-severity finding, located the existing test
   surface that would catch a regression so the consolidation can
   be done test-first.

### What "consolidate" means in this document

Three different shapes, increasing in cost:

- **Inline helper** — extract a private function in the same file or
  package. Zero callers across packages, no new types. Trivial.
- **Package-internal type or function** — promote shared plumbing to
  a `helpers.go` or new package-internal file. Still package-local.
- **New `internal/` package** — only proposed when callers cross
  package boundaries today and the seam already exists (e.g.
  approval dedup is shared infrastructure with two callers).

---

## 2. Findings

Ordered High → Low severity. Severity = (sites × per-site footprint
× drift risk).

### HIGH

#### F1. Text-generation per-task duplication

**Sites**: `app_commit_message.go` and `app_thread_title.go`.

**Symbols**:
- `generateCodexCommitMessage` (app_commit_message.go:131)
- `generateClaudeCommitMessage` (app_commit_message.go:196)
- `generateCodexThreadTitle`   (app_thread_title.go:72)
- `generateClaudeThreadTitle`  (app_thread_title.go:139)

**Shared skeleton** (4 methods, identical step ordering):

```
resolveGitPaths(thread)                       // workspace
  → providerBinaryPath / binary check
  → Codex only: createTextGenerationScratchFiles(schema)
  → build args (provider-specific shape)
  → cfg.Exec(ctx, textGenerationCLISpec{...})
  → if result.ExitCode != 0 → wrap CLI error
  → parse JSON (provider-specific decoder)
```

The shared infrastructure is **already present** in
`app_text_generation.go`:

- `textGenerationCLISpec`           (type)
- `textGenerationCLIResult`         (type)
- `execTextGenerationCLI`           (default executor)
- `cappedTextGenerationOutput`      (truncated capture)
- `createTextGenerationScratchFiles` (Codex scratch files)
- `readTextGenerationOutputFile`    (Codex stdout-via-file)
- `translateCLINotFound`            (binary-not-found wrapper)
- `firstNonEmptyMessage`            (error message picking)

What still duplicates: the provider-specific argv build, the schema
literal, and the decoder. Both methods per provider do the same shell
around the same primitives.

**Proposal**:

Add to `app_text_generation.go` a single generic helper:

```go
type textGenerationTask[T any] struct {
    workspace    string
    binaryPath   string
    schema       string                                    // optional
    buildArgs    func(schemaPath, outputPath string) []string
    timeout      time.Duration
    promptStdin  string
    cliName      string                                    // for error wrapping
    decode       func(stdout []byte, outputFile string) (T, error)
}

func runTextGenerationTask[T any](ctx context.Context, cfg textGenerationConfig, t textGenerationTask[T]) (T, error)
```

Each `generateXY` becomes 20-30 lines: assemble `textGenerationTask`,
call `runTextGenerationTask`, return.

**Risk and verification**: Both methods already share
`textGenerationConfig` and the executor seam used by
`app_text_generation_test.go`. Per-method tests
(`app_commit_message_test.go`, `app_thread_title_test.go`) cover the
output shapes — leave those untouched, add per-helper tests for the
new `runTextGenerationTask` that exercise: (a) scratch-file cleanup
on success and on error, (b) exit-code error wrapping, (c)
binary-not-found path, (d) decoder error pass-through.

**Match-language-idioms check**: Yes — if we ever fix the
scratch-file cleanup, the timeout policy, the stderr capture cap, or
the not-found wrapping, all four methods must change together. They
already cross-call into the same `textGenerationConfig`; the helper
makes that contract explicit.

---

#### F2. send / steer / flush user-message prologue

**Sites**: `app_send.go:103-301`, `app_steer.go:68-252`,
`app_flush_queue.go:345-477`. All three are entry points for "a user
message is about to reach the provider."

**Shared prologue** (lines 116-173 in `app_send.go`; lines 73-131 in
`app_steer.go`; lines 357-401 in `app_flush_queue.go`) — same five
helper calls in the same order:

1. `resolveSendMessageAttachments(threadID, attachmentIDs)`
2. `resolveSourceProposedPlan(threadID, opts.SourceProposedPlan, true)`
3. `resolveSourceProposedPlan(threadID, opts.RevisionSourceProposedPlan, false)`
   → if non-nil: `appendPlanRevisionCommentsToContent(...)`
4. `resolveSourceDiffReview(threadID, opts.RevisionSourceDiffReview)`
   → if non-nil: `appendDiffReviewCommentsToContent(...)`
5. `marshalUserMessageMeta(...)`

**Shared epilogue**:

- After persist:
  `MarkDiffReviewCommentsSent(threadID, scope, sourceKey, ids, ts, userItemID)`.

**What differs (load-bearing)**:

- Send: lazy provider session start; mode-switch on `opts.Mode`;
  optional title generation; full failure-rollback semantics.
- Steer: no lazy start (steer requires an active provider session);
  Codex-only `Steer` RPC path. Empty content path returns early.
- Flush: queue-flush wrapper around the same dispatch but with
  Codex `Steer→Send` fallback; flush carries pre-resolved IDs.

**Proposal**:

Extract a private helper in `package main` (the receiver is `*App`):

```go
type resolvedUserMessage struct {
    content                       string
    providerAttachments           []provider.ImageAttachment
    persistedAttachments          []store.Attachment
    sourcePlan                    *SourceProposedPlan
    revisionSourcePlan            *SourceProposedPlan
    revisionPlanCommentIDs        []string
    revisionSourceDiff            *SourceDiffReview
    revisionDiffCommentIDs        []string
    userMessageMeta               json.RawMessage
}

func (a *App) resolveUserMessageInputs(
    threadID, content string,
    inputs userMessageInputs,
) (resolvedUserMessage, error)
```

where `userMessageInputs` is the projection of the fields that differ
between `SendMessageOptions`, `SteerMessageOptions`, and the
flush-queue payload. Add a sibling `markUserMessageInputsSent` for the
single epilogue line.

After the extraction, each entry point becomes its specific
post-persist orchestration: send keeps lazy-start + mode-switch +
title generation; steer keeps the steer-only Codex path; flush keeps
the Steer→Send fallback. Those *are* the differences worth preserving.

**Risk and verification**:
- Existing per-entrypoint tests (`app_send_test.go`,
  `app_steer_test.go`, `app_flush_queue_test.go`) lock in the
  observable behaviour. Add a unit test for
  `resolveUserMessageInputs` that exercises the cross-product of
  attachment / source-plan / revision-plan / revision-diff
  states — today no single test covers all five branches in one
  pass.
- `marshalUserMessageMeta`'s signature is currently
  `func marshalUserMessageMeta(...)` (no receiver) and is called
  from all three sites — keep it free-standing.
- **Order matters** here: this is the first migration concern
  because Phase 1 will be moving send/steer/flush into a new
  package (or splitting them), and doing the consolidation before
  the move halves the migration diff.

**Match-language-idioms check**: Yes — every time we add a new
metadata field, or a new attachment branch, or a new revision-source
type, three files must change in lockstep. They already share the
underlying validators; making the bundle explicit closes the drift
hole.

---

#### F3. Provider probe wrapper duplication

**Sites**: `app_claude_probe.go` (107 lines) and `app_codex_probe.go`
(88 lines).

**Identical structure** (verified via diff):

| Element             | Claude                          | Codex                          |
|---------------------|---------------------------------|--------------------------------|
| Mutex               | `claudeProbeCacheMu`            | `codexProbeCacheMu`            |
| Cache var           | `claudeProbeCache`              | `codexProbeCache`              |
| Cache getter        | `claudeAccountProbeCache()`     | `codexAccountProbeCache()`     |
| Test reset          | `resetClaudeProbeCacheForTest`  | `resetCodexProbeCacheForTest`  |
| Public method       | `ProbeClaudeAccount`            | `ProbeCodexAccount`            |
| Recheck method      | `RecheckClaudeAccount`          | `RecheckCodexAccount`          |
| Cache hit path      | TTL check + return cached       | TTL check + return cached      |
| Cache miss path     | spawn → probe → store → emit    | spawn → probe → store → emit   |

The cache type itself (`provider.ProbeCache`) is **already shared** —
Claude/Codex each `type X = provider.ProbeCache` alias it.

**The single load-bearing difference**: Claude calls
`emitClaudeUnauthenticatedStatus()` when the probe returns a
zero-value AccountInfo (no subscription, no token source); Codex
doesn't because the wire signal there is ambiguous (a backend latency
spike can produce empty planType for an authenticated user).

**Proposal**:

Convert the per-provider boilerplate into a single helper, parameterized
by provider name and a probe closure:

```go
type providerProbeRunner struct {
    providerName     string                                  // "claude" / "codex"
    cache            *provider.ProbeCache
    probe            func(ctx context.Context) (provider.AccountInfo, error)
    unauthenticated  func(provider.AccountInfo) bool         // optional
    emitUnauth       func()                                  // optional
}

func (a *App) runAccountProbe(r providerProbeRunner) (provider.AccountInfo, error)
```

Claude wires both hooks; Codex leaves them nil. Public methods stay
as-is — they each become a 5-line dispatch to `runAccountProbe`.

**Risk and verification**:
- `app_claude_probe_test.go` and `app_codex_probe_test.go` exercise
  cache hit/miss, recheck-bypass, and the unauth-emit branch for
  Claude. Keep both files; they validate the wired closures.
- Add a test for `runAccountProbe` that exercises the optional-hook
  path with stubbed closures.

**Match-language-idioms check**: Yes — every cache invariant
(reset semantics, TTL, miss-once-emit-once for `provider:account`)
must hold for both providers. There's already drift in the doc
comments (the Codex one explains the "test reset" rationale more
tersely than Claude's); consolidating them removes the drift surface.

---

#### F4. Dead `provider.ProviderStatusEvent`

**Site**: `internal/provider/types.go:396-418`.

**Status**: defined and exported but **referenced by no Go code**.
The frontend-consumed `ProviderStatusEvent` lives in
`app_provider_status.go` with its own field set (`Status`, `Message`,
`Version`, `Actionable`, `ActionURL`).

```
$ grep -rn "provider\.ProviderStatusEvent\b\|ProviderStatusEventKind" --include='*.go' .
internal/provider/types.go:396       (definition)
internal/provider/types.go:400       (kind enum)
internal/provider/types.go:403-405   (kind values)
internal/provider/types.go:414       (Kind field on the struct)
```

The kind enum (`binary_missing`, `unauthenticated`,
`version_incompatible`) is mentioned in
`internal/triage/AGENTS.md` but no Go source consumes it.

**Proposal**: delete the type, the kind enum, and the kind constants
from `internal/provider/types.go`. Update the triage AGENTS reference
to point to `app_provider_status.go`'s in-use type instead. Pure
cleanup, no behavioural change.

**Risk and verification**: `make go-build` proves the absence of
in-tree consumers. No tests need to change.

---

### MEDIUM

#### F5. Approval dedup primitive duplication

**Sites**: `internal/provider/claude/session.go:832-861` and
`internal/provider/codex/session.go:2251-2277`.

The `claimApproval(requestID string, expectedKind provider.EventKind) bool`
function bodies are **byte-identical** across both providers — verified
via line-by-line comparison.

The shared dedup state:

- `resolvedApprovals map[string]struct{}` — set of answered IDs.
- `resolvedApprovalsSoftCap = 1000` — defined in both files
  (Claude:910, Codex:2368) with the same value.
- The soft-cap eviction (when the set grows past 1000, replace it
  with a fresh empty map) is the same line of code in both.

The dedup is per-`Session`, guarded by `approvalsMu` — that mutex
stays per-provider because it also guards pending state with different
fields (see drift D1).

**Proposal**:

Extract to `internal/provider/approvaldedup.go` (new file in shared
provider package):

```go
// ApprovalDeduper bounds a per-session set of resolved approval IDs.
// One Deduper per Session.
type ApprovalDeduper struct {
    mu        sync.Mutex
    resolved  map[string]struct{}
}

const resolvedApprovalsSoftCap = 1000

// Claim atomically: (a) checks resolved/pending consistency under
// the caller's lock, (b) returns false if already resolved or if
// pending state doesn't match expectedKind, (c) on first claim,
// adds to resolved set with soft-cap eviction.
func (d *ApprovalDeduper) Claim(/* args parameterized by what callers need */) bool
```

The tricky bit: today both `claimApproval` bodies read
`s.pendingApprovals[requestID]` under `s.approvalsMu`. The dedup is
intertwined with pending lookup. Two ways to extract:

- **Option A** (lighter touch): leave `claimApproval` per-session,
  extract only the soft-cap eviction loop + the `resolvedApprovals`
  field into the new type. Saves ~6 lines, low risk.
- **Option B** (full extraction): introduce a callback signature for
  the pending-check so the deduper owns the locking. More invasive,
  saves ~25 lines per provider.

Either way, the constant `resolvedApprovalsSoftCap = 1000` moves to
the shared file and the duplicate declaration is dropped.

**Recommendation**: ship Option A in Phase 1 (low risk, small win,
removes the constant duplication). Re-evaluate Option B in a later
phase if more providers are added.

**Risk and verification**:
- Both packages have approval round-trip tests in
  `provider/claude/session_test.go` and
  `provider/codex/session_test.go` that exercise the dedup branch.
- Add a unit test for `ApprovalDeduper` in the new file.

**Match-language-idioms check**: Yes — the soft-cap behaviour is
load-bearing and must agree across providers. Today an oversight could
change the cap in one file and leave the other untouched.

**Constraint check**: This crosses the principle-6 line at first
glance, but the deduper is provider-agnostic plumbing (a bounded set
of opaque IDs), not a wire-shape abstraction. The wire-shape layer
(`buildApprovalResponse` vs `buildApprovalResponseResult`,
`trackPendingApproval`'s int64-vs-string signature) stays separated.

---

#### F6. Five-fold `checkpoint:error` emit in captureMessageCheckpoint

**Site**: `app_checkpoint.go` — `captureMessageCheckpoint` emits the
exact same event shape five times in one function:

```go
a.emit("checkpoint:error", map[string]any{
    "threadId":    threadID,
    "userItemId":  userItemID,
    "turnIndex":   turnIndex,
    "error":       err.Error(),
})
```

**Proposal**: local helper

```go
func (a *App) emitCheckpointError(threadID, userItemID string, turnIndex int, err error) {
    a.emit("checkpoint:error", map[string]any{
        "threadId":    threadID,
        "userItemId":  userItemID,
        "turnIndex":   turnIndex,
        "error":       err.Error(),
    })
}
```

**Risk**: trivial. Existing tests cover the surface.

**Match-language-idioms check**: Yes — five callers, identical shape,
load-bearing (the frontend reads these exact field names).

---

#### F7. Checkpoint diff getters share a context-resolve preamble

**Sites**: `app_checkpoint.go` — four methods:

- `GetMessageCheckpointDiff`
- `GetMessageCheckpointRevertDiff`
- `GetSessionAgentDiff`
- `GetWorkspaceCurrentDiff`

**Shared preamble**:

```
store.GetThread(threadID)
  → fetch checkpoint row (varies per method)
  → validate checkpoint.workspace == thread.workspace
  → resolve git workspace path
```

**What differs (load-bearing)**: which `checkpointStore` method runs
last, and what's compared against (head vs revert vs another snapshot
vs the live workspace).

**Proposal**: extract a private helper

```go
func (a *App) resolveCheckpointDiffContext(threadID, checkpointID string) (resolved checkpointDiffContext, err error)
```

returning a struct with `thread`, `workspacePath`, validated
checkpoint metadata. Each public getter calls the helper, then runs
its single diff call.

**Risk and verification**: existing tests in `app_checkpoint_test.go`
cover the error-path validation. Low-risk extraction.

**Match-language-idioms check**: Yes — if the workspace-match check
changes (e.g. to allow cross-workspace diffs), four sites must move
together.

---

#### F8. Mode-changed event emit pattern

**Sites**: `app_thread_interaction_mode.go` and `app_runtime_mode.go`.

Both packages run the same shape:

1. Validate the requested mode (provider-specific validator).
2. Store update.
3. Check if the thread has an active provider session.
4. If active and the change requires a reconnect, tear down +
   re-spawn.
5. Emit `thread:mode_changed` (interaction) or
   `thread:runtime_mode_changed` (runtime).

**Proposal**: extract a shared helper `restartIfActiveLocked(threadID,
needsReconnect)` so both flows agree on the teardown ordering. Don't
unify the events themselves — they really are different concerns and
the frontend branches on them.

**Risk**: low. The two flows currently use slightly different
teardown ordering — make this a literal extract-and-call without
behaviour change, then put the harmonization in its own follow-up.

**Match-language-idioms check**: Partial. The event shapes are
intentionally separate (different fields, different consumers). The
*reconnect helper* is the only thing that has to agree.

---

### LOW

#### F9. Branch-name generation lives in two places

`worktree_branch.go` uses `gitops.BuildGeneratedWorktreeBranchNameWithPrefix`
for worktree-suggestion names; `app_commit_message.go` and
`app_thread_title.go` use the text-generation CLI route for human-readable
output.

**Verdict**: leave separate. Different inputs (git history vs user
message), different invariants (branch-name safety vs prose quality).
The match-language-idioms test answers "no" — they don't change
together.

#### F10. `idsFromProposedPlanComments` / `idsFromDiffReviewComments`

Two 4-line helpers in `app_proposed_plans.go` and
`app_diff_review_comments.go` that each extract `[]string{IDs}` from
their respective comment types.

**Verdict**: leave separate. Different types, distinct concepts.
DRY-for-DRY's-sake territory.

#### F11. `currentGitBranch`

A 6-line helper used by a single site. Leave alone.

---

## 3. Non-findings

The following look like duplication but represent separate concepts
that may need to evolve independently. Per match-language-idioms, do
not consolidate.

### Three "capped" buffers serve different invariants

| Type                                | Location                              | Invariant                                   |
|-------------------------------------|---------------------------------------|---------------------------------------------|
| `cappedTextGenerationOutput`        | `app_text_generation.go:137`          | Truncate at limit with `...truncated` mark  |
| `cappedCommandOutput`               | `internal/triage/codex_background.go` | Ring with `Replace` semantics for tail-view |
| `terminal.ringBuffer`               | `internal/terminal/ring.go`           | Mutex-protected 256 KiB replay buffer       |

The first is a one-shot capture; the second is a live tail-view ring;
the third is an xterm.js replay buffer with concurrent subscribers.
Sharing them would require a configurable type with three modes —
that's worse, not better.

### Parallel `provider/claude/probe.go` and `provider/codex/probe.go`

Both wire shells call:

```
spawn binary → write initialize → read response goroutine → parse
```

The wire formats differ (NDJSON `control_response` vs JSON-RPC `result`),
the request shapes differ (Claude sends `subtype: "initialize"`; Codex
sends `initialize` + `initialized` + `account/rateLimits/read`), and
the response parsing differs (`response.response.account` vs
`rateLimits.planType`). Principle 6 says keep them separate; the
parallel skeleton is incidental.

### `buildApprovalResponse` vs `buildApprovalResponseResult`

Different wire shapes (NDJSON `control_response` vs JSON-RPC reply).
Per principle 6, keep them separate.

### `spawnProviderSession` switch arms

Two arms (`claude.NewSession` / `codex.NewSession`) with parallel
shapes. Per principle 6, keep them separate.

### Per-provider `app_claude_*.go` / `app_codex_*.go` ratelimit handlers

The probe orchestration is per-provider for good reason — see drift
D3 below. Not a finding.

---

## 4. Drift detection

Cases where the parallel structures **already disagree** in a
load-bearing way. These are not consolidation candidates, but the
audit should flag them so the migration doesn't accidentally erase the
difference.

### D1. `pendingApproval` shape diverges

- `provider/claude/session.go:132-135` —
  `pendingApproval{resolveKind, userInputQuestions []provider.UserInputQuestion}`.
- `provider/codex/session.go:172-174` —
  `pendingApproval{resolveKind}`.

The `userInputQuestions` field is load-bearing for Claude: when
`RespondToUserInput` answers a user-input request, the SDK wire shape
requires the questions list to be echoed back as a
`{question: answer}` map. Claude carries the questions across the
roundtrip via `trackPendingApprovalWithQuestions` and
`pendingUserInputQuestions`. Codex's user-input wire format embeds the
questions in the response payload, so no per-session staging is
needed.

If F5 is implemented as Option A (keep per-provider `claimApproval`),
this stays untouched. If F5 is implemented as Option B (full
extraction), make sure the deduper's callback signature accepts an
opaque `pending` value the caller types.

### D2. `trackPendingApproval` signature drift

- Claude: `func trackPendingApproval(requestID string, resolveKind provider.EventKind)`.
- Codex: `func trackPendingApproval(rpcID int64, resolveKind provider.EventKind)` — internally `fmt.Sprintf("%d", rpcID)` to normalize to string.

Caller types differ (NDJSON `requestId` is opaque string; JSON-RPC
`id` is numeric). Both internalize to the same string-keyed map.
**Don't unify** — the call-site type safety is correct as-is.

### D3. Probe wrappers emit `provider:status` asymmetrically

- `app_claude_probe.go` emits `Status: "unauthenticated"` when the
  cached or fresh probe returns an empty AccountInfo.
- `app_codex_probe.go` does not.

This is **intentional**: Claude's empty `subscriptionType` + empty
`tokenSource` is an unambiguous unauthenticated signal (the CLI is
not logged in). Codex's empty `planType` is ambiguous — it can occur
during backend latency for an authenticated user. Surfacing an
unauthenticated banner on the latter would create false positives.

If F3 lands, the runner exposes an `emitUnauth` hook; Claude wires
it, Codex doesn't. Document this in a comment on the runner so a
future maintainer doesn't "fix" the inconsistency.

### D4. Probe-cache docstring drift

The two probe wrappers' doc comments diverge in length and reasoning
detail (Claude's is more explanatory about why a mutex over
`sync.Once`). Not a runtime drift, but worth normalizing when F3
lands so both reasons are visible from either site.

---

## Status (2026-05)

All Phase 1 duplication findings except F8 have been folded into the
codebase. F8's "shared restart-if-active helper" turned out to be
mismatched on closer reading — interaction-mode applies live via
Claude's `set_permission_mode` and only flags `NeedsReconnect=true`
when not applicable, while runtime-mode always restarts via
`startSession`. There is no shared teardown step today, so the
"agree on the teardown ordering" rationale doesn't fit. Skipping
unless a future flow surfaces a real shared restart contract.

| Finding | Status | Commit / note |
|---|---|---|
| F1 — textgen task helper | done | `17a6eba`, plus a further `f29cda5` lifting the runner into `internal/textgen` |
| F2 — send/steer/flush prologue | done | `449e510` |
| F3 — provider probe wrappers | done | `06998ba` |
| F4 — dead `provider.ProviderStatusEvent` | done | `e25ea45` |
| F5 — `ApprovalDeduper` | done | `62d054f` |
| F6 — `emitCheckpointError` helper | done | `ae4c154` |
| F7 — checkpoint diff context | done | `6d3a612` |
| F8 — mode-restart helper | skipped — no real shared step | see note above |

## 5. Recommended consolidation order

Each step is a discrete change; gate progress on `make go-build`,
`make go-test`, and `pnpm run check` passing.

1. **F4 — delete dead `provider.ProviderStatusEvent`** *(pure cleanup,
   zero risk, no test changes)*. Unblocks tightening
   `internal/provider/types.go`'s split with the in-use type.

2. **F2 — extract `resolveUserMessageInputs` + sibling epilogue marker**
   *(must happen before any structural move of send/steer/flush)*.
   The other Phase 1 agents will be touching these files; lock in the
   shared seam first so the migration moves three callers and one
   helper, not three near-copies.

3. **F1 — consolidate text-generation arms into `runTextGenerationTask[T]`**.
   `app_text_generation.go` already has the primitives; this folds
   `app_commit_message.go` and `app_thread_title.go` down to thin
   dispatchers, and creates a clean extension point for any future
   per-task generators (test-plan stubs, branch names, etc.).

4. **F3 — consolidate probe wrappers into `runAccountProbe`**. Smaller
   blast radius than F2/F1; depends on no other finding. Picks up D4
   for free (single canonical doc comment).

5. **F5 (Option A) — extract `ApprovalDeduper` + shared
   `resolvedApprovalsSoftCap` constant**. Cross-package; do after the
   in-package consolidations above. The full Option-B extraction can
   wait until the deduper has a second concrete need.

6. **F6 — local `emitCheckpointError` helper in `app_checkpoint.go`**.
   Trivial.

7. **F7 — `resolveCheckpointDiffContext` helper in `app_checkpoint.go`**.
   Trivial.

8. **F8 — `restartIfActiveLocked` helper, mode flows untouched**.
   Smallest leverage; do last.

### Test surface to ride on

| Finding | Existing tests                                                                                                  |
|---------|-----------------------------------------------------------------------------------------------------------------|
| F1      | `app_commit_message_test.go`, `app_thread_title_test.go`, `app_text_generation_test.go`                         |
| F2      | `app_send_test.go`, `app_steer_test.go`, `app_flush_queue_test.go` — keep all three                              |
| F3      | `app_claude_probe_test.go`, `app_codex_probe_test.go`, `internal/provider/probecache_test.go`                   |
| F4      | none required — compile-time check via `make go-build`                                                          |
| F5      | `internal/provider/claude/session_test.go`, `internal/provider/codex/session_test.go` (approval round-trips)    |
| F6, F7  | `app_checkpoint_test.go`                                                                                        |
| F8      | `app_thread_interaction_mode_test.go`, `app_runtime_mode_test.go`                                               |

For each consolidation: write the new helper's unit test in the same
commit; keep the existing callsite tests untouched as locked-in
behavioural anchors. If a callsite test fails after the consolidation,
the consolidation has a bug — don't relax the test.

---

## Coordination notes for parallel agents

- **Phase 1 binding inventory agent** will classify every `app_*.go`
  into buckets and destinations. The findings above cite specific
  source files; if the inventory recommends moving any of those
  files (especially `app_send.go`/`app_steer.go`/`app_flush_queue.go`
  for F2, or `app_commit_message.go`/`app_thread_title.go` for F1),
  do the consolidation in this document *before* the move, on the
  files in their current location. That keeps the migration diff
  honest.
- **Test strategy agent** will design the verification protocol.
  Section 5's per-finding test surface is the input — point them at
  this section if their plan needs duplication-specific coverage.
- No agent modifies code; all three write to
  `docs/architecture/refactor-phase-1/` so the human reviewer sees
  one coherent plan.
