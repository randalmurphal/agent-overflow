// AO tool rows read as sentences with structured results, on both mock
// providers (docs/specs/agent-thread-tools.md, How calls surface in the
// timeline). One lead thread calls each kind of tool through real MCP
// calls; the spec reads its timeline: each row's collapsed header, its
// expanded result as facts, links and lines rather than JSON, the
// refusal's message and code, and the spawn and send following their
// requests until the answers arrive, by a wake or by a status call that
// collected the answer. The spawn's prompt is over the wire projection's
// budget, so its header proves the clipped opening survives
// (internal/itemwire). Screenshots of every row collapsed and expanded go to
// the test's output directory.

import { test, expect, type SeedResult } from './fixtures.js';
import {
  LEAD_TITLE,
  SPAWN_TITLE,
  TARGET_TITLE,
  driveAoToolRows,
  expandRow,
  openLead,
  shootRow,
  toolRow,
} from './ao-tool-rows-helpers.js';
import { RESULT_TOKEN_PATTERN, awaitToolAnswer, plainScenario, setScenario, threadToolsScenario } from './thread-tools-helpers.js';

for (const provider of ['claude', 'codex'] as const) {
  test(`${provider}: AO tool rows say what the call did and show its result as facts`, async ({ harness, page }) => {
    const run = await driveAoToolRows(harness, provider, `desktop-${provider}`);
    await openLead(harness, page);
    const shot = (name: string, target: import('@playwright/test').Locator) =>
      shootRow(target, test.info().outputPath(`${provider}-${name}.png`));

    // Spawn: the new thread's title, from the reply. The answer's wake
    // lands once the spawned thread answers.
    const spawn = toolRow(page, 'spawn', SPAWN_TITLE);
    await expect(spawn).toHaveCount(1);
    await expect(spawn.getByTestId('tool-call-card-ao-live-hint')).toHaveText('(answer arrived)', { timeout: 30_000 });
    await shot('spawn-collapsed', spawn);
    await expandRow(spawn);
    const spawnOutcome = spawn.getByTestId('tool-call-card-ao-outcome');
    await expect(spawnOutcome).toContainText('backgrounded');
    await expect(spawn.getByTestId('tool-call-card-ao-link')).toHaveText(SPAWN_TITLE);
    await expect(spawn.locator('[data-testid="tool-call-card-ao-result-fact"][data-fact="notify"]')).toHaveText('armed');
    // The call's note said to wait; the answer has arrived, so it is gone.
    await expect(spawn.getByTestId('tool-call-card-ao-note')).toHaveCount(0);
    // The prompt is listed whole from the stored row, folded.
    const prompt = spawn.getByTestId('tool-call-card-ao-fact').filter({ hasText: 'Write the release notes' });
    await expect(prompt.getByTestId('tool-call-card-ao-fact-text')).toHaveAttribute('data-folded', 'true');
    await expect(spawn.getByTestId('tool-call-card-output')).not.toContainText('"token"');
    await shot('spawn-expanded', spawn);

    // The spawn link opens the spawned thread in a pane; then back to the lead.
    await spawn.getByTestId('tool-call-card-ao-link').click();
    // The spawned thread opens on the lead's prompt, an agent row from it.
    const spawnPrompt = page.locator(`[data-testid="agent-message-row"][data-agent-message-kind="spawn"]`);
    await expect(spawnPrompt.getByTestId('agent-message-sender').filter({ hasText: LEAD_TITLE }).first()).toBeVisible();
    await openLead(harness, page);

    // Send: the target's title and the message's first line.
    const send = toolRow(page, 'send', `${TARGET_TITLE} · Rebase onto main`);
    await expect(send).toHaveCount(1);
    await expect(send.getByTestId('tool-call-card-ao-live-hint')).toHaveText('(answer arrived)', { timeout: 30_000 });
    await shot('send-collapsed', send);
    await expandRow(send);
    await expect(send.getByTestId('tool-call-card-ao-link')).toHaveText(TARGET_TITLE);
    await expect(send.locator('[data-testid="tool-call-card-ao-result-fact"][data-fact="notify"]')).toHaveText('armed');
    await shot('send-expanded', send);

    // Status: what was asked, then one line per request.
    const status = toolRow(page, 'status', '1 request');
    await expect(status).toHaveCount(1);
    await shot('status-collapsed', status);
    await expandRow(status);
    await expect(status.getByTestId('tool-call-card-ao-outcome')).toContainText('1 request');
    await expect(status.getByTestId('tool-call-card-ao-line')).toHaveCount(1);
    await expect(status.getByTestId('tool-call-card-ao-line-link')).toHaveText(TARGET_TITLE);
    await shot('status-expanded', status);

    // Search: the query, then a count.
    const search = toolRow(page, 'search', '"Target"');
    await expect(search).toHaveCount(1);
    await shot('search-collapsed', search);
    await expandRow(search);
    await expect(search.getByTestId('tool-call-card-ao-outcome')).toContainText(/\d+ threads?|no threads/);
    await shot('search-expanded', search);

    // Show: the thread and the window, then the read's size and a link.
    const show = toolRow(page, 'read', `${TARGET_TITLE} · last answer`);
    await expect(show).toHaveCount(1);
    await shot('show-collapsed', show);
    await expandRow(show);
    await expect(show.getByTestId('tool-call-card-ao-outcome')).toContainText('last answer');
    await expect(show.getByTestId('tool-call-card-ao-link')).toHaveText(TARGET_TITLE);
    await shot('show-expanded', show);

    // Loop: its label, cadence and next fire.
    const loop = toolRow(page, 'loop', /^board · every 10m · next /);
    await expect(loop).toHaveCount(1);
    await shot('loop-collapsed', loop);
    await expandRow(loop);
    await expect(loop.getByTestId('tool-call-card-ao-outcome')).toContainText('next');
    await shot('loop-expanded', loop);

    // The refusal: the message and its code, never the raw text.
    const refused = toolRow(page, 'read', 'thread nosuch1 · last 20 turns');
    await expect(refused).toHaveCount(1);
    await expect(refused.getByTestId('row-error-code').first()).toHaveText('thread_not_found');
    await expect(refused).toContainText('No thread matches');
    await shot('error-collapsed', refused);
    await expandRow(refused);
    await expect(refused.getByTestId('tool-call-card-ao-error')).not.toContainText('[thread_not_found]');
    await shot('error-expanded', refused);

    // Workflow: run_start names the workflow and goal; run_status the run
    // and links it.
    const start = toolRow(page, 'start', /^digest · "Weekly summary"/);
    await expect(start).toHaveCount(1);
    await shot('run-start-collapsed', start);
    await expandRow(start);
    await expect(start.getByTestId('tool-call-card-ao-link')).toHaveAttribute('data-link-kind', 'run');
    await shot('run-start-expanded', start);
    const runStatus = toolRow(page, 'status', /digest/);
    await expect(runStatus).toHaveCount(1);
    await shot('run-status-collapsed', runStatus);
    await expandRow(runStatus);
    await expect(runStatus.getByTestId('tool-call-card-ao-link')).toHaveAttribute('data-link-kind', 'run');
    await shot('run-status-expanded', runStatus);

    // The run link opens the run in the workflows overlay.
    await runStatus.getByTestId('tool-call-card-ao-link').click();
    await expect(page.getByTestId('workflow-run-detail')).toHaveAttribute('data-item-id', run.runId);
    // Escape steps back to the run list, then closes the overlay.
    await page.keyboard.press('Escape');
    await expect(page.getByTestId('workflow-run-detail')).toHaveCount(0);
    await page.keyboard.press('Escape');
    await expect(page.getByRole('dialog', { name: 'Workflows' })).toHaveCount(0);

    expect(run.spawnedThreadId).not.toBe('');
  });

  // A thread_status that collects the answer is its delivery, so no wake row
  // follows; the send row still reads the answer as arrived rather than
  // following the target thread (GetThreadRequestStates' answered).
  test(`${provider}: a send whose answer a status call collected reads answer arrived`, async ({ harness, page }) => {
    const tag = `inline-${provider}`;
    const seed = await harness.rpc<SeedResult>('HarnessSeed', {
      projects: [
        { name: `ao-rows-lead-${tag}`, repo: {}, threads: [{ title: LEAD_TITLE, provider }] },
        { name: `ao-rows-target-${tag}`, repo: {}, threads: [{ title: TARGET_TITLE, provider }] },
      ],
    });
    const [lead, target] = seed.projects;
    await setScenario(harness, target.path, plainScenario({ name: `ao-rows-target-${tag}`, provider, texts: ['Rebased and green.'] }));
    await setScenario(harness, lead.path, threadToolsScenario({
      name: `ao-rows-lead-${tag}`,
      provider,
      echoUserMessage: true,
      turns: [{
        steps: [
          { call: { tool: 'thread_send', args: { thread_id: target.threadIds[0], message: 'Rebase onto main', notify: true } } },
          { capture: { var: 'SEND', from: '${MCP_RESULT}', pattern: RESULT_TOKEN_PATTERN } },
          // Parks until the target's turn settles the request, and carries
          // the answer back in its own reply.
          { call: { tool: 'thread_status', args: { tokens: ['${SEND}'], wait_seconds: 60 }, timeoutMs: 90_000 } },
        ],
        text: 'Read the answer.',
      }],
    }));

    await harness.rpc('StartSession', lead.threadIds[0]);
    await harness.rpc('SendMessage', lead.threadIds[0], 'rebase the target', null);
    const status = await awaitToolAnswer<{ requests: { state: string; delivered?: string }[] }>(
      harness, { tool: 'thread_status', cwd: lead.path, timeoutMs: 90_000 });
    expect(status.isError, status.text).toBe(false);
    expect(status.value?.requests[0]).toMatchObject({ state: 'finished', delivered: 'inline' });

    await openLead(harness, page, 'send');
    const send = toolRow(page, 'send', `${TARGET_TITLE} · Rebase onto main`);
    await expect(send.getByTestId('tool-call-card-ao-live-hint')).toHaveText('(answer arrived)');
    await expect(page.getByText('Read the answer.', { exact: true })).toBeVisible();
    await expect(page.getByTestId('agent-message-row')).toHaveCount(0);
  });
}
