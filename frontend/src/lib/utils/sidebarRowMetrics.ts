// The numbers and the time label the sidebar's 24px rows share.
//
// ThreadRow and ThreadGroupRow are siblings in one list: a group row and the
// member rows under it have to line their content up to the pixel, and the
// group's relative time has to read exactly like a thread's. Two copies of
// these constants is how that drifts, so they have one home here.

import { getSettings } from '../stores/settings.svelte';
import { relativeTime } from './format';

/**
 * Indent scale by row depth. Depth 0-1 (a project's top-level rows, and a
 * group's members) sit flush against the rail container's padding — the rail
 * itself is the visual nesting cue. Deeper levels step 8px, with the last
 * entry clamping so a malformed deep chain can't push titles off-screen.
 */
export const INDENT_PX = [0, 0, 8, 16];

/**
 * The leading gutter between the project rail and the row's first flex child,
 * reserved for the pin affordance. EVERY row reserves it — rows that render a
 * pin place it absolutely inside, rows that don't leave it empty so titles
 * stay aligned with their neighbours.
 */
export const PIN_SLOT_PX = 24;

/**
 * Row padding-left for an indent level: the pin gutter plus the indent step.
 * A row INSIDE a group reserves no gutter: nothing can be pinned there, and
 * the group rail already carries the nesting, so the empty 24px only pushed
 * the row's ring and content away from the line.
 */
export function sidebarRowPaddingLeftPx(indent: number, inGroup = false): number {
  const gutter = inGroup ? 0 : PIN_SLOT_PX;
  return gutter + INDENT_PX[Math.min(Math.max(indent, 0), INDENT_PX.length - 1)]!;
}

/**
 * The right-slot relative time. Shortened against the app-wide `relativeTime`
 * (no trailing "ago", "just now" becomes "now") because the slot is ~7 chars
 * wide, and only for the locale format — the absolute formats are already the
 * width the user asked for.
 *
 * Callers must read `getMinuteNow()` in the same derived: this function has no
 * clock dependency of its own, so an idle row would otherwise never re-render.
 */
export function sidebarTimeLabel(timestampMs: number): string {
  const label = relativeTime(timestampMs, getSettings().timestampFormat);
  if (getSettings().timestampFormat !== 'locale') return label;
  return label === 'just now' ? 'now' : label.replace(/ ago$/, '');
}
