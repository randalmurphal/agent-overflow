// Selected time period for the usage surfaces (sidebar UsageFooter +
// UsageModal). Persisted in localStorage so the choice survives a
// reload, mirroring the pattern in sidebarLayout.svelte.ts. The modal's
// period Segmented control and the footer's period label read/write the
// same store, so changing it in either place moves the other.
//
// Periods are CALENDAR-aligned, not rolling windows: 'day' resets at
// local midnight, 'week' at the most recent Sunday's midnight, 'month'
// at the 1st. This keeps every surface answering the same question —
// the heatmap and day buckets are already calendar-day based, so a
// rolling "last 24h" number would disagree with the heatmap's "today"
// cell whenever yesterday evening had usage.

const STORAGE_KEY = 'agent-overflow:usage:period';
const DEFAULT_PERIOD: UsagePeriod = 'month';

export type UsagePeriod = 'day' | 'week' | 'month' | 'all';

export const VALID_PERIODS: readonly UsagePeriod[] = ['day', 'week', 'month', 'all'];

// Pre-calendar persisted values (rolling windows, shipped 2026-07-03).
// Mapped on read so an existing localStorage entry lands on the closest
// calendar period instead of silently resetting to the default.
const LEGACY_PERIODS: Record<string, UsagePeriod> = {
  '1d': 'day',
  '1w': 'week',
  '30d': 'month',
};

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
    if (isUsagePeriod(raw)) return raw;
    if (typeof raw === 'string' && raw in LEGACY_PERIODS) return LEGACY_PERIODS[raw];
    return DEFAULT_PERIOD;
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

/**
 * Maps a period + "now" into a `UsageQuery.fromMillis` lower bound.
 * 'all' is unbounded (0, matching the query's own "zero means
 * unbounded" convention); the rest are the start of the current LOCAL
 * calendar period — midnight today, midnight of the most recent Sunday
 * (matching the Sunday-start heatmap weeks), or midnight of the 1st.
 * Local Date math on purpose: the backend query carries the same local
 * offset via tzOffsetMinutes, so both agree on where "today" starts.
 */
export function periodFromMillis(selected: UsagePeriod, nowMs: number): number {
  if (selected === 'all') return 0;
  const start = new Date(nowMs);
  start.setHours(0, 0, 0, 0);
  if (selected === 'week') {
    // getDay() is 0 on Sunday, so this is a no-op on Sundays.
    start.setDate(start.getDate() - start.getDay());
  } else if (selected === 'month') {
    start.setDate(1);
  }
  return start.getTime();
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
