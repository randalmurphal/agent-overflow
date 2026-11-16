// The sign-in marker of a checkout whose origin wants a login, against real
// git and a local HTTP remote in this worker that answers every request 401:
// - A background fetch pass (HarnessBackgroundFetchPass; an isolated boot
//   runs no cadence) fails without asking anyone, and the open thread's
//   header shows "Sign-in needed" carrying git's diagnosis with the remote
//   URL redacted. The Git actions menu offers "Fetch and sign in".
// - A second pass inside the backoff asks the remote nothing.
// - With origin answering again (a bare remote under the data root),
//   clicking the marker fetches, toasts, and the marker and the menu row go.
import { expect, test } from './fixtures.js';
import { loginWall, pointOriginAtOpenRemote, seedThreadWithOrigin } from './git-sign-in-helpers.js';
import { errorToasts, openThread } from './worktree-removal-helpers.js';

test('a background fetch that needs a login marks the header, and a clicked fetch clears it', async ({ harness, page }) => {
  const wall = await loginWall();
  try {
    // Origin is set before the page opens: the status caches its identity.
    const { root, title } = await seedThreadWithOrigin(harness, 'git-sign-in', wall.url);

    await harness.open(page);
    await openThread(page, title);
    const marker = page.getByTestId('git-sign-in-needed');
    await expect(page.getByRole('button', { name: 'More git actions' })).toBeVisible();
    await expect(marker).toHaveCount(0);

    await harness.rpc('HarnessBackgroundFetchPass');
    await expect(marker).toBeVisible();
    await expect(marker).toHaveText('Sign-in needed');
    const hint = (await marker.getAttribute('title')) ?? '';
    expect(hint).toContain('terminal prompts disabled');
    expect(hint).not.toContain('secret-group');
    expect(hint).not.toContain('127.0.0.1');

    await page.getByRole('button', { name: 'More git actions' }).click();
    const menu = page.getByRole('menu', { name: 'Git actions' });
    await expect(menu.getByRole('menuitem', { name: /Fetch and sign in/ })).toContainText('terminal prompts disabled');
    await page.keyboard.press('Escape');
    await expect(menu).toHaveCount(0);

    // The cadence's next tick is inside the backoff: the remote hears nothing.
    const asked = wall.requests();
    expect(asked).toBeGreaterThan(0);
    await harness.rpc('HarnessBackgroundFetchPass');
    expect(wall.requests()).toBe(asked);
    await expect(marker).toBeVisible();

    await pointOriginAtOpenRemote(harness, root, 'git-sign-in');
    await marker.click();
    await expect(page.getByText('Fetched origin')).toBeVisible();
    await expect(marker).toHaveCount(0);
    await page.getByRole('button', { name: 'More git actions' }).click();
    await expect(menu.getByRole('menuitem', { name: 'Pull', exact: true })).toBeVisible();
    await expect(menu.getByRole('menuitem', { name: /Fetch and sign in/ })).toHaveCount(0);
    await expect(errorToasts(page)).toHaveCount(0);
  } finally {
    await wall.close();
  }
});
