import { render, waitFor } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, onTestFinished } from 'vitest';
import '../../../app.css';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import ReviewCommentThread from './ReviewCommentThread.svelte';
import ReviewThreadComments from './ReviewThreadComments.svelte';
import ReviewPaneFind from './ReviewPaneFind.svelte';
import { getPaneFindProvider, openPaneFind, resetPaneFindForTest, setPaneFindQuery } from '../../stores/paneFind.svelte';
import type { ReviewPaneState } from '../../stores/reviewPane.svelte';
import { createMarkdownFindTextCache } from '../chat/markdown/markdownFindText';
import { createFindHighlighter, type FindHighlighter } from '../../utils/findHighlights';
import { parseReviewFiles } from '../../utils/patchStore';
import { scanReviewFind, type ReviewFindFileSpot, type ReviewFindHit } from '../../utils/reviewFindScan';
import type { DiffReviewComment, ReviewThread } from '../../types/models';
import { installDiagnosticsCapture } from '../../../test/helpers/diagnostics';

// Review find's count over comments is what its highlighter paints: for
// a draft and a PR thread's markdown comments, the scanner's matches per
// element (`data-find-key`, occurrence) are the painted matches per
// element, in the same order.

const PATCH = [
  'diff --git a/src/a.ts b/src/a.ts',
  '--- a/src/a.ts',
  '+++ b/src/a.ts',
  '@@ -0,0 +1,3 @@',
  '+one',
  '+two',
  '+three',
  '',
].join('\n');

const DRAFT: DiffReviewComment = {
  id: 'd1', threadId: 't', scope: 'pr', sourceKey: 'k', filePath: 'src/a.ts', status: 'draft',
  newLine: 2, side: 'new', selectedText: '', body: 'a **needle** in a draft\nand needle again', createdAt: 0, updatedAt: 0,
};

const THREAD: ReviewThread = {
  id: 'p1', path: 'src/a.ts', line: 3, side: 'right', isResolvable: true, isResolved: false, isOutdated: false,
  comments: [
    { authorLogin: 'priya', body: 'Rename to **nee**dle and `needle_two`.', createdAt: '', databaseID: 1 },
    { authorLogin: 'bot', body: '<details><summary>needle summary</summary>\n\nhidden needle\n\n</details>', createdAt: '', databaseID: 2 },
    { authorLogin: 'jrow', body: '| a | b |\n| --- | --- |\n| needle | [link](https://example.com/needle) |', createdAt: '', databaseID: 3 },
  ],
};

function commentRuns(): (body: string) => readonly string[] {
  const texts = createMarkdownFindTextCache({ embeddedHtml: true });
  return (body) => texts(body).runs;
}

let highlighter: FindHighlighter | null = null;

beforeEach(() => {
  setBindingMock('HighlightSchemaVersion', async () => 'hv-test');
  setBindingMock('HighlightClassNames', async () => ['none']);
  document.body.innerHTML = '';
});

afterEach(() => {
  highlighter?.dispose();
  highlighter = null;
  resetPaneFindForTest();
});

describe('review find over comments', () => {
  it.each(['needle', 'NEEDLE', '**', 'needle_two', 'needle summary', 'hidden needle', 'example.com'])('counts what paints for %s', async (query) => {
    const host = document.createElement('div');
    host.style.width = '700px';
    document.body.appendChild(host);
    render(ReviewCommentThread, { target: host, props: { comment: DRAFT, onUpdate: () => {}, onDelete: () => {} } });
    render(ReviewThreadComments, { target: host, props: { thread: THREAD, findKeys: true } });
    await waitFor(() => expect(host.querySelectorAll('[data-find-key^="pc:"]').length).toBe(3));

    // Each hit with where the scan read it.
    const hits: (ReviewFindHit & ReviewFindFileSpot)[] = [];
    const files = parseReviewFiles(PATCH);
    try {
      await scanReviewFind({ files, drafts: [DRAFT], prThreads: [THREAD], commentText: commentRuns() }, query, {
        limit: 1000, live: () => true, emit: (hit, _file, spot) => hits.push({ ...hit, ...spot }), pause: async () => {},
      });
    } finally {
      files[0].body.segments[0].store.dispose();
    }
    highlighter = createFindHighlighter({ root: host, name: 'ao-find-comments', rowSelector: '[data-find-key]', rowKeyAttribute: 'data-find-key', rowsOnly: true });
    highlighter.setQuery(query);
    const painted = highlighter.matches.map((match) => match.rowKey);
    const counted = hits.filter((hit) => hit.place.kind === 'draft' || hit.place.kind === 'thread').map((hit) => hit.rowKey);
    expect(counted).toEqual(painted);
    // Each counted occurrence names a painted range of its element.
    for (const hit of hits.filter((h) => h.place.kind !== 'line' && h.place.kind !== 'header')) {
      highlighter.setCurrent({ row: hit.rowKey, occurrence: hit.occurrence });
      expect(highlighter.currentRange(), `${hit.rowKey}#${hit.occurrence}`).not.toBeNull();
    }
  });
});

describe('review find over a draft open in its editor', () => {
  it('counts nothing of a draft while its editor is open, and recounts in place on entering and leaving it', async () => {
    const host = document.createElement('div');
    host.style.width = '700px';
    document.body.appendChild(host);
    const thread = render(ReviewCommentThread, { target: host, props: { comment: DRAFT, onUpdate: () => {}, onDelete: () => {} } });
    const files = parseReviewFiles(PATCH);
    const review = {
      drafts: [DRAFT], prThreads: [], collapsedPaths: new Set<string>(), expandedPRThreadIds: new Set<string>(),
    } as unknown as ReviewPaneState;
    render(ReviewPaneFind, {
      target: document.body,
      props: {
        findKey: 'pane-edit', title: 'Review', review, view: 'diff', files, root: host,
        diff: () => ({ findReadingPosition: () => null, revealFindHit: () => false, scrollFindBy: () => {} }),
        log: () => undefined,
      },
    });
    const find = getPaneFindProvider('pane-edit')!;
    try {
      openPaneFind('pane-edit');
      setPaneFindQuery('pane-edit', 'needle');
      await waitFor(() => expect([find.result.count, find.result.pending]).toEqual([2, false]));
      thread.getByRole('button', { name: 'Edit' }).click();
      await waitFor(() => expect(find.result.count).toBe(0));
      expect(host.querySelector('[data-find-key^="dc:"]')).toBeNull();
      // The editor's text is not on the page as text find can paint.
      const box = host.querySelector('textarea')!;
      box.value = 'needle needle needle';
      box.dispatchEvent(new Event('input', { bubbles: true }));
      await new Promise((resolve) => setTimeout(resolve, 50));
      expect(find.result.count).toBe(0);
      thread.getByRole('button', { name: 'Cancel' }).click();
      await waitFor(() => expect(find.result.count).toBe(2));
    } finally {
      files[0].body.segments[0].store.dispose();
    }
  });
});

describe('review find over a comment its projection misreads', () => {
  const diagnostics = installDiagnosticsCapture();

  it('reports the defect and counts the comment from its DOM, never nothing', async () => {
    const host = document.createElement('div');
    host.style.width = '700px';
    document.body.appendChild(host);
    render(ReviewThreadComments, { target: host, props: { thread: THREAD, findKeys: true } });
    await waitFor(() => expect(host.querySelectorAll('[data-find-key^="pc:"]').length).toBe(3));
    const review = {
      drafts: [], prThreads: [THREAD], collapsedPaths: new Set<string>(), expandedPRThreadIds: new Set<string>(),
    } as unknown as ReviewPaneState;
    const files = parseReviewFiles(PATCH);
    onTestFinished(() => files[0].body.segments[0].store.dispose());
    render(ReviewPaneFind, {
      target: document.body,
      props: {
        findKey: 'pane-drift', title: 'Review', review, view: 'diff', files, root: host,
        diff: () => ({ findReadingPosition: () => null, revealFindHit: () => false, scrollFindBy: () => {} }),
        log: () => undefined,
      },
    });
    const find = getPaneFindProvider('pane-drift')!;
    openPaneFind('pane-drift');
    setPaneFindQuery('pane-drift', 'needle');
    await waitFor(() => expect(find.result.pending).toBe(false));
    const before = find.result.count;
    expect(before).toBeGreaterThan(0);
    // The rendered comment gains a <details> its projection does not have.
    const row = host.querySelector('[data-find-key="pc:1:p1"]')!;
    row.insertAdjacentHTML('beforeend', '<details open><summary>drift</summary><p>extra needle</p></details>');
    await waitFor(() => expect(find.result.count).toBe(before + 1));
    expect(await diagnostics.messages()).toContain('review find comment details differ from their projection');
  });
});

