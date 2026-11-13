import type { DiffReviewComment, ReviewThread } from '../types/models';

// Comment counts and comment text helpers for the review pane: the file
// tree badges and the toolbar tally count every PR review thread
// (file-anchored AND PR-level conversation) and local draft in the
// current scope; the thread rows and cards read a comment's lead
// sentence and its visible body from here.

/** 'comment' = a non-resolvable thread (flat conversation comment) —
 * neutral, so it doesn't masquerade as "unresolved". */
export type CommentItemState = 'unresolved' | 'resolved' | 'draft' | 'comment';

export interface CommentFileCounts {
  total: number;
  unresolved: number;
}

export interface CommentCounts {
  /** Per file path; `''` holds the PR-level conversation threads. */
  byFile: ReadonlyMap<string, CommentFileCounts>;
  tally: CommentTally;
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
  let text = line.replace(/<[^>]+>/g, ' '); // HTML tags (<details>, <summary>, <img …>)
  // Links/images → their text, applied until stable: a badge row is a
  // link WRAPPING an image (`[![alt](img)](href)`), and one pass leaves
  // the outer link's `](href)` tail as visible syntax.
  for (let pass = 0; pass < 4; pass++) {
    const next = text.replace(/!?\[([^\]]*)\]\([^)]*\)/g, '$1');
    if (next === text) break;
    text = next;
  }
  return text
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

/** A comment's body with HTML comments removed. Bot reviewers leave
 * marker-only replies (`<!-- coderabbit resolve -->`) that render as an
 * empty card; a surface renders a comment only when this is non-empty,
 * and shows THIS so a marker glued to real prose never paints the
 * marker. A fenced code block is kept whole: a comment opener inside one
 * is code. Only a fence at the body's top level is read as one; a
 * comment that opens before a fence swallows it. */
export function visibleBody(body: string): string {
  const fences = /^ {0,3}(`{3,}|~{3,})([^\n]*)$/gm;
  // The next fence at or after a position, or null once there is none.
  let fence: { start: number; end: number } | null | undefined;
  const fenceFrom = (at: number): { start: number; end: number } | null => {
    if (fence === null || (fence && fence.start >= at)) return fence;
    fences.lastIndex = at;
    for (let open = fences.exec(body); open; open = fences.exec(body)) {
      const mark = open[1];
      // A backtick fence's info string holds no backtick.
      if (mark[0] === '`' && open[2].includes('`')) continue;
      const close = new RegExp(`^ {0,3}${mark[0]}{${mark.length},}[ \\t]*\\r?$`, 'gm');
      close.lastIndex = fences.lastIndex;
      const closed = close.exec(body);
      return (fence = { start: open.index, end: closed ? closed.index + closed[0].length : body.length });
    }
    return (fence = null);
  };
  let out = '';
  let at = 0;
  while (at < body.length) {
    const comment = body.indexOf('<!--', at);
    const next = fenceFrom(at);
    if (next && (comment < 0 || next.start < comment)) {
      out += body.slice(at, next.end);
      at = next.end;
      continue;
    }
    if (comment < 0) {
      out += body.slice(at);
      break;
    }
    out += body.slice(at, comment);
    const end = body.indexOf('-->', comment + 4);
    at = end < 0 ? body.length : end + 3;
  }
  return out.trim();
}

/** Forge timestamps are ISO strings; unparseable input maps to null so
 * the row simply omits the time instead of showing "NaN ago". */
function threadState(thread: ReviewThread): CommentItemState {
  // Outdated is a marker, not a state: an open thread on moved code is
  // still unresolved (utils/reviewThreadStyle.ts).
  // Legacy payloads without the field are all resolvable diff threads.
  if (thread.isResolvable === false) return 'comment';
  if (thread.isResolved) return 'resolved';
  return 'unresolved';
}

export function countReviewComments(input: {
  prThreads: readonly ReviewThread[];
  drafts: readonly DiffReviewComment[];
}): CommentCounts {
  const byFile = new Map<string, CommentFileCounts>();
  const tally: CommentTally = { unresolved: 0, drafts: 0, total: 0 };
  function add(filePath: string, state: CommentItemState): void {
    const counts = byFile.get(filePath) ?? { total: 0, unresolved: 0 };
    counts.total += 1;
    if (state === 'unresolved') counts.unresolved += 1;
    byFile.set(filePath, counts);
    tally.total += 1;
    if (state === 'unresolved') tally.unresolved += 1;
    if (state === 'draft') tally.drafts += 1;
  }
  for (const thread of input.prThreads) add(thread.path, threadState(thread));
  for (const draft of input.drafts) add(draft.filePath, 'draft');
  return { byFile, tally };
}

/** The thread as plain text for the clipboard: its location, then every
 * comment with a visible body as `@login: body`. The shape an agent
 * composer wants pasted in, which is what the Copy action is for. */
export function threadClipboardText(thread: ReviewThread): string {
  const location = thread.path === '' ? '' : thread.line ? `${thread.path}:${thread.line}` : thread.path;
  const comments = thread.comments
    .map((comment) => ({ login: comment.authorLogin, body: visibleBody(comment.body) }))
    .filter((comment) => comment.body !== '')
    .map((comment) => `@${comment.login}: ${comment.body}`);
  return [...(location === '' ? [] : [location]), ...comments].join('\n\n');
}
