// A send whose answer's wake row is outside the pane's loaded window still
// reads "answer arrived": the Agents listing names the confirmed wake row
// (ListThreadAgentRows' wakeItemId), so the row does not depend on the
// wake being loaded (docs/specs/agent-thread-tools.md, How calls surface in
// the timeline).
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
    // The request settled and its wake was confirmed just now, inside the
    // listing's settled window.
    const conn = new DatabaseSync(join(harness.bootstrap.dataDir, 'agent-overflow.db'));
    try {
      conn.exec('PRAGMA busy_timeout = 5000');
      const now = Date.now();
      conn.prepare(`INSERT INTO thread_requests (token, caller_thread_id, kind, target_thread_id, state, answer, answer_kind,
        settled_at, delivered_at, delivered_how, confirmed_at, created_at, updated_at)
        VALUES (?, ?, 'send', ?, 'finished', ?, 'final', ?, ?, 'queued', ?, ?, ?)`)
        .run(token, lead, workerId, Buffer.from('Rebased.'), now, now, now, now - 60_000, now);
    } finally {
      conn.close();
    }

    await harness.open(page);
    await page.getByTestId('thread-row-title').filter({ hasText: 'Lead far back' }).first().click();
    const scroller = page.getByTestId('message-timeline-scroll');
    await expect(scroller.getByText('The worker rebased.', { exact: true })).toBeVisible();
    await page.getByTestId('message-nav-rail').getByRole('button', { name: 'Jump to first message', exact: true }).click();
    const send = toolRow(page, 'send', /Docs worker/);
    await expect(send).toHaveCount(1);
    await expect(scroller.getByText('The worker rebased.', { exact: true })).toHaveCount(0);
    await expect(page.locator('[data-testid="agent-message-row"]')).toHaveCount(0);
    await expect(send.getByTestId('tool-call-card-ao-live-hint')).toHaveText('(answer arrived)');
  });
}
