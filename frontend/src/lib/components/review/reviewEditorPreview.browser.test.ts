// In real Chromium: an editor's Preview stands where its text box stood
// and as tall, so turning it on and off does not move the
// buttons under it, for a short body and for one the box had grown to;
// the description and review summary editors, which have their own
// boxes, keep their height too.

import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import { page } from 'vitest/browser';
import '../../../app.css';
import ReviewCommentEditorShell from './ReviewCommentEditorShell.svelte';
import ReviewDescriptionEditor from './ReviewDescriptionEditor.svelte';
import ReviewSendStrip from './ReviewSendStrip.svelte';
import { createEditorPreviews } from '../../stores/reviewEditorPreview.svelte';
import { HOME_BACKEND } from '../../transport/backendKey';

let target: HTMLDivElement;
let app: ReturnType<typeof mount> | undefined;
let stopPreviews: (() => void) | undefined;

beforeEach(async () => {
  await page.viewport(1200, 800);
  target = document.createElement('div');
  target.style.cssText = 'position:fixed;top:0;left:0;width:420px';
  document.body.append(target);
});

afterEach(() => {
  if (app) unmount(app);
  app = undefined;
  stopPreviews?.();
  target.remove();
});

function buttonsTop(): number {
  return target.querySelector('[data-editor-control="cancel"]')!.getBoundingClientRect().top;
}

describe('an editor\'s Preview', () => {
  for (const [name, body] of [['a short body', 'Short.'], ['a grown body', Array.from({ length: 14 }, (_, i) => `Line ${i + 1}`).join('\n')]] as const) {
    it(`keeps the buttons where they were for ${name}`, async () => {
      const previews = createEditorPreviews(() => new Set(['editor:1']), HOME_BACKEND);
      stopPreviews = previews.dispose;
      app = mount(ReviewCommentEditorShell, {
        target,
        props: { body, onBodyChange: () => {}, primary: { label: 'Comment', run: () => {} }, onCancel: () => {}, preview: previews.control('editor:1') },
      });
      flushSync();
      await new Promise((resolve) => requestAnimationFrame(resolve));
      const before = buttonsTop();
      (target.querySelector('[data-testid="review-editor-preview-toggle"]') as HTMLButtonElement).click();
      flushSync();
      await new Promise((resolve) => requestAnimationFrame(resolve));
      expect(target.querySelector('textarea')).toBeNull();
      expect(Math.abs(buttonsTop() - before)).toBeLessThanOrEqual(1);
      (target.querySelector('[data-testid="review-editor-preview-toggle"]') as HTMLButtonElement).click();
      flushSync();
      await new Promise((resolve) => requestAnimationFrame(resolve));
      expect(target.querySelector('textarea')).not.toBeNull();
      expect(Math.abs(buttonsTop() - before)).toBeLessThanOrEqual(1);
    });
  }
});

async function frame(): Promise<void> {
  flushSync();
  await new Promise((resolve) => requestAnimationFrame(resolve));
}

async function heightAcrossToggle(toggleTestid: string): Promise<[number, number, number]> {
  await frame();
  const before = target.getBoundingClientRect().height;
  (target.querySelector(`[data-testid="${toggleTestid}"]`) as HTMLButtonElement).click();
  await frame();
  const during = target.getBoundingClientRect().height;
  (target.querySelector(`[data-testid="${toggleTestid}"]`) as HTMLButtonElement).click();
  await frame();
  return [before, during, target.getBoundingClientRect().height];
}

describe('the description and summary editors\' Preview', () => {
  it('keeps the description editor\'s height', async () => {
    const previews = createEditorPreviews(() => new Set(['description']), HOME_BACKEND);
    stopPreviews = previews.dispose;
    const manage = {
      blockedReason: null, descriptionDraft: 'A **description**.\n\nWith two paragraphs.', descriptionError: null, descriptionSaving: false,
      saveDescription: async () => {}, cancelDescription: () => {}, setDescriptionDraft: () => {},
    } as never;
    app = mount(ReviewDescriptionEditor, { target, props: { manage, preview: previews.control('description'), onClosed: () => {} } });
    const [before, during, after] = await heightAcrossToggle('review-description-preview');
    expect(target.querySelector('[data-testid="review-description-preview-body"]')).toBeNull();
    expect(Math.abs(during - before)).toBeLessThanOrEqual(1);
    expect(Math.abs(after - before)).toBeLessThanOrEqual(1);
  });

  it('keeps the review summary\'s height', async () => {
    const previews = createEditorPreviews(() => new Set(['summary']), HOME_BACKEND);
    stopPreviews = previews.dispose;
    const review = {
      drafts: [], scope: 'pr', selectedCommitSHA: null, pinned: true, commentsGranted: true, sendingComments: false,
      effectiveSubmitTarget: 'pr', isTurnActive: false, prRef: null, prDetail: null, verdict: 'comment', setVerdict: () => {},
      summaryBody: 'Looks **good**.', setSummaryBody: () => {}, editorPreviews: previews,
    } as never;
    app = mount(ReviewSendStrip, { target, props: { review } });
    const [before, during, after] = await heightAcrossToggle('review-summary-preview-toggle');
    expect(Math.abs(during - before)).toBeLessThanOrEqual(1);
    expect(Math.abs(after - before)).toBeLessThanOrEqual(1);
  });
});
