// The Agents segment's Past list and row actions
// (docs/specs/standing-agents.md, "Agents segment"), end to end on both
// mock providers.
//
// Coverage: an ordinary thread's finished workers leave the open list and
// are counted beside the word Past and listed under it, newest settled
// first, 8 and then the rest per "Show N more…", and Show less goes back to
// the first 8; a thread_ask shows while it runs, and its click
// opens its copy beside the thread with a composer that takes no message
// and no Keep; once its copy answered, the copy's pane goes and the row
// lingers, leaves and is first in Past, where its click goes to the
// answer and opens no pane and its digest carries the answer, with a
// count that moved without a reload; a stopped
// worker joins Past; Open in Agent Pane opens a past worker beside the
// thread, an archived one included, and an answered ask's row the thread
// that was asked, labeled as that, which the row names with its provider,
// open and past; Show in Sidebar gives an
// owned worker its sidebar row and goes; with nothing open the toggle
// counts nothing; failed reads of the list and of Past say so with Retry,
// which reads them; an assistant's helper offers no Show in Sidebar, open
// or past, opens beside the assistant, and once settled is listed under
// Past alone; and a worker's question routes to the ordinary thread that
// spawned it, whose row says so, and no prompt waits on the user.
import { test, expect, type SeedResult } from './fixtures.js';
import {
  advanceGate,
  awaitGate,
  awaitTurnCompleted,
  plainScenario,
  setScenario,
  threadToolsScenario,
  type ProviderName,
  type TurnStep,
} from './thread-tools-helpers.js';
import { capture } from './wake-rows-helpers.js';
import {
  ASK_ANSWER,
  ASKED,
  HELPER,
  LEAD,
  PEER,
  agentsBody,
  companionPane,
  expectAskCopyReadOnly,
  expectPastCountBesideLabel,
  failAgentReads,
  ledgerRequests,
  openAgentRow,
  openAgents,
  openPast,
  pastList,
  pastRow,
  pastToggle,
  pastTotal,
  seedAssistantWithHelper,
  seedPastWithAsk,
} from './agents-tray-helpers.js';

const WORKERS = 10;
const HELD = 'Held worker';

for (const provider of ['claude', 'codex'] as const satisfies readonly ProviderName[]) {
  test(`${provider}: settled workers and asks list under Past, page, open beside and show in the sidebar`, async ({ harness, page }) => {
    test.setTimeout(300_000);
    const tag = `agents-past-${provider}`;
    const seed = await harness.rpc<SeedResult>('HarnessSeed', {
      projects: [
        { name: `${tag}-lead`, repo: {}, threads: [{ title: LEAD, provider }] },
        { name: `${tag}-peer`, repo: {}, threads: [{ title: PEER, provider }] },
        { name: `${tag}-workers`, repo: {}, threads: [] },
        { name: `${tag}-held`, repo: {}, threads: [] },
      ],
    });
    const [leadP, peerP, workersP, heldP] = seed.projects;
    const lead = leadP.threadIds[0];
    const peer = peerP.threadIds[0];

    // The asked thread runs a turn first, so its copy has a session to be
    // cut from; the copy runs in the same workspace and holds its answer.
    await setScenario(harness, peerP.path, plainScenario({ name: `${tag}-peer`, provider, texts: ['Read the changelog.'] }));
    await harness.rpc('StartSession', peer);
    await harness.rpc('SendMessage', peer, 'read the changelog', null);
    await awaitTurnCompleted(harness, peer);
    await setScenario(harness, peerP.path, threadToolsScenario({
      name: `${tag}-peer-copy`, provider,
      turns: [{ steps: [{ gate: 'ask-hold' }], text: ASK_ANSWER }],
    }));
    await setScenario(harness, workersP.path, threadToolsScenario({
      name: `${tag}-workers`, provider, turns: [{ text: 'Done.' }], afterTurns: 'repeatLast',
    }));
    await setScenario(harness, heldP.path, threadToolsScenario({
      name: `${tag}-held`, provider, turns: [{ steps: [{ gate: 'held-hold' }], text: 'Held.' }], afterTurns: 'repeatLast',
    }));
    const spawns: TurnStep[] = Array.from({ length: WORKERS }, (_, index) => ({
      call: { tool: 'thread_spawn', args: { prompt: `Do part ${index + 1}.`, title: `Worker ${index + 1}`, project_id: workersP.projectId, provider, notify: true } },
    }));
    await setScenario(harness, leadP.path, threadToolsScenario({
      name: `${tag}-lead`, provider, echoUserMessage: true,
      turns: [
        { steps: [
          { call: { tool: 'thread_spawn', args: { prompt: 'Hold on.', title: HELD, project_id: heldP.projectId, provider, notify: true } } },
          ...spawns,
          { call: { tool: 'thread_ask', args: { thread_id: peer, question: 'Is the changelog complete?', wait_seconds: 0, notify: true } } },
        ], text: 'Started the parts.' },
        { text: 'Noted.' },
      ],
      afterTurns: 'repeatLast',
    }));
    await harness.rpc('StartSession', lead);
    await harness.rpc('SendMessage', lead, 'split the release', null);
    await awaitTurnCompleted(harness, lead);
    const requests = ledgerRequests(harness, lead);
    expect(requests.map((r) => r.kind)).toEqual(['spawn', ...Array(WORKERS).fill('spawn'), 'ask']);
    const held = requests[0];
    const workers = requests.slice(1, WORKERS + 1);
    const ask = requests[WORKERS + 1];
    await expect.poll(() => pastTotal(harness, lead), { timeout: 90_000 }).toBe(WORKERS);
    // The last worker is archived before the page opens: Past still lists
    // it and opens it, and offers no sidebar row for it.
    const archived = workers[WORKERS - 1];
    await harness.rpc('ArchiveThread', archived.targetThreadId);

    await harness.open(page);
    await openAgents(page, LEAD);
    // The held worker and the ask are open; the finished workers are Past.
    await expect(agentsBody(page).locator('[data-testid="agent-tray-row"]:not([data-past])')).toHaveCount(2, { timeout: 30_000 });
    const askRow = openAgentRow(page, ask.token);
    await expect(askRow).toHaveAttribute('data-kind', 'ask');
    await expect(askRow.getByTestId('agent-tray-row-state')).toHaveText('running');
    // The row is the thread asked, its kind the ask.
    await expect(askRow.getByTestId('agent-tray-row-title')).toHaveText(PEER);
    await expect(askRow.getByTestId('agent-tray-row-kind')).toHaveText('ask');
    await expect(askRow.getByTestId('agent-tray-row-provider')).toHaveAttribute('data-provider', provider);
    await expect(openAgentRow(page, held.token).getByTestId('agent-tray-row-state')).toHaveText('running');
    await expect(pastToggle(page)).toHaveAttribute('aria-expanded', 'false');
    await expect(agentsBody(page).getByTestId('activity-rail-agents-past-count')).toHaveText(String(WORKERS));
    await expectPastCountBesideLabel(page);
    await capture(page, `agents-past-${provider}-open-and-collapsed.png`, agentsBody(page));

    // While it runs the ask's row opens its copy, its worker, beside the
    // thread: watched, never written to, and nothing to keep.
    const askHold = await awaitGate(harness, 'ask-hold', peerP.path);
    await expect(askRow.getByTestId('agent-tray-row-open')).toHaveAttribute('aria-label', `Open the copy asking ${PEER}`);
    await askRow.getByTestId('agent-tray-row-open').click();
    const copyPane = companionPane(page);
    await expect(copyPane.getByTestId('chat-header-title')).toHaveText(`Ask: ${PEER}`, { timeout: 30_000 });
    const copyInput = copyPane.getByTestId('composer-root').locator('textarea');
    await expect(copyInput).toBeDisabled();
    await expect(copyInput).toHaveAttribute('placeholder', "An agent's question runs in this copy. It takes no messages.");
    await expect(copyPane.getByTestId('side-chat-keep')).toHaveCount(0);
    await expectAskCopyReadOnly(copyPane);
    expect((await harness.rpc<{ askCopy?: boolean }>('GetThread', ask.targetThreadId)).askCopy).toBe(true);
    await capture(page, `agents-past-${provider}-ask-copy.png`, copyPane, askRow);

    // The ask's copy answers: the row settles and lingers, Past counts it
    // without a reload, and then the row leaves the open list. The copy is
    // deleted with its pane.
    await advanceGate(harness, askHold.mockId, 'ask-hold');
    await expect(askRow).toHaveAttribute('data-settled', 'true', { timeout: 30_000 });
    await expect(agentsBody(page).getByTestId('activity-rail-agents-past-count')).toHaveText(String(WORKERS + 1));
    await expect(copyPane).toHaveCount(0, { timeout: 30_000 });
    await expect(askRow).toHaveCount(0, { timeout: 30_000 });

    // A stopped worker joins Past too.
    await openAgentRow(page, held.token).getByTestId('agent-tray-row-stop').click();
    await expect(page.getByTestId('toast-message').filter({ hasText: `Stopped ${HELD}` })).toBeVisible({ timeout: 30_000 });
    await expect(agentsBody(page).getByTestId('activity-rail-agents-past-count')).toHaveText(String(WORKERS + 2), { timeout: 30_000 });
    await expect(openAgentRow(page, held.token)).toHaveCount(0, { timeout: 30_000 });
    // Nothing is open: the toggle names the segment and counts nothing.
    await expect(page.getByTestId('activity-rail-agents-count').filter({ visible: true })).toHaveCount(0);
    await expect(page.getByTestId('activity-rail-agents-toggle').filter({ visible: true })).toHaveAttribute('aria-label', 'Agents');

    // Past opens on its newest 8, the stop and the ask first.
    const list = await openPast(page);
    const rows = list.getByTestId('agent-tray-row');
    await expect(rows).toHaveCount(8);
    await expect(rows.nth(0)).toHaveAttribute('data-token', held.token);
    await expect(rows.nth(0).getByTestId('agent-tray-row-state')).toHaveText('stopped');
    await expect(rows.nth(1)).toHaveAttribute('data-token', ask.token);
    await expect(rows.nth(1).getByTestId('agent-tray-row-state')).toHaveText(/replied|finished/);
    // Its copy is deleted: the row is still the thread asked, with its provider.
    await expect(rows.nth(1).getByTestId('agent-tray-row-title')).toHaveText(PEER);
    await expect(rows.nth(1).getByTestId('agent-tray-row-provider')).toHaveAttribute('data-provider', provider);
    await expect(rows.nth(2).getByTestId('agent-tray-row-state')).toHaveText('finished');
    await expect(list.getByTestId('agent-tray-row-stop')).toHaveCount(0);
    await expect(rows.nth(2).getByTestId('agent-tray-row-provider')).toHaveAttribute('data-provider', provider);
    const more = agentsBody(page).getByTestId('activity-rail-agents-past-more');
    const less = agentsBody(page).getByTestId('activity-rail-agents-past-less');
    await expect(more).toHaveText(`Show ${WORKERS + 2 - 8} more…`);
    await expect(less).toHaveCount(0);
    await capture(page, `agents-past-${provider}-expanded.png`, agentsBody(page));

    // The ask's copy is gone: its row's click goes to the answer, which
    // landed in this thread, and opens no pane; its digest carries it.
    const pastAsk = pastRow(page, ask.token);
    await expect(pastAsk.getByTestId('agent-tray-row-open')).toHaveAttribute('aria-label', `Show the answer from ${PEER}`);
    await pastAsk.getByTestId('agent-tray-row-open').click();
    await expect(companionPane(page)).toHaveCount(0);
    await pastAsk.getByTestId('agent-tray-row-details').click();
    await expect(pastAsk.getByTestId('agent-tray-row-digest')).toContainText(ASK_ANSWER);
    await capture(page, `agents-past-${provider}-digest.png`, pastAsk);

    await more.click();
    await expect(rows).toHaveCount(WORKERS + 2);
    await expect(more).toHaveCount(0);

    // The archived worker opens beside the thread and offers no sidebar row.
    const archivedRow = pastRow(page, archived.token);
    await expect(archivedRow.getByTestId('agent-tray-row-show-in-sidebar')).toHaveCount(0);
    await archivedRow.getByTestId('agent-tray-row-open-pane').click();
    await expect(companionPane(page).getByTestId('chat-header-title')).toHaveText(`Worker ${WORKERS}`, { timeout: 30_000 });

    // The ask's row offers the thread that was asked, as that.
    await expect(pastAsk.getByTestId('agent-tray-row-open-pane')).toHaveAttribute('aria-label', `Open ${PEER}, the thread asked, in Agent Pane`);
    await pastAsk.getByTestId('agent-tray-row-open-pane').click();
    await expect(companionPane(page).getByTestId('chat-header-title')).toHaveText(PEER, { timeout: 30_000 });

    // Show in Sidebar gives an owned worker its row, and goes.
    const first = pastRow(page, workers[0].token);
    const sidebarRow = page.getByTestId('thread-row-title').getByText('Worker 1', { exact: true });
    await expect(sidebarRow).toHaveCount(0);
    const show = first.getByTestId('agent-tray-row-show-in-sidebar');
    await expect(show).toHaveAttribute('title', 'Show in sidebar');
    await show.click();
    await expect(sidebarRow).toBeVisible({ timeout: 30_000 });
    await expect(show).toHaveCount(0);
    expect((await harness.rpc<{ placement: string }>('GetThread', workers[0].targetThreadId)).placement).toBe('standalone');

    // Show less goes back to the first page; Show more pages on from it again.
    await less.click();
    await expect(rows).toHaveCount(8);
    await expect(rows.nth(0)).toHaveAttribute('data-token', held.token);
    await expect(less).toHaveCount(0);
    await expect(more).toHaveText(`Show ${WORKERS + 2 - 8} more…`);
    await more.click();
    await expect(rows).toHaveCount(WORKERS + 2);
    await expect(less).toBeVisible();
    await expect(agentsBody(page).getByTestId('activity-rail-agents-past-count')).toHaveText(String(WORKERS + 2));

    // Collapsing drops the rows; the count stays.
    await pastToggle(page).click();
    await expect(pastList(page)).toHaveCount(0);
    await expect(agentsBody(page).getByTestId('activity-rail-agents-past-count')).toHaveText(String(WORKERS + 2));
  });

  test(`${provider}: a failed read says so with Retry, and Retry shows Past with an ask as the thread asked`, async ({ harness, page }) => {
    test.setTimeout(240_000);
    const { askToken, workerToken } = await seedPastWithAsk(harness, provider, `agents-past-fail-${provider}`);
    const reads = await failAgentReads(page);
    await harness.open(page);
    await openAgents(page, LEAD);
    const body = agentsBody(page);
    // Both reads failed: the segment is there, says so, and counts nothing.
    const error = body.getByTestId('activity-rail-agents-error');
    await expect(error.getByTestId('row-error-msg')).toContainText('Could not read agents:', { timeout: 30_000 });
    const pastError = body.getByTestId('activity-rail-agents-past-error');
    await expect(pastError.getByTestId('row-error-msg')).toContainText('Could not read past agents:');
    await expect(page.getByTestId('activity-rail-agents-count').filter({ visible: true })).toHaveCount(0);
    const retry = error.getByTestId('activity-rail-agents-error-retry');
    await expect(retry).toHaveAttribute('title', 'Retry');
    await expect(retry).toHaveAttribute('aria-label', 'Retry reading agents');
    await capture(page, `agents-past-${provider}-read-failed.png`, body);

    reads.failing = false;
    await retry.click();
    await expect(error).toHaveCount(0);
    await expect(pastError).toBeVisible();
    await pastError.getByTestId('activity-rail-agents-past-error-retry').click();
    await expect(pastError).toHaveCount(0);
    await expect(body.getByTestId('activity-rail-agents-past-count')).toHaveText('2');

    const list = await openPast(page);
    await expect(list.getByTestId('agent-tray-row')).toHaveCount(2);
    const askRow = pastRow(page, askToken);
    await expect(askRow.getByTestId('agent-tray-row-kind')).toHaveText('ask');
    await expect(askRow.getByTestId('agent-tray-row-title')).toHaveText(ASKED);
    await expect(askRow.getByTestId('agent-tray-row-provider')).toHaveAttribute('data-provider', provider);
    await askRow.getByTestId('agent-tray-row-details').click();
    await expect(askRow.getByTestId('agent-tray-row-digest')).toContainText(ASK_ANSWER);
    await expect(pastRow(page, workerToken).getByTestId('agent-tray-row-state')).toHaveText(/finished|replied/);
    await capture(page, `agents-past-${provider}-ask.png`, body);
  });

  test(`${provider}: an assistant's helper opens beside it and offers no sidebar row, open or past`, async ({ harness, page }) => {
    test.setTimeout(240_000);
    const { home, token, helper } = await seedAssistantWithHelper(harness, provider, `agents-helper-${provider}`);
    const hold = await awaitGate(harness, 'helper-hold', home);
    await harness.open(page);
    await openAgents(page, LEAD);
    const row = openAgentRow(page, token);
    await expect(row.getByTestId('agent-tray-row-title')).toHaveText(HELPER, { timeout: 30_000 });
    await expect(row.getByTestId('agent-tray-row-state')).toHaveText('running');
    await expect(row.getByTestId('agent-tray-row-show-in-sidebar')).toHaveCount(0);
    await expect(row.getByTestId('agent-tray-row-open-pane')).toBeVisible();
    await capture(page, `agents-helper-${provider}-open.png`, agentsBody(page));

    // The helper is in no catalog: Open in Agent Pane reads it, then opens it beside.
    await row.getByTestId('agent-tray-row-open-pane').click();
    await expect(companionPane(page).getByTestId('chat-header-title')).toHaveText(HELPER, { timeout: 30_000 });
    await expect(companionPane(page).locator(`[data-thread-id="${helper}"]`).first()).toBeVisible();

    // It finishes: Past lists it once, still with no sidebar row to give.
    await advanceGate(harness, hold.mockId, 'helper-hold');
    await expect(agentsBody(page).getByTestId('activity-rail-agents-past-count')).toHaveText('1', { timeout: 30_000 });
    await openPast(page);
    const past = pastRow(page, token);
    await expect(past.getByTestId('agent-tray-row-show-in-sidebar')).toHaveCount(0);
    await expect(past.getByTestId('agent-tray-row-open-pane')).toBeVisible();
    await expect(openAgentRow(page, token), 'listed above and under Past').toHaveCount(0);
    await capture(page, `agents-helper-${provider}-past.png`, agentsBody(page));
  });

  test(`${provider}: an ordinary thread's worker asks it, not the user, and its row says so`, async ({ harness, page }) => {
    test.setTimeout(180_000);
    const tag = `agents-route-${provider}`;
    const seed = await harness.rpc<SeedResult>('HarnessSeed', {
      projects: [
        { name: `${tag}-lead`, repo: {}, threads: [{ title: LEAD, provider }] },
        { name: `${tag}-worker`, repo: {}, threads: [] },
      ],
    });
    const [leadP, workerP] = seed.projects;
    const lead = leadP.threadIds[0];
    await setScenario(harness, leadP.path, threadToolsScenario({
      name: `${tag}-lead`, provider, echoUserMessage: true,
      turns: [
        { steps: [{ call: { tool: 'thread_spawn', args: { prompt: 'Ship release 4.2.', title: 'Release worker', project_id: workerP.projectId, provider, notify: true } } }], text: 'Started it.' },
        { steps: [
          { capture: { var: 'TOKEN', from: '${USER_INPUT}', pattern: 'thread_reply, token (\\S+), once' } },
          { gate: 'lead-answer' },
          { call: { tool: 'thread_reply', args: { token: '${TOKEN}', text: 'staging' } } },
        ], text: 'Told it staging.' },
        { text: 'Noted.' },
      ],
      afterTurns: 'repeatLast',
    }));
    await setScenario(harness, workerP.path, threadToolsScenario({
      name: `${tag}-worker`, provider,
      turns: [{ steps: [
        { gate: 'then-ask' },
        { approval: {
          toolName: 'AskUserQuestion',
          input: { questions: [{ id: 'target', header: 'Target', question: 'Which environment?', options: [{ label: 'staging', description: 'staging' }, { label: 'production', description: 'production' }], multiSelect: false }] },
          onAllow: [],
        } },
      ], text: 'Shipped to staging.' }],
      afterTurns: 'silent',
    }));
    await harness.rpc('StartSession', lead);
    await harness.rpc('SendMessage', lead, 'ship release 4.2', null);
    await awaitTurnCompleted(harness, lead);
    const [spawn] = ledgerRequests(harness, lead);
    const parked = await awaitGate(harness, 'then-ask', workerP.path);
    await advanceGate(harness, parked.mockId, 'then-ask');
    const answerGate = await awaitGate(harness, 'lead-answer', leadP.path);

    // The worker's question waits on its spawner, an ordinary thread.
    const pending = await harness.rpc<{ userInputs: { routedTo?: { threadId: string; title: string } }[] }>(
      'ListPendingInteractiveRequests', spawn.targetThreadId);
    expect(pending.userInputs).toHaveLength(1);
    expect(pending.userInputs[0].routedTo).toEqual({ threadId: lead, title: LEAD });

    await harness.open(page);
    await openAgents(page, LEAD);
    const row = openAgentRow(page, spawn.token);
    await expect(row.getByTestId('agent-tray-row-state')).toHaveText('blocked');
    await expect(row.getByTestId('agent-tray-row-blocked')).toHaveText(`Asks: Which environment?, waiting on ${LEAD}`);
    await capture(page, `agents-route-${provider}-blocked.png`, agentsBody(page));

    // The spawner answers; the worker runs on and finishes.
    await advanceGate(harness, answerGate.mockId, 'lead-answer');
    await expect(row).toHaveAttribute('data-settled', 'true', { timeout: 30_000 });
    expect((await harness.rpc<{ userInputs: unknown[] }>('ListPendingInteractiveRequests', spawn.targetThreadId)).userInputs).toHaveLength(0);
  });
}
