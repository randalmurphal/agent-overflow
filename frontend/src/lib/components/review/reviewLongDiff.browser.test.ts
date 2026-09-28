// A review diff taller than the browser can lay out, in real Chromium:
// ReviewDiffBody holds a range of its rows (LongListVirtualizer), and
// every navigation and restore must still reach the right line.

import { afterEach, beforeAll, beforeEach, describe, expect, it } from 'vitest';
import { createRawSnippet, flushSync, mount, unmount } from 'svelte';
import '../../../app.css';
import ReviewDiffBody from './ReviewDiffBody.svelte';
import type { DiffReviewComment } from '../../types/models';
import type { CommentAnchor } from '../../utils/reviewRows';
import { parseReviewFiles, type ReviewFile } from '../../utils/patchStore';
import { HELD_EDGE_PX, HELD_LIMIT_PX } from '../../utils/virtual/heldRows';
import { waitFor } from '../../../test/helpers/browserFrames';
import { rawProps } from '../../../test/helpers/rawProps.svelte';
import { setBindingMock } from '../../../test/mocks/bindings-app';

// 700 files of 1000 added lines: 14M px, past the held limit.
const FILE_COUNT = 700;
const LINES = 1000;
const VIEWPORT_PX = 600;

let patch = '';
beforeAll(() => {
  const out: string[] = [];
  for (let file = 0; file < FILE_COUNT; file += 1) {
    const path = `src/f${String(file).padStart(3, '0')}.ts`;
    out.push(`diff --git a/${path} b/${path}`, 'new file mode 100644', '--- /dev/null', `+++ b/${path}`, `@@ -0,0 +1,${LINES} @@`);
    for (let line = 1; line <= LINES; line += 1) out.push(`+f${file}:${line}`);
  }
  patch = out.join('\n') + '\n';
});

beforeEach(() => {
  setBindingMock('HighlightSchemaVersion', async () => 'test-schema');
  setBindingMock('HighlightClassNames', async () => ['none']);
  setBindingMock('HighlightPatch', async () => ({ lines: [] }));
});

const mounted: { app: object; host: HTMLElement; files: ReviewFile[][] }[] = [];

afterEach(() => {
  for (const { app, host, files } of mounted.splice(0)) {
    unmount(app);
    host.remove();
    for (const set of files) set[0]?.body.segments[0]?.store.dispose();
  }
});

function readFiles(): ReviewFile[] {
  const files = parseReviewFiles(patch);
  mounted.at(-1)?.files.push(files);
  return files;
}

async function mountBody() {
  const host = document.createElement('div');
  host.style.cssText = `position:fixed;top:0;left:0;width:900px;height:${VIEWPORT_PX}px;display:flex`;
  document.body.appendChild(host);
  const entry = { app: {} as object, host, files: [] as ReviewFile[][] };
  mounted.push(entry);
  const topFiles: number[] = [];
  const props = rawProps({
    subjectId: 'subject',
    spanOwner: 'thread-1',
    scope: 'workspace',
    files: readFiles(),
    viewMode: 'stacked' as 'stacked' | 'split',
    wordWrap: false,
    collapsedPaths: new Set<string>() as ReadonlySet<string>,
    onToggleCollapsed: () => {},
    jumpToFilePath: null as string | null,
    onJumpConsumed: () => { props.jumpToFilePath = null; },
    drafts: [] as DiffReviewComment[],
    commentThread: createRawSnippet((threadKey: () => string, _anchor: () => CommentAnchor) => ({
      render: () => `<div data-testid="thread" data-thread="${threadKey()}" style="height:80px">${threadKey()}</div>`,
    })),
    jumpToRowKey: null as string | null,
    onJumpRowConsumed: () => { props.jumpToRowKey = null; },
    onTopFileChange: (fileIndex: number) => { topFiles.push(fileIndex); },
  });
  entry.app = mount(ReviewDiffBody, { target: host, props });
  const scrollEl = host.querySelector('[data-testid="review-scroll"]') as HTMLElement;
  await settle(scrollEl);
  return { props, scrollEl, topFiles };
}

async function settle(scrollEl: HTMLElement): Promise<void> {
  let last = '';
  let stable = 0;
  await waitFor(() => {
    const now = `${scrollEl.scrollTop}:${scrollEl.scrollHeight}:${scrollEl.querySelectorAll('[data-testid="review-line-block"]').length}`;
    stable = now === last ? stable + 1 : 0;
    last = now;
    return stable >= 3;
  }, 'stable review geometry');
}

/** The diff line at a viewport y: its text and its top. */
function lineAt(scrollEl: HTMLElement, y: number): { text: string; top: number } {
  const rect = scrollEl.getBoundingClientRect();
  let el = document.elementFromPoint(rect.left + 200, y) as HTMLElement | null;
  while (el && el.parentElement?.dataset.testid !== 'review-line-block') el = el.parentElement;
  if (!el) throw new Error(`no diff line at y=${y}`);
  return { text: el.textContent!.replace(/\s+/g, ' ').trim(), top: el.getBoundingClientRect().top };
}

function lineAtTop(scrollEl: HTMLElement): { text: string; top: number } {
  // Below the sticky header overlay.
  return lineAt(scrollEl, scrollEl.getBoundingClientRect().top + 60);
}

describe('a review diff past the held limit', () => {
  it('holds a range of rows and jumps to any file', async () => {
    const { props, scrollEl, topFiles } = await mountBody();
    expect(scrollEl.scrollHeight).toBeLessThanOrEqual(HELD_LIMIT_PX);
    for (const target of [650, 3, 699, 350]) {
      props.jumpToFilePath = `src/f${String(target).padStart(3, '0')}.ts`;
      await settle(scrollEl);
      expect(topFiles.at(-1)).toBe(target);
      const header = scrollEl.querySelector(`[data-testid="review-file-header"][data-path="src/f${String(target).padStart(3, '0')}.ts"]`)
        ?? [...scrollEl.querySelectorAll('[data-testid="review-file-header-path"]')].find((el) => el.textContent?.includes(`f${String(target).padStart(3, '0')}.ts`));
      expect(header, `header of file ${target}`).toBeTruthy();
      expect(lineAtTop(scrollEl).text).toContain(`f${target}:`);
    }
  });

  it('reads across a move of the held rows without moving the line being read', async () => {
    const { props, scrollEl } = await mountBody();
    props.jumpToFilePath = 'src/f350.ts';
    await settle(scrollEl);
    // Short of the end of the held rows by 100 px, then 300 px on.
    const total = scrollEl.scrollHeight;
    scrollEl.scrollTop = total - HELD_EDGE_PX - VIEWPORT_PX - 100;
    await settle(scrollEl);
    const before = scrollEl.scrollTop;
    const probe = lineAt(scrollEl, scrollEl.getBoundingClientRect().top + 360);
    scrollEl.scrollTop = before + 300;
    await settle(scrollEl);
    expect(scrollEl.scrollTop).toBeLessThan(before - 1_000_000);
    const now = lineAt(scrollEl, scrollEl.getBoundingClientRect().top + 60);
    expect(now).toEqual({ text: probe.text, top: probe.top - 300 });
  });

  it('jumps to a comment row outside the held rows and flashes it', async () => {
    const { props, scrollEl } = await mountBody();
    props.drafts = [{
      id: 'far', threadId: 'thread-1', scope: 'workspace', sourceKey: 'source', filePath: 'src/f650.ts',
      status: 'draft', newLine: 10, side: 'new', selectedText: '', body: 'far', createdAt: 1, updatedAt: 1,
    }];
    await settle(scrollEl);
    props.jumpToRowKey = 't:far';
    await settle(scrollEl);
    const thread = scrollEl.querySelector<HTMLElement>('[data-thread="far"]');
    expect(thread).not.toBeNull();
    expect(thread!.getBoundingClientRect().top).toBe(scrollEl.getBoundingClientRect().top);
    expect(thread!.parentElement!.className).toContain('bg-accent/15');
  });

  it('keeps the line being read when the files are read again', async () => {
    const { props, scrollEl } = await mountBody();
    props.jumpToFilePath = 'src/f600.ts';
    await settle(scrollEl);
    scrollEl.scrollTop += 4567;
    await settle(scrollEl);
    const before = lineAtTop(scrollEl);
    props.files = readFiles();
    await settle(scrollEl);
    expect(lineAtTop(scrollEl)).toEqual(before);
  });

  it('keeps the line being read across word wrap and back', async () => {
    const { props, scrollEl } = await mountBody();
    props.jumpToFilePath = 'src/f520.ts';
    await settle(scrollEl);
    scrollEl.scrollTop += 2345;
    await settle(scrollEl);
    const before = lineAtTop(scrollEl);
    props.wordWrap = true;
    flushSync();
    await settle(scrollEl);
    expect(lineAtTop(scrollEl).text).toBe(before.text);
    props.wordWrap = false;
    flushSync();
    await settle(scrollEl);
    expect(lineAtTop(scrollEl)).toEqual(before);
  });
});
