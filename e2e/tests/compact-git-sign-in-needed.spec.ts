// The sign-in marker on the compact layout, against real git and a local
// HTTP remote that answers every request 401: the header rolls the git
// actions into its menu but keeps "Sign-in needed" on screen, inside the
// header's width and at the compact hit-target height; "Git actions…"
// offers "Fetch and sign in"; and a tap on the marker, with origin
// answering again, fetches and takes the marker away.
import { expect, test } from './fixtures.js';
import { loginWall, pointOriginAtOpenRemote, seedThreadWithOrigin } from './git-sign-in-helpers.js';
import { errorToasts } from './worktree-removal-helpers.js';

test('the compact header keeps the sign-in marker on screen and a tap fetches', async ({ harness, page }) => {
  const wall = await loginWall();
  try {
    const { root, title } = await seedThreadWithOrigin(harness, 'compact-git-sign-in', wall.url);

    await harness.open(page);
    await page.getByTestId('thread-row').filter({ hasText: title }).click();
    await expect(page.getByLabel('Message Input')).toBeVisible();
    const more = page.getByTestId('chat-header-more');
    const marker = page.getByTestId('git-sign-in-needed');
    await expect(more).toBeVisible();
    await expect(marker).toHaveCount(0);

    await harness.rpc('HarnessBackgroundFetchPass');
    await expect(marker).toBeVisible();
    await expect(marker).toHaveText('Sign-in needed');
    const viewport = page.viewportSize()!;
    const markerBox = (await marker.boundingBox())!;
    const moreBox = (await more.boundingBox())!;
    expect(markerBox.height).toBeGreaterThanOrEqual(36);
    expect(markerBox.x).toBeGreaterThanOrEqual(0);
    expect(markerBox.x + markerBox.width).toBeLessThanOrEqual(moreBox.x);
    expect(moreBox.x + moreBox.width).toBeLessThanOrEqual(viewport.width);

    await more.click();
    await page.getByRole('menuitem', { name: 'Git actions…' }).click();
    const menu = page.getByRole('menu', { name: 'Git actions' });
    await expect(menu.getByRole('menuitem', { name: /Fetch and sign in/ })).toContainText('terminal prompts disabled');
    await page.keyboard.press('Escape');
    await expect(menu).toHaveCount(0);

    await pointOriginAtOpenRemote(harness, root, 'compact-git-sign-in');
    await marker.tap();
    await expect(page.getByText('Fetched origin')).toBeVisible();
    await expect(marker).toHaveCount(0);
    await expect(more).toBeVisible();
    await expect(errorToasts(page)).toHaveCount(0);
  } finally {
    await wall.close();
  }
});
