// The numbers and the time label the sidebar's 24px rows share.
//
// ThreadRow and ThreadGroupRow are siblings in one list: a group row and the
// member rows under it have to line their content up to the pixel, and the
// group's relative time has to read exactly like a thread's. Two copies of
// these constants is how that drifts, so they have one home here.

import { getSettings } from '../stores/settings.svelte';
import { HOME_BACKEND, type BackendKey } from '../transport/backendKey';
import { relativeTime } from './format';

/**
 * Indent scale by row depth. Depth 0-1 (a project's top-level rows, and a
 * group's members) sit flush against the rail container's padding — the rail
 * itself is the visual nesting cue. Deeper levels step 8px, with the last
 * entry clamping so a malformed deep chain can't push titles off-screen.
 */
export const INDENT_PX = [0, 0, 8, 16];

/**
 * The leading gutter between the rail and a top-level thread row's first flex
 * child, reserved for the pin affordance. Rows that render a pin centre it
 * inside; rows that don't leave it empty so titles stay aligned.
 */
export const PIN_SLOT_PX = 24;

function indentStepPx(indent: number): number {
  return INDENT_PX[Math.min(Math.max(indent, 0), INDENT_PX.length - 1)]!;
}

/** Thread row padding-left for an indent level: the pin gutter plus the indent step. */
export function sidebarRowPaddingLeftPx(indent: number): number {
  return PIN_SLOT_PX + indentStepPx(indent);
}

// ── Group geometry ─────────────────────────────────────────────────────────
//
// A group is never pinned, so its row spends no gutter. Instead its chevron
// is centred on the top-level pin column, and its members' pins are centred
// under its folder glyph. Every number below is measured from the left edge
// of the project's thread list and derives from the row grammar ThreadGroupRow
// renders: chevron (w-4), gap-1.5, folder glyph (11px), gap-1.5, name.

const CHEVRON_PX = 16;
const ROW_GAP_PX = 6;
const FOLDER_GLYPH_PX = 11;

/** Group row padding-left: centres the chevron on the top-level pin column. */
export const GROUP_ROW_PADDING_LEFT_PX = (PIN_SLOT_PX - CHEVRON_PX) / 2;

/** The member rail drops from the chevron's centre (it is a 1px border). */
export const GROUP_RAIL_LEFT_PX = GROUP_ROW_PADDING_LEFT_PX + CHEVRON_PX / 2;

/** Where a member row's box starts: just right of the rail. */
const GROUP_MEMBER_ROW_LEFT_PX = GROUP_RAIL_LEFT_PX + 1;

const GROUP_FOLDER_CENTRE_PX = GROUP_ROW_PADDING_LEFT_PX + CHEVRON_PX + ROW_GAP_PX + FOLDER_GLYPH_PX / 2;

const GROUP_NAME_LEFT_PX = GROUP_ROW_PADDING_LEFT_PX + CHEVRON_PX + ROW_GAP_PX + FOLDER_GLYPH_PX + ROW_GAP_PX;

/**
 * Left offset, inside a member row, of its PIN_SLOT_PX-wide pin slot, so the
 * pin centres under the group's folder glyph.
 */
export const GROUP_MEMBER_PIN_SLOT_LEFT_PX = Math.round(
  GROUP_FOLDER_CENTRE_PX - PIN_SLOT_PX / 2 - GROUP_MEMBER_ROW_LEFT_PX,
);

/**
 * Member row padding-left: its title lines up with the group's name. A
 * member's own discussion children step in from there.
 */
export function sidebarGroupMemberPaddingLeftPx(indent: number): number {
  return GROUP_NAME_LEFT_PX - GROUP_MEMBER_ROW_LEFT_PX + indentStepPx(indent) - indentStepPx(2);
}

/**
 * The right-slot relative time. Shortened against the app-wide `relativeTime`
 * (no trailing "ago", "just now" becomes "now") because the slot is ~7 chars
 * wide, and only for the locale format — the absolute formats are already the
 * width the user asked for.
 *
 * Callers must read `getMinuteNow()` in the same derived: this function has no
 * clock dependency of its own, so an idle row would otherwise never re-render.
 *
 * `backendId` is the machine whose clock minted `timestampMs`
 * (`transport/backendClock.ts`); omitting it means home, which is what a
 * single-backend page has always meant.
 */
export function sidebarTimeLabel(timestampMs: number, backendId: BackendKey = HOME_BACKEND): string {
  const label = relativeTime(timestampMs, getSettings().timestampFormat, backendId);
  if (getSettings().timestampFormat !== 'locale') return label;
  return label === 'just now' ? 'now' : label.replace(/ ago$/, '');
}
