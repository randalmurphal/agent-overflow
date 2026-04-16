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
