import { createRawSnippet, flushSync, mount, unmount } from 'svelte';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import '../../../app.css';
import ReviewDiffBody from './ReviewDiffBody.svelte';
import ReviewCommentThread from './ReviewCommentThread.svelte';
import ReviewPRThreadRow from './ReviewPRThreadRow.svelte';
import ReviewCILogBlockRow from './ReviewCILogBlockRow.svelte';
import { CILogBlockCache } from '../../stores/ciLogBlocks.svelte';
import { createMarkdownFindTextCache } from '../chat/markdown/markdownFindText';
import { conflictPatchFile } from '../../utils/conflictFile';
import { createFindHighlighter, type FindHighlighter } from '../../utils/findHighlights';
import { countFindMatches } from '../../utils/findMatch';
import { parseReviewFiles, reviewFileFromPatchFile, type ReviewFile } from '../../utils/patchStore';
import { scanReviewFind, type ReviewFindFileSpot, type ReviewFindHit } from '../../utils/reviewFindScan';
import type { CommentAnchor } from '../../utils/reviewRows';
import type { DiffReviewComment, ReviewThread } from '../../types/models';
import { waitFor } from '../../../test/helpers/browserFrames';
import { FakeCILog } from '../../../test/helpers/ciLog';
import { rawProps } from '../../../test/helpers/rawProps.svelte';
import { setBindingMock } from '../../../test/mocks/bindings-app';

// Review find counts only searched text, so every piece of chrome a row
// renders (line numbers, badges, authors, times, buttons, fold and gap
// labels, the log's clip note) is outside its `data-find-key` element or
// under `data-find-skip`: for a needle that occurs only in chrome, the
// painted matches are the counted ones, per row kind.

const PATCH = [
  'diff --git a/src/a.ts b/src/a.ts',
  '--- a/src/a.ts',
  '+++ b/src/a.ts',
  '@@ -1,3 +1,3 @@',
  ' alpha one',
  '-alpha two',
  '+alpha 2',
  ' alpha three',
  '@@ -40,3 +40,3 @@',
  ' alpha forty',
  '-alpha old',
  '+alpha new',
  ' alpha tail',
  'diff --git a/src/b.ts b/src/b.ts',
  'new file mode 100644',
  '--- /dev/null',
  '+++ b/src/b.ts',
  '@@ -0,0 +1,1 @@',
  '+alpha added',
  '',
].join('\n');

const DRAFT: DiffReviewComment = {
  id: 'd1', threadId: 'thread-1', scope: 'workspace', sourceKey: 'source', filePath: 'src/a.ts', status: 'draft',
  newLine: 2, side: 'new', selectedText: '', body: 'alpha words', createdAt: 1, updatedAt: 1,
};

const minutesAgo = new Date(Date.now() - 5 * 60_000).toISOString();
const THREAD: ReviewThread = {
  id: 'p1', path: 'src/a.ts', line: 41, side: 'right', isResolvable: true, isResolved: false, isOutdated: false,
  comments: [
    { authorLogin: 'priya', authorName: 'Priya Rao', body: 'alpha first', createdAt: minutesAgo, databaseID: 1 },
    { authorLogin: 'review-bot[bot]', body: 'alpha second', createdAt: minutesAgo, databaseID: 2 },
  ],
};

const DIFF_CHROME = ['unchanged', 'lines', 'added', '41', '+2', '-2', 'You', 'draft', 'Priya', '@priya', 'bot', 'ago', 'Reply', 'Resolve', 'unresolved', 'RB', '·'];
const CONFLICT_CHROME = ['unchanged', 'conflict', 'origin/main', 'feature', '12'];
const LOG_CHROME = ['clipped', '64 KB', 'Save to file'];

beforeEach(() => {
  setBindingMock('HighlightSchemaVersion', async () => 'test-schema');
  setBindingMock('HighlightClassNames', async () => ['none']);
  setBindingMock('HighlightPatch', async () => ({ lines: [] }));
});

const cleanups: (() => void)[] = [];
let highlighter: FindHighlighter | null = null;

afterEach(() => {
  highlighter?.dispose();
  highlighter = null;
  for (const cleanup of cleanups.splice(0).reverse()) cleanup();
});

function painted(root: HTMLElement, query: string): (string | null)[] {
  highlighter ??= createFindHighlighter({ root, name: 'ao-find-chrome', rowSelector: '[data-find-key]', rowKeyAttribute: 'data-find-key', rowsOnly: true });
  highlighter.setQuery(query);
  return highlighter.matches.map((match) => match.rowKey);
}

function host(): HTMLElement {
  const el = document.createElement('div');
  el.style.cssText = 'position:fixed;top:0;left:0;width:900px;height:1600px;display:flex';
  document.body.appendChild(el);
  cleanups.push(() => el.remove());
  return el;
}

async function settle(scrollEl: HTMLElement): Promise<void> {
  let last = '';
  let stable = 0;
  await waitFor(() => {
    const now = `${scrollEl.scrollHeight}:${scrollEl.querySelectorAll('[data-find-key]').length}`;
    stable = now === last ? stable + 1 : 0;
    last = now;
    return stable >= 3;
  }, 'stable review rows');
}

async function mountDiff(files: ReviewFile[], drafts: DiffReviewComment[], prThreads: ReviewThread[]) {
  const target = host();
  cleanups.push(() => { for (const file of files) file.body.segments[0]?.store.dispose(); });
  const draftRow = createRawSnippet((threadKey: () => string) => ({
    render: () => '<div></div>',
    setup: (node: Element) => {
      const comment = drafts.find((draft) => draft.id === threadKey())!;
      const app = mount(ReviewCommentThread, { target: node, props: { comment, onUpdate: () => {}, onDelete: () => {} } });
      return () => { void unmount(app); };
    },
  }));
  const threadRow = createRawSnippet((thread: () => ReviewThread, anchor: () => CommentAnchor, collapsed: () => boolean, orphaned: () => boolean) => ({
    render: () => '<div></div>',
    setup: (node: Element) => {
      const app = mount(ReviewPRThreadRow, {
        target: node,
        props: {
          thread: thread(), anchor: anchor(), collapsed: collapsed(), orphaned: orphaned(), body: '', error: null, sending: false,
          resolving: false, resolveError: null, onToggle: () => {}, onBodyChange: () => {}, onSendReply: () => {},
          onResolve: () => {}, onJumpToConversation: () => {},
        },
      });
      return () => { void unmount(app); };
    },
  }));
  const props = rawProps({
    subjectId: 'subject', spanOwner: 'thread-1', scope: 'workspace', files,
    viewMode: 'stacked' as 'stacked' | 'split', wordWrap: false,
    collapsedPaths: new Set<string>() as ReadonlySet<string>, onToggleCollapsed: () => {},
    jumpToFilePath: null as string | null, onJumpConsumed: () => {},
    drafts, commentThread: draftRow, prThreads, expandedPRThreadIds: new Set<string>() as ReadonlySet<string>, prThread: threadRow,
    jumpToRowKey: null as string | null, onJumpRowConsumed: () => {},
  });
  const app = mount(ReviewDiffBody, { target, props });
  cleanups.push(() => { void unmount(app); });
  const scrollEl = target.querySelector('[data-testid="review-scroll"]') as HTMLElement;
  await settle(scrollEl);
  return { target, props, scrollEl };
}

async function counted(files: ReviewFile[], drafts: DiffReviewComment[], prThreads: ReviewThread[], query: string, foldLines?: (path: string) => ReadonlyMap<number, readonly string[]> | null): Promise<string[]> {
  const texts = createMarkdownFindTextCache({ embeddedHtml: true });
  // Each hit with where the scan read it.
  const hits: (ReviewFindHit & ReviewFindFileSpot)[] = [];
  await scanReviewFind({ files, drafts, prThreads, foldLines, commentText: (body) => texts(body).runs }, query, {
    limit: 1000, live: () => true, emit: (hit, _file, spot) => hits.push({ ...hit, ...spot }), pause: async () => {},
  });
  return hits.map((hit) => hit.rowKey);
}

describe('review find over row chrome', () => {
  it.each(['stacked', 'split'] as const)('counts what paints in diff, header, gap, draft and thread rows (%s)', async (viewMode) => {
    const files = parseReviewFiles(PATCH);
    const { target, props, scrollEl } = await mountDiff(files, [DRAFT], [THREAD]);
    props.viewMode = viewMode;
    flushSync();
    await settle(scrollEl);
    // Every kind of row and chrome is on the page.
    for (const testid of ['review-file-header', 'review-line-block', 'review-comment-thread', 'review-pr-thread']) {
      expect(target.querySelector(`[data-testid="${testid}"]`), testid).not.toBeNull();
    }
    for (const query of DIFF_CHROME) {
      const scan = await counted(files, [DRAFT], [THREAD], query);
      expect(scan, query).toEqual(painted(target, query));
      expect(target.textContent, `${query} is on the page`).toContain(query);
    }
    // Not vacuous: searched text paints and counts, one key per match.
    const all = await counted(files, [DRAFT], [THREAD], 'alpha');
    expect(all.length).toBe(12);
    expect(all).toEqual(painted(target, 'alpha'));
  });

  it('counts what paints in conflict rows: markers, folds and the conflict badge', async () => {
    const content = [...Array.from({ length: 12 }, (_, i) => `beta ${i + 1}`), '<<<<<<< origin/main', 'beta left', '=======', 'beta right', '>>>>>>> feature', 'beta end'].join('\n');
    const folds = new Map<number, readonly string[]>();
    const files = [reviewFileFromPatchFile(conflictPatchFile('c.go', content, {
      baseLabel: 'origin/main', headLabel: 'feature', onFold: (id, lines) => folds.set(id, lines),
    }))];
    expect(folds.size).toBe(1);
    const { target } = await mountDiff(files, [], []);
    expect(target.querySelector('[data-testid="review-conflict-count"]')).not.toBeNull();
    const foldLines = () => folds;
    for (const query of CONFLICT_CHROME) {
      // A match in a fold is counted where the fold is, not painted until
      // it opens: compare the rows on the page.
      const scan = (await counted(files, [], [], query, foldLines)).filter((key) => target.querySelector(`[data-find-key="${CSS.escape(key)}"]`));
      expect(scan, query).toEqual(painted(target, query));
      expect(target.textContent, `${query} is on the page`).toContain(query);
    }
    const shown = (await counted(files, [], [], 'beta', foldLines)).filter((key) => target.querySelector(`[data-find-key="${CSS.escape(key)}"]`));
    expect(shown.length).toBeGreaterThan(3);
    expect(shown).toEqual(painted(target, 'beta'));
  });

  it('counts what paints in a log row with a clipped line', async () => {
    const lines = ['short gamma', `${'x'.repeat(40)} gamma`, '\u001b[31mgamma red\u001b[0m'];
    const fake = new FakeCILog(lines);
    const blocks = new CILogBlockCache(async (from, count) => ({ ...(await fake.fetch(from, count)), clipped: [1] }));
    blocks.setIndex(fake.version, lines.length, 0);
    const target = host();
    const app = mount(ReviewCILogBlockRow, { target, props: { blocks, from: 0, to: lines.length, linePx: 18 } });
    cleanups.push(() => { void unmount(app); });
    await waitFor(() => target.textContent!.includes('gamma red'), 'log lines');
    // The search reads each line as the view shows it, without escapes.
    const shownText = ['short gamma', `${'x'.repeat(40)} gamma`, 'gamma red'];
    for (const query of [...LOG_CHROME, 'gamma']) {
      const count = shownText.reduce((sum, line) => sum + countFindMatches(line, query), 0);
      expect(painted(target, query).length, query).toBe(count);
      expect(target.textContent, `${query} is on the page`).toContain(query);
    }
    // The note follows its line, and the next line starts on its own.
    expect(target.querySelector('[data-find-key]')!.textContent).toBe(`short gamma\n${'x'.repeat(40)} gamma … line clipped at 64 KB; Save to file has the whole line\ngamma red`);
  });
});

describe('a CI log row in a narrow pane', () => {
  // Laid out at 18 px a line (leading-[18px]); 400 characters cannot fit
  // one line of a 240 px row at any monospace width the theme picks.
  const LINE_PX = 18;

  it.each([false, true])('wraps a long line inside the row (clipped line: %s)', async (clipped) => {
    const lines = ['short', `${'y'.repeat(400)} zq`, 'after'];
    const fake = new FakeCILog(lines);
    const blocks = new CILogBlockCache(async (from, count) => ({ ...(await fake.fetch(from, count)), clipped: clipped ? [1] : [] }));
    blocks.setIndex(fake.version, lines.length, 0);
    const target = document.createElement('div');
    target.style.cssText = 'position:fixed;top:0;left:0;width:240px';
    document.body.appendChild(target);
    cleanups.push(() => target.remove());
    const app = mount(ReviewCILogBlockRow, { target, props: { blocks, from: 0, to: lines.length, linePx: LINE_PX } });
    cleanups.push(() => { void unmount(app); });
    await waitFor(() => target.textContent!.includes('after'), 'log lines');
    const row = target.querySelector<HTMLElement>('[data-find-key]')!;
    // Three lines, the long one wrapped onto several more.
    expect(row.getBoundingClientRect().height).toBeGreaterThan(LINE_PX * 6);
    expect(row.scrollWidth).toBeLessThanOrEqual(row.clientWidth);
    // The match at the end of the long line lies inside the row.
    const text = [...row.querySelectorAll('pre')].flatMap((pre) => [...pre.childNodes]).find((node) => node.textContent!.includes('zq'))!;
    const range = document.createRange();
    const at = text.textContent!.indexOf('zq');
    range.setStart(text, at);
    range.setEnd(text, at + 2);
    const box = row.getBoundingClientRect();
    expect(range.getBoundingClientRect().right).toBeLessThanOrEqual(box.right);
    // Selecting the row copies the lines as one block of text, the clip
    // note after its line, as the single <pre> did.
    const selection = window.getSelection()!;
    selection.selectAllChildren(row);
    const note = clipped ? ' … line clipped at 64 KB; Save to file has the whole line' : '';
    expect(selection.toString()).toBe(`short\n${'y'.repeat(400)} zq${note}\nafter`);
    selection.removeAllRanges();
  });
});
