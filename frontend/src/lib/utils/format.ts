/**
 * Format a Unix timestamp (milliseconds) as a time string.
 * When format is 'locale' (default), uses relative time ("5m ago").
 * When format is '12-hour' or '24-hour', uses absolute time.
 */
export function relativeTime(
  timestampMs: number,
  format: 'locale' | '12-hour' | '24-hour' = 'locale',
): string {
  if (format === '12-hour') {
    return new Date(timestampMs).toLocaleString(undefined, {
      hour: 'numeric', minute: '2-digit', hour12: true,
      month: 'short', day: 'numeric',
    });
  }
  if (format === '24-hour') {
    return new Date(timestampMs).toLocaleString(undefined, {
      hour: '2-digit', minute: '2-digit', hour12: false,
      month: 'short', day: 'numeric',
    });
  }

  const now = Date.now();
  const diffMs = now - timestampMs;

  if (diffMs < 0) return 'just now';

  const seconds = Math.floor(diffMs / 1000);
  if (seconds < 60) return 'just now';

  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m ago`;

  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours}h ago`;

  const days = Math.floor(hours / 24);
  if (days < 30) return `${days}d ago`;

  const months = Math.floor(days / 30);
  return `${months}mo ago`;
}

// Hoisted: toLocaleTimeString with an options bag constructs a fresh
// Intl.DateTimeFormat per call — the dominant cost of this helper, paid
// once per row mount across the whole transcript during scroll bursts.
const TIME_OF_DAY_FORMAT = new Intl.DateTimeFormat(undefined, {
  hour: 'numeric',
  minute: '2-digit',
});

/**
 * Format a Unix timestamp (milliseconds) as the locale clock time shown
 * on transcript row headers (e.g. "8:05 PM") — hour + minute, no seconds.
 * Callers pass required DB-sourced epoch values; non-finite input renders
 * "Invalid Date" (matching toLocaleTimeString) instead of throwing.
 */
export function formatTimeOfDay(timestampMs: number): string {
  if (!Number.isFinite(timestampMs)) return 'Invalid Date';
  return TIME_OF_DAY_FORMAT.format(timestampMs);
}

/**
 * Format a token count for display (e.g., 1500 -> "1.5k", 2000000 -> "2.0M",
 * 11776335004 -> "11.8B").
 *
 * The billions step is not hypothetical: Codex reports a lifetime account
 * total, which passes 10^9 within months of daily use and read as a
 * five-digit "11776.3M" without it.
 */
export function formatTokens(n: number): string {
  if (n >= 1_000_000_000) return (n / 1_000_000_000).toFixed(1) + 'B';
  if (n >= 1_000_000) return (n / 1_000_000).toFixed(1) + 'M';
  if (n >= 1_000) return (n / 1_000).toFixed(1) + 'k';
  return String(n);
}

/**
 * Format a USD amount for the usage surfaces (composer usage chip,
 * sidebar usage footer, usage modal). Cents ALWAYS show, at every
 * magnitude — an unqualified "$118" reading as "exactly $118" was a
 * bug:
 *
 *   0            -> "$0.00"
 *   0.0042       -> "<$0.01"
 *   0.32         -> "$0.32"
 *   42.104       -> "$42.10"
 *   142.7        -> "$142.70"
 *
 * Non-finite / negative inputs collapse to "$0.00".
 */
export function formatUsd(usd: number): string {
  if (!Number.isFinite(usd) || usd <= 0) return '$0.00';
  if (usd < 0.01) return '<$0.01';
  return '$' + usd.toFixed(2);
}

/**
 * Format a duration in seconds as "Xs" (<60s), "Xm Ys" (<1h), or
 * "Xh Ym Zs" (>=1h).
 * Used by compact duration labels. Negative / non-finite inputs clamp
 * to zero; callers that want the section omitted
 * should branch on the zero case themselves.
 *
 * Examples: 12 -> "12s", 60 -> "1m 0s", 90 -> "1m 30s", 3600 -> "1h 0m 0s".
 */
export function formatElapsedSeconds(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds < 0) return '0s';
  const whole = Math.floor(seconds);
  if (whole < 60) return `${whole}s`;
  const minutes = Math.floor(whole / 60);
  if (minutes < 60) return `${minutes}m ${whole % 60}s`;
  return `${Math.floor(minutes / 60)}h ${minutes % 60}m ${whole % 60}s`;
}

/**
 * Format a completion duration measured in milliseconds for tool-call
 * row headers. Sub-second values stay as "Nms" so a 12ms cache lookup
 * is distinguishable from a 1.2s call; ≥1s collapses to one decimal
 * second; ≥1m drops back to integer-second granularity inside the
 * minute. Used by AdvisorRow/AgentRow/GenericToolCallRow.
 */
export function formatDurationMs(ms: number): string {
  if (ms < 1000) return `${ms}ms`;
  const seconds = ms / 1000;
  if (seconds < 60) return `${seconds.toFixed(1)}s`;
  const minutes = Math.floor(seconds / 60);
  const remSec = Math.round(seconds - minutes * 60);
  return `${minutes}m ${remSec}s`;
}

/**
 * Format a future Unix timestamp (seconds, like Claude's `resetsAt`
 * and Codex's `resets_at`) as a "Resets in Xh Ym" countdown.
 *
 * Sub-minute → "Resets in <1m" (collapse so we don't flicker through
 * "in 59s"/"in 58s" — at sub-minute the countdown is no longer the
 * meaningful fact). Past timestamps → "Resetting now" (the wire is
 * stale by a few seconds; the next event will refresh it).
 *
 * The wire is in seconds for both providers (Anthropic + OpenAI
 * convention). Callers pass the raw `resetsAt` value; this function
 * compares it against `Date.now() / 1000`.
 */
export function formatResetCountdown(resetsAtSeconds: number): string {
  const span = formatCountdownSpan(resetsAtSeconds * 1000, Date.now());
  if (span === '') return '';
  return span === 'now' ? 'Resetting now' : `Resets in ${span}`;
}

/**
 * The bare span of a countdown — `<1m`, `12m`, `1h 12m`, `2d 3h` — against an
 * EXPLICIT clock, so a caller riding the shared 1Hz clock has no `Date.now()`
 * in its derived path (the run map's `resumes in X` chip, §7).
 *
 * `''` for a target that is not set, `'now'` once it has passed: the caller
 * owns the verb, this owns the collapse rules (sub-minute never counts down
 * second by second, and a multi-day window never becomes a six-figure hour).
 */
export function formatCountdownSpan(targetMs: number, nowMs: number): string {
  if (!Number.isFinite(targetMs) || targetMs <= 0 || !Number.isFinite(nowMs)) return '';
  const diffSec = Math.floor(targetMs / 1000) - Math.floor(nowMs / 1000);
  if (diffSec <= 0) return 'now';
  if (diffSec < 60) return '<1m';
  const minutes = Math.floor(diffSec / 60);
  if (minutes < 60) return `${minutes}m`;
  const hours = Math.floor(minutes / 60);
  const remMin = minutes % 60;
  if (hours < 24) return remMin > 0 ? `${hours}h ${remMin}m` : `${hours}h`;
  const days = Math.floor(hours / 24);
  const remHr = hours % 24;
  return remHr > 0 ? `${days}d ${remHr}h` : `${days}d`;
}

/**
 * "12m" / "1h 4m" / "48s" — a LENGTH in milliseconds, in the same shapes every
 * span on the run map uses. Exported because a ceiling is not a span between two
 * timestamps: a wall-clock budget states a duration outright, and formatting it
 * with a second set of rules would put "30 min" beside "1h 4m" on one line.
 */
export function workflowSpanMs(ms: number): string {
  const seconds = Math.max(0, Math.round((Number.isFinite(ms) ? ms : 0) / 1000));
  if (seconds < 60) return `${seconds}s`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m`;
  const hours = Math.floor(minutes / 60);
  const remainder = minutes % 60;
  return remainder > 0 ? `${hours}h ${remainder}m` : `${hours}h`;
}

function spanLabel(startedAt: number, endMs: number): string {
  return workflowSpanMs(endMs - startedAt);
}

/**
 * "12m" / "1h 4m" / "48s" — elapsed span of one phase attempt, unit or run.
 * Seconds are deliberately dropped above a minute: the run map's 1Hz clock
 * feeds this, and a ticking seconds digit on a two-hour phase is noise.
 *
 * `endedAt` of 0 means "still going", so `nowMs` closes the span. It is
 * REQUIRED, with no `Date.now()` default: a default is only correct for a
 * caller that re-derives against a ticking clock, and the two callers that
 * silently took it were inside a `$derived` nothing re-ran — so an open span
 * froze at whatever second the component last happened to render. A caller
 * with no clock wants `workflowClosedDuration`, which cannot fabricate one.
 */
export function workflowDuration(startedAt: number, endedAt: number, nowMs: number): string {
  if (!startedAt) return '';
  return spanLabel(startedAt, endedAt || nowMs);
}

/**
 * The same span for a caller that has NO clock, and therefore may only render a
 * span that is already closed. An open one answers `''` rather than freezing an
 * elapsed value that would then be wrong for as long as the row is on screen.
 */
export function workflowClosedDuration(startedAt: number, endedAt: number): string {
  if (!startedAt || !endedAt) return '';
  return spanLabel(startedAt, endedAt);
}

/**
 * Format a turn-completion token count as a human-readable label fragment.
 * Returns "150 tokens" below 1_000 and "1.23k tokens" at/above 1_000 (two
 * decimals of k per docs/architecture/turn-lifecycle.md). Negative / non-
 * finite inputs produce "0 tokens"; callers check for meaningful counts
 * before calling.
 *
 * Examples: 150 -> "150 tokens", 1234 -> "1.23k tokens".
 */
export function formatTurnTokens(n: number): string {
  if (!Number.isFinite(n) || n < 0) return '0 tokens';
  if (n >= 1_000) return `${(n / 1_000).toFixed(2)}k tokens`;
  return `${Math.floor(n)} tokens`;
}

/**
 * Format a byte count as gibibytes (1024³) with one decimal — used by
 * the sidebar SystemStatsFooter's `used / total GB` display. The "GB"
 * label is colloquial (htop / Activity Monitor convention): a stick
 * sold as 16 GB shows as 16.0 here rather than 17.2, which matches
 * what users expect.
 *
 * Non-finite / negative inputs collapse to "0.0" rather than NaN so
 * the row stays readable if the wire ever sends garbage.
 */
export function formatGiB(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes < 0) return '0.0';
  return (bytes / 1024 ** 3).toFixed(1);
}

/**
 * `3 threads` / `1 thread` — a count with its noun, pluralized by adding an
 * `s`. Only for nouns whose plural is regular; anything else (`entry`,
 * `child`) needs its own wording rather than a broken suffix rule.
 */
export function countNoun(count: number, noun: string): string {
  return `${count} ${noun}${count === 1 ? '' : 's'}`;
}

/**
 * `/home/u/repos/…/frontend` — drop the MIDDLE of an over-long string.
 *
 * For paths, which is what wants this: the head says where in the filesystem
 * it lives and the tail says which directory it is, so a trailing ellipsis
 * (what CSS truncation does) removes the only part that identifies it. The
 * full value belongs in a `title` alongside; this is the readable stand-in.
 *
 * `max` counts the RESULT, ellipsis included, so a caller sizing a column can
 * trust it. Below four characters there is no room to say anything, so the
 * value comes back whole rather than as punctuation.
 */
export function truncateMiddle(value: string, max: number): string {
  if (max < 4 || value.length <= max) return value;
  const head = Math.ceil((max - 1) / 2);
  const tail = max - 1 - head;
  return `${value.slice(0, head)}…${value.slice(value.length - tail)}`;
}
