import type { DiffReviewComment, ReviewThread } from '../types/models';
import type { PatchFile } from './patchFiles';

// Pure list model behind the review rail's Comments tab: every PR
// review thread (file-anchored AND PR-level conversation) and local
// draft in the current scope, grouped by file in diff order with the
// conversation group first, actionable items (unresolved threads,
// drafts) leading within each group. Each item carries the ROW KEY of
// its diff row (`pt:<threadId>` / `t:<draftId>` — the same keys
// buildReviewRows emits) so clicking a list entry can jump the diff
// body straight to the row; items with no diff row (conversation
// threads, files outside the diff) expand inline instead.

/** 'comment' = a non-resolvable thread (flat conversation comment) —
 * neutral, so it doesn't masquerade as "unresolved". */
export type CommentItemState = 'unresolved' | 'resolved' | 'outdated' | 'draft' | 'comment';

export interface CommentEntry {
  author: string;
  body: string;
}

export interface CommentListItem {
  /** Diff row key to jump to (matches buildReviewRows' rowKeys). */
  rowKey: string;
  kind: 'pr-thread' | 'draft';
  /** PR thread id (for expanding the thread on jump); drafts: null. */
  threadId: string | null;
  /** Empty for PR-level conversation threads. */
  filePath: string;
  line: number | null;
  author: string;
  snippet: string;
  state: CommentItemState;
  orphaned: boolean;
  /** False when there is no diff row to jump to (file outside the
   * current diff, or a conversation thread) — the list expands the
   * item inline instead. */
  inDiff: boolean;
  replies: number;
  /** First comment's creation time (epoch ms); null when unparseable. */
  createdAtMs: number | null;
  /** Full thread bodies for the inline expansion of non-jumpable items. */
  comments: readonly CommentEntry[];
}

export interface CommentFileGroup {
  /** Empty for the PR-level conversation group. */
  filePath: string;
  inDiff: boolean;
  items: CommentListItem[];
}

export interface CommentTally {
  unresolved: number;
  drafts: number;
  total: number;
}

// Sized for the list's two-line clamp at typical rail widths.
const SNIPPET_MAX_CHARS = 160;

/** First meaningful prose line of a comment, markdown stripped. Bot
 * reviewers (CodeRabbit et al.) open with badge lines like
 * `_🛠️ Functional Correctness_ | 🟠 Major | ⚡ Quick win` — those are
 * category chrome, identical across findings, so label/table/fence/tag
 * lines are skipped until real prose (usually the bolded finding
 * title) is found. */
export function commentSnippet(body: string): string {
  let inFence = false;
  for (const raw of body.replace(/<!--[\s\S]*?-->/g, '').split('\n')) {
    const line = raw.trim();
    if (/^(```|~~~)/.test(line)) {
      inFence = !inFence;
      continue;
    }
    if (inFence || line === '') continue;
    // Table rows and badge lines: `|`-separated label segments.
    if (line.startsWith('|') || line.includes(' | ')) continue;
    // HTML-structural lines (<details>, <summary>label</summary>, …).
    if (line.startsWith('<')) continue;
    // A short fully-italic line is a category label (`_⚠️ Potential issue_`),
    // not prose.
    if (line.length < 48 && /^_[^_].*_$/.test(line)) continue;
    const stripped = stripInlineMarkdown(line);
    if (!/[a-zA-Z]/.test(stripped)) continue;
    if (stripped.length <= SNIPPET_MAX_CHARS) return stripped;
    return `${stripped.slice(0, SNIPPET_MAX_CHARS - 1)}…`;
  }
  return '';
}

function stripInlineMarkdown(line: string): string {
  return line
    .replace(/<[^>]+>/g, ' ') // HTML tags (<details>, <summary>, <img …>)
    .replace(/!?\[([^\]]*)\]\([^)]*\)/g, '$1') // links/images → text
    .replace(/^#{1,6}\s+/, '') // heading marker
    .replace(/^(>\s*)+/, '') // blockquote markers
    .replace(/^([-*+]|\d+[.)])\s+/, '') // list marker
    .replace(/[`*]/g, '') // code spans, bold/italic asterisks
    // Emphasis underscores only at word edges — `primary_tier` keeps its
    // inner underscores.
    .replace(/(^|\s)_+/g, '$1')
    .replace(/_+(\s|[.,;:!?)]|$)/g, '$1')
    .replace(/\s+/g, ' ')
    .trim();
}

/** Forge timestamps are ISO strings; unparseable input maps to null so
 * the row simply omits the time instead of showing "NaN ago". */
function parseTimestampMs(iso: string | undefined): number | null {
  if (!iso) return null;
  const parsed = Date.parse(iso);
  return Number.isFinite(parsed) ? parsed : null;
}

function threadState(thread: ReviewThread): CommentItemState {
  if (thread.isOutdated) return 'outdated';
  // Legacy payloads without the field are all resolvable diff threads.
  if (thread.isResolvable === false) return 'comment';
  if (thread.isResolved) return 'resolved';
  return 'unresolved';
}

// Actionable first: unresolved threads and drafts, then settled ones.
function stateRank(state: CommentItemState): number {
  return state === 'unresolved' || state === 'draft' ? 0 : 1;
}

export function buildCommentGroups(input: {
  files: readonly PatchFile[];
  prThreads: readonly ReviewThread[];
  drafts: readonly DiffReviewComment[];
  orphanedDraftIds: ReadonlySet<string>;
}): CommentFileGroup[] {
  const diffPaths = new Set(input.files.map((file) => file.path));
  const itemsByPath = new Map<string, CommentListItem[]>();

  function add(item: CommentListItem): void {
    const bucket = itemsByPath.get(item.filePath) ?? [];
    bucket.push(item);
    itemsByPath.set(item.filePath, bucket);
  }

  for (const thread of input.prThreads) {
    const first = thread.comments[0];
    add({
      rowKey: `pt:${thread.id}`,
      kind: 'pr-thread',
      threadId: thread.id,
      filePath: thread.path,
      line: thread.line ?? null,
      author: first?.authorLogin ?? '',
      snippet: commentSnippet(first?.body ?? ''),
      state: threadState(thread),
      orphaned: thread.isOutdated,
      inDiff: thread.path !== '' && diffPaths.has(thread.path),
      replies: Math.max(0, thread.comments.length - 1),
      createdAtMs: parseTimestampMs(first?.createdAt),
      comments: thread.comments.map((comment) => ({ author: comment.authorLogin, body: comment.body })),
    });
  }

  for (const draft of input.drafts) {
    add({
      rowKey: `t:${draft.id}`,
      kind: 'draft',
      threadId: null,
      filePath: draft.filePath,
      line: draft.side === 'file' ? null : (draft.newLine ?? draft.oldLine ?? null),
      author: 'You',
      snippet: commentSnippet(draft.body),
      state: 'draft',
      orphaned: input.orphanedDraftIds.has(draft.id),
      inDiff: diffPaths.has(draft.filePath),
      replies: 0,
      createdAtMs: draft.createdAt > 0 ? draft.createdAt : null,
      comments: [{ author: 'You', body: draft.body }],
    });
  }

  for (const bucket of itemsByPath.values()) {
    bucket.sort((a, b) =>
      stateRank(a.state) - stateRank(b.state)
      || (a.line ?? Number.MAX_SAFE_INTEGER) - (b.line ?? Number.MAX_SAFE_INTEGER)
      || a.rowKey.localeCompare(b.rowKey));
  }

  // The PR-level conversation group leads, then diff-order groups, then
  // comment-only paths (threads on files outside the current diff)
  // alphabetically at the end.
  const groups: CommentFileGroup[] = [];
  const conversation = itemsByPath.get('');
  if (conversation) {
    groups.push({ filePath: '', inDiff: false, items: conversation });
    itemsByPath.delete('');
  }
  for (const file of input.files) {
    const items = itemsByPath.get(file.path);
    if (items) {
      groups.push({ filePath: file.path, inDiff: true, items });
      itemsByPath.delete(file.path);
    }
  }
  for (const filePath of [...itemsByPath.keys()].sort()) {
    groups.push({ filePath, inDiff: false, items: itemsByPath.get(filePath)! });
  }
  return groups;
}

export function commentCountsByFile(groups: readonly CommentFileGroup[]): Map<string, number> {
  return new Map(groups.map((group) => [group.filePath, group.items.length]));
}

export function commentTally(groups: readonly CommentFileGroup[]): CommentTally {
  let unresolved = 0;
  let drafts = 0;
  let total = 0;
  for (const group of groups) {
    for (const item of group.items) {
      total += 1;
      if (item.state === 'unresolved') unresolved += 1;
      if (item.state === 'draft') drafts += 1;
    }
  }
  return { unresolved, drafts, total };
}
