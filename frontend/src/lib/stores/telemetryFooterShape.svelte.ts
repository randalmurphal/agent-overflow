// The sidebar telemetry footers' last settled shape per selection: how many
// content lines the usage footer painted (provider rows plus its
// unavailable line), or how many computer rows the system stats footer
// painted and whether it showed its computer picker. A footer paints that
// shape until its own answer settles, so a launch does not resize the
// sidebar above it.
import { untrack } from 'svelte';
import { onFrontendValueChanged, readFrontendValue, writeFrontendValue } from './frontendStorage';

export interface FooterShape {
  readonly lines: number;
  readonly picker: boolean;
}

const STORAGE_KEY = 'telemetry-footer-shapes';
export const MAX_FOOTER_SHAPES = 32;
export const MAX_FOOTER_LINES = 16;

function clampLines(lines: number): number {
  return Math.min(Math.max(Math.trunc(lines), 0), MAX_FOOTER_LINES);
}

function readShapes(): ReadonlyMap<string, FooterShape> {
  const raw = readFrontendValue(STORAGE_KEY);
  if (!Array.isArray(raw)) return new Map();
  const out = new Map<string, FooterShape>();
  for (const entry of raw.slice(-MAX_FOOTER_SHAPES)) {
    if (!Array.isArray(entry) || entry.length !== 3) continue;
    const [key, lines, picker] = entry as unknown[];
    if (typeof key !== 'string' || key.length === 0 || key.length > 1024) continue;
    if (!Number.isInteger(lines) || (lines as number) < 0 || (lines as number) > MAX_FOOTER_LINES) continue;
    if (picker !== 0 && picker !== 1) continue;
    out.set(key, { lines: lines as number, picker: picker === 1 });
  }
  return out;
}

let shapes = $state.raw(readShapes());
onFrontendValueChanged(STORAGE_KEY, () => { shapes = readShapes(); });

/** One key per footer, computer selection (null is the default) and, for usage, period. */
export function telemetryFooterShapeKey(footer: 'usage' | 'system', selection: readonly string[] | null, period = ''): string {
  return JSON.stringify([footer, selection === null ? null : [...selection].sort(), period]);
}

export function getTelemetryFooterShape(key: string): FooterShape | null {
  return shapes.get(key) ?? null;
}

/** Records a settled shape. Every record refreshes the key's recency, so
 *  the least recently settled selection is the one evicted. Reads the map
 *  untracked: a footer's recording effect must not re-run because another
 *  footer recorded. */
export function recordTelemetryFooterShape(key: string, shape: FooterShape): void {
  const value = { lines: clampLines(shape.lines), picker: shape.picker };
  const previous = untrack(() => shapes);
  const current = previous.get(key);
  const last = [...previous.keys()].at(-1);
  if (last === key && current?.lines === value.lines && current.picker === value.picker) return;
  const next = new Map(previous);
  next.delete(key);
  next.set(key, value);
  while (next.size > MAX_FOOTER_SHAPES) next.delete(next.keys().next().value!);
  shapes = next;
  writeFrontendValue(STORAGE_KEY, [...next].map(([k, v]) => [k, v.lines, v.picker ? 1 : 0]));
}

export function resetTelemetryFooterShapesForTest(): void {
  shapes = new Map();
  writeFrontendValue(STORAGE_KEY, null);
}
