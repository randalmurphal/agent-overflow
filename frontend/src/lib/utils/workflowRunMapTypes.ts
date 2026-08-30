// The run map's rendered shape (spec: docs/architecture/workflow-run-map.md
// §5). Types only, no logic: `workflowRunMap.ts` re-exports every name here, so
// a consumer imports one module and this file stays the single place the shape
// is stated.
//
// Two rules run through all of it. Statuses are discriminated unions rather
// than raw strings, so a component switches on `kind` and the compiler catches
// the case it forgot; and display strings (durations, labels, metas) are
// precomputed, so rendering is a read and never a derivation.

import type { WorkflowRunMapRefusalCode } from '../types/workflow';
import type { WorkflowNodeSignal } from './workflowRunSignal';

// ---------------------------------------------------------------- statuses

/**
 * Node signal, extended with the two states the tree never had: not yet
 * reached, and a status this build has no vocabulary for.
 *
 * `unknown` is a signal of its own rather than a fold into `pending` on
 * purpose. A status the engine grew and this build has not learnt is not
 * "queued", and rendering it as queued is a silent relabel — the reader is
 * told something false about a run rather than told the build cannot say.
 */
export type RunMapSignal = WorkflowNodeSignal | 'ghost' | 'unknown';

export type RunMapPhaseStatus =
  | { kind: 'ghost' }
  | { kind: 'running' }
  | { kind: 'completed' }
  | { kind: 'parked'; cause: string }
  | { kind: 'failed'; cause: string }
  | { kind: 'cancelled' }
  | { kind: 'unknown'; raw: string };

export type RunMapUnitStatus =
  | { kind: 'pending'; provider: string }
  | { kind: 'running' }
  | { kind: 'done' }
  | { kind: 'failed' }
  | { kind: 'dropped' }
  | { kind: 'taken-over' }
  | { kind: 'unknown'; raw: string };

export type RunMapRunStatus =
  | { kind: 'running' }
  | { kind: 'needs-human'; reason: string }
  | { kind: 'done' }
  | { kind: 'failed'; reason: string }
  | { kind: 'cancelled' }
  | { kind: 'unknown'; raw: string };

// ---------------------------------------------------------------- model

export interface RunMapUnitTotals {
  total: number;
  pending: number;
  running: number;
  done: number;
  failed: number;
  dropped: number;
  takenOver: number;
  unknown: number;
  joins: number;
}

export type RunMapWaveOutcome =
  | { kind: 'looped' }
  | { kind: 'done' }
  | { kind: 'failed'; reason: string }
  | { kind: 'cancelled' }
  | { kind: 'running' }
  | { kind: 'needs-human'; reason: string }
  | { kind: 'unknown'; raw: string };

/**
 * The folded row's content. Every part is a separate field: the row is one flex
 * line whose parts yield by CSS priority (§6, text), so the projection never
 * pre-joins them into a string the component would have to take apart.
 */
export interface RunMapWaveSummary {
  label: string;
  duration: string;
  units: RunMapUnitTotals;
  unitsLabel: string;
  outcome: RunMapWaveOutcome;
  outcomeLabel: string;
  reasonLabel: string;
  retries: number;
  retriesLabel: string;
}

export interface RunMapWave {
  key: string;
  itemId: string;
  workflowId: string;
  /** 1-based and chain-local: position in THIS run's wave chain, never a seed. */
  ordinal: number;
  /**
   * Which try at this lap, when a retried tail call put more than one wave at
   * one ordinal; 0 when the lap has a single wave. Same convention as an
   * attempt's `·N`, and for the same reason: two rows both labelled "wave 2"
   * are two runs a reader cannot tell apart.
   */
  lapSeq: number;
  /** Records-only mode (§5.8): this run's frozen definition supplied no phases. */
  recordsOnly: boolean;
  /**
   * Why records-only, when the snapshot was CORRUPT rather than absent. An
   * absent one is ordinary history; a corrupt one is a defect the reader must
   * be told about instead of being shown a shortened run as normal.
   */
  skeletonError: string;
  /** A terminal run has nothing left to show live — it folds to its summary row. */
  folded: boolean;
  status: RunMapRunStatus;
  signal: RunMapSignal;
  summary: RunMapWaveSummary;
  /**
   * This wave's nodes, or null when the wave is folded and nobody asked for it
   * (§6, vertical scale). Built in the SAME walk as the rest of the model:
   * `buildRunMap` is one `buildIndex` + one frontier collection per call, and a
   * per-wave builder made both of those per-wave too.
   */
  segments: RunMapSegmentNode[] | null;
  startedAt: number;
  endedAt: number;
  autoResumeAt: number;
  softStop: boolean;
  onFrontierPath: boolean;
}

export interface RunMapLoop {
  key: string;
  itemId: string;
  phaseId: string;
  label: string;
  lapCount: number;
  /** 0 when the call edge declares no bound — a real state, not a missing one. */
  maxDepth: number;
  /**
   * How many WAVES the declared bound permits, 0 when it declares none.
   *
   * `max_depth` counts EDGE TRAVERSALS (`engine/calls.go#checkCallDepth`
   * refuses the call whose ancestry already holds that many), so a root plus
   * `maxDepth` child waves is legal and the ceiling a reader compares a lap
   * number against is `maxDepth + 1`. Reading the raw bound rendered a
   * perfectly legal final wave as "lap 3 of ≤2".
   */
  waveCeiling: number;
  lapLabel: string;
  softStopArmed: boolean;
  softStopNote: string;
  decided: 'loop' | 'done' | null;
  /** Ghost outcome stubs are drawn only while a live run can still decide (§5.6). */
  showOutcomeStubs: boolean;
}

export type RunMapPathKind = 'wave' | 'phase' | 'unit' | 'call';

export interface RunMapPathPart {
  kind: RunMapPathKind;
  label: string;
  key: string;
}

export interface RunMapFrontierBase {
  key: string;
  itemId: string;
  phaseId: string;
  attempt: number;
  label: string;
  /** The top-level wave this leaf lives under — what a follow action expands. */
  waveItemId: string;
  waveOrdinal: number;
  /** Breadcrumb length; the "deepest" of the follow priority (§13). */
  depth: number;
  needsHuman: boolean;
  signal: RunMapSignal;
  cause: string;
  /** The run's own reason, so `budget-exhausted` / `wiring-error` reach the strip. */
  reason: string;
  reasonLabel: string;
  autoResumeAt: number;
  duration: string;
  threadId: string;
  /** Most recent transition of this leaf — the last tiebreak of the priority. */
  transitionAt: number;
  path: RunMapPathPart[];
  /** Every node key on the path, so segments can mark themselves expanded. */
  nodeKeys: string[];
}

export type RunMapFrontierEntry =
  | (RunMapFrontierBase & { kind: 'phase'; status: RunMapPhaseStatus })
  | (RunMapFrontierBase & { kind: 'unit'; status: RunMapUnitStatus; unitId: string });

/**
 * The tree's dollars, halves apart (§12). `totalUsd` is a LOWER BOUND whenever
 * `unpricedRows > 0`: those rows' tokens are counted and their dollars are in
 * nothing, so a surface that renders the number alone tells a reader a campaign
 * cost less than it did.
 */
export interface RunMapSpend {
  /** wireUsd + estimatedUsd — what the providers reported plus what we priced. */
  totalUsd: number;
  wireUsd: number;
  /** Priced from the rate table because the provider reported tokens only. */
  estimatedUsd: number;
  /** Ledger rows whose model resolves to no rate; their dollars are unknown. */
  unpricedRows: number;
  /** Any part of the total came from the rate table rather than a provider. */
  estimated: boolean;
  /** `$4.12`, or `''` when the tree has cost nothing and priced nothing. */
  label: string;
  /** `3 rows unpriced`, or `''`. What makes `label` a lower bound. */
  unpricedLabel: string;
}

/**
 * The ceiling in FORCE — `engine.ResolveBudget`'s answer, so a ceiling a run
 * inherits from its project profile is here exactly as a declared one is. Null
 * means no ceiling, which is a different statement from a ceiling of zero.
 *
 * `kind` names which PAIR below carries the ceiling, and all three pairs are
 * kept: a run bounded by tokens or by wall-clock has a real bound whose only
 * rendering used to be nothing at all, because the map carried the dollar pair
 * and dropped the other two on the way in.
 */
export interface RunMapBudget {
  /** `usd` | `tokens` | `wall_clock` — which pair below is the ceiling. */
  kind: string;
  ceilingUsd: number;
  spentUsd: number;
  ceilingTokens: number;
  spentTokens: number;
  ceilingMillis: number;
  elapsedMillis: number;
  /** Not clamped: a run parks the first time it goes over (§12). */
  percent: number;
  exhausted: boolean;
  /** Ledger rows the dollar figure could not price; 0 for the exact kinds. */
  unpricedRows: number;
  /** The dollar figure was partly priced from the rate table, not reported. */
  estimated: boolean;
  /** Set when the ceiling belongs to an ANCESTOR — §12 enforces it tree-wide. */
  rootItemId: string;
}

/** A map this backend will never answer. Every code is PERMANENT (§4.2). */
export interface RunMapRefusal {
  code: WorkflowRunMapRefusalCode | string;
  message: string;
}

export interface RunMapModel {
  rootItemId: string;
  waves: RunMapWave[];
  loop: RunMapLoop | null;
  frontier: RunMapFrontierEntry[];
  followTarget: RunMapFrontierEntry | null;
  /**
   * Set when the answer was a refusal: `waves` is empty and there is nothing to
   * draw, so the surface renders this sentence instead of an empty map. It is
   * NOT an error state — the RPC succeeded, and retrying cannot change it.
   */
  refusal: RunMapRefusal | null;
  spend: RunMapSpend;
  /** Null when no ceiling is in force, which is most runs. */
  budget: RunMapBudget | null;
  /**
   * The one-line money summary (§11): `$4.12 of $10.00`, `$4.12 spent`,
   * `$4.12 priced · 3 rows unpriced`, or `''` when there is nothing to say.
   * Precomputed like every other display string here, and the ONE place the
   * lower-bound distinction is worded.
   */
  moneyLabel: string;
  /**
   * The NON-DOLLAR ceiling, said in its own units: `12.3k of 50.0k tokens`,
   * `4m of 30m`. Empty for a dollar ceiling — `moneyLabel` already compares
   * those, and one line carrying the same comparison twice is noise — and empty
   * when no ceiling is in force.
   *
   * Rendered unconditionally rather than only when the loop declares no
   * `maxDepth` (§3's "show lap N plus the budget line"). A ceiling in force is
   * worth stating on either kind of run, and the unconditional rule is a
   * superset of the conditional one, so a boolean deciding it was a flag with
   * no reader.
   */
  budgetLabel: string;
}

// ---------------------------------------------------------------- segments

export interface RunMapUnitChip {
  key: string;
  unitId: string;
  unitIndex: number;
  label: string;
  isJoin: boolean;
  status: RunMapUnitStatus;
  signal: RunMapSignal;
  provider: string;
  duration: string;
  meta: string;
  /** `dropped` keeps its record inside the done chip, struck (§6, fan scale). */
  struck: boolean;
  unitAttempt: number;
  threadId: string;
  childRunCount: number;
  startedAt: number;
  endedAt: number;
  onFrontierPath: boolean;
}

export interface RunMapBranch {
  key: string;
  unit: RunMapUnitChip;
  /**
   * What the lane header prints. Usually the unit id; a lane whose sole child
   * run is merged into it (or folded away) also carries the child's workflow
   * name — `PORT-0 · port-subsystem` — because the header is then the only
   * line that can say what the lane ran. Composed here, not in the component:
   * components render the model, they don't derive one.
   */
  title: string;
  /**
   * The called runs hanging off this lane, or `[]` while the lane is collapsed
   * — the same "not built" convention `RunMapWave.segments` uses, so there is
   * one answer to "is this open" rather than a flag a caller could disagree
   * with.
   */
  chain: RunMapCompositionNode[];
  /**
   * A SETTLED lane folds to its header alone (§7): the header carries the
   * glyph, the unit id and the duration, which is the whole summary, and the
   * subtree it finished is one click away rather than painted forever.
   */
  collapsed: boolean;
  /** False for a lane with nothing under it, and for a live one that never folds. */
  toggleable: boolean;
  onFrontierPath: boolean;
}

export interface RunMapUnitGroup {
  kind: 'queued' | 'done';
  key: string;
  count: number;
  droppedCount: number;
  /**
   * `ports 2–4 · queued` when the group is one contiguous run of lanes off one
   * phase, else the count (`3 units · queued`). The range is what the reader
   * can act on — which lanes these are — and a bare `·N` never said it.
   */
  label: string;
  /**
   * The units behind the label, or `[]` when the group has nothing a click
   * would ADD. A queued lane has no records, no thread and no duration, so its
   * chip repeats what the group label already said; `count` is what says how
   * many there are. `done` keeps its entries because the dropped ones live in
   * there (§6, fan scale) and nothing else states them.
   */
  entries: RunMapUnitChip[];
  /**
   * Render the entries directly in the flow, no click. True for a done group
   * of at most eight units: "what completed" is the first thing a reader asks
   * of a finished fan, and a count chip made them click for the answer — per
   * lap, per composition. Past eight the group folds behind its labelled
   * count, because a forty-unit sweep as forty chips is the wall again.
   */
  inline: boolean;
}

/**
 * A fan-out attempt, partitioned into what gets geometry and what gets
 * arithmetic (§6). There is deliberately no tally field beside these three: the
 * fan's width is `columns.length + queued.count + done.entries.length`, the
 * wave's summary row already states the wave's own unit counts, and a second
 * number saying the same thing under the same node was noise the reader had to
 * reconcile.
 */
export interface RunMapFan {
  key: string;
  attempt: number;
  /**
   * How the fan draws. `columns` is the top-level idiom: side-by-side lanes
   * under a fork bar, each wide enough to read. `stacked` is every fan BELOW
   * that — a fan inside a lane's composition renders its branches as
   * full-width blocks in its parent's flow, because columns inside a column
   * can only divide a width that was already the minimum: the nested fan is
   * what put a horizontal scrollbar inside a 200px lane.
   */
  layout: 'columns' | 'stacked';
  columns: RunMapBranch[];
  queued: RunMapUnitGroup;
  done: RunMapUnitGroup;
  join: RunMapUnitChip | null;
}

export interface RunMapAttempt {
  key: string;
  phaseId: string;
  attempt: number;
  label: string;
  status: RunMapPhaseStatus;
  signal: RunMapSignal;
  duration: string;
  cause: string;
  /** An intervention was recorded on this attempt — "touched by hand" (§7). */
  touched: boolean;
  interventionKind: string;
  threadId: string;
  startedAt: number;
  endedAt: number;
  fan: RunMapFan | null;
  chain: RunMapCompositionNode[];
  onFrontierPath: boolean;
}

export interface RunMapNodeBase {
  key: string;
  itemId: string;
  phaseId: string;
  label: string;
  shape: string;
  isCheck: boolean;
  ghost: boolean;
  /**
   * Declared before the run's furthest recorded point and never recorded: a
   * loop-back re-entry skipped it (§5.5). It is a ghost with no future in it —
   * position already says it will not happen — so it must not read as "not yet".
   */
  skipped: boolean;
  /** A record whose phase left the definition on a rerun — appended, never dropped. */
  notInDefinition: boolean;
  status: RunMapPhaseStatus;
  signal: RunMapSignal;
  attempts: RunMapAttempt[];
  onFrontierPath: boolean;
}

export type RunMapSegmentNode =
  | (RunMapNodeBase & { kind: 'phase' })
  | (RunMapNodeBase & { kind: 'fan'; ghostLabel: string })
  | (RunMapNodeBase & { kind: 'call'; callTarget: string })
  | (RunMapNodeBase & { kind: 'decision'; loop: RunMapLoop });

export interface RunMapCompositionSummary {
  runCount: number;
  waveCount: number;
  attemptCount: number;
  unitCount: number;
  runningCount: number;
  parkedCount: number;
  label: string;
}

/**
 * One lap of a called run's own wave chain. Identical in kind to a top-level
 * `RunMapWave` — a lap is a lap — so it folds by the same rule and answers
 * "open" the same way: `segments === null` is closed, and the reader's
 * `expandedWaveIds` opens it. An open composition therefore shows its LIVE
 * lap's flow and nothing more; its finished laps are one row each.
 */
export interface RunMapCompositionWave {
  key: string;
  itemId: string;
  ordinal: number;
  /** Which try at this lap; 0 when the lap has a single wave (see RunMapWave). */
  lapSeq: number;
  /**
   * Terminal: this lap owns a working fold. Whether it is currently open is
   * `segments !== null`, not this flag — the final lap (the one that called
   * no successor) is folded AND open by default, since it is the lap the
   * composition's own status quotes; a click inverts either default.
   */
  folded: boolean;
  status: RunMapRunStatus;
  signal: RunMapSignal;
  summary: RunMapWaveSummary;
  /** Null when folded and unopened — the same "not built" rule as RunMapWave. */
  segments: RunMapSegmentNode[] | null;
  onFrontierPath: boolean;
}

export interface RunMapCompositionNode {
  key: string;
  itemId: string;
  workflowId: string;
  label: string;
  /** Levels below the wave that owns it: 1 directly inside, 2 one call deeper. */
  depth: number;
  status: RunMapRunStatus;
  signal: RunMapSignal;
  duration: string;
  /**
   * The amber line the sub-card carries while this called run waits on a
   * person, `''` otherwise (§4, R1). Precomputed like every other string here.
   */
  blockerLabel: string;
  /**
   * Collapsed to one summary row. TRUE by default for every composition off
   * the frontier path, at every depth (§3): the map's reading rule is that only
   * the live path is open, so a settled or not-yet-reached call is one line
   * with its subtree counts on it and nothing else. The frontier path is never
   * collapsed — that is the "no clicks to see what is running" half.
   */
  collapsed: boolean;
  /**
   * Whether this row's collapse can be toggled at all — false on the frontier
   * path, which is force-open, and on a run merged into its fan lane, whose
   * fold is the lane's toggle. The rule is the projection's, so the component
   * reads the answer rather than re-deriving it.
   */
  toggleable: boolean;
  /**
   * Skip the composition's own header row. True when this run is MERGED into
   * its fan lane as the sole child: the lane header already names the lane,
   * the workflow name moves up onto it, and repeating glyph + duration one
   * line apart was the `PORT-0 14h 37m` / `go… 14h 37m` stutter. Only ever
   * true on an OPEN composition — merging clears `toggleable`, which pins
   * `collapsed` false.
   */
  headerless: boolean;
  summary: RunMapCompositionSummary;
  /** `[]` while collapsed: a folded composition builds no laps at all. */
  waves: RunMapCompositionWave[];
  onFrontierPath: boolean;
}

export interface RunMapBuildOptions {
  /**
   * Folded waves the reader opened, by wave item id — top-level laps and the
   * laps inside an open composition alike, because a lap is a lap. A wave named
   * here gets its `segments` built; the live wave always does, because it is
   * what the map is for.
   */
  expandedWaveIds?: Iterable<string>;
  /** Called-run ids the reader opened; every composition off the frontier is folded by default. */
  expandedCompositionIds?: Iterable<string>;
  /**
   * Fan lanes the reader opened, by BRANCH key. Settled lanes fold to their
   * header (§7); this is what puts their subtree back on screen. Keyed apart
   * from `expandedCompositionIds` because a lane is a unit, not a called run —
   * one lane can hold several called runs, and one click has to open the lane.
   */
  expandedLaneIds?: Iterable<string>;
}

/**
 * Where a run IS, as two label parts — the header's narrow read (§11.4).
 * `leaf` is empty when the deepest part of the path IS the wave, so a caller
 * joining the two never renders "wave 3 · wave 3".
 */
export interface RunMapPosition {
  wave: string;
  leaf: string;
}
