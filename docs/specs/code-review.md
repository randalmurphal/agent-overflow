# Code review

Status: design, signed off 2026-08-23. Nothing implemented yet. Decisions
were settled in a brainstorm session and a codex consult; the
`(Qn)`/`(Dn)`/`(An)` tags are those sessions' ids and carry no other
meaning.

## Goal

Native code review in agent-overflow on the user's own provider
subscriptions: forge-integrated (GitLab + GitHub merge requests, webhook
or poll driven) and local (a branch, commit, range, or the uncommitted
workspace, started from a thread). Deterministic engineering in the
shape of alibaba/open-code-review (OCR): file selection, rule matching,
comment anchoring, coverage accounting, false-positive verification.
No external review tool in the loop.

Token efficiency is a first-order goal. The model never explores to
find what changed, never emits line numbers, and never carries the
general-purpose harness into a review turn.

## Approach

One target-agnostic `review` workflow on the existing workflows engine
(`internal/workflow/`, spec: [workflows-system.md](workflows-system.md)):

```
materialize → prepare → review units (fan-out) → anchor → verify → post | emit
  builtin     builtin     agent                   builtin  agent    builtin
```

Forge reviews wrap it with durable webhook intake and an automation.
Local reviews start it from a thread through the `/ao-review` composer
block. OCR's parser, hunk model, rule matcher, and tests are lifted into
`internal/review` (Apache-2.0, attribution in file headers); its
resolver, bundler, fingerprint, and manifest are rewritten to the
policies below; its LLM loop and tools are not used.

The engine gains three primitives, each useful beyond review:
`builtin:` steps, per-phase `harness:` profiles, and a webhook trigger
kind.

## Engine primitives

### `builtin:` on tool phases (Q11, D23, A1, A2)

A `driver: tool` phase or fan-out unit binds to exactly one of
`check:`, `command:`, or `builtin:`. `builtin: <name>` names a Go
function in a closed registry compiled into the app. It receives a
`context.Context`, the phase's resolved inputs, and a narrow capability
object for the services it needs; it returns typed outputs that persist
through the engine's normal phase-output path. It never serializes
through `AO_ENVELOPE` (that is the subprocess ABI). Missing names, type
mismatches, oversized outputs, panics, and cancellation fail the phase
visibly. Gates route on builtin outputs like any other.

Builtins exist so the app can ship deterministic steps that work on any
project with zero profile setup. User-written tools stay profile
commands.

Registry for this feature: `review.materialize`, `review.prepare`,
`review.anchor`, `review.post`, `review.emit`, `forge.intake`,
`forge.reply`, `forge.status`.

### Commands stay profile-bound (Q11, D30)

Workflow YAML never carries argv. Commands and secrets are declared in
profiles under AO's config dir (operator-owned, never in the repo). A
**shared-scope profile** (`<configRoot>/profile.yaml`, project profile
wins on name collision, same precedence model as workflow scopes) lets
a command or secret be declared once for every project. In-repo files
this feature reads (`.agent-overflow/review.json`) are data: globs and
rule text, never executables.

Deferred, own design later (S1): `capture:` (stdout of a command as a
named output). No phase in this feature consumes it.

### `harness:` on phases and units (Q6, D5, D20, A31, A32)

`harness: <profile-name>` selects a named harness profile (see
[Harness profiles](#harness-profiles-q6-d20-a31a34)). The profile body for the phase's
provider is resolved once at run creation, snapshotted with a digest
into the run, and applied at every spawn in that run. A missing or
unsupported provider body fails before fan-out. The inert
`Phase.Capabilities`, `Phase.MCP`, and `Phase.Commands` fields are
deleted from the type and the JSON schema: a field that validates clean
and does nothing is how the `poll-jira-and-start` starter shipped with
zero grants.

### Webhook trigger kind (Q10, D2, D18, A21–A30)

The workflows spec's "poll with CLIs, no inbound listener" decision is
overturned for sources that have webhooks. Polling stays as an explicit
per-automation choice (cron + `forge.intake` with a cursor) and is the
supported desktop path for GitHub.com, which cannot reach a LAN
listener; the LAN-reachable work GitLab uses webhooks.

```json
{"kind":"webhook","endpoint":"<id>",
 "on":["mr.opened","mr.ready","mr.pushed","mr.comment","mr.closed"],
 "quiet":"2m","max_wait":"10m"}
```

Intake (HTTP handler, bounded work only): cap the raw body,
authenticate with the source's scheme, parse the supported event,
normalize it, insert a deduplicated receipt and update the pending
desired-head record in one SQLite transaction, return 2xx. It never
starts a run, calls a provider, or fetches a repo.

- Endpoints: `{id, source: gitlab|github, auth}` in settings. Route
  `POST /hooks/<id>` on the transport server, exempt from session auth,
  subject to the LAN allow-list, reachable through the existing LAN-bind
  toggle.
- Auth per source: GitHub verifies `X-Hub-Signature-256` HMAC-SHA256
  over the raw body, constant-time compare. GitLab verifies
  `X-Gitlab-Token` (constant-time) or, when the connection declares it,
  Standard Webhooks HMAC mode (GitLab ≥ 19.1) with timestamp freshness.
- Dedupe: GitHub `(hook, X-GitHub-Delivery)`; GitLab
  `(hook, Idempotency-Key)`. A replay is a recorded skip.
- Normalized event: `{source, key, kind, repo, mr, head, base, draft,
  sender, sender_role, comment, delivery_id}`. Mappers are per source;
  the scheduler never learns forge shapes. A Jira mapper is a later
  addition in the same slot.
- Debounce is scheduler-owned, state in SQLite (`not_before`,
  `max_wait_at`) so restarts keep the quiet window. `quiet` applies to
  pushes; `mr.opened`, `mr.ready`, commands, and close/reopen bypass it.
  One pending record per `key`; a newer push replaces the desired head.
  At fire time the current MR state and head are refetched; the payload
  is a hint.
- Exactly one review automation per MR target, enforced at config time.
  Publication holds a per-target lock.
- A webhook fire is one more command on the single scheduler goroutine.
  The scheduler still never imports the engine. Fire context carries the
  automation id.
- GitHub does not redeliver failed deliveries; GitLab disables a hook
  after 4 consecutive failures. The receipt table is the audit trail,
  and boot reconciliation (below) covers heads missed while down.
  Commands missed while down are lost; the status comment says when the
  app last reconciled.

## Review core (`internal/review`)

From OCR (`/tmp/ocr` at consult time; upstream
github.com/alibaba/open-code-review):

| Piece | Source | Action |
|---|---|---|
| diff grammar, hunk model, tests | `internal/diff/{hunk,parser}.go` | LIFT behind AO-owned reader APIs; stderr warnings become a sink; binary detection added |
| ordered path→rule matching, brace expansion, overlay layering | `internal/config/rules/system_rules.go` | LIFT the matcher; config homes and file-reference model are AO's |
| rule grouping | `internal/delegate/rulegroup.go` | LIFT; dedupe instruction text, keep every file's provenance (A47) |
| `--diff-merges=first-parent`, untracked-file synthesis | `internal/diff/git.go` | LIFT those two behaviors only |
| anchoring resolver | `internal/diff/resolver.go` | REWRITE to the unique-match policy below |
| batching | `internal/scan/batch.go` | REWRITE (OCR chunks by count, not size) |
| fingerprint, manifest | `internal/session/manifest.go` | REWRITE (model kept, code not) |
| rule docs | `internal/config/rules/rule_docs/` | `go`, `rust`, `python` LIFT; `ts_js` REWRITE; `svelte`, `react` AUTHOR (D10) |

Not used: the LLM loop, relocation fallback, tools, telemetry, the
`.gitignore` matcher (tracked files in a forge diff are reviewable
regardless of ignore rules; Git's own exclude machinery enumerates
untracked files for local workspace review) (A45).

### Targets and materialization (A5, A6)

Targets: `workspace` (staged + unstaged + untracked, reviewed in place
on the given path), `commit`, `range from..to` (merge-base semantics),
`branch vs base`. For forge targets `review.materialize` resolves the
forge's latest diff version (base/start/head), fetches and verifies
those exact objects through the connection (forks included), and checks
out head into a detached review worktree. Every later phase reads that
snapshot.

`ReviewInput` is persisted per run, immutable: forge diff version,
base/start/head, merge base, ancestry result, normalized diff digest,
selected paths, resolved rules with provenance, overlay digest, prompt
and schema versions, provider/model per phase, harness profile digest,
reviewer code version.

### Selection and rules (Q17, D11)

Selection order: binary → user `exclude` → user `admit` (escape hatch
admitting an unsupported extension) → extension allowlist → default path
excludes → hardcoded directory blocklist. Generated, vendored, binary,
and deleted files never consume a model unit.

Rules: embedded defaults, then the AO project profile overlay, then the
in-repo `.agent-overflow/review.json`; repo wins. Overlay entries are
`{path, rule, merge_system_rule}` plus `exclude`/`admit`. React composes
as React + TS/JS for `**/*.{tsx,jsx}` at load so the text lives once.
Overlays are untrusted review criteria: they narrow or add rules and
can never touch tools, secrets, network, posting, command parsing,
output schema, or severity floors (A33).

### Bundles (D12, A13, A14)

Bundle key = resolved rule digest + component (first-level directory
under the nearest package/module root). Caps are tunables with defaults
4 files and ~12k diff tokens per bundle; a file over ~6k diff tokens
reviews alone. Bundles never grow to fit the fan-out width; files past
the width cap skip visibly with reason `budget` and the status comment
says a manual full review is needed.

### Findings (Q25, A9, A43)

```
{path, existing_code, claim, mechanism, consequence, confidence,
 category, severity, pre_existing, suggestion_code?}
```

Severity `critical|high|medium|low`; category
`bug|security|performance|maintainability|test|style|documentation|other`;
`pre_existing` marks a bug the change did not introduce. Enums are
strict: an invalid value fails the unit's envelope validation and the
unit retries. No private chain-of-thought is requested or forwarded;
`claim`/`mechanism`/`consequence` are the auditable argument.
`existing_code` is quoted verbatim from added lines.

### Anchoring (D7, A7, A35, A36)

Deterministic, after the unit join, per candidate: a unique exact match
(whitespace preserved, blank lines dropped on both sides) among the
added lines of the candidate's own file. Zero or multiple matches →
unanchored with a typed reason (`not_found`, `ambiguous`,
`outside_diff`). Inline anchors are single-line. Cross-file re-filing
and multi-line ranges are deferred until anchor telemetry exists.
Unanchored findings are never dropped: they go to the findings comment.

### Coverage (D9, A44)

The manifest starts from every discovered path and records each file's
eligibility outcome (`excluded`, `binary`, `deleted`, `generated`,
`oversize`, `budget`) or unit outcome (`reviewed`, `failed`, `reused`),
then verification and publication outcomes per finding. Eligible files
are sealed before dispatch. Run status derives from coverage only.
Checklist identity is `(path, status)`.

### Delta cursor (Q2, D17, A16–A20)

File fingerprint = status, old/new path, mode, object kind,
binary/submodule marker, and canonical ordered hunks with object ids
and hunk coordinates removed and whitespace preserved. Reuse key =
file fingerprint + range epoch (base/head ancestry and merge base) +
`ReviewInput` digest, so a rule, prompt, model, or profile change never
inherits "reviewed".

A later event reviews files whose key changed. Rebase-only or
merge-from-target with unchanged keys → no review, status note. Pure
renames (unambiguous via forge/Git rename metadata) reuse; rename plus
edit, splits, joins, and a non-descendant head with any ambiguous
mapping → full eligible-file pass.

Finding fingerprints (file + normalized claim + anchored quote) are the
write-idempotency layer. Semantic duplicate detection is the verifier's
job (below).

## Workflow

`review` (shared-scope starter, editable like any workflow):

| Phase | Driver | Notes |
|---|---|---|
| `materialize` | `builtin: review.materialize` | forge version → verified objects → review worktree; `ReviewInput` |
| `prepare` | `builtin: review.prepare` | selection, rules, bundles, sealed manifest |
| `review` | fan-out over `prepare.bundles`, agent units, `harness: review`, `access: read-only` | one unit per bundle; findings + per-file outcome |
| `anchor` | `builtin: review.anchor` | unique own-file match; typed failures |
| `verify` | agent, `session: fresh`, strong model | per candidate or small related batch: structured candidate, hunk plus enough resulting source, rule text and provenance, prior active findings on the same file; disposition `keep | remove(ground) | duplicate_of | supersedes`; for each prior finding on a changed file: `fixed | still_valid | unknown`. Read-only tools allowed when evidence is insufficient. OCR's removal grounds stay the only grounds: code absent from the diff, or a diff line literally contradicting the claim; a protected subject (memory safety, concurrency, auth, persistence, data loss) is kept unless the contradiction is literal |
| `post` / `emit` | `builtin` | forge posting (below) or the local findings artifact |

Per-phase `provider`/`model`/`effort` pick Claude or Codex freely; both
ship a `review` harness body (Q7). Luna does no judgment work: review
units and verification run on strong models; luna is reserved for prose
rendering (summary text) and may be removed from the workflow entirely
without loss (S2). The workflow never fixes code (D28).

Unit prompt shape (D27): the bundle's files, each with its rule group;
`(path, status)` checklist; strict focus (findings only on bundle
files, context reads are for understanding); tool-call budget block;
verbatim-quote contract; the findings schema. Verification prompt
adapted from OCR's `review_filter_task_*.md` with the analysis field
ordered before the ids field so the model reasons before it commits.

## Forge integration

### Connections and adapters (Q1, D13, A37)

Settings hold connections `{host, kind: gitlab|github, secret, auth}`;
the secret is a shared-scope profile secret (env or file source,
masked, never stored by AO). One Go interface, two adapters: open MRs,
MR metadata (head, base, draft, state, author), latest diff version
refs, object fetch, inline discussion create, top-level comment create
and update-by-marker, thread reply, thread resolve. REST throughout,
plus one GraphQL mutation on GitHub (`resolveReviewThread`, which has
no REST equivalent). No `glab`/`gh` dependency.

Positions: GitLab requires `position_type=text`, old/new path, and the
latest diff version's `base_sha`/`start_sha`/`head_sha`; added lines use
`new_line`. GitHub requires `path`, `line`, `side=RIGHT`, and the
analyzed `commit_id`. Both are generated from `ReviewInput` against the
head confirmed by the compare-and-swap below.

### Triggers and commands (Q5, Q15, Q16, Q21, D19, D25, D26, A29)

Automatic: MR opened non-draft, draft → ready, push (after `quiet`, at
most 5 automatic reviews per MR, then the status comment says manual
only). Drafts ignore pushes. Commands run on drafts too. Numbers and
the command set are per-automation settings.

Commands, parsed deterministically (regex): `@ao review` (delta),
`@ao review full`, `@ao pause`, `@ao resume`, `@ao help`.
`@ao <question>` inside a finding thread answers in that thread with
the finding's context (`forge.reply` + an agent phase on a strong
model). Authorization: the sender must hold a configurable forge role
(default GitLab Developer / GitHub write). Comments from the posting
identity or carrying AO markers are ignored (loop prevention).

### Push during a running review (Q14, D24, A12)

Finish, re-anchor, coalesce. In-flight units finish. Before posting,
the current head is refetched and compared with the materialized head:
equal → post inline; different → post the findings comment with every
finding under "superseded by a later push" (no inline writes against a
stale head) and the pending desired head runs as a normal delta review
after `quiet`. Cancel only on MR close or merge.

Field check: CodeRabbit never cancels and auto-pauses after 5;
Anthropic Code Review queues behind a running review and lists findings
on vanished lines under "Additional findings"; Sourcery caps automatic
re-reviews at 5 and lets a command reset the counter.

### Posting (Q4, Q12, Q13, Q18, D14, D16, A38–A42)

`review.post`, deterministic, through a durable outbox (per-operation
ids, bounded retries with backoff on 429/5xx, partial-write
reconciliation by reading back marker/ids):

1. Noise gate: critical/high always; medium posted with context; low
   suppressed; verifier-removed dropped; `duplicate_of` merged.
2. Fingerprint dedupe against findings already posted on this MR.
3. Deterministic order (severity, path, line); inline cap per run
   (default 20); body cap 8 KiB; overflow listed in the findings comment.
4. Inline discussion per finding: headline (severity, category,
   pre-existing marker), claim + consequence, `<details>` "Agent prompt"
   holding a fenced block (the forge's code-block copy button is the
   copy button).
5. One findings comment per run: counts, coverage, skipped files with
   reasons, unanchored findings, overflow, superseded section
   (collapsed).
6. One persistent status comment per MR, upserted by marker: analysis
   state (running / complete / partial with reasons) and publication
   state (posted / partial / failed with cause), paused, auto budget
   spent, last reconcile time.
7. Resolve threads whose prior finding the verifier marked `fixed`.
   `unknown` stays open.
8. Advance the cursor and record posted fingerprints only after the
   outbox is drained.

A per-automation option holds findings for approval in AO before
posting, through the engine's `human` gate (Q19). Posting never
approves or blocks an MR.

### State (D21, Q20)

`review_targets` keyed by `(connection, repo, mr)`: reuse keys per
file, auto-review count, paused flag, posted finding fingerprints with
thread ids, status-comment id, last reconcile. `webhook_receipts` and
`review_outbox` hold intake and publication records. Written by
builtins and intake only. On boot, webhook automations reconcile open
MRs against `review_targets` and enqueue changed heads.

## Local reviews (Q22, Q23, Q24, D31, D32, D33)

`/ao-review` in the composer inserts a block like `/workflow`'s: how to
start a `review` run with a target, wait, and read results. The agent
runs `agent-overflow run start review --seed target=...`, `run wait`,
`run output --out <file>` and reads the findings file. The run also
shows in the workflows section with the findings view.

Findings view (one component for local results, the approval hold, and
run history): per finding, open file at line and copy the agent prompt.

## Harness profiles (Q6, D20, A31–A34)

Named, reusable, provider-generic by name with per-provider bodies.
Internally a body is two composable parts: instructions (prompt) and
tool policy (Claude disallowed tools, Codex toggles), so a later
workflow can reuse one without cloning the other. Selectable on any
thread at creation and referenced by workflow phases through
`harness:`. Spawn-only, snapshotted per run with a digest; never
reconciled live.

Composition order, fixed: engine safety floor → profile → workflow-owned
review instructions → repo rule overlay (criteria only) → unit prompt.
The `review` profile's floor disables writes, network tools, MCP
servers, and secret-bearing or environment-inspecting tools, not just
filesystem writes. The settings-level overrides spec's "per-thread:
non-goal" line is retired.

## Threat model (A4, A29, A33, A34)

Untrusted inputs: MR code, repo rule overlays, forge comments, webhook
bodies. Controls: overlays are criteria only; commands need a forge
role and the posting identity is ignored; webhook auth per source with
replay dedupe and body caps; connection hosts are operator-configured
(no user-supplied URLs reach the adapter); review sessions cannot write
or reach the network; secrets resolve at spawn, are masked in logs, and
never enter prompts; filenames and content are treated as data in every
prompt and comment body.

## Observability (A41, A49)

Per phase and unit: provider, model, input/output/cache tokens, tool
calls, wall and queue time, retries, throttles. Per run: candidate →
anchored → verified → deduped → gated → posted counts with reasons for
every drop. Per automation: daily review cap (setting) and spend
summary. All of it is run state in the workflows UI, not log lines.

## Non-goals and deferrals

- Automatic fixing inside the review workflow.
- Send-to-thread from the findings view.
- Jira webhooks (mapper slot only).
- Bot identity management beyond the provided secret.
- Rule docs beyond go, rust, python, ts/js, svelte, react.
- Deferred: `capture:` (S1); cross-file re-filing and multi-line
  anchors (A35, A36); an evaluation corpus (S3: OCR's published
  benchmark covers the lifted pieces; the rewrites are covered by
  conformance fixtures instead).

## Migration / removal

| Old | New | Action |
|-----|-----|--------|
| `Phase.Capabilities`, `Phase.MCP`, `Phase.Commands` | `harness:`, `builtin:` | DELETE |
| `poll-jira-and-start` `capabilities:` line | `grants: [start-run, update-notes]` | MIGRATE |
| workflows-system.md "External sources: poll with authenticated CLIs, no webhooks" | webhook trigger section | MIGRATE |
| prompt-tool-overrides.md "per-thread overrides: non-goal" | harness profiles | MIGRATE |

## Success criteria

- [ ] `/ao-review` in a thread → run → findings file; every discovered file carries an outcome; findings carry file:line or a typed unanchored reason
- [ ] MR opened on a GitLab and a GitHub test repo → inline findings, findings comment, status comment after `quiet`
- [ ] push mid-review → no cancel; stale head → summary-only with superseded section; then a delta review posting zero duplicate findings
- [ ] rebase-only push → no review, status note; 6th push → status says manual only
- [ ] `@ao pause` / `resume` / `review` / `review full` work for an authorized sender and are ignored for an unauthorized one; a thread reply is answered; drafts ignore pushes and ready triggers a full review
- [ ] bad source-specific auth → rejected; replayed delivery → recorded skip, no second run; heads changed while the app was down are reviewed at boot
- [ ] a rule or model change invalidates reuse keys; a pure rename reuses
- [ ] publication failure mid-batch leaves the status comment saying partial, and a rerun completes without duplicates
- [ ] harness profile verified on the wire for Claude and Codex (prompt replaced, tools absent, network tools absent)
- [ ] `poll-jira-and-start` runs with grants; `make check` and `make test` green

## Testing

- `internal/review`: OCR's parser and matcher tests ported; conformance fixtures for anchoring (blank lines, CRLF, Unicode, repeated quotes, literal `+`/`-` prefixes, ambiguity), fingerprints (rename, split/join, force-push, merge-base move), selection, bundling, manifest.
- Scheduler and intake: per-source auth and dedupe, receipt transactions, debounce across restart, `max_wait`, bypass events, one-automation-per-target refusal.
- Forge adapters: `httptest` fakes for both forges including position rejection, 429, partial batch failure; mapper fixtures from recorded real payloads.
- Engine: builtin registry and typed outputs, harness snapshot at run creation, deleted fields fail to compile.
- Profiles: spawn-path assertions on argv/config for both providers.
- e2e harness: mocked providers emit fixed findings → comments land on a fake forge; stale-head path; outbox reconciliation.
- Live, manual: one real MR per forge.
