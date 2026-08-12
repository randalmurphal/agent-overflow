// The run map's presentation vocabulary (RUN-MAP §2), in one pure place for the
// same reason `workflowRunSignal.ts` is one: R1's two hues are a rule about the
// whole app, not a per-component styling choice. Components ask for a style and
// render it; no component names a colour, a glyph or a glow of its own.
//
// Amber is human-blocked and red is failed. Everything else — done, queued,
// dropped, not-yet-reached — is typographic weight and border style only.
// Running is the standing spinner plus solid weight: no pulse, no new hue.
//
// NON-MAP SURFACES CONSUME THIS DELIBERATELY, and the name stays as it is.
// `WorkflowEvidence.svelte`'s checks strip asks for `runMapNodeStyle` because a
// completed check and a completed node are the same statement, and the app has
// one vocabulary for it — the alternative is a second glyph/tone table that
// starts identical and drifts on the first tuning change. The module is named
// after where the vocabulary was DECIDED, not after the only place it may be
// read; anything that draws a run's status is a legitimate caller.

import { runMapTone } from './workflowRunMapIndex';
import type { WorkflowRunMapRefusalCode } from '../types/workflow';
import type { RunMapSignal } from './workflowRunMapTypes';

export interface RunMapNodeStyle {
  /** Text colour for the row's glyph, cause chip and metas. */
  tone: string;
  /**
   * Text classes for the row's LABEL. Same hue as `tone` unless §2 gives the
   * signal a typographic emphasis of its own, which only ever restates a
   * NEUTRAL step (`text-fg`, `text-fg-subtle`) — never a hue. R1's two
   * meanings are therefore declared exactly once, in `runMapTone`.
   */
  label: string;
  /** The status glyph, or `''` when the row draws a `SteppedSpinner` instead. */
  glyph: string;
  /** Border classes for the row's marker, including the ghost dash. */
  border: string;
  /** The one glow on the surface: amber, and only for human-blocked. */
  glow: string;
  /** Running rows draw the app's standing spinner in place of a glyph. */
  spinner: boolean;
}

// `◌` for queued (spec §2) rather than the tree's `○`: the map draws a real
// pending record and a not-yet-reached ghost on the same line, and two rings of
// the same weight read as one state.
//
// `?` for unknown, because there is no honest glyph for a status this build has
// no vocabulary for — the row says so instead of borrowing one that would read
// as a state the run is not in.
const GLYPHS: Record<RunMapSignal, string> = {
  done: '✓',
  running: '',
  pending: '◌',
  failed: '✗',
  dropped: '⊘',
  parked: '◍',
  ghost: '·',
  unknown: '?',
};

const BORDERS: Record<RunMapSignal, string> = {
  done: 'border-border-subtle',
  running: 'border-border-strong',
  pending: 'border-border-subtle',
  failed: 'border-error',
  dropped: 'border-border-subtle',
  parked: 'border-warning',
  ghost: 'border-dashed border-border-subtle',
  unknown: 'border-dotted border-border-subtle',
};

/**
 * Label treatment ON TOP of the signal's tone, and the ONLY thing that varies
 * per signal here: §2's emphasis axis is typography, so a row either adds
 * weight or steps the NEUTRAL text colour. Nothing in this table may name
 * `text-error` or `text-warning` — those come from `runMapTone` alone, and a
 * second declaration is how the two hues drift apart. Pinned by a test.
 */
const LABEL_EMPHASIS: Record<RunMapSignal, string> = {
  done: '',
  running: 'text-fg font-medium',
  pending: 'text-fg-subtle',
  failed: '',
  dropped: '',
  parked: '',
  ghost: '',
  unknown: '',
};

/**
 * A phase declared before the run's furthest point and never recorded (§5.5).
 * It is a ghost by STATUS and the past by POSITION, and its own rule is that it
 * must not read as "not yet" — so it keeps the ghost's neutral hue and takes a
 * struck label and a glyph of its own rather than rendering identically.
 */
const SKIPPED_GLYPH = '⊝';
const SKIPPED_LABEL = 'line-through decoration-1';

export function runMapNodeStyle(signal: RunMapSignal, skipped = false): RunMapNodeStyle {
  const tone = runMapTone(signal);
  const label = LABEL_EMPHASIS[signal] || tone;
  return {
    tone,
    label: skipped ? `${label} ${SKIPPED_LABEL}` : label,
    glyph: skipped ? SKIPPED_GLYPH : GLYPHS[signal],
    border: BORDERS[signal],
    glow: signal === 'parked' ? 'status-glow-warning' : '',
    spinner: signal === 'running' && !skipped,
  };
}

/**
 * Middle-truncation budget for a node or unit label. Phase names and unit ids
 * are engine-stamped but unbounded in principle (§6, text), so every label on
 * the spine is truncated to one line with the full text in `title`.
 */
export const RUN_MAP_LABEL_MAX = 56;

/**
 * What a refusal (§4.2) IS, in the reader's terms. The backend already ships a
 * user-shaped sentence — it names the run, never a path or an internal type —
 * and that sentence is the DETAIL: it says what happened to this particular
 * run. The headline says what it means for the surface in front of them, which
 * is the part every refusal of a given code shares and no per-run message can
 * carry.
 *
 * Keyed on the wire union so a code the backend grows and this build has not
 * learnt fails `pnpm run check` here rather than rendering a bare sentence with
 * no heading. `runMapRefusalHeadline` is the one place a genuinely-unknown wire
 * string is allowed to reach it, and it says the honest thing: the map cannot
 * be drawn, and this build cannot say why.
 */
const REFUSAL_HEADLINES: Record<WorkflowRunMapRefusalCode, string> = {
  'not-found': 'This run is gone',
  'too-large': 'This campaign is too big to draw',
  'corrupt-linkage': 'This run’s call linkage is broken',
};

const REFUSAL_FALLBACK_HEADLINE = 'This run’s map cannot be drawn';

export function runMapRefusalHeadline(code: string): string {
  return REFUSAL_HEADLINES[code as WorkflowRunMapRefusalCode] ?? REFUSAL_FALLBACK_HEADLINE;
}
