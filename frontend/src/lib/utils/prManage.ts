// Pure rules of the PR management controls (docs/specs/pull-requests.md,
// "Managing a PR"): which controls a detail offers, how a picker's
// selection becomes one change, and the picker's search query bound.
// Every control reads the forge's own flags (PRDetail.manage.can); a
// detail without them (an older backend) offers none.

import type { MergeBlocker, PRDetail, PRLabel, PRLabelChange, PRPeopleChange, PRPerson, PRReviewer } from '../types/models';

export type PRStateChange = 'close' | 'reopen' | 'draft';

export type PRManageRow = 'reviewers' | 'assignees' | 'labels';

export const PR_STATE_LABEL: Readonly<Record<PRStateChange, string>> = {
  close: 'Close',
  reopen: 'Reopen',
  draft: 'Convert to draft',
};

/** The title bar's `⋯` items, in menu order; none on a merged PR. */
export function prStateMenuItems(detail: PRDetail): PRStateChange[] {
  const can = detail.manage?.can;
  if (!can || detail.state.toLowerCase() === 'merged') return [];
  const out: PRStateChange[] = [];
  if (can.convertToDraft && !detail.draft) out.push('draft');
  if (can.close) out.push('close');
  if (can.reopen) out.push('reopen');
  return out;
}

/** Whether the row's Edit shows. */
export function canEditRow(detail: PRDetail, row: PRManageRow): boolean {
  return !!detail.manage?.can[row];
}

/** A reviewer's re-request icon: the viewer may, and the review is
 * settled (any state but pending) or on an older commit. */
export function canRerequest(detail: PRDetail, reviewer: PRReviewer): boolean {
  if (!detail.manage?.can.rerequest || reviewer.team) return false;
  return reviewer.state !== 'PENDING' || !!reviewer.stale;
}

/** The changes_requested blocker's GitHub Dismiss entries. */
export function dismissibleChangeRequests(detail: PRDetail, blocker: MergeBlocker) {
  if (blocker.kind !== 'changes_requested' || !detail.manage?.can.dismiss) return [];
  return blocker.changeRequests ?? [];
}

/** The changes_requested blocker's GitLab Bypass. */
export function canBypass(detail: PRDetail, blocker: MergeBlocker): boolean {
  return blocker.kind === 'changes_requested' && !!blocker.bypassable && !!detail.manage?.can.bypass;
}

/** A person's identity in a picker: logins are unique per forge host,
 * and a GitHub team's is its "org/slug". */
export function personKey(person: PRPerson): string {
  return `${person.team ? 't' : 'u'}:${person.login.toLowerCase()}`;
}

/** A label's identity in a picker: its name, unique per repository. */
export function labelKey(label: PRLabel): string {
  return label.name.toLowerCase();
}

/** The add and remove sets that take `initial` to `selected`, each in
 * the order its map holds; both empty when nothing changed. */
export function selectionChange<T>(initial: ReadonlyMap<string, T>, selected: ReadonlyMap<string, T>): { add: T[]; remove: T[] } {
  const add: T[] = [];
  const remove: T[] = [];
  for (const [key, value] of selected) if (!initial.has(key)) add.push(value);
  for (const [key, value] of initial) if (!selected.has(key)) remove.push(value);
  return { add, remove };
}

/** A person as the backend gave it (from `manage` or a search), without
 * a reviewer's state: the forge adapter decides which field addresses
 * it, so nothing is rebuilt from parts. */
export function wirePerson(person: PRPerson | PRReviewer): PRPerson {
  const { state: _state, stale: _stale, ...rest } = person as PRReviewer;
  return rest;
}

/** The wire shape of a people change: each person as the backend gave it. */
export function peopleChange(change: { add: PRPerson[]; remove: PRPerson[] }): PRPeopleChange {
  return { add: change.add.map(wirePerson), remove: change.remove.map(wirePerson) };
}

/** The wire shape of a label change: each label as the backend gave it. */
export function labelChange(change: { add: PRLabel[]; remove: PRLabel[] }): PRLabelChange {
  return { add: [...change.add], remove: [...change.remove] };
}

/** The backend reads at most this many bytes of a picker query. */
export const PICKER_QUERY_MAX_BYTES = 100;

/** The query as the backend takes it: one line, trimmed, at most
 * PICKER_QUERY_MAX_BYTES of UTF-8, cut at a character boundary. */
export function pickerQuery(text: string): string {
  const line = text.replace(/[\r\n]+/g, ' ').trim();
  const encoder = new TextEncoder();
  if (encoder.encode(line).length <= PICKER_QUERY_MAX_BYTES) return line;
  let out = '';
  let bytes = 0;
  for (const char of line) {
    const size = encoder.encode(char).length;
    if (bytes + size > PICKER_QUERY_MAX_BYTES) break;
    out += char;
    bytes += size;
  }
  return out;
}

/** A merge method whose merge makes a commit, so its message can be edited. */
export function methodHasCommitMessage(method: string): boolean {
  return method === 'merge' || method === 'squash';
}

/** A "#rrggbb" forge color, or null; anything else is not painted. */
export function labelColor(label: PRLabel): string | null {
  return label.color && /^#[0-9a-f]{6}$/i.test(label.color) ? label.color : null;
}

export const REVIEWER_STATE_LABEL: Readonly<Record<string, string>> = {
  APPROVED: 'approved',
  CHANGES_REQUESTED: 'changes requested',
  COMMENTED: 'commented',
  PENDING: 'pending',
};

/** The title bar's verdict pill tint for a reviewer state. */
export function reviewerStateClass(state: string): string {
  switch (state) {
    case 'APPROVED':
      return 'bg-success/12 text-success';
    case 'CHANGES_REQUESTED':
      return 'bg-error/12 text-error';
    default:
      return 'bg-surface-2 text-fg-muted';
  }
}
