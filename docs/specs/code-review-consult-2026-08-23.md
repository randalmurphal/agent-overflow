Status: consult record, codex gpt-5.6-sol at xhigh, session 01a02d37-068f-7c53-bb81-110a7f51c42d, 2026-08-23. Input was the first draft of [code-review.md](code-review.md); adjudication outcomes live in that spec and the commit that added both files. Citations are the consult's own.

# Senior design opinion: code review workflow

## 1. Verdict in five lines

1. The product shape is right: a deterministic review shell around provider-native agents, durable workflow runs, delta review, and one stable status surface is a credible design for subscriptions rather than API-token economics.
2. The current spec is not ready to build because its correctness boundary ends too early: it lacks an immutable review snapshot, durable webhook intake and posting, automation-scoped state, and a final head compare-and-swap before publication.
3. `builtin:` belongs in the engine; `capture:` does not belong beside it as a first-class execution primitive, and the proposed builtin subprocess-envelope emulation is the wrong abstraction.
4. The OCR port plan overstates what can be lifted: the resolver, batching, ignore behavior, rule grouping, enum handling, fingerprinting, and manifest denominator all differ materially from the behavior this spec promises.
5. I would ship a narrower anchoring/comment surface and spend the saved complexity on precision evaluation, failure visibility, token accounting, delivery idempotency, and exact forge semantics.

## 2. Disagreements with the owner's rulings

I agree with the product rulings as directions: both forges, both providers with per-phase choice, deterministic commands, named spawn profiles, repo rule precedence, workflow-owned runs, webhooks with explicit polling fallback, in-process builtins, finish/re-anchor/queue on push, thread-local `/ao-review`, the two findings actions, and append-only per-run findings plus one persistent status comment. I have three qualified disagreements with the mechanisms currently attached to those rulings.

1. **The conflict/debounce resource may be per merge request, but review state cannot be.** If two enabled automations match the same `(connection, repository, merge request)`, a target-only debounce key lets one automation replace the other's pending event, and a target-only cursor makes one configuration declare the other's files reviewed. Either enforce exactly one review automation per target in v1, or key pending work, cursors, budgets, and configuration snapshots by `(automation_id, connection, repository, merge_request)` while retaining a separate target resource lock to prevent concurrent publication.
2. **A per-file diff fingerprint is a useful delta cache key, not sufficient evidence that reuse is safe across every history rewrite.** I would keep the ruling, but require a range epoch and conservative invalidation for non-descendant heads, changed merge bases, ambiguous rename/split mappings, and review-input changes. Otherwise a target-branch change can alter the behavior of an unchanged patch without changing that file's fingerprint.
3. **Keep an OCR-style noise gate, but do not copy OCR's hunk-only reflection contract or “protected subject means approve” branch.** Literal presence in a hunk disproves neither a semantic false positive nor an obsolete prior finding. A cheap verifier should receive the candidate's claim and evidence, relevant source, applicable rule, and bounded tools, and it should be stricter—not bypassed—when the candidate concerns security, auth, persistence, concurrency, or data loss.

## 3. Answers A–H

### A. `builtin:` and `capture:`

`builtin:` is the right capability; `capture:` is the wrong peer primitive.

The engine needs a closed, in-process binding for deterministic operations that must share transactions, typed state, cancellation, and application services. Review preparation, anchoring, aggregation, and publication are good examples. But “capture stdout” describes a result codec for an external command, not a kind of work. Making it a top-level phase primitive produces an awkward matrix as future runners appear: builtin/capture, command/capture, container/capture, and then special rules about which combinations may emit envelopes.

I would make a tool phase resolve one discriminated executor and, only for subprocess executors, one bounded result codec:

```yaml
tool:
  uses: builtin:review.prepare
```

```yaml
tool:
  uses: command:review-indexer
  result:
    codec: ao-envelope # or stdout
    max_bytes: 1048576
```

`builtin:` names remain a closed Go registry. `command:` resolves only an operator-owned, profile-bound argv definition; it never parses a shell string and never executes a repository-provided path. A later genuinely sandboxed extension can add an honest namespace such as `wasm:` or `container:` with its own trust contract. That is cleaner than pretending an arbitrary executable becomes an in-process builtin by agreeing on stdout.

Builtins should return typed engine values directly and persist the canonical result through the engine's normal phase-output path. They should not manufacture `AO_ENVELOPE` bytes and feed them back through the subprocess parser. The envelope is a subprocess ABI. Reusing it in-process throws away type safety, adds serialization failure modes, and makes the builtin registry look less privileged than it is. Each builtin should receive `context.Context`, immutable resolved inputs, a narrow capability object for the services it actually needs, and a declared output type/size. Missing names, invalid types, oversized output, panics, and cancellation all fail visibly.

For future user-defined deterministic tools, an operator-owned named command is the safe first answer. The security property comes from *who controls the argv, cwd policy, environment allowlist, secret references, executable digest, and output limit*, not from calling stdout `capture:`. Repository content remains input, never authority. I would also require an explicit project allowlist for globally defined commands before a workflow in that project can invoke them; an operator-owned global profile is not enough if an untrusted repository can add or alter the workflow that names it.

Nothing in this review flow needs raw stdout in v1. Cut `capture:` now; add `result.codec: stdout` only when a real user-defined tool needs it and its truncation, encoding, and failure semantics have a testable consumer.

### B. Workflow granularity and reflection input

The conceptual stages are nearly right, but the actual correctness pipeline should be:

`materialize immutable input → prepare units → review units → deterministic validate/anchor → verifier fanout → aggregate/dedupe/rank → re-anchor to current head + publish`

The first missing stage matters most. `prepare` must resolve the forge version, fetch and verify the exact base/start/head objects, create a read-only review worktree or equivalent immutable checkout, and store an input manifest. Every unit, rule selection, model session, and initial anchor must read that same snapshot. “Finish the current run, then re-anchor” is only meaningful if everyone agrees what the current run analyzed.

I would keep anchor after the unit join for v1. Anchoring is deterministic and cheap; one batch pass can parse each file once, preserve a single resolution policy, deduplicate candidates globally, and avoid teaching the workflow engine a nested map-within-map construct just for this feature. Internally, anchor each candidate independently and retain its `unit_id`, file eligibility, original quote, rule provenance, and failure reason. If the engine later gains pipelined maps, validation can be fused into unit completion to start verification earlier, but that is a throughput optimization rather than a new semantic phase.

Reflection should not see hunks alone. Hunk-only reflection can answer “does this exact text occur here?”; it cannot answer “is the claim true?”, “did the change introduce it?”, “is the behavior guarded elsewhere?”, or “is this a duplicate of an open finding?” It should see:

- the structured candidate: concise claim, mechanism, consequence, confidence, category, severity, cited path/quote, and the unit's concise evidence;
- the applicable hunk plus enough of the resulting file to establish behavior, with access to bounded read-only repository search/read tools;
- the exact rule text and provenance that caused the candidate to be sought;
- prior active findings that could be duplicates or continuations; and
- explicit verification questions: reproduce the code path, look for guards/callers/tests, decide introduced/pre-existing/unsupported, and cite the disconfirming or confirming evidence.

Do not ask the first model for private chain-of-thought and do not pass it onward. Ask for a short auditable argument and evidence fields. The verifier can inspect that reasoning without turning unbounded prose into the protocol. Luna/Fable are plausible verifier models only after an evaluation corpus demonstrates they preserve recall on the important classes; category protection should raise the evidence threshold or route to Sol/Opus, not auto-approve the first model.

The final current-head anchor belongs immediately before posting, not merely before reflection. Re-fetch the forge head, compare it with the expected head, and either map every accepted finding onto that exact head or write only the unanchored run summary and enqueue the new head. There must be no interval in which a stale `head_sha` or GitHub commit is knowingly used for inline publication.

### C. Rule-group bundles versus one file per unit

Between the two proposed choices, **one file per unit is the safer precision baseline**. Arbitrary same-rule bundles optimize repeated prompt text while mixing unrelated call paths, increasing attention dilution and making attribution, retry, coverage, and token accounting coarser. Claude Code and Codex already have repository tools; the reviewer can start from one focused diff and inspect exactly the neighboring definitions it needs instead of receiving several unrelated diffs because they share a rule.

One file per provider process is not automatically token-efficient, however. It repeats startup instructions, repository orientation, and rule material; tiny related files may be impossible to understand separately. My production shape would therefore be **topology-aware microbundles**, not rule-only bundles:

- same resolved rule digest and same component/package/first-level module;
- at most 4 changed files and roughly 12,000 diff-input tokens as an initial, measured cap;
- one file alone when it exceeds roughly 6,000 diff tokens or is marked high-risk;
- generated/vendor/binary/deleted files never consume a model unit merely to be classified;
- the model may read other repository files, but may emit findings only for explicitly eligible changed files in its unit; and
- concurrency and bundle size remain independent: a 32-file change becomes queued waves, never larger bundles to fit a fanout limit.

Those numbers are hypotheses, not industry truths. Before choosing them permanently, run file-only, two-file, and four-file related bundles over a labeled corpus and measure important-finding recall, precision, duplicate rate, anchor success, input/output tokens, wall time, and subscription throttling. The design currently fixes a policy before defining that experiment. For Sol/Opus/Sonnet, focus generally buys more than broad diff packing; for Luna/Fable, smaller tasks also reduce instruction loss. Shared prompt-prefix caching may make repeated rules cheaper than the spec assumes, while a large mixed bundle can waste far more reasoning tokens than it saves.

### D. Per-file diff fingerprints

A per-file fingerprint works well for exact repeat detection and partial retry. It breaks when “same patch bytes” ceases to mean “same review obligation.”

- **Renames:** a path-bearing fingerprint changes on every rename, forcing review but orphaning prior identities; a path-free fingerprint can wrongly transfer a finding into a semantically different location. Use forge/Git rename metadata only to transfer logical identity when old/new blob or normalized patch equivalence is unambiguous. Pure renames can be reused; rename-plus-edit is a new unit.
- **Splits and joins:** there is no one-to-one file cursor. Review all destination files. Content similarity may help suppress duplicate prose, but must never mark a destination covered or resolve a source finding automatically.
- **Conflict resolutions:** identical added lines can acquire different surrounding control flow, imports, ownership, or synchronization. Include ordered surrounding context in the fingerprint and invalidate when a target/base change touches a dependency identified by the original finding or reviewer.
- **Squash and force-push:** patch equivalence can justify reuse after a harmless history rewrite, but a non-descendant head destroys the commit-range cursor and can conceal removed/reintroduced behavior. Record ancestry and merge-base epochs; fall back to a full eligible-file pass unless every file maps unambiguously and the complete normalized patch set is equivalent.
- **Rebases and base movement:** deleting object IDs and hunk line numbers gives a reasonably stable patch identity, but the target branch may have changed behavior outside the patch. A changed merge base should at least rerun semantic verification against the new snapshot, even if generation is reused.
- **Modes, symlinks, submodules, binaries, and path case:** content-only fingerprints miss executable-bit changes, symlink-target changes, gitlink updates, binary replacement, and case-only renames. Status, old/new paths, modes, object kind, and binary/submodule markers belong in the canonical record.

The OCR fingerprint hashes mode, old path, new path, and its raw per-file diff after trimming trailing newlines. Raw hunk headers and context therefore make it more position- and diff-algorithm-sensitive than the spec's “rebase with identical content does not re-review” promise. I would canonicalize ordered hunks while excluding object IDs and hunk coordinates, retain exact whitespace, and include file status/path/mode/object kind. Do not use Git's stable patch-id directly: its whitespace insensitivity is useful for commit equivalence but too lossy for code review.

The actual reusable key is:

`file_patch_fingerprint + range_epoch + review_input_digest`

`review_input_digest` must include the resolved rule set and provenance, repository overlay, prompt/schema version, builtin/reviewer version, provider/model, harness-profile snapshot, and materialization mode. A model or rule change should not silently inherit an old “reviewed” claim. Store base/start/head, merge base, ancestry result, rename map, and input digest beside each file decision.

Finally, exact finding fingerprints should be the fast idempotency layer, not the only duplicate detector. Model wording and anchor lines drift. The verifier should compare a new candidate to active findings for the same logical file/component and classify `same`, `supersedes`, or `distinct`, while deterministic IDs prevent duplicate API writes.

### E. Webhook key, debounce, and conflict handling

Debounce semantics belong in the scheduler, but webhook durability belongs in intake.

The HTTP handler should do only bounded work: limit the raw body, authenticate it with the provider-specific scheme, parse the supported event, normalize a delivery and resource key, authorize/ignore the actor where relevant, transactionally insert a deduplicated receipt and update pending-trigger state, then return 2xx comfortably inside GitHub's 10-second deadline. It should not start a workflow, call a provider, fetch a repository, or hold an in-memory debounce timer.

The scheduler can remain single-goroutine if that goroutine only owns ordering decisions and timers. Pending events and `not_before`/`max_wait_at` timestamps must live in SQLite so restart does not erase the quiet window. On boot, scan due/pending triggers and reconcile enabled review targets with the forge head. At fire time, fetch current MR state and head; webhook payloads are hints, not authoritative inputs.

Use two different keys:

- **pending/debounce key:** `(automation_id, connection_id, repository_id, merge_request_id, trigger_class)`;
- **publication resource lock:** `(connection_id, repository_id, merge_request_id)`.

This allows two explicit review automations to retain independent cursors/configurations while preventing them from racing the same comments. If that multiplicity is not wanted, make one-review-automation-per-target a schema invariant and fail configuration instead of silently coalescing across definitions.

Two minutes of quiet is a reasonable starting default, but add a maximum wait—10 minutes is a defensible first value—so a continuously pushed branch cannot starve forever. `review`, `full`, `resume`, ready-for-review, close/reopen, and policy changes should have specified bypass/reset behavior; they should not accidentally inherit a push debounce. `on_conflict: queue_latest` is correct for pushes during a running review, but “latest” must be a durable desired-head record, not a queued copy of an old webhook payload.

This durability is not optional. GitHub says it does not automatically redeliver failed deliveries, while GitLab disables failing hooks after repeated failures. A boot-time head scan can recover a missed push, but it cannot recover a lost comment command. The intake record is also the audit trail for duplicate delivery IDs, unauthorized commands, malformed payloads, and why a run did or did not launch.

### F. Harness profiles

Named, provider-specific, spawn-only profiles are the correct model. The main future hazard is treating a mutable name as the configuration.

Resolve the selected profile once at run creation, validate the requested provider body, snapshot its canonical body and digest into `ReviewInput`, and pass that snapshot to every later spawn. If an operator edits `strict-review` while 20 units are queued, the run must not contain two tool policies under one name. Display both name and digest/version in the run. Missing provider bodies or unsupported provider flags fail before fanout; they do not fall back to defaults.

Keep the persisted model domain-neutral enough for other workflows. A profile should compose at least two independently reusable concerns:

- provider-specific instructions/prompt additions; and
- provider-specific tool/capability policy.

Review then selects a profile and overlays non-negotiable workflow invariants. In particular, a repository overlay may change what to review but must not re-enable writes, network, MCP servers, environment inspection, shell escape routes, or secret-bearing tools that the review sandbox disabled. “Read-only filesystem” alone does not prevent source or secret exfiltration through network tools. Provider-native names differ, so validate each provider body rather than inventing a false cross-provider tool abstraction.

Specify composition order now: engine safety floor → operator profile → workflow-owned review instructions → repository rule overlay as *review criteria only* → unit prompt. Repository instructions are untrusted data. They may narrow or add rules because the owner deliberately chose repo overlay precedence, but they cannot alter authorization, tool grants, post destinations, token identity, command parsing, output schema, or severity protections.

For non-review workflows, the likely pressure will be “same tool policy, different instructions” or “same instructions, stronger model.” If the schema makes the named profile an indivisible review-shaped blob, users will clone profiles and they will drift. Preserve references/composition internally even if the first UI exposes a single name.

### G. What to cut and what to add

#### Cut for the first shippable version

1. Cut `capture:`; no success criterion needs it.
2. Cut multi-line inline ranges and arbitrary unchanged-line comments; post one changed-line anchor or place the finding in the per-run summary. This removes two forge-specific failure surfaces without losing a finding.
3. Cut OCR's cross-file quote relocation. Require a unique eligible added-line match in the candidate's own file; otherwise preserve the finding unanchored. Cross-file moves can be added after measured anchor telemetry exists.
4. Cut rule-only variable-size bundling; ship file units plus at most a simple same-component microbundle behind a measured cap.
5. Cut generalized user-defined in-process plugins. Operator-owned named argv tools are enough; a sandboxed plugin ABI is a separate design.
6. Cut active thread auto-resolution if the first-release criteria permit it; native “outdated” state plus the persistent status comment is safer than false resolution. If auto-resolution is non-negotiable, it must include a semantic prior-finding verifier and GitHub GraphQL, so it is not a small REST adapter feature.
7. Cut any approval/blocking behavior; neutral/comment-only output is the least disruptive initial social contract.

I would not cut either forge, either provider, webhooks, polling fallback, delta review, command handling, persistent status, local review, re-anchor-on-push, or the reflection/noise gate; those define the product being evaluated.

#### Add before implementation

- **Repository materialization:** define connection-to-project mapping, fork/source-repository handling, authenticated fetch, object verification, exact-SHA checkout, mirror/worktree retention, submodules/LFS policy, and deletion. The spec currently jumps from a forge event to a local diff without defining how the code arrives.
- **Immutable `ReviewInput`:** forge version, base/start/head, merge base, ancestry, normalized diff digest, selected paths, rules, profile, prompts, provider/model, and code/schema versions.
- **Durable inbox and outbox:** webhook receipts, desired-head coalescing, deterministic publication operation IDs, retries with backoff, partial-write reconciliation, and cursor advancement only after the intended publication state is durable.
- **Command authorization and loop prevention:** require a configurable forge role (for example GitHub collaborator/write or GitLab Developer) for token-spending/state-changing commands; ignore the configured posting identity and machine markers.
- **Failure as product state:** separate analysis coverage from publication delivery; expose per-unit failure, auth/permission failure, rate limiting, stale-head requeue, unanchored findings, partial posting, and recovery action in the workflow run and status comment.
- **Budgeting and observability:** input/output/cache tokens, tool calls, provider/model, wall time, queue time, retries, subscription throttles, candidate/anchor/verify/dedupe/noise/post disposition counts, and per-run/per-repo caps.
- **An evaluation gate:** a versioned labeled corpus with seeded production defects and clean controls, measured by severity recall, precision, comments per KLOC/diff, duplicate rate, anchor rate, tokens, latency, and mutation sensitivity.
- **Publication policy:** deterministic ordering, a configurable inline cap, body byte cap well below forge limits, one batched GitHub review where safe or isolated/reconcilable writes where not, and an explicit overflow summary.
- **Head CAS:** re-read the current head immediately before each publication batch and abort/re-anchor/requeue on mismatch.
- **Security/threat model:** untrusted PR code and repo rules, forked PR credentials, command spoofing, webhook replay, SSRF via self-hosted forge URLs, provider-tool exfiltration, malicious filenames/content, and secret redaction in prompts/logs.

### H. OCR lift plan

The lift plan is too optimistic. OCR is a valuable behavior reference and source of small algorithms; it is not a package boundary that already matches Agent Overflow.

1. **`internal/diff` parser and types: adapt, do not wholesale lift.** The parser imports OCR runner/model/warning machinery and reads full files. Port the diff grammar, hunk model, and focused tests behind AO-owned byte/reader APIs, strict limits, explicit warnings, binary detection, and errors.
2. **The resolver must be rewritten to the promised policy.** OCR tries new-hunk context+added lines, old-hunk context+deleted lines, then the full new file; it takes the first within-file match, strips leading `+`/`-` from actual content, and has inconsistent blank-line normalization between candidate and target. That can anchor deleted, context, outside-diff, or ambiguous text. AO should require a unique own-file eligible added-side match, retain exact whitespace, and return typed ambiguity/not-found reasons.
3. **`internal/scan/batch.go` is not the specified bundler.** OCR's scan batching groups full-scan items by language, first-level directory, or none and chunks by file count. It does not implement diff-byte/token caps, rule-group bundles, or AO coverage. Reuse no more than generic chunking tests.
4. **Rule-group semantics contradict the spec.** OCR's `delegate.GroupRules` key includes source, pattern, and rule text specifically so identical text with different provenance remains separate. The spec says identical rule text shares work. Choose one behavior explicitly; I recommend deduplicating model instruction text while retaining all provenance records on every result.
5. **The rule loader is entangled.** Its useful parts are ordered rule matching, language/path patterns, system-versus-overlay concepts, and perhaps brace expansion. Its config homes, warning sinks, file-reference model, and defaults are OCR-specific. Note that its brace expansion handles only the first brace group, so do not advertise general glob expansion without new tests.
6. **Do not apply OCR's root `.gitignore` matcher to tracked MR diffs.** A tracked file does not cease to be part of a review because a later ignore rule matches it. Let Git enumerate tracked changes; use `git ls-files --others --exclude-standard` for untracked local review. OCR also silently ignores unreadable ignore files/untracked files and lacks binary detection in untracked synthesis; AO's error policy requires explicit outcomes.
7. **Enums must be strict.** OCR maps unknown category to `other` and unknown severity to `low`; under AO's low-severity noise gate, a typo can silently erase a finding. Reject invalid schema values or preserve `unknown` as a visible parse failure.
8. **The manifest idea is more valuable than the concrete denominator.** OCR registers selected files after deletions/filters, then seals `completed/reused/failed/waived`; excluded, binary, deleted, oversize, and filtered files disappear before the denominator. AO needs a versioned manifest from discovered file through eligibility, unit, result, verification, and publication, with explicit skip reasons and separate eligible-review coverage.
9. **Lift more manifest metadata conceptually.** OCR records resolved commits, repository identity, source artifact, rules/config hashes, runtime config, and versioned schema. AO should retain analogous immutable provenance rather than only final counts.
10. **Do not lift the raw fingerprint as the delta contract.** OCR hashes mode, paths, and raw per-file patch text. It is fine as a cache checksum, but it does not meet the promised stable-rebase semantics and omits the broader review-input digest.
11. **Treat the reflection prompts as seeds.** Their conservative two-ground filter is useful wording, not demonstrated precision evidence. Rebuild the protocol around structured claims, evidence, prior findings, and explicit verifier dispositions, then benchmark it.
12. **Preserve attribution and tests.** Port only the Apache-2.0-covered code actually used, keep notices where required, and translate behavioral tests—including pathological blank lines, prefixes, duplicate quotes, CRLF, rename, binary, and position cases—rather than copying packages and deleting their dependencies afterward.

The exact OCR sources make these mismatches inspectable: [diff resolver](https://github.com/alibaba/open-code-review/blob/66120291271b2e605e420e9f11fbd6448f06163f/internal/diff/resolver.go), [scan batching](https://github.com/alibaba/open-code-review/blob/66120291271b2e605e420e9f11fbd6448f06163f/internal/scan/batch.go), [rule grouping](https://github.com/alibaba/open-code-review/blob/66120291271b2e605e420e9f11fbd6448f06163f/internal/delegate/rulegroup.go), [gitignore handling](https://github.com/alibaba/open-code-review/blob/66120291271b2e605e420e9f11fbd6448f06163f/internal/diff/gitignore.go), and [session manifest](https://github.com/alibaba/open-code-review/blob/66120291271b2e605e420e9f11fbd6448f06163f/internal/session/manifest.go).

## 4. Research findings with sources

### What current review products actually document

| Product | Push/incremental behavior and anti-spam | Duplicate/stale handling | Comment shape and commands |
|---|---|---|---|
| CodeRabbit | Incremental review on each new push is on by default and focuses on commits since the last review; automatic review pauses after **5 reviewed commits by default**, with `0` disabling the pause and `1–2` recommended for active PRs. | Its public docs describe incremental versus full review, but do not document a debounce interval, force-push/rebase fallback algorithm, or exact cross-round finding fingerprint. Do not infer those internals. | `@coderabbitai review`, `full review`, `pause`, and `resume`; the first is incremental and full starts from scratch. [Automatic review controls](https://docs.coderabbit.ai/configuration/auto-review), [commands](https://docs.coderabbit.ai/guides/commands). |
| Anthropic Code Review | Configurable once-on-open, after every push, or manual. `@claude review`/`once` is one run; `@claude review always` subscribes future pushes. Requests made during a run queue. Anthropic reports about **20 minutes average** and no automatic retry after failure. | It documents specialized generation, verification against actual behavior, deduplication, severity ranking, and resolving a thread on the next push when fixed. A push during a run can leave moved-line results under **Additional findings** rather than dropping them. | Important/nit/pre-existing labels, compact inline finding plus collapsible verification reasoning, a neutral check with a complete fallback finding list. Commands require owner/member/collaborator access. [Claude Code Review](https://code.claude.com/docs/en/code-review). |
| GitHub Copilot code review | Manual is the default. Repository rulesets can request automatic review and separately enable review on every new push. | GitHub explicitly warns that a re-review may repeat comments even if they were resolved or downvoted—useful evidence that duplicate suppression is not solved merely by using the forge's thread state. | It submits a `COMMENT` review rather than approving/blocking and supports thumbs feedback; the documented interaction is reviewer assignment/UI, not a PR comment-command vocabulary. [Copilot code review](https://docs.github.com/en/copilot/how-tos/copilot-on-github/use-copilot-agents/copilot-code-review). |
| Graphite | New and updated matching PRs are reviewed, but public docs do not publish debounce, commit-range, force-push, or stale-thread algorithms. PRs above **200,000 changed characters** are not reviewed. | It exposes accepted-issue and downvote-rate analytics, but not a public cross-round dedupe contract. | Each inline comment uses the socially sound three-part form: problem, why it matters, concrete fix. [Review comments](https://graphite.com/docs/ai-review-comments), [customization](https://graphite.com/docs/ai-review-customization). |
| Qodo / PR-Agent | Push-triggered tools are opt-in and draft PRs are skipped by default. The open-source improve tool's recommended low-noise configuration uses persistent inline comments off, focuses on problems, defaults to **3 suggestions per chunk**, and caps processing at **3 model calls**. Incremental update is documented only for GitHub. | Persistent inline mode embeds hidden fingerprints to update suggestions, but is off by default; the project advertises a self-reflection step. | Deterministic commands include `/review`, `/describe`, `/improve`, `/ask`, and related tools. A table is the default suggestions presentation; inline comments are a noisier opt-in. [Improve tool](https://github.com/The-PR-Agent/pr-agent/blob/main/docs/docs/tools/improve.md), [automations](https://github.com/qodo-ai/pr-agent/blob/main/docs/docs/usage-guide/automations_and_usage.md), [project](https://github.com/qodo-ai/pr-agent). |
| Sourcery | Reviews on open and uses a lighter pass on every later commit. Automatic re-reviews are capped at **5 per PR**; `@sourcery-ai review` resets the counter and performs a full review. Separate diff-character limits vary by plan. | Each re-review rechecks existing comments and resolves those addressed, reruns security scans, and leaves summary/guide/other comments in place. Consistently negative reactions suppress similar future comments; an outdated comment counts as positive feedback. | Typed concise prefixes such as `issue (bug_risk):`, optional suggestion blocks, a stable summary and reviewer guide, plus `review`, `summary`, `guide`, `title`, `resolve`, `dismiss`, and reply chat. Bot comments are ignored to prevent loops. [Anatomy](https://docs.sourcery.ai/reviews/anatomy-of-a-review/), [commands](https://docs.sourcery.ai/reviews/commands/). |

The recurring industry lesson is not one magic delta algorithm. It is layered noise control: focus the generation unit, verify candidates, deduplicate against history, cap automatic rounds or output, preserve a complete non-inline fallback, collect accept/downvote signals, and leave a deterministic manual full-review escape hatch. AO has several of these, but its current reflection and stale-resolution rules are weaker than the products it is emulating.

The comment formats with the best published rationale are also restrained: one actionable problem, one consequence, one proposed direction; severity/category as small metadata; optional evidence/details collapsed or in the run summary. A comment should not contain the whole rule, model transcript, generic praise, or a long tutorial. Keep each inline body well below forge limits—an application cap around 4–8 KiB is ample—and put coverage, skipped files, and run-wide detail in the per-run comment.

### GitLab and GitHub API reality

#### GitLab discussions

A diff discussion position is not merely `path + line`. GitLab requires `position_type=text`, `old_path`, `new_path`, and the `base_sha`, `start_sha`, and `head_sha` from the **latest merge-request diff version**. Added lines use `new_line`; removed lines use `old_line`; an unchanged context line uses both. Multi-line positions add `line_range` with typed start/end endpoints. The adapter must fetch the latest version and generate positions against it; a merge base guessed locally is not a substitute. Wrong or stale SHA triples are an expected post-time failure, especially across rebase/force-push. [GitLab Discussions API](https://docs.gitlab.com/api/discussions/).

Treat “unchanged line” as “unchanged context represented by the forge diff,” not arbitrary repository code. If a finding is outside the postable diff or its line mapping is ambiguous, put it in the per-run findings comment with `path:line`; do not expand diff context through expensive speculative API calls merely to force an inline.

GitLab caps an issue/MR/commit at **5,000 comments** and a comment at approximately **1 million characters / 1 MB**. Self-managed note-creation throttling is configurable and documented with a **300 requests/minute default** when enabled/configured; those high ceilings do not make dozens of model comments socially acceptable. [GitLab instance limits](https://docs.gitlab.com/administration/instance_limits/), [note creation rate limit](https://docs.gitlab.com/administration/settings/rate_limit_on_notes_creation/).

#### GitHub review comments

For a line comment, GitHub's REST API requires `path`, `line`, and `side`; `side=LEFT` addresses a deletion, while `RIGHT` addresses an addition or context in the resulting file. A file-level comment uses `subject_type=file` instead of a line. For a multi-line range, `line` is the end, `start_line` is the start, and `start_side` is required; ranges must be representable in the same diff hunk. `position` is being closed down in favor of line/side. Send the analyzed `commit_id` explicitly even though the endpoint can default to the latest commit. [GitHub review-comment REST API](https://docs.github.com/en/rest/pulls/comments).

GitHub's Files Changed UI began previewing comments on unchanged lines in changed files, but GitHub said API support was still limited; the REST contract remains diff-oriented. AO should therefore make unchanged/outside-hunk findings summary-only until an API capability probe says otherwise. [GitHub unchanged-line preview](https://github.blog/changelog/2025-09-25-pull-request-files-changed-public-preview-now-supports-commenting-on-unchanged-lines/).

GitHub can create multiple comments as one pending review, which reduces notification and request volume, but a bad child comment can reject the review request; the outbox needs per-finding operation identity and reconciliation rather than treating a 422 as “nothing happened.” Individual comment writes isolate bad anchors but amplify notifications and content-creation throttles. GitHub documents a general secondary ceiling of **80 content-generating requests/minute and 500/hour**, warns that some endpoints are lower, and may change undisclosed limits. The review-comment endpoint does not publish a reliable per-PR comment-count or body-size maximum, so AO should enforce its own small caps rather than depending on folklore. [Create a review](https://docs.github.com/en/rest/pulls/reviews), [GitHub REST rate limits](https://docs.github.com/en/rest/using-the-rest-api/rate-limits-for-the-rest-api).

The spec's “all forge adapters use REST” rule is incompatible with automatic GitHub thread resolution. GitHub exposes `resolveReviewThread` as a **GraphQL mutation**, not a REST pull-review-comment operation. Either permit a narrow GraphQL operation, cut automatic resolution, or say only that comments become natively outdated; do not promise resolved threads with a REST-only adapter. [GitHub GraphQL pull-request mutations](https://docs.github.com/en/graphql/reference/mutations#resolvereviewthread).

#### Webhook authentication, delivery, and ingress

GitHub signs the **raw request body** with HMAC-SHA256 in `X-Hub-Signature-256` using a `sha256=` prefix; compare in constant time. `X-GitHub-Delivery` is the stable globally unique delivery identifier and remains the same on manual redelivery. GitHub requires a 2xx response within **10 seconds**, can deliver events out of order, caps payloads at **25 MB**, and explicitly does **not** automatically redeliver failed deliveries. [Validating GitHub deliveries](https://docs.github.com/en/webhooks/using-webhooks/validating-webhook-deliveries), [best practices](https://docs.github.com/en/webhooks/using-webhooks/best-practices-for-using-webhooks), [failed deliveries](https://docs.github.com/en/webhooks/using-webhooks/handling-failed-webhook-deliveries), [events and payloads](https://docs.github.com/en/webhooks/webhook-events-and-payloads).

The conventional GitLab project webhook secret is **not HMAC**: GitLab sends the configured value in `X-Gitlab-Token`, which must be compared securely. Standard Webhooks signing was introduced behind a flag in GitLab 19.0 and became generally available in 19.1; that mode signs `{webhook-id}.{webhook-timestamp}.{raw-body}` with HMAC-SHA256, so it is also not GitHub's signature algorithm with different header names. Older self-hosted installations may not support it, so the connection must declare the authentication scheme and signing mode must enforce timestamp freshness. GitLab provides `Idempotency-Key` for delivery/retry identity (and GitLab 19 exposes `webhook-id`, equal to it), recommends asynchronous receivers, and can deliver duplicates. After **4 consecutive failures** it temporarily disables a hook; after **40** it disables it permanently, with temporary backoff growing from about a minute to as much as 24 hours. [GitLab webhooks](https://docs.gitlab.com/user/project/integrations/webhooks/).

Therefore the success criterion should be “bad provider-specific webhook authentication is rejected,” not “bad HMAC” for both forges. Store the raw-body authentication result, normalized provider delivery ID, event UUID where present, receive time, actor, target, and outcome. Do not use an event UUID alone for GitLab dedupe if its documented semantics span triggered events differently from retries; prefer `Idempotency-Key`/delivery identity plus the hook identity.

Finally, GitHub.com cannot call a desktop listener that exists only on a private LAN. It needs a publicly reachable HTTPS reverse proxy/tunnel or a later headless server. The self-hosted GitLab can reach the stated LAN endpoint. Polling is not merely a preference for the desktop GitHub case; it is the working alternative unless setup includes public ingress. GitHub's troubleshooting guidance explicitly treats local testing through webhook forwarding rather than direct localhost delivery. [GitHub webhook troubleshooting](https://docs.github.com/en/webhooks/testing-and-troubleshooting-webhooks/troubleshooting-webhooks).

### Anchoring, reflection, and incremental evidence

There is no published evidence that CodeRabbit, Anthropic, Copilot, Graphite, Qodo, or Sourcery uses the OCR technique “have the model quote an added line verbatim, then substring-search it.” Their public descriptions discuss inline output, full-code context, verification, and stale handling, not anchor internals. The correct conclusion is that content anchoring is a reasonable AO implementation technique, not an industry-validated strong-tool design.

Its known failure modes are straightforward: repeated braces/imports/error strings; formatter-only whitespace; tabs/Unicode normalization/CRLF; the model adding Markdown or ellipses; quoting context or deleted lines; literal code beginning with `+` or `-`; generated/minified lines; moved code; line truncation; multiple copies in one file; cross-file copies; and source changing between generation and publication. Requiring a unique exact own-file added-side match, recording ambiguity, preserving unanchored output, and final re-anchoring against the forge's exact head is much safer than OCR's first-match fallbacks. The independent check/annotation fallback Anthropic documents is a useful product lesson: an anchor rejection should not erase the finding. [Anthropic Code Review](https://code.claude.com/docs/en/code-review), [OCR resolver](https://github.com/alibaba/open-code-review/blob/66120291271b2e605e420e9f11fbd6448f06163f/internal/diff/resolver.go).

For false-positive filtering, the strongest practical pattern in the surveyed products is generation → evidence-based verification → dedupe/rank. Anthropic explicitly describes this pipeline. A 2026 industrial study of 433 static-analysis alerts reported that hybrid LLM/static validation removed **94–98%** of false positives with high recall, though this is security-alert triage rather than general PR review and should not be treated as a directly transferable product metric. A separate study of a second LLM critic found F1 gains when the critic received both source and the first model's rationale; again, it supports the shape, not a guaranteed AO result. [Industrial false-positive validation](https://arxiv.org/abs/2601.18844), [LLM critic study](https://arxiv.org/abs/2601.09905).

The low-token design is a small classifier call per candidate or small related batch, not a second full review: provide the claim, evidence, rule, relevant source, and nearby prior findings; request a strict enum plus a short evidence citation; allow tools only when the evidence is insufficient; route protected/high-severity uncertainty upward. Cache verification by candidate/evidence/input digest. Measure it against labeled clean cases, because a cheap model that rejects true positives is merely an inexpensive recall destroyer.

On incremental review, public precedent favors a last-reviewed commit range with conservative fallbacks. Bitbucket's iterative review is explicitly based on changes since the last reviewed commit and documents that merge commits and destructive force-pushes break the model. Docker's open-source review action stores the last reviewed head, diffs `last..HEAD`, and falls back to a full review when force-push/rebase/base-merge invalidates that range. Git's patch-id is designed to be reasonably stable across line-number changes, but its stable mode ignores whitespace and file order—too coarse as AO's sole review key. [Bitbucket iterative review](https://www.atlassian.com/blog/bitbucket/iterative-review), [Docker review action](https://github.com/docker/docker-agent-action/blob/main/review-pr/README.md), [Git patch-id](https://git-scm.com/docs/git-patch-id).

I found no published controlled comparison showing per-file diff fingerprints outperform commit-range deltas for LLM code review. Per-file reuse is an AO optimization with good intuitive economics, especially for partial retries and unchanged files, but it should sit inside a commit-range/ancestry model rather than replace it. Raw diff fingerprints are also algorithm-dependent: research comparing diff algorithms found materially different churn results, reinforcing the need to pin the diff producer/version and canonicalization rules. [Diff-algorithm study](https://arxiv.org/abs/1902.02467).

## 5. Concrete amendments to the spec

1. Replace `builtin:` plus `capture:` phase kinds with `tool.uses = builtin:<name>|command:<profile>` and make stdout/envelope a bounded subprocess result codec.
2. Specify that builtins return typed engine outputs directly and never serialize through `AO_ENVELOPE`.
3. Remove `capture:` from v1 because no review phase consumes it.
4. Require every global operator-owned command profile to be explicitly allowed for the invoking project, with fixed argv, cwd policy, environment allowlist, secret refs, executable identity, timeout, and output cap.
5. Add a materialization stage that resolves forge diff version, fetches and verifies exact objects, handles forks, and creates an immutable review snapshot.
6. Persist an immutable `ReviewInput` containing forge/base/start/head/merge-base/ancestry, diff, rule, prompt/schema, provider/model, profile, and implementation digests.
7. Keep deterministic anchoring after the unit join in v1, but anchor each candidate independently and retain unit provenance and typed failure reasons.
8. Change reflection input from hunks-only to structured candidate evidence plus relevant resulting source, rule provenance, prior active findings, and bounded read-only tools.
9. Forbid private chain-of-thought in the candidate protocol and require only concise claim, mechanism, consequence, confidence, and evidence fields.
10. Replace protected-category auto-approval with a higher verification threshold or escalation to the configured strong model.
11. Add deterministic aggregate/dedupe/rank after verification and before publication.
12. Add a final current-head compare-and-swap immediately before publication; on mismatch, re-anchor or publish summary-only and queue the newest head.
13. Make one-file units the baseline and permit only measured topology-aware microbundles sharing rule digest and component, initially capped at 4 files and about 12,000 diff tokens.
14. Never enlarge bundles to meet concurrency; queue bounded fanout waves and record explicit budget skips.
15. Define a benchmark that selects bundle caps using recall, precision, duplicate rate, anchor rate, tokens, latency, and subscription throttling.
16. Define the file fingerprint over status, old/new path, modes/object kind, binary/submodule marker, and canonical ordered hunks while excluding object IDs and hunk coordinates but preserving whitespace.
17. Key reuse by file fingerprint plus range epoch plus review-input digest, not file fingerprint alone.
18. Record base/head ancestry and force full eligible-file review on ambiguous non-descendant, split/join, or rename mappings.
19. On merge-base changes with identical file patches, reuse generation only after rerunning semantic verification against the new snapshot.
20. Add semantic `same|supersedes|distinct` comparison against active prior findings while retaining deterministic exact IDs for write idempotency.
21. Split webhook processing into a bounded authenticated durable intake and scheduler-owned durable debounce/launch logic.
22. Persist webhook receipts and pending desired heads transactionally before returning 2xx, and recover due pending triggers on boot.
23. Key pending work and cursors by automation plus target, while using a separate target-only publication lock; alternatively enforce one review automation per target.
24. Add `max_wait = 10m` beside the `quiet_period = 2m` default and define which non-push events bypass or reset debounce.
25. Refetch current merge-request state and head when a debounced trigger fires; treat webhook payload state as a hint.
26. Define GitHub authentication as raw-body `X-Hub-Signature-256` HMAC-SHA256 and GitLab authentication as declared `X-Gitlab-Token` or supported Standard Webhooks HMAC mode.
27. Change the cross-forge acceptance test from “bad HMAC” to “bad configured provider-specific authentication.”
28. Deduplicate GitHub with hook plus `X-GitHub-Delivery` and GitLab with hook plus `Idempotency-Key`/delivery identity, not payload hash alone.
29. Require comment-command authorization by configurable forge role and ignore comments from the configured posting identity or AO machine markers.
30. Document that GitHub.com webhooks require public HTTPS ingress and make polling the supported desktop fallback when no ingress exists.
31. Snapshot the named harness profile body/digest at run creation and fail closed on a missing/unsupported provider body.
32. Separate reusable provider instructions from tool/capability policy inside a named profile and define composition precedence explicitly.
33. Declare repo rule overlays untrusted review criteria that cannot alter tools, secrets, network, posting, command parsing, schema, or safety floors.
34. Make review profiles disable writes, network, MCP, secret-bearing environment/tools, and other exfiltration paths rather than filesystem writes alone.
35. Restrict v1 inline anchors to a unique eligible added line in the candidate's own file; preserve every other accepted finding in the per-run summary.
36. Defer multi-line and arbitrary unchanged-line inline comments until forge-specific conformance tests prove their position rules.
37. Permit a narrow GitHub GraphQL adapter for `resolveReviewThread` or remove automatic GitHub thread resolution from the REST-only contract.
38. Never resolve an old thread merely because its line changed and no new candidate appeared; require explicit `fixed|still_valid|unknown`, leaving `unknown` open/outdated.
39. Add a durable publication outbox with deterministic operation IDs, bounded retries, rate-limit backoff, partial-write reconciliation, and response IDs.
40. Advance the delta cursor only after the intended publication state is durable; never equate successful analysis with successful delivery.
41. Split status into analysis coverage and publication state, and surface unit failures, budget skips, unanchored findings, stale-head requeues, rate limits, and auth failures.
42. Cap inline comment count and body bytes well below forge limits, order findings deterministically, and summarize overflow instead of dropping it.
43. Replace silent unknown category/severity coercion with strict schema failure or a visible `unknown` disposition.
44. Build the coverage manifest from all discovered paths and retain deleted, binary, generated, ignored, oversize, failed, reused, waived, and published outcomes separately.
45. Do not apply repository `.gitignore` matching to tracked forge diffs; use Git's own exclude machinery only for untracked local-review discovery.
46. Rewrite OCR's resolver, batcher, rule integration, and fingerprint rather than labeling them lifts; port only the parser concepts, pure matchers, provenance ideas, and relevant tests.
47. Preserve OCR rule provenance even when identical rule text is deduplicated for one model invocation.
48. Add binary/NUL, CRLF, Unicode, repeated-quote, literal-prefix, rename, split/join, force-push, stale-position, and partial-publication conformance fixtures for both forges.
49. Add per-phase/provider/model token, cache, tool-call, wall/queue time, retry, throttle, and disposition telemetry plus per-run/per-repo budgets.
50. Gate release on a versioned labeled review corpus with seeded bugs and clean controls, including mutation tests proving the generator and verifier affect outcomes.
