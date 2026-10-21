// The Pull requests page's attention vocabulary: one state per row, the
// reason it is in that state, and the two groups. The backend computes
// the state and the reason from the forge's own record of the viewer's
// activity (internal/app, the attention state function); this module only
// words them, so the page, the sidebar badge and the compact list never
// disagree (docs/specs/pull-requests.md, "The page").

import type { ReadinessTone } from './reviewReadiness';

export type PullState =
  | 'your_turn'
  | 'pipeline_failed'
  | 'ready'
  | 'changes_requested'
  | 'waiting_on_author'
  | 'approved'
  | 'draft'
  | 'open';

export type PullGroup = 'needs_you' | 'waiting';

/** Why a row is in its state, as the wire spells it (PRListReason):
 * for `your_turn` which turn it is (`review_requested`, `pushed`,
 * `replied`), for `approved` whether the viewer approved or only
 * reviewed (`approved`, `reviewed`); '' when the state needs none. */
export interface PullReason {
  kind: string;
  /** Commits since the viewer's review, when the forge counts them. */
  commits: number;
  /** The author's replies since the viewer's review, when counted. */
  replies: number;
}

/** The backend's attention order (prStateOrder in app_pr_attention.go). */
export const PULL_STATE_ORDER: readonly PullState[] = [
  'your_turn', 'pipeline_failed', 'ready', 'changes_requested', 'waiting_on_author', 'approved', 'draft', 'open',
];

const TONES: Record<PullState, ReadinessTone> = {
  your_turn: 'accent',
  pipeline_failed: 'error',
  ready: 'success',
  changes_requested: 'warning',
  waiting_on_author: 'muted',
  approved: 'muted',
  draft: 'muted',
  open: 'muted',
};

export function pullStateRank(state: string): number {
  const at = PULL_STATE_ORDER.indexOf(state as PullState);
  return at < 0 ? PULL_STATE_ORDER.length : at;
}

/** The tone of a row's state label; an unknown state reads as open. */
export function pullStateTone(state: string): ReadinessTone {
  return TONES[state as PullState] ?? TONES.open;
}

/**
 * The row's state label: for a turn, what happened (`Review requested`,
 * `+3 commits since your review`, `2 replies since your review`); for
 * every other state, the state. The label is the one coloured text on
 * the row, so it says why the row is where it is.
 */
export function pullStateText(state: string, reason: PullReason | null | undefined): string {
  switch (state as PullState) {
    case 'your_turn':
      switch (reason?.kind) {
        case 'pushed':
          return reason.commits > 0
            ? `+${reason.commits} ${reason.commits === 1 ? 'commit' : 'commits'} since your review`
            : 'Pushed since your review';
        case 'replied':
          return reason.replies > 0
            ? `${reason.replies === 1 ? '1 reply' : `${reason.replies} replies`} since your review`
            : 'Author replied';
        default:
          return 'Review requested';
      }
    case 'pipeline_failed':
      return 'Pipeline failed';
    case 'ready':
      return 'Ready to merge';
    case 'changes_requested':
      return 'Changes requested';
    case 'waiting_on_author':
      return 'Waiting on author';
    case 'approved':
      return reason?.kind === 'reviewed' ? 'Reviewed by you' : 'Approved by you';
    case 'draft':
      return 'Draft';
    default:
      return 'Open';
  }
}

export function pullGroupLabel(group: string): string {
  return group === 'needs_you' ? 'Needs you' : 'Waiting on others';
}
