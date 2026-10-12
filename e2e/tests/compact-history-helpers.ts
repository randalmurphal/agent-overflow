import { expect, type Page } from '@playwright/test';

// On compact the app keeps one Back sentinel history entry while it is away
// from root, and drops it with history.back() once it is back at root, after
// the dismissed surface's exit transition
// (frontend/src/lib/utils/compactHistoryBack.svelte.ts). That traversal
// cancels a reload issued before it runs: the reload never requests a
// document and page.reload waits out the test timeout. A reload at root
// waits for the sentinel to be gone first. Off compact there is no sentinel.
export async function reloadAtRoot(page: Page): Promise<void> {
  await expect.poll(() => page.evaluate(() => (history.state as { aoBack?: unknown } | null)?.aoBack === true)).toBe(false);
  await page.reload();
}
