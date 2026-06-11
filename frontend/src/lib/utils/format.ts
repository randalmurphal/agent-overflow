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
 * Format a token count for display (e.g., 1500 -> "1.5k", 2000000 -> "2.0M").
 */
export function formatTokens(n: number): string {
  if (n >= 1_000_000) return (n / 1_000_000).toFixed(1) + 'M';
  if (n >= 1_000) return (n / 1_000).toFixed(1) + 'k';
  return String(n);
}

/**
 * Format a USD cost for display.
 */
export function formatCost(usd: number): string {
  return '$' + usd.toFixed(4);
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
  if (!Number.isFinite(resetsAtSeconds) || resetsAtSeconds <= 0) return '';
  const nowSec = Math.floor(Date.now() / 1000);
  const diffSec = resetsAtSeconds - nowSec;
  if (diffSec <= 0) return 'Resetting now';
  if (diffSec < 60) return 'Resets in <1m';
  const minutes = Math.floor(diffSec / 60);
  if (minutes < 60) return `Resets in ${minutes}m`;
  const hours = Math.floor(minutes / 60);
  const remMin = minutes % 60;
  if (hours < 24) {
    return remMin > 0 ? `Resets in ${hours}h ${remMin}m` : `Resets in ${hours}h`;
  }
  const days = Math.floor(hours / 24);
  const remHr = hours % 24;
  return remHr > 0 ? `Resets in ${days}d ${remHr}h` : `Resets in ${days}d`;
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
