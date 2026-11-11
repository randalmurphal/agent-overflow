import { render, waitFor } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import '../../../app.css';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import ReviewCommentThread from './ReviewCommentThread.svelte';
import ReviewThreadComments from './ReviewThreadComments.svelte';
import { createMarkdownFindTextCache } from '../chat/markdown/markdownFindText';
import { createFindHighlighter, type FindHighlighter } from '../../utils/findHighlights';
import { parseReviewFiles } from '../../utils/patchStore';
import { scanReviewFind, type ReviewFindHit } from '../../utils/reviewFindScan';
import type { DiffReviewComment, ReviewThread } from '../../types/models';

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

let highlighter: FindHighlighter | null = null;

beforeEach(() => {
  setBindingMock('HighlightSchemaVersion', async () => 'hv-test');
  setBindingMock('HighlightClassNames', async () => ['none']);
  document.body.innerHTML = '';
});

afterEach(() => {
  highlighter?.dispose();
  highlighter = null;
});

describe('review find over comments', () => {
  it.each(['needle', 'NEEDLE', '**', 'needle_two', 'needle summary', 'hidden needle', 'example.com'])('counts what paints for %s', async (query) => {
    const host = document.createElement('div');
    host.style.width = '700px';
    document.body.appendChild(host);
    render(ReviewCommentThread, { target: host, props: { comment: DRAFT, onUpdate: () => {}, onDelete: () => {} } });
    render(ReviewThreadComments, { target: host, props: { thread: THREAD, findKeys: true } });
    await waitFor(() => expect(host.querySelectorAll('[data-find-key^="pc:"]').length).toBe(3));

    const hits: ReviewFindHit[] = [];
    const files = parseReviewFiles(PATCH);
    try {
      await scanReviewFind({ files, drafts: [DRAFT], prThreads: [THREAD], commentText: createMarkdownFindTextCache({ embeddedHtml: true }) }, query, {
        limit: 1000, live: () => true, emit: (hit) => hits.push(hit), pause: async () => {},
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
