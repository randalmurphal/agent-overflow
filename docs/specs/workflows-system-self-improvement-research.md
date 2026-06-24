# Self-Improving Workflows — Research Digest

> **DECISION: EXCLUDED from the product** (not core). This file is retained only as the
> rationale for that call — it is *not* a plan of record. The durable knowledge that
> matters already lives in the repo (code, project profile, `CLAUDE.md` rules, ADRs);
> AI-managed memory is historically junk; a workflow-tuning "watcher," if ever wanted, is
> just a later §11 automation, not a subsystem. See `workflows-system.md` → "Deliberately
> out of scope."
>
> Source: a 6-lens parallel deep-research workflow (Anthropic; memory architectures;
> self-evolving-agent methods; eval-driven optimization + reward-hacking defense;
> orchestration prior art; coding build-and-validate loops) → synthesis → adversarial
> critique. 8 Opus agents, ~566k tokens. This file is the durable record; §12 of
> `workflows-system.md` is shaped from it.
>
> Read the **critique** before the synthesis — its "single biggest risk" reframes how
> much of the synthesis actually applies to an N=1, multi-repo desktop tool.

---

## TL;DR for §12 (author's reframe, grounded in the critique)

Split §12 into **two layers** instead of one optimization loop:

- **Layer 0 — single-run-valid, always on (no corpus needed).** Works at N=1, which is
  the steady state for most workflows on this product:
  - The **immutable acceptance contract** (the watcher may propose edits but may never
    author or weaken what judges them; contract-touching edits → forced human gate).
  - **Environmental hardening**: deterministic backstop runs in a **fresh checkout of the
    base branch + the agent's diff**, never the agent's working tree; **fail-closed**
    envelope-schema parsing; **screen the diff for edits to test/CI/conftest files**.
  - **Two-trust-tier memory** (VERIFIED vs ADVISORY), with the VERIFIED mint path guarded
    (see critique §2c).
  - **Independent cross-family verifier** judging the *diff against the goal* (not "tests
    pass").
  - **Version-pinned rollback.**
- **Layer 1 — corpus-conditional (activates per-workflow only when run volume crosses a
  threshold; may never, for many workflows).** Held-out partitions, the Δ metric **as a
  gate**, effect-size / CI / multiple-comparisons discipline, statistical canary,
  GEPA-style prompt auto-apply, predictive test selection, flaky-test registry.

Two **flagged amendments to earlier sections** (do not silently apply):
1. **§3 amendment** — split agent-visible self-validation from a **held-out backstop the
   agent never sees**; track **Δ = visible-pass − held-out-pass** as a *diagnostic* (not a
   gate yet — gameable + underpowered at N=1).
2. **§8/§10 net-new primitive** — `workflow_version_id` + a **versioned workflow-definition
   store** (draft / published / latest) separate from execution history. Bigger than a
   field: the spec today never says where workflow definitions live or that they're
   versioned.

---

## Synthesis — recommended §12 design

### The spine: two Goodhart surfaces, one cardinal rule

A self-improving loop is optimization pressure applied to your own workflow, and pressure
on any gate produces reward hacking — empirically the dominant failure of self-improving
code agents, and it **compounds with depth** (reported: proxy-gain-without-real-gain rose
26.4%→57.8% from 10→100 optimization steps; 73.8% of self-optimizations gamed the proxy
with no real gain). Two distinct Goodhart surfaces, each needing a different defense:

- **Surface 1 — per-run** (coding agent vs its gate). Addressed by §3's deterministic
  backstop; needs *hardening*, not invention.
- **Surface 2 — meta** (the watcher vs its *own* gate). The deadly one. If a proposed
  refinement can weaken what judges it — relax a check, soften a rubric, swap to a
  friendlier judge, raise a threshold, narrow the held-out suite — anti-theater dies
  **permanently, for every future run**.

**Cardinal rule:** the *acceptance contract* — held-out check scripts, rubric text, judge
model+version, gate pass criteria — is **immutable relative to the proposer**. The watcher
may propose phase/prompt/tool/routing edits; it may never author or weaken what judges
them. Any diff touching a check, rubric, judge binding, threshold, or gate-criterion is
**contract-class → forced human gate**. Own the contract in the **project-profile/platform
layer** (`workflow declares; project binds`, §8), versioned separately from the editable
surface.

Running principle: **PROPOSE ≠ VALIDATE.** The watcher proposes via self-reflection; it
must never be its own validator, and the optimization target must **never be
gate-pass-rate** (optimizing pass-rate *is* Goodharting the gate).

**The watcher is a §11 automation** (reuse, don't invent): internal-event trigger on
`item done`/`failed` → optional run-if → action = enqueue a `refinement-analysis` item
whose workflow reads the run record, reflects, and emits a proposed edit as its typed
envelope → routes through an ordinary §4 gate.

### Part 1 — the self-improvement loop

The watcher evaluates each completed run **against the item's goal**, not gate-pass-rate.
Its judge returns a **score + natural-language feedback grounded in the run record**
(GEPA's `metric→score+feedback` interface; production in DSPy at Shopify). Objective is
**multi-signal "is the change sound"**: checks pass AND Δ-gap near-zero AND no
check/test/marker weakened AND diagnosis quality holds AND held-out behavior preserved. A
single visible scalar is exactly what the loop learns to game (72% of reward-hacking
exploits came with chain-of-thought rationalizing the shortcut — *an honest narrative is
steering signal, not a verdict*).

**Edit risk tiers → autonomy:**

| Tier | Edit kind | Autonomy | Mechanism / evidence |
|---|---|---|---|
| **A — auto-validatable** | phase prompt text, few-shot examples | propose → validate → canary → adopt | GEPA (reflect on traces, Pareto-over-held-out; ~35× fewer rollouts than RL; production *offline/labeled*). MIPRO BootstrapFewShot keeping only traces whose output passes the **deterministic** check. |
| **B — contract-class** | gate thresholds, check defs, rubric, judge binding, pass criteria | **human-gated, always** | loosening a threshold *is* the meta-Goodhart. |
| **C — structural** | phase topology (add/remove/reorder/route) | **human-gated + DEFERRED** | no production optimizer; ADAS/Gödel paper-only with explicit divergence risk. |

**Adoption path (Tier A):** scoped SEARCH/REPLACE diff over a bounded marked region (never
a free rewrite) → **shadow-replay over a frozen held-out partition the watcher never saw**
→ multiple-comparisons discipline (predeclared min effect size, non-overlapping CIs across
N trials, judge+rubric pinned) → **canary** to a slice with auto-rollback on regression
(regression subset must hold ~100%) → versioned, rollback-able adoption. Never mutate a
definition in place (orc's global-mutable phase-template update destroys the record of what
a past run executed); a re-run branch needs its own identity (LangGraph fork-id bug).

### Part 2 — the memory system (designed against the junk drawer)

Strongest cross-lens finding: **do not adopt a conversational-memory vendor, and do not
build a new store.** The cognitive split maps onto primitives you already own. Evidence vs
hype: Zep's 84% LOCOMO collapsed to 58.44% on independent reproduction; mem0's own paper
shows full-context beats it on accuracy; on LongMemEval 74.6% of errors are *reasoning*
failures more retrieval can't fix.

Three tiers onto existing primitives:
- **Semantic = the project profile, extended.** Stable per-project facts (build/test
  idioms, flaky tests, check quirks), each a citation-bound object
  `{subject, fact, citation, reasoning, source_run_id, trust_tier}`. (GitHub Copilot's
  shipped repo-scoped memory is the closest production analogue.)
- **Episodic = the run record (do NOT build a new store).** Each run is an episode; net-new
  work is only (a) an index for retrieval and (b) a distiller that extracts durable facts.
- **Procedural = the watcher's distilled lessons.** Reflexion-style; a candidate is **not
  trusted until validated across >1 run**.

**Two trust tiers (anti-poisoning + anti-theater core):**
- **VERIFIED** — earned only when a **deterministic check passes on real code**, carrying
  its citation + check command. **Only this tier is gate-eligible.**
- **ADVISORY** — agent self-assertions/reflections; hints only, **never fed to a gate**. So
  an agent cannot write itself a memory that moves a gate.

Curation against silent junk-drawer growth: write policy with **NOOP as a first-class
outcome**; **archive, not append-log** (keep only high-scoring AND diverse — MAP-Elites /
ADAS archive); **decay = re-verification failure, not elapsed time** (a 6-month-old flaky
note is still true if still flaky; yesterday's fix is worthless if its file was rewritten);
**conflict = bi-temporal supersession, never deletion**; **retrieval structured-first,
vector-second** with a hard top-k cap. Coding-specific content: flaky-test registry,
predictive test-selection priors (Meta: deployed >1yr, 99.9% of regressions at ~⅓ suite —
the lone hard production anchor), validated check commands, fix-patterns keyed by error
signature.

### Part 3 — reward-hacking defense (prove soundness, not pass tests)

1. **Split agent-visible self-validation from a held-out backstop** (the §3 amendment);
   track Δ = visible−held-out. Held-out alone is weak → **layer it** (held-out + cross-family
   verifier + diff/test-file screening + property/metamorphic invariants).
2. **Independent cross-family verifier judging the diff against the goal** (generator ≠
   verifier; hide refinement provenance from the judge; pin judge model+version+rubric
   hash). The watcher must never be its own judge.
3. **Environmental hardening** (highest-ROI, production-proven): fail-closed schema parsing
   + restrict the agent's visibility into grader internals (reported ~87.7% relative exploit
   reduction, zero task-success loss). Run the backstop in a **fresh checkout + diff
   applied**, never the agent's tree; screen diffs editing test/CI/conftest as a suspicious
   class (Berkeley: agent rewrote all test outcomes to "passed" via a conftest hook in the
   same container as the grader).
4. **Property/metamorphic invariants** (quantify over generated inputs; hardcoding one case
   doesn't satisfy them). Author them in a phase *other* than the implementer.
5. **Never optimize toward a single fixed LLM judge** — gameable the moment you optimize
   against it. Anthropic's verification hierarchy: rules-based > visual > LLM-judge ("LLM
   judging generally not very robust") → keep deterministic checks authoritative; treat any
   ai-judgment gate as the most reward-hackable component.

### Part 4 — commit vs defer (from the synthesis)

**Commit (dependency order):** `workflow_version_id` + draft/published history → run-record
index + distiller → two-trust-tier memory write policy → split visible/held-out + Δ metric
→ held-out partition + effect-size gating → immutable-contract rule + contract-class
human-gate → GEPA-style prompt refinement as the first/only auto-apply class.
**Defer:** structural/topology edits, self-referential improvement, AlphaEvolve-scale
search, MIPRO-beyond-bootstrap.
**Honest costs:** cold-start (needs a corpus); compute (shadow-replay × N trials);
adoption reality (only ~8.9% of teams use any prompt optimizer in prod; autonomous
structural self-modification validated by self-judgment is production-proven nowhere).

---

## Critique — adversarial pressure-test (read this first)

The synthesis is unusually self-aware; this attacks where it still hand-waves, contradicts
the spec, or assumes away the hardest part.

**1. Overclaims (academic/hyperscale re-imported as de-risking).** "GEPA … production" is
for *offline prompt optimization against a curated labeled eval set* — not online from
un-curated self-generated run history with a self-constructed held-out partition and no
human gold. → **downgrade Tier A from "production" to "adapted, unproven here"; keep a human
in the loop until measured.** The 87.7% hardening number is from RHB's sandbox (mechanism
transfers, number doesn't). Meta predictive-test-selection is a supervised model on a
hyperscale labeled corpus — proves the *category* at FB scale, says nothing about N=1.

**2. Memory holes.** (a) **Retrieval-miss asserted away** — the high-value lesson lives in
the **narrative file the system is forbidden to parse** (§3/§10); the distiller must
LLM-read narratives (the lossy, hallucination-prone step the synthesis elsewhere distrusts).
(b) **Decay-by-re-verification is unpriced** — re-running a check can mean spinning up
`live-stack` (capacity 1, §6), contending with real work; cheap citation re-check ≠ semantic
staleness. (c) **VERIFIED laundering** — an agent can mint a VERIFIED memory by passing a
check it *also weakened in the same diff*; the immutable-contract guard protects the workflow
but not the check that mints memory. Second unguarded "green check → trusted artifact" path.

**3. Reward hacking — residual holes.** The **Δ metric is gameable by the watcher**: it
constructs the held-out partition, so it can drift partition *composition/sampling* toward
easy runs without touching the "contract" as defined. And **"no check/test/marker was
weakened" has no stated detector** — detecting a weakened test across an arbitrary diff
(renamed assertion? loosened tolerance? `skip`?) is itself unsolved; it's a TODO wearing
the costume of a requirement, and it's the single most important detector in the story.

**4. Eval noise / churn — the numbers don't exist at this scale.** Non-overlapping CIs and
predeclared effect sizes assume dozens-to-hundreds of comparable runs per workflow. The
queue is **"often N=1" across many repos** (§6/§11). With single-digit runs the statistical
gate **never fires** → §12 either does nothing forever, or quietly relaxes to "one good run
= adopt" (the churn it was built to prevent). Cold-start is framed as warm-up; for a
single-dev multi-repo tool it is plausibly the **permanent steady state**.

**5. Cost.** Shadow-replay over held-out × N trials = e.g. 20 runs × 5 = **100 full workflow
executions to evaluate one prompt tweak** — plausibly more compute than the productive work
the queue is doing. No argument the loop is ever net-positive for a single dev. (Divergence
risk is genuinely low for what's committed — the real risk is **irrelevance**, cost > benefit.)

**6. Safety gaps.** `workflow_version_id` is undersold: the spec has **no workflow-definition
store at all** — you need a versioned store + draft/published lifecycle + run→version
binding, not a field. **Canary on a traffic slice assumes throughput an N=1 queue lacks** →
degrades to "try the next item and eyeball it," not the Argo-style statistical canary.

**7. Missing pieces.** (1) **No human-attention budget** — a single-dev tool with a watcher
firing on every `item done` floods the one reviewer; needs a throttle / confidence bar /
batching, or it generates more review load than it saves. (2) **Distiller is itself an
un-validated LLM step** in the trusted path. (3) **§7 human-takeover/hand-filled runs
contaminate the corpus** — the run set is heterogeneous (autonomous vs steered vs
hand-filled); the Δ-math silently assumes homogeneity. (4) **Flaky-registry vs §4 diagnosis
phase overlap** unreconciled, and the registry needs multiple runs to bootstrap (N=1 again).

### Component verdicts

| Component | Verdict |
|---|---|
| Immutable acceptance contract | **COMMIT** — but extend it to the check that mints VERIFIED memory, and freeze held-out *sampling policy*, not just suite definition. |
| `workflow_version_id` + versioned definition store + draft/published | **COMMIT** — scope honestly as net-new definition infra, not a field. |
| Watcher as §11 automation | **COMMIT — with a throttle** (proposal-frequency / attention budget). |
| Two-trust-tier memory | **COMMIT** — fix the VERIFIED-laundering path and the unvalidated distiller first. |
| Split agent-visible vs held-out (§3 amendment) | **COMMIT the split; DEFER Δ-as-gate** (track Δ as a diagnostic only). |
| Held-out + effect-size + multiple-comparisons | **DEFER** — inapplicable at this data scale for most workflows; don't build until a workflow demonstrably accumulates enough comparable runs. |
| GEPA prompt refinement as Tier-A auto-apply | **DEFER, not Commit** — keep a human in the loop until measured on this system. |
| Environmental hardening (fresh-checkout backstop, fail-closed parse, test-file screening) | **COMMIT** — confirm the backstop runs outside the agent's writable tree (§9 worktrees don't currently guarantee this). |
| Independent cross-family verifier on the diff | **COMMIT** — maps to §4 ai-judgment gate; bounded extra cost. |
| Property/metamorphic invariants | **DEFER** — auto-gen 62–82% accurate; reserve. |
| Canary + rollback by version-repointing | **COMMIT rollback; DEFER statistical canary** (degrade to try-next-item-and-review). |
| Structural/topology, self-referential, AlphaEvolve, MIPRO-beyond-bootstrap | **REJECT for now / reserve** — ensure nothing forecloses them. |

### Single biggest risk

**The loop is designed for a data regime this system rarely inhabits.** Held-out
partitions, the Δ-metric, effect-size gating, CIs, statistical canary, predictive-test
selection, flaky registries — all need many comparable runs per workflow; the queue is
"often N=1" across many repos. For a single-developer multi-project tool, **most workflows
never accumulate the statistical mass**, so §12 either does nothing (best case) or relaxes
its own discipline onto single-run noise (the failure it was built to prevent). The honest
§12 leads with the **cheap, single-run-valid components** (immutable contract, fresh-checkout
backstop, two-tier memory with a guarded VERIFIED path, independent verifier, version-pinned
rollback); **everything statistical is conditional on a per-workflow run-volume threshold
many workflows will never cross.**

---

## Key sources (full list in the workflow transcript)

- Anthropic: Building Effective Agents; Effective context engineering; Effective harnesses
  for long-running agents; Claude Agent SDK; Memory tool + context editing; recursive
  self-improvement (forward-looking only — ships **no** autonomous self-improvement).
- Memory: MemGPT (2310.08560); Reflexion (2303.11366); Generative Agents (2304.03442); Zep
  (2501.13956) + the 84%→58.44% reproduction (getzep/zep-papers#5); mem0 (2504.19413);
  LangMem; GitHub Copilot agentic memory; AgentPoison (2407.12784) + OWASP ASI06.
- Self-evolving: ADAS (2408.08435); Gödel Agent (2410.04444); AlphaEvolve (2506.13131) +
  OpenEvolve (2510.14150); Promptbreeder (2309.16797); OPRO (2309.03409); TextGrad
  (2406.07496); GEPA (2507.19457); DSPy MIPROv2 / GEPA docs.
- Eval/reward-hacking: Goodhart/specification-gaming corpus; Denison et al. 2024
  (reward-function tampering); Prover-Verifier (Anthropic); RHB environmental hardening.
- Orchestration: LangGraph/LangSmith; Temporal worker-versioning; n8n/Dify draft-published;
  Argo Rollouts canary.
- Coding: SWE-agent/SWE-bench; AlphaCodium; Meta predictive test selection; property/
  metamorphic testing; EvilGenie.

> Caution flags from the research itself (do NOT cite as fact): SpecBench/EvilGenie exact
> percentages are single-source and future-dated (qualitative holds, precise numbers don't);
> memory-vendor leaderboard numbers are largely vendor theater; the recursive-self-improvement
> 80%/76%/52× figures are likely a fetch confabulation; "SAGE" is an Amazon paper, not
> Anthropic. Anthropic's only reliable RSI content is hedged forward-looking framing.
