// ao-thread-tools across three paired computers, all of them real harness
// backends with their own stores, settings and mock providers.
//
// Coverage: `thread_search`'s `computers` filter narrowing a fan-out to one
// named computer; the Agents segment nesting a spawn two computers away (a
// spawn on the studio whose own spawn runs on the bench) and a Stop on the
// first row cascading through both; and a paired computer that is down
// contributing an `errors` row beside the computers that did answer instead
// of failing the call. Specs: docs/specs/agent-thread-tools.md ("Addressing
// threads and computers", `thread_search`), docs/specs/standing-agents.md
// ("Agents segment").
import { test, expect } from '@playwright/test';
import { launchHarness, type HarnessApp, type HarnessMockEventData } from '../src/harness.js';
import { headlessPairing } from './headless-pairing-helpers.js';
import { awaitGate, awaitToolAnswer, awaitTurnCompleted, setScenario, threadToolsScenario } from './thread-tools-helpers.js';
import { backgroundShellSteps } from './agents-tray-helpers.js';

test.describe.configure({ mode: 'serial' });

let home: HarnessApp;
let studio: HarnessApp;
// Cleared once the last test takes this computer down for good, so nothing
// after that point addresses a backend that has already been reaped.
let bench: HarnessApp | undefined;
let studioID = '';
let benchID = '';
// The bench as the studio knows it: the studio's spawns reach it too.
let studioBenchID = '';

/** Own-device enrollment, the pairing the thread tools require. */
async function pair(host: HarnessApp, peer: HarnessApp, name: string): Promise<string> {
  const pairing = await headlessPairing(peer);
  let id = '';
  try {
    const attachment = await host.rpc<{ id: string; verificationNumber: string }>(
      'AddBackend',
      pairing.invite.url,
    );
    await pairing.confirm(attachment.verificationNumber);
    id = attachment.id;
  } finally {
    pairing.close();
  }
  await host.rpc('RenameBackend', id, name);
  return id;
}

test.beforeAll(async () => {
  // Three backends and two pairing handshakes are past the 60s the hook
  // otherwise inherits from the suite timeout.
  test.setTimeout(180_000);
  home = await launchHarness();
  studio = await launchHarness();
  bench = await launchHarness();
  studioID = await pair(home, studio, 'Studio computer');
  benchID = await pair(home, bench, 'Bench computer');
  studioBenchID = await pair(studio, bench, 'Bench computer');
  expect(studioID).not.toBe(benchID);
});

// The backends live for the whole file, so an answer one test never awaited
// would otherwise be the next test's match for the same tool.
test.beforeEach(() => {
  home.clearEvents();
  studio.clearEvents();
  bench?.clearEvents();
});

test.afterAll(async () => {
  await home?.close();
  await studio?.close();
  await bench?.close();
});

interface SeededProject {
  projectId: string;
  path: string;
  threadIds: string[];
}

async function seed(app: HarnessApp, name: string, title: string): Promise<SeededProject> {
  const result = await app.rpc<{ projects: SeededProject[] }>('HarnessSeed', {
    projects: [
      {
        name,
        repo: {},
        threads: [
          {
            title,
            provider: 'claude',
            turns: [
              {
                userText: `set up ${title}`,
                items: [{ kind: 'assistant_text', summary: `${title} is ready.` }],
              },
            ],
          },
        ],
      },
    ],
  });
  return result.projects[0];
}

interface SearchAnswer {
  computers: Array<{
    computer_id?: string;
    computer?: string;
    rows: Array<{ thread_id: string; title: string; computer_id?: string }>;
  }>;
  errors?: Array<{ computer_id: string; computer: string; error: string; error_code?: string }>;
  note?: string;
}

let caller: SeededProject;
let onStudio: SeededProject;
let onBench: SeededProject;

test('computers narrows a search to the one computer it names', async () => {
  test.setTimeout(120_000);
  caller = await seed(home, 'three-caller', 'Quasar caller');
  onStudio = await seed(studio, 'three-studio', 'Quasar on studio');
  onBench = await seed(bench!, 'three-bench', 'Quasar on bench');

  await setScenario(
    home,
    caller.path,
    threadToolsScenario({
      name: 'three-search',
      provider: 'claude',
      turns: [
        {
          steps: [{ call: { tool: 'thread_search', args: { query: 'Quasar' }, timeoutMs: 60_000 } }],
          text: 'All three answered.',
        },
        {
          steps: [
            {
              call: {
                tool: 'thread_search',
                args: { query: 'Quasar', computers: [studioID] },
                timeoutMs: 60_000,
              },
            },
          ],
          text: 'Only the studio answered.',
        },
        {
          steps: [
            {
              call: {
                tool: 'thread_search',
                args: { query: 'Quasar', computers: ['local'] },
                timeoutMs: 60_000,
              },
            },
          ],
          text: 'Only this computer answered.',
        },
      ],
    }),
  );
  await home.rpc('StartSession', caller.threadIds[0]);
  await home.rpc('SendMessage', caller.threadIds[0], 'what is on every computer?', null);

  // The default fan-out covers this computer and both paired ones.
  const every = await awaitToolAnswer<SearchAnswer>(home, {
    tool: 'thread_search',
    cwd: caller.path,
    timeoutMs: 90_000,
  });
  expect(every.isError, every.text).toBe(false);
  expect(every.value!.errors ?? []).toEqual([]);
  const everyIDs = every.value!.computers.map((group) => group.computer_id ?? '');
  expect(everyIDs).toContain(studioID);
  expect(everyIDs).toContain(benchID);
  const rowsOf = (answer: SearchAnswer, computerID: string) =>
    (answer.computers.find((group) => group.computer_id === computerID)?.rows ?? []).map(
      (row) => row.thread_id,
    );
  expect(rowsOf(every.value!, studioID)).toContain(onStudio.threadIds[0]);
  expect(rowsOf(every.value!, benchID)).toContain(onBench.threadIds[0]);

  // Naming one computer drops every other group, this computer's included.
  await awaitTurnCompleted(home, caller.threadIds[0]);
  await home.rpc('SendMessage', caller.threadIds[0], 'now only the studio', null);
  const narrowed = await awaitToolAnswer<SearchAnswer>(home, {
    tool: 'thread_search',
    cwd: caller.path,
    timeoutMs: 90_000,
  });
  expect(narrowed.isError, narrowed.text).toBe(false);
  expect(narrowed.value!.errors ?? []).toEqual([]);
  expect(narrowed.value!.computers).toHaveLength(1);
  const onlyStudio = narrowed.value!.computers[0];
  expect(onlyStudio.computer_id).toBe(studioID);
  expect(onlyStudio.computer).toBe('Studio computer');
  expect(onlyStudio.rows.map((row) => row.thread_id)).toContain(onStudio.threadIds[0]);
  expect(onlyStudio.rows.every((row) => row.computer_id === studioID)).toBe(true);
  // The bench's and this computer's own hits are not in a narrowed answer.
  const narrowedIDs = narrowed.value!.computers.flatMap((group) =>
    group.rows.map((row) => row.thread_id),
  );
  expect(narrowedIDs).not.toContain(onBench.threadIds[0]);
  expect(narrowedIDs).not.toContain(caller.threadIds[0]);

  // "local" is the same filter pointed at the caller's own computer.
  await awaitTurnCompleted(home, caller.threadIds[0]);
  await home.rpc('SendMessage', caller.threadIds[0], 'now only this computer', null);
  const local = await awaitToolAnswer<SearchAnswer>(home, {
    tool: 'thread_search',
    cwd: caller.path,
    timeoutMs: 90_000,
  });
  expect(local.isError, local.text).toBe(false);
  expect(local.value!.computers).toHaveLength(1);
  expect(local.value!.computers[0].rows.map((row) => row.thread_id)).toContain(
    caller.threadIds[0],
  );
  expect(
    local.value!.computers[0].rows.map((row) => row.thread_id),
  ).not.toContain(onStudio.threadIds[0]);
  await awaitTurnCompleted(home, caller.threadIds[0]);
});

// Last: this test takes the bench computer down for good.
test('the Agents segment nests a spawn two computers away and its Stop cascades through both', async ({ page }) => {
  test.setTimeout(240_000);
  page.setDefaultTimeout(30_000);
  const lead = await seed(home, 'three-agents-lead', 'Two hop lead');
  const [child, grand] = [
    (await studio.rpc<{ projects: SeededProject[] }>('HarnessSeed', { projects: [{ name: 'three-agents-child', repo: {}, threads: [] }] })).projects[0],
    (await bench!.rpc<{ projects: SeededProject[] }>('HarnessSeed', { projects: [{ name: 'three-agents-grand', repo: {}, threads: [] }] })).projects[0],
  ];
  await studio.rpc('UpdateNewThreadDefaults', {
    projectId: child.projectId, provider: 'claude', model: 'claude-opus-4-7', runtimeMode: 'full-access',
  });
  await bench!.rpc('UpdateNewThreadDefaults', {
    projectId: grand.projectId, provider: 'claude', model: 'claude-opus-4-7', runtimeMode: 'full-access',
  });
  await setScenario(home, lead.path, threadToolsScenario({
    name: 'three-agents-lead', provider: 'claude',
    turns: [
      { steps: [{ call: {
        tool: 'thread_spawn',
        args: { prompt: 'Build it on the studio.', title: 'Studio child', computer_id: studioID, project_id: child.projectId, notify: true },
        timeoutMs: 120_000,
      } }], text: 'Started it on the studio.' },
      { text: 'Noted.' },
    ],
  }));
  // The studio's child spawns on the bench, starts a shell and holds.
  await setScenario(studio, child.path, threadToolsScenario({
    name: 'three-agents-child', provider: 'claude',
    turns: [
      { steps: [
        { call: {
          tool: 'thread_spawn',
          args: { prompt: 'Package it on the bench.', title: 'Bench grandchild', computer_id: studioBenchID, project_id: grand.projectId, notify: true },
          timeoutMs: 120_000,
        } },
        ...backgroundShellSteps('claude'),
        { gate: 'two-hop-child-hold' },
      ], text: 'Built.' },
      { text: 'Noted.' },
    ],
  }));
  await setScenario(bench!, grand.path, threadToolsScenario({
    name: 'three-agents-grand', provider: 'claude',
    turns: [{ steps: [{ gate: 'two-hop-grand-hold' }], text: 'Packaged.' }, { text: 'Noted.' }],
  }));

  await home.rpc('StartSession', lead.threadIds[0]);
  await home.rpc('SendMessage', lead.threadIds[0], 'build it two computers away', null);
  const spawn = await awaitToolAnswer<{ token: string; thread_id: string }>(home, { tool: 'thread_spawn', cwd: lead.path, timeoutMs: 120_000 });
  expect(spawn.isError, spawn.text).toBe(false);
  const nested = await awaitToolAnswer<{ token: string; thread_id: string }>(studio, { tool: 'thread_spawn', cwd: child.path, timeoutMs: 120_000 });
  expect(nested.isError, nested.text).toBe(false);
  await awaitGate(studio, 'two-hop-child-hold', child.path);
  await awaitGate(bench!, 'two-hop-grand-hold', grand.path);

  await home.open(page);
  await page.getByTestId('thread-row-title').getByText('Two hop lead', { exact: true }).click();
  await page.getByTestId('activity-rail-agents-toggle').filter({ visible: true }).click();
  const body = page.getByTestId('activity-rail-agents-body').filter({ visible: true });
  const row = (token: string) => body.locator(`[data-testid="agent-tray-row"][data-token="${token}"]`);
  const childRow = row(spawn.value!.token);
  const grandRow = row(nested.value!.token);
  await expect(childRow.getByTestId('agent-tray-row-computer')).toHaveText('on Studio computer');
  await expect(childRow.getByTestId('agent-tray-row-provider')).toHaveAttribute('data-provider', 'claude');
  await expect(grandRow.getByTestId('agent-tray-row-provider')).toHaveAttribute('data-provider', 'claude');
  await expect(grandRow).toHaveAttribute('data-depth', '1');
  await expect(grandRow.getByTestId('agent-tray-row-title')).toHaveText('Bench grandchild');
  await expect(grandRow.getByTestId('agent-tray-row-computer')).toHaveText('on Bench computer');
  await body.screenshot({ path: test.info().outputPath('agents-two-hop-nested.png'), animations: 'disabled' });

  const childInterrupted = studio.waitForEvent<HarnessMockEventData>(
    'harness:mock', (ev) => ev.report.kind === 'turn_interrupted' && ev.cwd === child.path, 90_000);
  const grandInterrupted = bench!.waitForEvent<HarnessMockEventData>(
    'harness:mock', (ev) => ev.report.kind === 'turn_interrupted' && ev.cwd === grand.path, 90_000);
  // The cascade disarms the child's own spawn there: the person stopped
  // the child's work, so the child takes no new turn for it.
  const childWoken = studio.waitForEvent<HarnessMockEventData>(
    'harness:mock',
    (ev) => ev.report.kind === 'user_input' && ev.cwd === child.path && (ev.report.input ?? '').includes(nested.value!.token),
    30_000).then(() => true, () => false);
  await childRow.getByTestId('agent-tray-row-stop').click();
  await expect(page.getByTestId('toast-message').filter({ hasText: 'Stopped Studio child' }))
    .toHaveText('Stopped Studio child: its turn, 1 background task and 1 agent of its own.', { timeout: 60_000 });
  await childInterrupted;
  await grandInterrupted;
  await expect(childRow.getByTestId('agent-tray-row-state')).toHaveText('stopped', { timeout: 60_000 });
  expect(await childWoken, 'the child took a turn for the agent the person stopped').toBe(false);
});

test('a paired computer that is down becomes an errors row beside the live rows', async () => {
  test.setTimeout(120_000);
  // Its own caller thread: a scenario rule is read when a mock starts, so
  // the thread the first test is still running would keep its old script.
  const offlineCaller = await seed(home, 'three-offline-caller', 'Quasar offline caller');
  await setScenario(
    home,
    offlineCaller.path,
    threadToolsScenario({
      name: 'three-search-offline',
      provider: 'claude',
      turns: [
        {
          steps: [{ call: { tool: 'thread_search', args: { query: 'Quasar' }, timeoutMs: 90_000 } }],
          text: 'One of them is down.',
        },
      ],
    }),
  );
  // Down for the rest of the file: the reference goes first, so afterAll
  // has nothing left to close even if this teardown throws.
  const down = bench!;
  bench = undefined;
  await down.close();
  await home.rpc('StartSession', offlineCaller.threadIds[0]);
  await home.rpc('SendMessage', offlineCaller.threadIds[0], 'search every computer again', null);

  const answer = await awaitToolAnswer<SearchAnswer>(home, {
    tool: 'thread_search',
    cwd: offlineCaller.path,
    timeoutMs: 120_000,
  });
  // The call succeeds: the computers that answered still return their rows.
  expect(answer.isError, answer.text).toBe(false);
  const groups = answer.value!.computers;
  expect(groups.map((group) => group.computer_id ?? '')).toContain(studioID);
  expect(groups.map((group) => group.computer_id ?? '')).not.toContain(benchID);
  const studioRows = groups.find((group) => group.computer_id === studioID)!.rows;
  expect(studioRows.map((row) => row.thread_id)).toContain(onStudio.threadIds[0]);
  const localRows = groups.find((group) => (group.computer_id ?? '') !== studioID)!.rows;
  expect(localRows.map((row) => row.thread_id)).toContain(offlineCaller.threadIds[0]);

  // The computer that did not answer is named, with a reason and a code.
  expect(answer.value!.errors).toHaveLength(1);
  const failure = answer.value!.errors![0];
  expect(failure.computer_id).toBe(benchID);
  expect(failure.computer).toBe('Bench computer');
  expect(failure.error_code).toBeTruthy();
  expect(failure.error).toContain('Bench computer');
  expect(answer.value!.note).toContain('did not answer');
});
