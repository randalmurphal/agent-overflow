import { render, waitFor } from '@testing-library/svelte';
import { flushSync } from 'svelte';
import { afterEach, beforeEach, describe, expect, it, onTestFinished } from 'vitest';
import '../../../app.css';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import ReviewCommentThread from './ReviewCommentThread.svelte';
import ReviewThreadComments from './ReviewThreadComments.svelte';
import ReviewPaneFind from './ReviewPaneFind.svelte';
import { getPaneFindProvider, openPaneFind, resetPaneFindForTest, setPaneFindQuery } from '../../stores/paneFind.svelte';
import type { ReviewPaneState } from '../../stores/reviewPane.svelte';
import { createMarkdownFindTextCache } from '../chat/markdown/markdownFindText';
import { createFindHighlighter, findHighlightName, type FindHighlighter, type FindRun } from '../../utils/findHighlights';
import { parseReviewFiles } from '../../utils/patchStore';
import { scanReviewFind, type ReviewFindFileSpot, type ReviewFindHit } from '../../utils/reviewFindScan';
import type { DiffReviewComment, ReviewThread } from '../../types/models';
import { installDiagnosticsCapture } from '../../../test/helpers/diagnostics';
import { rawProps } from '../../../test/helpers/rawProps.svelte';
import type { ReviewFind } from '../../stores/reviewFind.svelte';

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

function commentRuns(): (body: string) => readonly FindRun[] {
  const texts = createMarkdownFindTextCache({ embeddedHtml: true });
  return (body) => texts(body).all;
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
        diff: () => ({ findReadingPosition: () => null, revealFindPlace: () => false, scrollFindBy: () => {} }),
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

  it('reports the defect and counts the comment as written', async () => {
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
        diff: () => ({ findReadingPosition: () => null, revealFindPlace: () => false, scrollFindBy: () => {} }),
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
    const row = host.querySelector('[data-find-key="pc:p1:2"]')!;
    row.insertAdjacentHTML('beforeend', '<details open><summary>drift</summary><p>extra needle</p></details>');
    await waitFor(async () => expect(await diagnostics.messages()).toContain('review find comment details differ from their projection'));
    // Its toggles cannot be read against the projection: it counts as written.
    row.querySelector('details')!.open = true;
    await new Promise((resolve) => setTimeout(resolve, 100));
    expect([find.result.count, find.result.pending]).toEqual([before, false]);
  });
});


describe('review find\'s current comment match across a recount held open', () => {
  // A large file after the thread's keeps a recount running for many frames.
  const BIG = [
    'diff --git a/src/big.ts b/src/big.ts', '--- a/src/big.ts', '+++ b/src/big.ts', '@@ -1,400000 +1,400000 @@',
    ...Array.from({ length: 400_000 }, (_, i) => ` filler ${i}`), '',
  ].join('\n');
  const comments = [
    { authorLogin: 'a', body: 'needle first', createdAt: '', databaseID: 11 },
    { authorLogin: 'b', body: '<details><summary>sum</summary>\n\nneedle hidden\n\n</details>\n\nneedle after', createdAt: '', databaseID: 12 },
    { authorLogin: 'c', body: 'needle third', createdAt: '', databaseID: 13 },
  ];
  const thread: ReviewThread = { ...THREAD, comments };

  // The current highlight's text, and the text of the node it is in.
  const current = (key: string) => {
    const [range] = [...(CSS.highlights.get(`${findHighlightName(key)}-current`) ?? [])] as Range[];
    return range ? { text: range.toString(), node: range.startContainer.textContent ?? '', row: range.startContainer.parentElement?.closest('[data-find-key]')?.getAttribute('data-find-key') } : null;
  };

  async function setup(key: string) {
    const host = document.createElement('div');
    host.style.width = '700px';
    document.body.appendChild(host);
    const shown = render(ReviewThreadComments, { target: host, props: { thread, findKeys: true } });
    await waitFor(() => expect(host.querySelectorAll('[data-find-key^="pc:"]').length).toBe(3));
    const files = [...parseReviewFiles(PATCH), ...parseReviewFiles(BIG)];
    onTestFinished(() => { for (const file of files) file.body.segments[0].store.dispose(); });
    const review = rawProps({ drafts: [] as DiffReviewComment[], prThreads: [thread], collapsedPaths: new Set<string>(), expandedPRThreadIds: new Set<string>() });
    render(ReviewPaneFind, {
      target: document.body,
      props: {
        findKey: key, title: 'Review', review: review as unknown as ReviewPaneState, view: 'diff', files, root: host,
        diff: () => ({ findReadingPosition: () => null, revealFindPlace: () => true, scrollFindBy: () => {} }),
        log: () => undefined,
      },
    });
    const find = getPaneFindProvider(key) as ReviewFind<ReviewFindHit>;
    openPaneFind(key);
    setPaneFindQuery(key, 'needle');
    await waitFor(() => expect([find.result.count, find.result.pending]).toEqual([3, false]), { timeout: 10_000 });
    return { host, shown, review, find };
  }

  it('a deleted earlier comment leaves the current match painted in its own comment', async () => {
    const { shown, review, find } = await setup('pane-held-delete');
    find.next();
    find.next();
    await waitFor(() => expect(current('pane-held-delete')).toMatchObject({ text: 'needle', node: 'needle third' }));
    const rest: ReviewThread = { ...thread, comments: comments.slice(1) };
    review.prThreads = [rest];
    await shown.rerender({ thread: rest, findKeys: true });
    flushSync();
    for (let i = 0; i < 4; i += 1) await new Promise((resolve) => requestAnimationFrame(resolve));
    expect(find.result.pending).toBe(true);
    expect(current('pane-held-delete')).toMatchObject({ text: 'needle', node: 'needle third' });
    await waitFor(() => expect(find.result.pending).toBe(false), { timeout: 10_000 });
    expect([find.result.count, current('pane-held-delete')?.node]).toEqual([2, 'needle third']);
  });

  it('a <details> opened before the current match leaves it painted where it is', async () => {
    const { host, find } = await setup('pane-held-details');
    find.next();
    await waitFor(() => expect(current('pane-held-details')).toMatchObject({ text: 'needle', row: 'pc:p1:12' }));
    expect(current('pane-held-details')?.node).toContain('after');
    host.querySelector<HTMLElement>('[data-find-key="pc:p1:12"] summary')!.click();
    await waitFor(() => expect(find.result.pending).toBe(true));
    for (let i = 0; i < 4; i += 1) await new Promise((resolve) => requestAnimationFrame(resolve));
    expect(find.result.pending).toBe(true);
    expect(current('pane-held-details')?.node).toContain('after');
    await waitFor(() => expect(find.result.pending).toBe(false), { timeout: 10_000 });
    expect(find.result.count).toBe(4);
    expect(current('pane-held-details')?.node).toContain('after');
  });
});
