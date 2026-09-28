// The gate diff's expanded file in real Chromium: blocks keep the line
// pitch their estimate claims, a file of any length opens and every line
// is reachable, and long lines scroll sideways.

import { afterEach, describe, expect, it } from 'vitest';
import { mount, unmount } from 'svelte';
import '../../../app.css';
import WorkflowDiff from './WorkflowDiff.svelte';
import { parseReviewFiles, type ReviewFile } from '../../utils/patchStore';
import { HELD_LIMIT_PX } from '../../utils/virtual/heldRows';
import { waitFor } from '../../../test/helpers/browserFrames';

const mounted: { app: object; host: HTMLElement; files: ReviewFile[] }[] = [];

afterEach(() => {
  for (const { app, host, files } of mounted.splice(0)) {
    unmount(app);
    host.remove();
    files[0]?.body.segments[0]?.store.dispose();
  }
});

function added(lines: number, line: (index: number) => string = (index) => `+line ${index + 1}`): string {
  const out = ['diff --git a/big.txt b/big.txt', 'new file mode 100644', '--- /dev/null', '+++ b/big.txt', `@@ -0,0 +1,${lines} @@`];
  for (let index = 0; index < lines; index += 1) out.push(line(index));
  return out.join('\n') + '\n';
}

async function expandFile(patch: string) {
  const files = parseReviewFiles(patch);
  const host = document.createElement('div');
  host.style.cssText = 'position:fixed;top:0;left:0;width:700px';
  document.body.appendChild(host);
  const app = mount(WorkflowDiff, { target: host, props: { files, expandFirst: true } });
  mounted.push({ app, host, files });
  const box = await waitForBox(host);
  await settle(box);
  return Object.assign(box, { lineCount: files[0].body.lineCount });
}

async function waitForBox(host: HTMLElement): Promise<HTMLElement> {
  let box: HTMLElement | null = null;
  await waitFor(() => (box = host.querySelector('[data-testid="wf-diff-hunks"]')) !== null, 'expanded file');
  return box!;
}

async function settle(box: HTMLElement): Promise<void> {
  let last = '';
  let stable = 0;
  await waitFor(() => {
    const now = `${box.scrollTop}:${box.scrollHeight}:${box.textContent?.length}`;
    stable = now === last ? stable + 1 : 0;
    last = now;
    return stable >= 3;
  }, 'stable box', 600);
}

describe('an expanded gate diff file', () => {
  it('lays each block out at exactly the height it claims', async () => {
    const box = await expandFile(added(450));
    // Its four header lines, its hunk header and 450 added lines.
    const tail = box.lineCount - 400;
    expect(tail).toBe(55);
    expect(box.getBoundingClientRect().height).toBe(288);
    expect(box.scrollHeight).toBe(200 * 20 + 8 + 200 * 20 + tail * 20 + 8);
    const first = box.querySelector('pre')!;
    expect(first.getBoundingClientRect().height).toBe(4008);
    expect(first.scrollHeight).toBe(first.clientHeight);
    expect(first.textContent!.split('\n').slice(0, 6)).toEqual(['diff --git a/big.txt b/big.txt', 'new file mode 100644', '--- /dev/null', '+++ b/big.txt', '@@ -0,0 +1,450 @@', '+line 1']);
    box.scrollTop = box.scrollHeight;
    await settle(box);
    const last = [...box.querySelectorAll('pre')].at(-1)!;
    expect(last.getBoundingClientRect().height).toBe(tail * 20 + 8);
    expect(last.scrollHeight).toBe(last.clientHeight);
    expect(last.getBoundingClientRect().bottom).toBe(box.getBoundingClientRect().bottom);
    expect(last.textContent!.endsWith('+line 450')).toBe(true);
  });

  it('opens a file taller than the browser can lay out, and reaches its last line', async () => {
    const lines = 1_200_000;
    const box = await expandFile(added(lines));
    expect(box.scrollHeight).toBeLessThanOrEqual(HELD_LIMIT_PX);
    for (let step = 0; step < 20 && !box.textContent!.includes(`+line ${lines}\n`) && !box.textContent!.endsWith(`+line ${lines}`); step += 1) {
      box.scrollTop = box.scrollHeight;
      await settle(box);
    }
    const last = [...box.querySelectorAll('pre')].at(-1)!;
    expect(last.textContent!.endsWith(`+line ${lines}`)).toBe(true);
    expect(last.getBoundingClientRect().bottom).toBe(box.getBoundingClientRect().bottom);
  }, 120_000);

  it('scrolls sideways to the end of its longest line', async () => {
    const box = await expandFile(added(3, (index) => `+${String(index).repeat(2000)}`));
    await waitFor(() => box.scrollWidth > box.clientWidth + 1000, 'wide content');
    box.scrollLeft = box.scrollWidth;
    const pre = box.querySelector('pre')!;
    const range = document.createRange();
    range.selectNodeContents(pre.firstChild!);
    const rects = [...range.getClientRects()];
    const right = Math.max(...rects.map((rect) => rect.right));
    // The end of the longest line is inside the box once scrolled to.
    expect(right).toBeLessThanOrEqual(box.getBoundingClientRect().right);
    expect(right).toBeGreaterThan(box.getBoundingClientRect().left);
  });
});
