// Selected time period for the usage surfaces (sidebar UsageFooter +
// UsageModal). Persisted in localStorage so the choice survives a
// reload, mirroring the pattern in sidebarLayout.svelte.ts. The modal's
// period Segmented control and the footer's period label read/write the
// same store, so changing it in either place moves the other.

const STORAGE_KEY = 'agent-overflow:usage:period';
const DEFAULT_PERIOD: UsagePeriod = '30d';

export type UsagePeriod = '1d' | '1w' | '30d' | 'all';

export const VALID_PERIODS: readonly UsagePeriod[] = ['1d', '1w', '30d', 'all'];

function isUsagePeriod(value: unknown): value is UsagePeriod {
  return typeof value === 'string' && (VALID_PERIODS as readonly string[]).includes(value);
}

/**
 * Exported for direct unit testing of the "corrupt stored value falls
 * back to default" behaviour, which otherwise only runs at module
 * import and can't be re-driven from a test.
 */
export function readPersistedPeriod(): UsagePeriod {
  if (typeof localStorage === 'undefined') return DEFAULT_PERIOD;
  try {
    const raw = localStorage.getItem(STORAGE_KEY);
    return isUsagePeriod(raw) ? raw : DEFAULT_PERIOD;
  } catch {
    return DEFAULT_PERIOD;
  }
}

function write(value: UsagePeriod): void {
  if (typeof localStorage === 'undefined') return;
  try {
    localStorage.setItem(STORAGE_KEY, value);
  } catch {
    // Best-effort persistence; in-memory period still updates.
  }
}

let period: UsagePeriod = $state(readPersistedPeriod());

export function getUsagePeriod(): UsagePeriod {
  return period;
}

export function setUsagePeriod(next: UsagePeriod): void {
  period = next;
  write(next);
}

/** Advances to the next period in a fixed rotation. Used by the footer's
 *  period label click target. */
export function cycleUsagePeriod(): void {
  const index = VALID_PERIODS.indexOf(period);
  setUsagePeriod(VALID_PERIODS[(index + 1) % VALID_PERIODS.length]);
}

const DAY_MS = 24 * 60 * 60 * 1000;

const PERIOD_DAYS: Record<Exclude<UsagePeriod, 'all'>, number> = {
  '1d': 1,
  '1w': 7,
  '30d': 30,
};

/**
 * Maps a period + "now" into a `UsageQuery.fromMillis` lower bound.
 * 'all' is unbounded (0, matching the query's own "zero means
 * unbounded" convention); the rest are rolling windows ending at
 * `nowMs`.
 */
export function periodFromMillis(selected: UsagePeriod, nowMs: number): number {
  if (selected === 'all') return 0;
  return nowMs - PERIOD_DAYS[selected] * DAY_MS;
}

/** Test helper — restore the default + wipe storage between unit tests. */
export function resetUsagePeriodForTest(): void {
  period = DEFAULT_PERIOD;
  if (typeof localStorage === 'undefined') return;
  try {
    localStorage.removeItem(STORAGE_KEY);
  } catch {
    // ignore
  }
}
