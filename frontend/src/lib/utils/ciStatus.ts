// Shared presentation helpers for normalized CI statuses
// (internal/git/ci.go vocabulary): chip dots, text tints, durations.

export function ciStatusDotClass(status: string): string {
  switch (status) {
    case 'success':
      return 'bg-success';
    case 'failed':
      return 'bg-error';
    case 'running':
      return 'bg-accent animate-pulse';
    case 'pending':
      return 'bg-warning';
    case 'manual':
      return 'bg-fg-muted';
    default:
      return 'bg-fg-subtle';
  }
}

export function ciStatusTextClass(status: string): string {
  switch (status) {
    case 'success':
      return 'text-success';
    case 'failed':
      return 'text-error';
    case 'running':
      return 'text-accent';
    case 'pending':
      return 'text-warning';
    default:
      return 'text-fg-muted';
  }
}

export function formatCIDuration(seconds?: number): string {
  if (!seconds || seconds <= 0) return '';
  const total = Math.round(seconds);
  if (total < 60) return `${total}s`;
  const minutes = Math.floor(total / 60);
  if (minutes < 60) return `${minutes}m ${total % 60}s`;
  return `${Math.floor(minutes / 60)}h ${minutes % 60}m`;
}
