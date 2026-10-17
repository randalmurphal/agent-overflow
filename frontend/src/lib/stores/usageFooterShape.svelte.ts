// The sidebar usage footer's last answered shape: how many provider rows
// the reply for a computer selection and period had. The footer paints
// that many placeholder rows (none when the last reply was empty) before
// its first reply, so a launch does not resize the sidebar.
import { onFrontendValueChanged, readFrontendValue, writeFrontendValue } from './frontendStorage';

const STORAGE_KEY = 'usage-footer-shapes';
const MAX_SHAPES = 32;
const MAX_ROWS = 16;

function readShapes(): ReadonlyMap<string, number> {
  const raw = readFrontendValue(STORAGE_KEY);
  if (!Array.isArray(raw)) return new Map();
  return new Map(raw.slice(-MAX_SHAPES).filter((entry): entry is [string, number] =>
    Array.isArray(entry) && entry.length === 2 && typeof entry[0] === 'string' && entry[0].length > 0 && entry[0].length <= 1024
    && Number.isInteger(entry[1]) && entry[1] >= 0 && entry[1] <= MAX_ROWS));
}

let shapes = $state.raw(readShapes());
onFrontendValueChanged(STORAGE_KEY, () => { shapes = readShapes(); });

/** One key per computer selection (null is every computer) and period. */
export function usageFooterShapeKey(selection: readonly string[] | null, period: string): string {
  return JSON.stringify([selection === null ? null : [...selection].sort(), period]);
}

export function getUsageFooterShape(key: string): number | null {
  return shapes.get(key) ?? null;
}

export function recordUsageFooterShape(key: string, rows: number): void {
  const value = Math.min(Math.max(Math.trunc(rows), 0), MAX_ROWS);
  if (shapes.get(key) === value) return;
  const next = new Map(shapes);
  next.delete(key);
  next.set(key, value);
  while (next.size > MAX_SHAPES) next.delete(next.keys().next().value!);
  shapes = next;
  writeFrontendValue(STORAGE_KEY, [...next]);
}

export function resetUsageFooterShapesForTest(): void {
  shapes = new Map();
  writeFrontendValue(STORAGE_KEY, null);
}
