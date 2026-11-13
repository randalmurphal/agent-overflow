// In real Chromium: a sent comment's hover actions and their in-place
// delete confirm never move the header's text, a range's lines are
// tinted on the range's half only, and a submit failure shows its whole
// sentence.

import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import { page, userEvent } from 'vitest/browser';
import '../../../app.css';
import ReviewThreadComments from './ReviewThreadComments.svelte';
import ReviewLineBlockRow from './ReviewLineBlockRow.svelte';
import ReviewPaneBanners from './ReviewPaneBanners.svelte';
import type { ReviewPaneState } from '../../stores/reviewPane.svelte';
import { createCommentWrites } from '../../stores/reviewCommentWrites.svelte';
import { HOME_BACKEND } from '../../transport/backendKey';
import { parseReviewFiles } from '../../utils/patchStore';
import { FILE_START, materializeRows } from '../../utils/patchRows';
import type { ReviewThread } from '../../types/models';
import { setBindingMock } from '../../../test/mocks/bindings-app';

const THREAD: ReviewThread = {
  id: 't1', path: 'src/a.ts', line: 3, side: 'right', isResolvable: true, isResolved: false, isOutdated: false,
  comments: [{
    id: 'node-1', authorLogin: 'octocat', authorName: 'Mona Lisa Octocat', body: 'A comment with a body.', createdAt: '2026-01-01T00:00:00Z',
    editedAt: '2026-01-02T00:00:00Z', databaseID: 1, viewerCanEdit: true, viewerCanDelete: true,
  }],
};

let target: HTMLDivElement;
let app: ReturnType<typeof mount> | undefined;

beforeEach(async () => {
  await page.viewport(1200, 800);
  target = document.createElement('div');
  target.style.cssText = 'position:fixed;top:0;left:0;width:340px';
  document.body.append(target);
  setBindingMock('HighlightSchemaVersion', async () => 'test-schema');
  setBindingMock('HighlightClassNames', async () => ['none']);
  setBindingMock('HighlightPatch', async () => ({ lines: [] }));
});

afterEach(() => {
  if (app) unmount(app);
  app = undefined;
  target.remove();
});

function textBoxes(): { left: number; right: number; top: number }[] {
  const header = target.querySelector('[data-testid="review-comment-header"]')!;
  return [...header.querySelectorAll(':scope > span')]
    .filter((span) => !span.matches('[data-testid="review-comment-actions"]') && span.textContent!.trim() !== '')
    .map((span) => {
      const box = span.getBoundingClientRect();
      return { left: box.left, right: box.right, top: box.top };
    });
}

describe('a sent comment\'s header actions', () => {
  it('never move the header text, revealed or confirming', async () => {
    const writes = createCommentWrites({
      backend: HOME_BACKEND,
      ref: () => ({ forge: 'github', host: 'github.com', namespace: 'o', repo: 'r', number: 1 }),
      entityKey: () => 'k',
      launch: () => 'l',
      updateFailing: () => false,
      withheld: () => null,
      refreshThreads: async () => {},
      showPostedComment: () => {},
    });
    const review = { sentComments: writes, sentCommentActions: true, nowWithheldReason: null };
    app = mount(ReviewThreadComments, { target, props: { thread: THREAD, review } });
    flushSync();
    const before = textBoxes();
    expect(before.length).toBeGreaterThanOrEqual(3);
    const icons = target.querySelector<HTMLElement>('[data-testid="review-comment-actions"] > span')!;
    expect(getComputedStyle(icons).opacity).toBe('0');

    // Hovering the body reveals them too: the comment is the hover group.
    await userEvent.hover(target.querySelector('[data-testid="review-thread-comment"] [data-find-key], [data-testid="review-thread-comment"] .col-start-2')!);
    await expect.poll(() => getComputedStyle(icons).opacity).toBe('1');
    expect(textBoxes()).toEqual(before);
    expect(target.querySelector('[data-testid="review-comment-edited"]')?.textContent).toBe('edited');

    await userEvent.click(target.querySelector('[data-testid="review-comment-delete"]')!);
    flushSync();
    const confirm = target.querySelector('[data-testid="review-comment-confirm"]')!;
    expect(confirm.textContent).toContain('Delete?');
    expect(textBoxes()).toEqual(before);
    // The confirm is transparent over the header: the text it would cover
    // hides, and what still shows ends before it.
    await expect.poll(() => {
      const left = confirm.getBoundingClientRect().left;
      return [...target.querySelectorAll<HTMLElement>('[data-testid="review-comment-header"] > span')]
        .filter((span) => !span.matches('[data-testid="review-comment-actions"]') && span.textContent!.trim() !== '' && getComputedStyle(span).visibility === 'visible')
        .every((span) => span.getBoundingClientRect().right <= left);
    }).toBe(true);
    const time = [...target.querySelectorAll<HTMLElement>('[data-testid="review-comment-header"] > span')].find((span) => span.textContent!.startsWith('·'))!;
    expect(getComputedStyle(time).visibility).toBe('hidden');
    writes.dispose();
  });
});

describe('a range\'s tint', () => {
  const PATCH = [
    'diff --git a/src/a.ts b/src/a.ts', 'index 1..2 100644', '--- a/src/a.ts', '+++ b/src/a.ts',
    '@@ -10,3 +10,4 @@', ' keep ten', '-gone eleven', '+new eleven', '+new twelve', ' keep twelve',
  ].join('\n');

  function tinted(): string[] {
    return [...target.querySelectorAll<HTMLElement>('[data-testid="review-line-block"] > div')]
      .filter((row) => getComputedStyle(row, '::after').backgroundColor !== 'rgba(0, 0, 0, 0)' && getComputedStyle(row, '::after').content !== 'none')
      .map((row) => row.textContent!.replace(/\s+/g, ' ').trim());
  }

  it('covers the new-half lines from the first to the last, not a deletion between them', () => {
    const file = parseReviewFiles(PATCH)[0];
    const { rows } = materializeRows(file, FILE_START, 5);
    app = mount(ReviewLineBlockRow, {
      target,
      props: { rows, file, path: file.path, spanOwner: 'row', wordWrap: false, gutterCh: 3, ranges: [{ filePath: file.path, half: 'new', from: 10, to: 12 }] },
    });
    flushSync();
    const marked = [...target.querySelectorAll('[data-review-range]')].map((row) => row.textContent!.replace(/\s+/g, ' ').trim());
    expect(marked).toEqual(['10 10 keep ten', '11 +new eleven', '12 +new twelve']);
    expect(tinted()).toEqual(marked);
  });
});

describe('the submit failure banner', () => {
  // A GitLab submit whose held reply and approval both failed: the
  // review's sentence, then the approval's, in a narrow pane.
  const SENTENCE = 'The review posted, but a held reply did not. The forge did not confirm it, so it may already be on the PR; check the PR before sending it again. '
    + 'GitLab did not publish the reply: its discussion is gone. The approval did not go through either: GitLab refused the approval because the merge request changed since it was loaded.';

  it('wraps the whole sentence, clipping none of it', () => {
    const review = { error: null, submitError: SENTENCE, conflictsError: null, prUpdateError: null, prUpdateFailure: null, prStale: false, loading: false, scope: 'pr', prRef: null } as unknown as ReviewPaneState;
    app = mount(ReviewPaneBanners, { target, props: { review, branchesError: null } });
    flushSync();
    const banner = target.querySelector<HTMLElement>('[data-testid="review-submit-error"]')!;
    expect(banner.textContent!.trim()).toBe(SENTENCE);
    expect(banner.getBoundingClientRect().height).toBeGreaterThan(60);
    expect(banner.scrollWidth).toBeLessThanOrEqual(banner.clientWidth);
    expect(banner.scrollHeight).toBeLessThanOrEqual(banner.clientHeight);
  });
});
