// A settled send whose answer came by a wake and whose late reply the
// caller then collected with thread_status: the late reply's inline delivery
// is its own (thread_requests.late_delivered_how), so the Agents row keeps
// "Show the answer" and lands on the wake row that carried the first answer
// (docs/specs/standing-agents.md, "Agents segment").
import { join } from 'node:path';
import { DatabaseSync } from 'node:sqlite';
import { randomUUID } from 'node:crypto';
import { test, expect, type SeedResult } from './fixtures.js';
import { agentRow, agentsBody, openAgents } from './agents-tray-helpers.js';
import { awaitToolAnswer, setScenario, threadToolsScenario } from './thread-tools-helpers.js';

const WORKER = 'Docs worker';
const LEAD = 'Docs lead';

for (const provider of ['claude', 'codex'] as const) {
  test(`${provider}: a late reply a status call collected keeps the row's link to the answer's wake`, async ({ harness, page }) => {
    const token = randomUUID();
    const workers = await harness.rpc<SeedResult>('HarnessSeed', {
      projects: [{ name: `late-inline-worker-${provider}`, repo: {}, threads: [{ title: WORKER, provider }] }],
    });
    const workerId = workers.projects[0].threadIds[0];
    const seed = await harness.rpc<SeedResult>('HarnessSeed', {
      projects: [{ name: `late-inline-${provider}`, repo: {}, threads: [
        { title: LEAD, provider, turns: [
          { userText: 'Ask the docs worker.', items: [{ kind: 'assistant_text', summary: 'Asked.' }] },
          {
            userText: `"${WORKER}" (${workerId}) [token ${token}]: Rebased.`,
            userMeta: { origin: 'agent-thread', originThread: { threadId: workerId, title: WORKER, kind: 'wake', token }, sendId: `thread-wake:${token}` },
            items: [{ kind: 'assistant_text', summary: 'The worker rebased.' }],
          },
        ] },
      ] }],
    });
    const project = seed.projects[0];
    const lead = project.threadIds[0];
    // The send settled just now and its answer came by the confirmed wake
    // above. The worker has since written a late reply; its wake is owed
    // (notify, which keeps a send listed) but not due for an hour, so only
    // the status call below delivers it.
    const conn = new DatabaseSync(join(harness.bootstrap.dataDir, 'agent-overflow.db'));
    try {
      conn.exec('PRAGMA busy_timeout = 5000');
      const now = Date.now();
      conn.prepare(`INSERT INTO thread_requests (token, caller_thread_id, kind, target_thread_id, target_thread_title, notify, state, answer, answer_kind,
        settled_at, delivered_at, delivered_how, confirmed_at, late_reply, late_reply_at, revision, wake_next_check, created_at, updated_at)
        VALUES (?, ?, 'send', ?, ?, 1, 'finished', ?, 'final', ?, ?, 'queued', ?, ?, ?, 2, ?, ?, ?)`)
        .run(token, lead, workerId, WORKER, Buffer.from('Rebased.'), now, now, now, Buffer.from('Also ran the tests.'), now, now + 3_600_000, now - 60_000, now);
    } finally {
      conn.close();
    }
    const listed = async () => (await harness.rpc<{ rows: { token: string; wakeItemId?: string }[] }>('ListThreadAgentRows', lead))
      .rows.find((row) => row.token === token);
    const wakeItemId = (await listed())?.wakeItemId;
    expect(wakeItemId, 'the first answer names its wake row').toBeTruthy();

    // The lead collects the late reply with thread_status: the reply is its
    // delivery.
    await setScenario(harness, project.path, threadToolsScenario({
      name: `late-inline-${provider}`,
      provider,
      echoUserMessage: true,
      turns: [{ steps: [{ call: { tool: 'thread_status', args: { tokens: [token] } } }], text: 'Read the late reply.' }],
    }));
    await harness.rpc('StartSession', lead);
    await harness.rpc('SendMessage', lead, 'check the worker', null);
    const status = await awaitToolAnswer<{ requests: { delivered?: string; answer?: string }[] }>(harness, { tool: 'thread_status', cwd: project.path });
    expect(status.isError, status.text).toBe(false);
    expect(status.value?.requests[0]).toMatchObject({ delivered: 'inline', answer: 'Also ran the tests.' });
    await expect.poll(async () => {
      const db = new DatabaseSync(join(harness.bootstrap.dataDir, 'agent-overflow.db'), { readOnly: true });
      try {
        return db.prepare('SELECT late_delivered_at IS NOT NULL AS delivered FROM thread_requests WHERE token = ?').get(token);
      } finally {
        db.close();
      }
    }).toEqual({ delivered: 1 });

    await harness.open(page);
    await openAgents(page, LEAD);
    const row = agentRow(page, token);
    const open = row.getByTestId('agent-tray-row-open');
    await expect(open).toHaveAttribute('aria-label', `Show the answer from ${WORKER}`);
    await open.click();
    const timeline = page.getByTestId('message-timeline-scroll').filter({ visible: true }).first();
    await expect(timeline.locator(`[data-item-id="${wakeItemId}"]`)).toBeInViewport({ timeout: 10_000 });
    await expect(agentsBody(page).locator(`[data-token="${token}"]`)).toHaveCount(0);
    expect((await listed())?.wakeItemId, 'the listing still names the first answer\'s wake').toBe(wakeItemId);
  });
}
