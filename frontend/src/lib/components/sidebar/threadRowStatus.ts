// Pure classification for the thread-row live-status dot.
//
// Extracted from ThreadRow.svelte. Each function is a switch over the
// thread-status projection that produces a CSS class string or an accessible
// label. Keeping these in one place makes the styling trivial to audit
// when the design token table changes.

import type { ThreadLiveStatus } from '../../stores/threadStatuses.svelte';

export function statusDotClass(status: ThreadLiveStatus): string {
  switch (status) {
    case 'running':
      return 'bg-warning animate-pulse';
    case 'pending-approval':
      return 'bg-accent';
    case 'error':
      return 'bg-error';
    case 'idle':
    default:
      return '';
  }
}

export function statusDotLabel(status: ThreadLiveStatus): string {
  switch (status) {
    case 'running':
      return 'Running';
    case 'pending-approval':
      return 'Pending approval';
    case 'error':
      return 'Error';
    case 'idle':
    default:
      return '';
  }
}
