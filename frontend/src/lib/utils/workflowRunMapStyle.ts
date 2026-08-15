// The run map's presentation vocabulary (RUN-MAP §2), in one pure place for the
// same reason `workflowRunSignal.ts` is one: R1's attention hues are a rule
// about the whole app, not a per-component styling choice. Components ask for a
// style and render it; no component names a colour, a glyph or a glow of its
// own.
//
// Amber is human-blocked and red is failed — the ATTENTION hues, still
// exclusive. On top of them the map carries two clarity hints (R1 amendment,
// §13 fourth pass): a done glyph is green (`glyphTone` — the ✓ only, never the
// text), and the `now ▸` position marker is the accent. Hierarchy beyond that
// is SURFACE, not ink: what happened gets a quiet fill (`fill`), what is live
// gets fill + a real border, and what has not happened gets no box at all —
// ghosts render as bare marks on the spine, so borders stop being the only
// thing the eye has to parse.
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
  /** Text colour for the row's cause chip and metas. */
  tone: string;
  /**
   * The status GLYPH's colour — where the map's colour hints live. Green for
   * the done ✓ (the one place `--success` appears on this surface); everything
   * else restates `tone`, so attention hues still arrive through one mapping.
   */
  glyphTone: string;
  /**
   * Text classes for the row's LABEL. Same hue as `tone` unless §2 gives the
   * signal a typographic emphasis of its own, which only ever restates a
   * NEUTRAL step (`text-fg`, `text-fg-subtle`) — never a hue. R1's attention
   * meanings are therefore declared exactly once, in `runMapTone`.
   */
  label: string;
  /** The status glyph, or `''` when the row draws a `SteppedSpinner` instead. */
  glyph: string;
  /** Border classes for a BOXED rendering of this signal. */
  border: string;
  /**
   * Background fill for a boxed rendering: reality is a surface, not an
   * outline. `''` for the signals that never box (ghost, unknown keeps its
   * dotted hairline alone).
   */
  fill: string;
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

// Settled work is a quiet SURFACE (`fill`, hairline-free): a border earns its
// ink by marking something still in motion or still wrong. Live and attention
// signals keep a real border on top of their fill; ghosts get neither, because
// they are not boxed at all.
const BORDERS: Record<RunMapSignal, string> = {
  done: 'border-transparent',
  running: 'border-border-strong',
  pending: 'border-border-subtle',
  failed: 'border-error',
  dropped: 'border-transparent',
  parked: 'border-warning',
  ghost: 'border-dashed border-border-subtle',
  unknown: 'border-dotted border-border-subtle',
};

const FILLS: Record<RunMapSignal, string> = {
  done: 'bg-surface-2/50',
  running: 'bg-surface-2/60',
  pending: 'bg-surface-1/40',
  failed: 'bg-error/10',
  dropped: 'bg-surface-2/30',
  parked: 'bg-warning/10',
  ghost: '',
  unknown: '',
};

/**
 * The done ✓ is green — the map's one `--success` use, and a GLYPH hue only:
 * the label beside it stays neutral, so colour reads as a mark, not a field.
 * Every other signal's glyph restates its `tone`.
 */
const GLYPH_TONES: Partial<Record<RunMapSignal, string>> = {
  done: 'text-success',
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
    glyphTone: GLYPH_TONES[signal] ?? tone,
    label: skipped ? `${label} ${SKIPPED_LABEL}` : label,
    glyph: skipped ? SKIPPED_GLYPH : GLYPHS[signal],
    border: BORDERS[signal],
    fill: FILLS[signal],
    glow: signal === 'parked' ? 'status-glow-warning' : '',
    spinner: signal === 'running' && !skipped,
  };
}

/**
 * Middle-truncation budget for a node or unit label. Phase names and unit ids
 * are engine-stamped but unbounded in principle (§6, text). Labels WRAP —
 * CSS ellipsis is banned on this surface, because a map whose every node says
 * "Implement …" says nothing — so this bound is not a line budget: it is the
 * runaway guard for a label that is effectively a paragraph, far past any
 * real phase name. The full text always rides in `title`.
 */
export const RUN_MAP_LABEL_MAX = 96;

// ---------------------------------------------------------------- geometry
//
// §2's node geometry, in the same module as its hues and for the same reason:
// the map reads as a FLOW rather than a list only while every node on it is
// the same kind of box, and a per-component literal is how "the same box"
// stops being true. Nothing here names a colour — the border WIDTH is
// geometry, the border hue is `runMapNodeStyle`'s.
//
// The classes are structural Tailwind only. The connective tissue between
// boxes — the spine, the fan's fork and rejoin, the lane drops — is pure CSS
// in `app.css` under `.run-map-*`, because it is drawn with pseudo-elements
// that no utility class can express, and because it must stay DECORATION: the
// boxes themselves are ordinary block-level descendants of the scroller's row
// flow, which is what keeps §9.7's anchor descent able to find them.

/**
 * A node on the spine: an intrinsic-width box, centered by its column, capped
 * at its container so a long label wraps inside it rather than stretching the
 * flow to the card edge.
 *
 * INLINE FLOW, not flex: the glyph, label and meta read as one line of text
 * that wraps mid-line wherever it must. The flex version wrapped whole ITEMS —
 * a label whose max-content width overflowed moved below the glyph as a unit,
 * stranding a lone `·` or `✓` on the first line, which read as a rendering
 * bug. Children space themselves with margins (`mr-1.5` after a glyph,
 * `ml-1.5` before a meta) because inline flow has no `gap`.
 */
export const RUN_MAP_NODE_BOX =
  'inline-block max-w-full rounded-md border px-2.5 py-1 text-left text-xs';

/**
 * A ghost row: the same line of text with NO box around it. The future is
 * most of a real campaign's map, and boxing it at full weight buried the live
 * minority — dashes everywhere read as wireframe. A bare mark on the spine
 * recedes; the spine's own connector line keeps it structured.
 */
export const RUN_MAP_GHOST_ROW =
  'inline-block max-w-full px-2.5 py-0.5 text-left text-[0.6875rem]';

/**
 * The frame around a body of flow: the current wave, and a live composition's
 * sub-card. §2's one structural emphasis — the live path is the thing with a
 * box around it — so it gets room to breathe that a folded row does not.
 */
export const RUN_MAP_CARD = 'rounded-lg border px-2.5 py-2';

/** A fan lane's name, above its column (§7, lane headers). */
export const RUN_MAP_LANE_HEADER =
  'text-[0.625rem] font-semibold uppercase tracking-[0.09em]';

/**
 * An OPEN fan lane's width band (§6). The floor keeps a wrapped label from
 * shattering into one-word lines; the cap keeps three lanes from spreading a
 * 1700px card so far apart the fork reads as three unrelated columns. Lanes
 * flex between the two and the lane ROW wraps when they cannot all fit —
 * nothing on the map scrolls sideways. `app.css` uses the same values for the
 * leave-animation fallback; a test cross-checks the two.
 */
export const RUN_MAP_LANE_MIN = '15rem';
export const RUN_MAP_LANE_MAX = '26rem';

/**
 * Middle-truncation budget for a FOLDED lane's title — the one deliberately
 * single-line text on the map (§6): a folded lane is a summary by definition,
 * and letting its title wrap re-grew the wall the fold exists to prevent. It
 * is also `flex: none`, so an unbounded title would push the row past the
 * card edge; the full text rides in `title` like everywhere else.
 */
export const RUN_MAP_FOLDED_LABEL_MAX = 40;

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
