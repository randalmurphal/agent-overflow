import type { Page } from '@playwright/test';

/** How long a press on selected text is held before it drags on macOS:
 * past Chromium's text-drag delay there (150 ms), which the browser times
 * from the press event itself. */
const MAC_TEXT_DRAG_HOLD_MS = 300;

/**
 * Press the primary button to drag the selected text under the pointer.
 * macOS starts a text drag only from a press held past its delay; a move
 * before that starts a new selection. Elsewhere a press on selected text
 * drags at once, so nothing is held. The hold is the gesture's own length,
 * which no page clock or event can stand in for, not a wait for the app.
 * It is the one timed hold the gate allows (src/wait-scan.ts WAIT_ALLOWED);
 * a page's waitForTimeout throws (tests/base-test.ts).
 */
export async function pressToDragText(page: Page): Promise<void> {
  await page.mouse.down();
  if (process.platform === 'darwin') await new Promise((resolve) => setTimeout(resolve, MAC_TEXT_DRAG_HOLD_MS));
}
