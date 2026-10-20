// The merge readiness vocabulary every review surface shares: the pill on
// the title bar's refs line and the Merge section's header read the same
// label and tone for one PRDetail, so a PR never says two things about
// whether it can merge (docs/specs/pull-requests.md).

import type { MergeReadiness, PRDetail } from '../types/models';

export type ReadinessTone = 'success' | 'warning' | 'accent' | 'error' | 'muted';

export interface ReadinessPill {
  label: string;
  tone: ReadinessTone;
  /** The readiness state the label stands for. */
  state: string;
}

export const READINESS_PILL_CLASS: Record<ReadinessTone, string> = {
  success: 'bg-success/12 text-success',
  warning: 'bg-warning/12 text-warning',
  accent: 'bg-accent/12 text-accent',
  error: 'bg-error/12 text-error',
  muted: 'bg-surface-2 text-fg-muted',
};

/** The pill for a PR's readiness; null when the backend sent none. */
export function readinessPill(detail: Pick<PRDetail, 'readiness'> | null | undefined): ReadinessPill | null {
  const readiness: MergeReadiness | undefined = detail?.readiness ?? undefined;
  if (!readiness || !readiness.state) return null;
  switch (readiness.state) {
    case 'ready':
      return { label: 'Ready to merge', tone: 'success', state: readiness.state };
    case 'blocked': {
      const count = readiness.blockers?.length ?? 0;
      return { label: count > 0 ? `Blocked · ${count}` : 'Blocked', tone: 'warning', state: readiness.state };
    }
    case 'checking':
      return { label: 'Checking…', tone: 'muted', state: readiness.state };
    case 'draft':
      return { label: 'Draft', tone: 'muted', state: readiness.state };
    case 'merged':
      return { label: 'Merged', tone: 'accent', state: readiness.state };
    case 'closed':
      return { label: 'Closed', tone: 'error', state: readiness.state };
    default:
      return { label: readiness.state, tone: 'muted', state: readiness.state };
  }
}

/** The method rows of the Merge menu, in the project's order. */
export function mergeMethodLabel(method: string): string {
  switch (method) {
    case 'squash':
      return 'Squash and merge';
    case 'rebase':
      return 'Rebase and merge';
    default:
      return 'Merge commit';
  }
}

// What a forge event row says after the actor's name.
const EVENT_LABEL: Record<string, string> = {
  approved: 'approved',
  unapproved: 'revoked approval',
  review_dismissed: 'dismissed a review',
  merged: 'merged',
  closed: 'closed',
  reopened: 'reopened',
  marked_draft: 'marked as draft',
  marked_ready: 'marked as ready',
  auto_merge_set: 'set auto-merge',
  auto_merge_canceled: 'canceled auto-merge',
  force_pushed: 'force-pushed',
  rebased: 'rebased',
  review_requested: 'requested a review',
  base_changed: 'changed the target branch',
};

export function reviewEventLabel(kind: string): string {
  return EVENT_LABEL[kind] ?? kind.replaceAll('_', ' ');
}

/** The tone of an event's rail dot. */
export function reviewEventTone(kind: string): ReadinessTone {
  switch (kind) {
    case 'approved':
    case 'merged':
    case 'marked_ready':
      return 'success';
    case 'unapproved':
    case 'review_dismissed':
    case 'force_pushed':
    case 'marked_draft':
      return 'warning';
    case 'closed':
      return 'error';
    case 'auto_merge_set':
    case 'reopened':
      return 'accent';
    default:
      return 'muted';
  }
}
