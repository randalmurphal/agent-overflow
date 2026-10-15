// A session start that replaces a live session mid-turn
// (stopExistingSessionLocked, reached by StartSession on a thread whose
// session is still running) ends the closed session's open turn as
// interrupted (triage EndReplacedSessionTurn), on both providers. The
// process that ran the turn is gone and its replacement never continues
// it, so the sidebar's Working dot clears and the turn's partial rows
// settle errored with the interrupted suffix, as a Stop leaves them. The
// mock parks the turn mid-stream (midturn-gated-helpers.ts), so the
// replacement lands at a known point.
import { test, expect } from './fixtures.js';
import { GATED_CLAUDE, GATED_CODEX, sendAndHoldMidStream, startThread, type Item } from './midturn-gated-helpers.js';

const INTERRUPTED = ' \u2014 interrupted';

const settledRow = {
  claude: ['assistant_text', 'errored', `Turn 1 first half.${INTERRUPTED}`],
  codex: ['tool_call', 'errored', `Bash: sleep 30${INTERRUPTED}`],
} as const;

for (const provider of ['claude', 'codex'] as const) {
  test(`${provider}: a session replaced mid-turn ends that turn as interrupted`, async ({ harness, page }) => {
    await harness.rpc('HarnessSetScenario', { scenario: provider === 'claude' ? GATED_CLAUDE : GATED_CODEX });
    const title = `Replace ${provider}`;
    const { threadId, driver } = await startThread(harness, { project: `replace-${provider}`, title, provider });
    if (provider === 'claude') {
      await sendAndHoldMidStream(harness, threadId, driver, 'run the long one');
    } else {
      await harness.rpc('SendMessage', threadId, 'run the long one', null);
      await driver.gate('hold');
    }

    await harness.open(page);
    const row = page.getByTestId('thread-row').filter({ hasText: title });
    await row.getByTestId('thread-row-title').click();
    await expect(row.getByTestId('thread-row-status-dot')).toHaveAttribute('aria-label', 'Working');

    await harness.rpc('StartSession', threadId);

    // An idle row has no status dot at all, so the check is that no
    // Working dot is left rather than a dot with another label.
    await expect(row.locator('[data-testid="thread-row-status-dot"][aria-label="Working"]')).toHaveCount(0, {
      timeout: 10_000,
    });
    const items = await harness.rpc<Item[]>('ListItems', threadId, true);
    expect(items.filter((i) => i.status === 'streaming' || i.status === 'running' || i.status === 'inProgress')).toEqual([]);
    expect(items.map((i) => [i.kind, i.status, i.summary])).toContainEqual([...settledRow[provider]]);
    if (provider === 'claude') {
      await expect(page.getByText(/\u2014 interrupted/)).toBeVisible();
    }
  });
}
