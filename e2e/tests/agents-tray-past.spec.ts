// The Agents segment's Past list and row actions
// (docs/specs/standing-agents.md, "Agents segment"), end to end on both
// mock providers.
//
// Coverage: an ordinary thread's finished workers leave the open list and
// are counted and listed under Past, newest settled first, 8 and then the
// rest per "Show N more…"; a thread_ask shows while it runs and, once its
// copy answered, lingers, leaves and is first in Past with its answer in
// the digest and a count that moved without a reload; a stopped worker
// joins Past; Open in Agent Pane opens a past worker beside the thread, an
// archived one included, and an ask's row the thread that was asked; Show
// in Sidebar gives an owned worker its sidebar row and goes; and a worker's
// question routes to the ordinary thread that spawned it, whose row says
// so, and no prompt waits on the user.
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
  LEAD,
  PEER,
  agentsBody,
  companionPane,
  ledgerRequests,
  openAgentRow,
  openAgents,
  openPast,
  pastList,
  pastRow,
  pastToggle,
} from './agents-tray-helpers.js';

const WORKERS = 10;
const HELD = 'Held worker';
const ASK_ANSWER = 'The changelog covers every merged change.';

interface PastPage {
  rows: { token: string }[];
  total: number;
}

function pastTotal(harness: { rpc<T>(method: string, ...args: unknown[]): Promise<T> }, thread: string): Promise<number> {
  return harness.rpc<PastPage>('ListThreadPastAgentRows', thread, { settledAt: 0, token: '' }, 1).then((page) => page.total);
}

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
    await expect(askRow.getByTestId('agent-tray-row-title')).toHaveText(`Ask: ${PEER}`);
    await expect(openAgentRow(page, held.token).getByTestId('agent-tray-row-state')).toHaveText('running');
    await expect(pastToggle(page)).toHaveAttribute('aria-expanded', 'false');
    await expect(agentsBody(page).getByTestId('activity-rail-agents-past-count')).toHaveText(String(WORKERS));
    await capture(page, `agents-past-${provider}-open-and-collapsed.png`, agentsBody(page));

    // The ask's copy answers: the row settles and lingers, Past counts it
    // without a reload, and then the row leaves the open list.
    const askHold = await awaitGate(harness, 'ask-hold', peerP.path);
    await advanceGate(harness, askHold.mockId, 'ask-hold');
    await expect(askRow).toHaveAttribute('data-settled', 'true', { timeout: 30_000 });
    await expect(agentsBody(page).getByTestId('activity-rail-agents-past-count')).toHaveText(String(WORKERS + 1));
    await expect(askRow).toHaveCount(0, { timeout: 30_000 });

    // A stopped worker joins Past too.
    await openAgentRow(page, held.token).getByTestId('agent-tray-row-stop').click();
    await expect(page.getByTestId('toast-message').filter({ hasText: `Stopped ${HELD}` })).toBeVisible({ timeout: 30_000 });
    await expect(agentsBody(page).getByTestId('activity-rail-agents-past-count')).toHaveText(String(WORKERS + 2), { timeout: 30_000 });
    await expect(openAgentRow(page, held.token)).toHaveCount(0, { timeout: 30_000 });
    await expect(page.getByTestId('activity-rail-agents-count').filter({ visible: true })).toHaveText('0');

    // Past opens on its newest 8, the stop and the ask first.
    const list = await openPast(page);
    const rows = list.getByTestId('agent-tray-row');
    await expect(rows).toHaveCount(8);
    await expect(rows.nth(0)).toHaveAttribute('data-token', held.token);
    await expect(rows.nth(0).getByTestId('agent-tray-row-state')).toHaveText('stopped');
    await expect(rows.nth(1)).toHaveAttribute('data-token', ask.token);
    await expect(rows.nth(1).getByTestId('agent-tray-row-state')).toHaveText(/replied|finished/);
    await expect(rows.nth(2).getByTestId('agent-tray-row-state')).toHaveText('finished');
    await expect(list.getByTestId('agent-tray-row-stop')).toHaveCount(0);
    await expect(rows.nth(2).getByTestId('agent-tray-row-provider')).toHaveAttribute('data-provider', provider);
    const more = agentsBody(page).getByTestId('activity-rail-agents-past-more');
    await expect(more).toHaveText(`Show ${WORKERS + 2 - 8} more…`);
    await capture(page, `agents-past-${provider}-expanded.png`, agentsBody(page));

    // The ask's digest carries its answer.
    const pastAsk = pastRow(page, ask.token);
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

    // The ask's row opens the thread that was asked: its copy is gone.
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

    // Collapsing drops the rows; the count stays.
    await pastToggle(page).click();
    await expect(pastList(page)).toHaveCount(0);
    await expect(agentsBody(page).getByTestId('activity-rail-agents-past-count')).toHaveText(String(WORKERS + 2));
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
