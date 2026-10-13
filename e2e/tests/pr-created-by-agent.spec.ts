// A PR/MR an agent creates with the forge CLI shows in the chat header as
// soon as the command settles, not when the cached "no PR" answer expires.
// The thread's branch is published with no PR/MR and the app has looked it
// up; the agent's Bash call runs `gh pr create` / `glab mr create`, the
// forge gains the PR/MR while that call runs, and the header badge appears
// well inside the 30s lookup cache. Nothing else in the turn touches the
// workspace, so no filesystem event refreshes git status.
import { expect, test } from './fixtures.js';
import {
  RESULT_LINE,
  advance,
  claudeScenario,
  startMock,
  textLines,
  toolResultLine,
  toolUseLine,
  waitForGate,
} from './agent-visibility-helpers.js';
import { BRANCH, openThread, seedPublishedBranch } from './create-pr-flow.js';
import { expectEveryForgeCallHandled, seedForge } from './forge-helpers.js';

const cases = [
  { forge: 'github', command: 'gh pr create --title "Add the feature" --body "Adds feature.md."', open: 'Open PR' },
  { forge: 'gitlab', command: 'cd "$PWD" && glab mr create --title "Add the feature" --yes', open: 'Open MR' },
] as const;

for (const { forge, command, open } of cases) {
  test(`a ${forge} PR the agent creates shows in the header when its command settles`, async ({ harness, page }) => {
    const name = `agent-pr-${forge}`;
    const { repo, threadId } = await seedPublishedBranch(harness, forge, name);
    await harness.rpc('HarnessSetScenario', {
      scenario: claudeScenario(name, [
        { emit: { lines: [toolUseLine('msg-1', 'toolu_create', 'Bash', { command })] } },
        { waitSignal: { name: 'creating' } },
        { emit: { lines: [toolResultLine('toolu_create', 'https://example.com/pull/1'), ...textLines('msg-2', 'Opened it.'), RESULT_LINE] } },
      ]),
    });
    await harness.open(page);
    await openThread(page, name);

    // The app has asked the forge and cached that the branch has no PR/MR.
    await page.getByRole('button', { name: 'More git actions' }).click();
    const menu = page.getByRole('menu', { name: 'Git actions' });
    await expect(menu.getByRole('menuitem', { name: open.replace('Open', 'Create') })).toBeEnabled({ timeout: 30_000 });
    await page.keyboard.press('Escape');
    const badge = page.getByTestId('chat-header-pr-badge');
    await expect(badge).toHaveCount(0);

    const mockId = await startMock(harness, threadId);
    const creating = waitForGate(harness, 'creating');
    const completed = harness.waitForEvent('provider:turn_completed');
    await harness.rpc('SendMessage', threadId, 'open a PR', null);
    await creating;
    await seedForge(harness, [{ ...repo, pulls: [{ number: 1, title: 'Add the feature', headRef: BRANCH }] }]);
    await advance(harness, mockId, 'creating');
    await completed;

    await expect(badge).toBeVisible({ timeout: 5_000 });
    await expect(badge).toContainText('1');
    await expectEveryForgeCallHandled(harness);
  });
}
