// The Pull requests page's attention vocabulary: one state pill per row,
// the "since you looked" fragment, and the two groups. The backend
// computes the state (internal/app, the attention state function); this
// module only names it, so the page, the sidebar badge and the compact
// list never disagree (docs/specs/pull-requests.md, "The page").

import type { ReadinessTone } from './reviewReadiness';

export type PullState =
  | 'your_turn'
  | 'pipeline_failed'
  | 'ready'
  | 'changes_requested'
  | 'waiting_on_author'
  | 'draft'
  | 'open';

export type PullGroup = 'needs_you' | 'waiting';

export interface PullStatePill {
  label: string;
  tone: ReadinessTone;
}

const PILLS: Record<PullState, PullStatePill> = {
  your_turn: { label: 'Your turn', tone: 'accent' },
  pipeline_failed: { label: 'Pipeline failed', tone: 'error' },
  ready: { label: 'Ready to merge', tone: 'success' },
  changes_requested: { label: 'Changes requested', tone: 'warning' },
  waiting_on_author: { label: 'Waiting on author', tone: 'muted' },
  draft: { label: 'Draft', tone: 'muted' },
  open: { label: 'Open', tone: 'muted' },
};

/** The pill for a row's state; an unknown state reads as open. */
export function pullStatePill(state: string): PullStatePill {
  return PILLS[state as PullState] ?? PILLS.open;
}

export function pullGroupLabel(group: string): string {
  return group === 'needs_you' ? 'Needs you' : 'Waiting on others';
}

/** What the backend knows changed since the app last opened the PR. */
export interface PullSince {
  headMoved: boolean;
  newComments: number;
  newCommits: number;
}

/**
 * The "since you looked" fragment, or null when nothing moved (a quiet
 * row) or the PR was never opened here (nothing to compare against).
 * Commit counts come from the forge list where it carries them (GitHub);
 * a head that moved without a count reads as "pushed".
 */
export function sinceYouLookedText(since: PullSince | null | undefined): string | null {
  if (!since) return null;
  const parts: string[] = [];
  if (since.newCommits > 0) {
    parts.push(`+${since.newCommits} ${since.newCommits === 1 ? 'commit' : 'commits'}`);
  } else if (since.headMoved) {
    parts.push('pushed');
  }
  if (since.newComments > 0) {
    parts.push(`${since.newComments} new ${since.newComments === 1 ? 'comment' : 'comments'}`);
  }
  if (parts.length === 0) return null;
  return `${parts.join(' · ')} since you looked`;
}

/** A row the reader has seen with nothing new on it dims. */
export function pullRowQuiet(seen: boolean, since: PullSince | null | undefined): boolean {
  return seen && sinceYouLookedText(since) === null;
}
