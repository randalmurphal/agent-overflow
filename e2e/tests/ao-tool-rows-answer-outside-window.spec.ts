// A send whose answer's wake row is outside the pane's loaded window, settled
// long enough ago that the Agents listing has dropped it, still reads
// "answer arrived": the row reads the request ledger, which names the
// confirmed wake row (GetThreadRequestStates' wakeItemId), so it depends on
// neither the wake being loaded nor the listing (docs/specs/agent-thread-tools.md,
// How calls surface in the timeline).
import { join } from 'node:path';
import { DatabaseSync } from 'node:sqlite';
import { randomUUID } from 'node:crypto';
import { test, expect, type SeedResult } from './fixtures.js';
import { toolRow } from './ao-tool-rows-helpers.js';

// More rows between the send and its wake than a jump's slice holds
// (SLICE_AROUND_ITEM_BUDGET), so the jump to the first message leaves the
// wake unloaded.
const FILLER_TURNS = 160;

for (const provider of ['claude', 'codex'] as const) {
  test(`${provider}: a send whose answer is outside the loaded window reads answer arrived`, async ({ harness, page }) => {
    const token = randomUUID();
    const workerId = randomUUID();
    const seed = await harness.rpc<SeedResult>('HarnessSeed', {
      projects: [{ name: `answer-outside-${provider}`, repo: {}, threads: [
        { title: 'Docs worker', provider },
        { title: 'Lead far back', provider, turns: [
          {
            userText: 'Ask the docs worker.',
            items: [{
              kind: 'tool_call', toolName: 'MCP/thread_send', summary: 'MCP/thread_send: …',
              meta: { mcp: { server: 'ao-thread-tools', tool: 'thread_send' }, input: { thread_id: workerId, message: 'Rebase onto main' },
                ao_result: { token, thread_id: workerId, title: 'Docs worker', state: 'queued', outcome: 'queued' } },
            }],
          },
          ...Array.from({ length: FILLER_TURNS }, (_, i) => ({
            userText: `Filler question ${i}`,
            items: [{ kind: 'assistant_text', summary: `Filler answer ${i}\n\n${'A detailed answer. '.repeat(40)}` }],
          })),
          {
            userText: `"Docs worker" (${workerId}) [token ${token}]: Rebased.`,
            userMeta: { origin: 'agent-thread', originThread: { threadId: workerId, title: 'Docs worker', kind: 'wake', token }, sendId: `thread-wake:${token}` },
            items: [{ kind: 'assistant_text', summary: 'The worker rebased.' }],
          },
        ] },
      ] }],
    });
    const lead = seed.projects[0].threadIds[1];
    // The request settled and its wake was confirmed two minutes ago, past
    // the listing's settled window, with the row the echo recorded
    // (wake_item_id).
    const conn = new DatabaseSync(join(harness.bootstrap.dataDir, 'agent-overflow.db'));
    try {
      conn.exec('PRAGMA busy_timeout = 5000');
      const now = Date.now() - 120_000;
      conn.prepare(`INSERT INTO thread_requests (token, caller_thread_id, kind, target_thread_id, notify, state, answer, answer_kind,
        settled_at, delivered_at, delivered_how, confirmed_at, wake_item_id, created_at, updated_at)
        VALUES (?, ?, 'send', ?, 1, 'finished', ?, 'final', ?, ?, 'queued', ?,
          (SELECT id FROM items WHERE thread_id = ? AND kind = 'user_text' AND json_extract(meta, '$.sendId') = ?), ?, ?)`)
        .run(token, lead, workerId, Buffer.from('Rebased.'), now, now, now, lead, `thread-wake:${token}`, now - 60_000, now);
    } finally {
      conn.close();
    }

    // The listing has dropped the request; the ledger names its wake row,
    // which the jump below leaves unloaded.
    const listing = await harness.rpc<{ rows: { token: string }[] }>('ListThreadAgentRows', lead);
    expect(listing.rows.find((row) => row.token === token)).toBeUndefined();
    const states = await harness.rpc<{ token: string; wakeItemId?: string }[]>('GetThreadRequestStates', lead, [token]);
    expect(states.find((state) => state.token === token)?.wakeItemId).toBeTruthy();
    await harness.open(page);
    await page.getByTestId('thread-row-title').filter({ hasText: 'Lead far back' }).first().click();
    const scroller = page.getByTestId('message-timeline-scroll');
    await expect(scroller.getByText('The worker rebased.', { exact: true })).toBeVisible();
    await page.getByTestId('message-nav-rail').getByRole('button', { name: 'Jump to first message', exact: true }).click();
    // The send folds into the first turn's activity run.
    const run = page.getByTestId('activity-run').filter({ hasText: 'Thread send' });
    await expect(run).toHaveCount(1);
    if ((await run.getAttribute('data-collapsed')) === 'true') await run.getByTestId('activity-run-header').click();
    const send = toolRow(page, 'send', /Docs worker/);
    await expect(send).toHaveCount(1);
    await expect(scroller.getByText('The worker rebased.', { exact: true })).toHaveCount(0);
    await expect(page.locator('[data-testid="agent-message-row"]')).toHaveCount(0);
    await expect(send.getByTestId('tool-call-card-ao-live-hint')).toHaveText('(answer arrived)');
  });
}
