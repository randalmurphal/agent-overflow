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
 * Format a duration in seconds as "Xs" (<60s) or "Xm Ys" (>=60s).
 * Used by the completion divider's "Worked for ..." suffix. Negative /
 * non-finite inputs clamp to zero; callers that want the section omitted
 * should branch on the zero case themselves.
 *
 * Examples: 12 -> "12s", 60 -> "1m 0s", 90 -> "1m 30s", 3600 -> "60m 0s".
 */
export function formatElapsedSeconds(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds < 0) return '0s';
  const whole = Math.floor(seconds);
  if (whole < 60) return `${whole}s`;
  const minutes = Math.floor(whole / 60);
  const remainder = whole % 60;
  return `${minutes}m ${remainder}s`;
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
