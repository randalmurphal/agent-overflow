import { test, expect } from './fixtures.js';

export function notificationRetryFlow(): void {
  test('a failed notification stays visible and retryable without clipping', async ({ harness, page }) => {
    await harness.open(page);
    await harness.rpc('HarnessNotify', 'Ready', 'Open a missing conversation', {
      kind: 'thread', threadId: 'missing-notification-target',
    });
    const failure = page.getByTestId('toast').filter({ hasText: 'Could not open notification.' });
    await expect(failure).toHaveCount(1);
    await expect(failure.getByRole('button', { name: 'Try again' })).toBeVisible();
    const first = await failure.textContent();
    const original = await failure.elementHandle();
    await failure.getByRole('button', { name: 'Try again' }).click();
    // Retrying replaces the prior failure, including while its exit animation
    // runs. Waiting for one settled toast catches accidental accumulation.
    await expect.poll(() => original!.evaluate((element) => element.isConnected)).toBe(false);
    await expect(failure).toHaveCount(1);
    await expect(failure).toHaveText(first!);
    const box = await failure.boundingBox();
    expect(box).not.toBeNull();
    expect(box!.x).toBeGreaterThanOrEqual(0);
    expect(box!.x + box!.width).toBeLessThanOrEqual(page.viewportSize()!.width);
    await failure.getByRole('button', { name: 'Dismiss Notification' }).click();
    await expect(failure).toHaveCount(0);
  });
}
