// ao-thread-tools across two paired computers, both of them real harness
// backends with their own stores, settings and mock providers.
//
// Coverage: the paired (grouped) shape of thread_options and thread_search;
// a spawn that runs on the other computer and wakes the caller through the
// source-side poller; an ask that forks and answers there with only the
// answer crossing; thread_status and a thread-id prefix resolved across
// computers; a destination whose own switch is off still serving a
// forwarded request; a cancel that stops work there; and forgetting a
// computer with open requests. Spec: docs/specs/agent-thread-tools.md.
import { test, expect } from '@playwright/test';
import { launchHarness, type HarnessApp, type HarnessMockEventData } from '../src/harness.js';
import { headlessPairing } from './headless-pairing-helpers.js';
import {
  FOOTER_TOKEN_PATTERN,
  RESULT_TOKEN_PATTERN,
  advanceGate,
  awaitGate,
  awaitToolAnswer,
  plainScenario,
  setScenario,
  threadRows,
  threadToolsScenario,
} from './thread-tools-helpers.js';

test.describe.configure({ mode: 'serial' });

let home: HarnessApp;
let remote: HarnessApp;
let remoteID = '';
let homeID = '';

test.beforeAll(async () => {
  home = await launchHarness();
  remote = await launchHarness();
  // Own-device enrollment: the personal pairing the thread tools require,
  // and the one that introduces the reverse connection as well.
  const pairing = await headlessPairing(remote);
  try {
    const attachment = await home.rpc<{ id: string; verificationNumber: string }>(
      'AddBackend',
      pairing.invite.url,
    );
    await pairing.confirm(attachment.verificationNumber);
    remoteID = attachment.id;
  } finally {
    pairing.close();
  }
  await home.rpc('RenameBackend', remoteID, 'GPU computer');
  await expect
    .poll(async () => (await remote.rpc<Array<{ id: string }>>('ListAgentComputers')).length)
    .toBeGreaterThan(0);
  [{ id: homeID }] = await remote.rpc<Array<{ id: string }>>('ListAgentComputers');
});

test.afterAll(async () => {
  await home?.close();
  await remote?.close();
});

interface SeededProject {
  projectId: string;
  path: string;
  threadIds: string[];
}

async function seed(
  app: HarnessApp,
  name: string,
  titles: string[],
  withHistory = false,
): Promise<SeededProject> {
  const result = await app.rpc<{ projects: SeededProject[] }>('HarnessSeed', {
    projects: [
      {
        name,
        repo: {},
        threads: titles.map((title) => ({
          title,
          provider: 'claude',
          ...(withHistory
            ? {
                turns: [
                  {
                    userText: `set up ${title}`,
                    items: [{ kind: 'assistant_text', summary: `${title} is ready.` }],
                  },
                ],
              }
            : {}),
        })),
      },
    ],
  });
  return result.projects[0];
}

test('thread_options and thread_search answer in the paired shape and reach the other computer', async () => {
  test.setTimeout(120_000);
  const caller = await seed(home, 'paired-caller', ['Paired caller']);
  const there = await seed(remote, 'paired-there', ['Kernel profiling'], true);

  await setScenario(
    home,
    caller.path,
    threadToolsScenario({
      name: 'paired-options',
      provider: 'claude',
      turns: [
        {
          steps: [
            { call: { tool: 'thread_options', args: {} } },
            { call: { tool: 'thread_search', args: { query: 'Kernel profiling' } } },
          ],
          text: 'Both computers answered.',
        },
      ],
    }),
  );
  await home.rpc('StartSession', caller.threadIds[0]);
  await home.rpc('SendMessage', caller.threadIds[0], 'what can you reach?', null);

  interface OptionsAnswer {
    computers: Array<{
      computer_id?: string;
      computer?: string;
      local?: boolean;
      reachable: boolean;
      projects: Array<{ project_id: string; project: string }>;
      providers: Array<{ provider: string }>;
    }>;
  }
  const options = await awaitToolAnswer<OptionsAnswer>(home, {
    tool: 'thread_options',
    timeoutMs: 60_000,
  });
  expect(options.isError, options.text).toBe(false);
  // The caller's own computer first, then the paired one, each with its
  // own projects and catalogs.
  expect(options.value!.computers[0].local).toBe(true);
  const paired = options.value!.computers.find((row) => row.computer_id === remoteID)!;
  expect(paired).toBeDefined();
  expect(paired.computer).toBe('GPU computer');
  expect(paired.reachable).toBe(true);
  expect(paired.projects.map((project) => project.project_id)).toContain(there.projectId);
  expect(paired.providers.length).toBeGreaterThan(0);

  interface SearchAnswer {
    computers: Array<{
      computer_id?: string;
      computer?: string;
      rows: Array<{ thread_id: string; title: string }>;
    }>;
  }
  const search = await awaitToolAnswer<SearchAnswer>(home, {
    tool: 'thread_search',
    timeoutMs: 60_000,
  });
  expect(search.isError, search.text).toBe(false);
  const remoteGroup = search.value!.computers.find((group) => group.computer_id === remoteID)!;
  expect(remoteGroup).toBeDefined();
  expect(remoteGroup.rows.map((row) => row.thread_id)).toContain(there.threadIds[0]);
});

test('a spawn on the other computer runs there and its answer wakes the caller', async () => {
  test.setTimeout(180_000);
  const caller = await seed(home, 'remote-spawn-caller', ['Remote spawn caller']);
  const there = await seed(remote, 'remote-spawn-work', []);

  await setScenario(
    remote,
    there.path,
    plainScenario({
      name: 'remote-spawn-worker',
      provider: 'claude',
      texts: ['The kernel build finished clean.'],
    }),
  );
  await setScenario(
    home,
    caller.path,
    threadToolsScenario({
      name: 'remote-spawn-caller-script',
      provider: 'claude',
      turns: [
        {
          steps: [
            {
              call: {
                tool: 'thread_spawn',
                args: {
                  prompt: 'Build the kernel and report what broke.',
                  title: 'Kernel build',
                  computer_id: remoteID,
                  project_id: there.projectId,
                  notify: true,
                },
                timeoutMs: 120_000,
              },
            },
          ],
          text: 'Started it over there.',
        },
        { text: 'Read the wake.' },
      ],
    }),
  );

  await home.rpc('StartSession', caller.threadIds[0]);
  await home.rpc('SendMessage', caller.threadIds[0], 'have the GPU computer build the kernel', null);

  interface SpawnAnswer {
    token: string;
    thread_id: string;
    computer_id?: string;
    computer?: string;
    state: string;
    outcome: string;
    notify: boolean;
  }
  const spawn = await awaitToolAnswer<SpawnAnswer>(home, {
    tool: 'thread_spawn',
    timeoutMs: 120_000,
  });
  expect(spawn.isError, spawn.text).toBe(false);
  const spawned = spawn.value!;
  expect(spawned.outcome).toBe('backgrounded');
  expect(spawned.computer_id).toBe(remoteID);
  expect(spawned.computer).toBe('GPU computer');

  // The prompt became the first user message of a thread on the OTHER
  // computer, with the footer naming the calling thread and its computer.
  const delivered = await remote.waitForEvent<HarnessMockEventData>(
    'harness:mock',
    (ev) => ev.report.kind === 'user_input' && ev.cwd === there.path,
    60_000,
  );
  expect(delivered.report.input).toContain('Build the kernel and report what broke.');
  expect(delivered.report.input).toContain('Remote spawn caller');
  expect(delivered.report.input).toContain(spawned.token);

  // The thread lives there, not here.
  const remoteRows = await threadRows(remote);
  const spawnedRow = remoteRows.find((row) => row.id === spawned.thread_id)!;
  expect(spawnedRow).toBeDefined();
  expect(spawnedRow.title).toBe('Kernel build');
  expect(spawnedRow.projectId).toBe(there.projectId);
  expect((await threadRows(home)).some((row) => row.id === spawned.thread_id)).toBe(false);

  // The source-side poller collects the settlement and wakes the caller.
  const wake = await home.waitForEvent<HarnessMockEventData>(
    'harness:mock',
    (ev) =>
      ev.report.kind === 'user_input' &&
      ev.cwd === caller.path &&
      (ev.report.input ?? '').includes(spawned.token),
    90_000,
  );
  expect(wake.report.input).toContain('The kernel build finished clean.');
  expect(wake.report.input).toContain('GPU computer');
});

test('an ask forks the thread on the other computer and only the answer crosses', async () => {
  test.setTimeout(180_000);
  const caller = await seed(home, 'remote-ask-caller', ['Remote ask caller']);
  const there = await seed(remote, 'remote-ask-target', ['Profiler run']);
  const target = there.threadIds[0];

  // The asked thread runs one real turn first, so the fork has a provider
  // session on that computer to be cut from.
  await setScenario(
    remote,
    there.path,
    plainScenario({
      name: 'remote-ask-source',
      provider: 'claude',
      texts: ['decodeChunk held 68% of the profile.'],
    }),
  );
  await remote.rpc('StartSession', target);
  await remote.rpc('SendMessage', target, 'profile the import path', null);
  await remote.waitForEvent('provider:turn_completed');

  // The fork runs in the same workspace, so replacing that rule scripts
  // the hidden copy.
  await setScenario(
    remote,
    there.path,
    threadToolsScenario({
      name: 'remote-ask-fork',
      provider: 'claude',
      turns: [
        {
          steps: [
            { capture: { var: 'TOKEN', from: '${USER_INPUT}', pattern: FOOTER_TOKEN_PATTERN } },
            {
              call: {
                tool: 'thread_reply',
                args: { token: '${TOKEN}', text: 'decodeChunk, at 68% of the profile.' },
              },
            },
          ],
          text: 'Answered from the fork.',
        },
      ],
    }),
  );
  await setScenario(
    home,
    caller.path,
    threadToolsScenario({
      name: 'remote-ask-caller-script',
      provider: 'claude',
      turns: [
        {
          steps: [
            {
              call: {
                tool: 'thread_ask',
                args: {
                  thread_id: target,
                  computer_id: remoteID,
                  question: 'What did the profiler blame?',
                  wait_seconds: 120,
                },
                timeoutMs: 180_000,
              },
            },
          ],
          text: 'Got the answer from the other computer.',
        },
      ],
    }),
  );

  await home.rpc('StartSession', caller.threadIds[0]);
  await home.rpc('SendMessage', caller.threadIds[0], 'ask the GPU computer what it profiled', null);

  interface AskAnswer {
    token: string;
    kind: string;
    thread_id: string;
    computer_id?: string;
    state: string;
    outcome: string;
    answer_kind?: string;
    answer?: string;
  }
  const ask = await awaitToolAnswer<AskAnswer>(home, { tool: 'thread_ask', timeoutMs: 150_000 });
  expect(ask.isError, ask.text).toBe(false);
  expect(ask.value!.outcome).toBe('settled');
  expect(ask.value!.state).toBe('replied');
  expect(ask.value!.answer).toContain('decodeChunk, at 68% of the profile.');
  expect(ask.value!.computer_id).toBe(remoteID);
  // The copy answered, not the thread that was asked.
  expect(ask.value!.thread_id).not.toBe(target);

  // The scratch copy is deleted there once its answer is stored, and
  // nothing of it was ever created here.
  await expect
    .poll(async () => (await threadRows(remote)).filter((row) => row.mode === 'scratch').length, {
      timeout: 30_000,
    })
    .toBe(0);
  expect((await threadRows(home)).some((row) => row.id === ask.value!.thread_id)).toBe(false);
});

test('thread_status watches a request running on the other computer, and a prefix resolves across computers', async () => {
  test.setTimeout(180_000);
  const caller = await seed(home, 'remote-status-caller', ['Remote status caller']);
  const there = await seed(remote, 'remote-status-target', ['Long remote job'], true);
  const target = there.threadIds[0];

  await setScenario(
    remote,
    there.path,
    threadToolsScenario({
      name: 'remote-status-target-script',
      provider: 'claude',
      turns: [
        {
          steps: [
            { capture: { var: 'TOKEN', from: '${USER_INPUT}', pattern: FOOTER_TOKEN_PATTERN } },
            { gate: 'hold-remote-status' },
            {
              call: {
                tool: 'thread_reply',
                args: { token: '${TOKEN}', text: 'The remote job is done.' },
              },
            },
          ],
          text: 'Finished over here.',
        },
      ],
    }),
  );
  await setScenario(
    home,
    caller.path,
    threadToolsScenario({
      name: 'remote-status-caller-script',
      provider: 'claude',
      turns: [
        {
          steps: [
            {
              call: {
                tool: 'thread_send',
                args: {
                  thread_id: target,
                  computer_id: remoteID,
                  message: 'start the long job',
                  wait_seconds: 0,
                  notify: true,
                },
                timeoutMs: 120_000,
              },
            },
            { capture: { var: 'TOKEN', from: '${MCP_RESULT}', pattern: RESULT_TOKEN_PATTERN } },
            { call: { tool: 'thread_status', args: { tokens: ['${TOKEN}'] }, timeoutMs: 60_000 } },
          ],
          text: 'It is running over there.',
        },
        {
          steps: [
            {
              call: {
                tool: 'thread_status',
                args: { tokens: ['${TOKEN}'], wait_seconds: 60 },
                timeoutMs: 120_000,
              },
            },
          ],
          text: 'It finished.',
        },
        {
          // A prefix of a thread id that exists only on the other
          // computer, with no computer_id to say so.
          steps: [
            {
              call: {
                tool: 'thread_show',
                args: { thread_id: target.slice(0, 8) },
                timeoutMs: 60_000,
              },
            },
          ],
          text: 'Read it by prefix.',
        },
      ],
    }),
  );

  await home.rpc('StartSession', caller.threadIds[0]);
  await home.rpc('SendMessage', caller.threadIds[0], 'give the GPU computer the long job', null);

  interface StatusAnswer {
    requests?: Array<{
      token: string;
      kind: string;
      state: string;
      thread_id?: string;
      computer_id?: string;
      computer?: string;
      answer_kind?: string;
      answer?: string;
    }>;
    woke_on?: string;
    timed_out?: boolean;
  }
  const running = await awaitToolAnswer<StatusAnswer>(home, {
    tool: 'thread_status',
    timeoutMs: 120_000,
  });
  expect(running.isError, running.text).toBe(false);
  const row = running.value!.requests![0];
  expect(row.thread_id).toBe(target);
  expect(row.computer_id).toBe(remoteID);
  expect(row.computer).toBe('GPU computer');
  expect(['accepted', 'running']).toContain(row.state);

  // Let the remote turn finish: the status wait on this computer ends on
  // the settlement the poller brings back.
  const gate = await awaitGate(remote, 'hold-remote-status', there.path);
  await home.rpc('SendMessage', caller.threadIds[0], 'watch it until it finishes', null);
  await advanceGate(remote, gate.mockId, 'hold-remote-status');

  const settled = await awaitToolAnswer<StatusAnswer>(home, {
    tool: 'thread_status',
    timeoutMs: 150_000,
  });
  expect(settled.isError, settled.text).toBe(false);
  const done = settled.value!.requests![0];
  expect(done.state).toBe('replied');
  expect(done.answer).toContain('The remote job is done.');
  expect(settled.value!.woke_on).toBe(done.token);

  // A bare prefix resolves across every paired computer.
  interface ShowAnswer {
    thread_id: string;
    computer_id?: string;
    computer?: string;
    title?: string;
  }
  await home.rpc('SendMessage', caller.threadIds[0], 'now read that thread by prefix', null);
  const show = await awaitToolAnswer<ShowAnswer>(home, { tool: 'thread_show', timeoutMs: 90_000 });
  expect(show.isError, show.text).toBe(false);
  expect(show.value!.thread_id).toBe(target);
  expect(show.value!.computer_id).toBe(remoteID);
  expect(show.value!.title).toBe('Long remote job');
});

test('the destination serves a forwarded request with its own thread tools switch off', async () => {
  test.setTimeout(180_000);
  const caller = await seed(home, 'remote-off-caller', ['Switch off caller']);
  const there = await seed(remote, 'remote-off-work', ['Untouched bystander']);

  // The switch means "agents on THIS computer get the tools". It is not a
  // door the paired computer comes through.
  await remote.rpc('UpdateSettings', { threadToolsEnabled: false });
  try {
    await setScenario(
      remote,
      there.path,
      plainScenario({
        name: 'remote-off-worker',
        provider: 'claude',
        texts: ['Checked the disks; nothing is failing.'],
      }),
    );
    await setScenario(
      home,
      caller.path,
      threadToolsScenario({
        name: 'remote-off-caller-script',
        provider: 'claude',
        turns: [
          {
            steps: [
              {
                call: {
                  tool: 'thread_spawn',
                  args: {
                    prompt: 'Check the disks on this machine.',
                    title: 'Disk check',
                    computer_id: remoteID,
                    project_id: there.projectId,
                    wait_seconds: 90,
                  },
                  timeoutMs: 150_000,
                },
              },
            ],
            text: 'It answered even with its own switch off.',
          },
        ],
      }),
    );

    await home.rpc('StartSession', caller.threadIds[0]);
    await home.rpc('SendMessage', caller.threadIds[0], 'have the GPU computer check its disks', null);

    interface SpawnAnswer {
      token: string;
      thread_id: string;
      state: string;
      outcome: string;
      answer_kind?: string;
      answer?: string;
    }
    const spawn = await awaitToolAnswer<SpawnAnswer>(home, {
      tool: 'thread_spawn',
      timeoutMs: 150_000,
    });
    expect(spawn.isError, spawn.text).toBe(false);
    expect(spawn.value!.outcome).toBe('settled');
    expect(spawn.value!.answer).toContain('Checked the disks; nothing is failing.');

    // The switch is what that computer's own agents get. A thread nobody
    // asked anything of has no thread tools while it is off.
    interface McpRow {
      name: string;
      disabled: boolean;
    }
    const hasTools = async (threadID: string) =>
      (await remote.rpc<McpRow[]>('ListThreadMcpServers', threadID)).some(
        (server) => server.name === 'ao-thread-tools' && !server.disabled,
      );
    expect(await hasTools(there.threadIds[0])).toBe(false);

    // The thread the request opened had them for as long as the request
    // was open, which is how it could have replied, and returns to the
    // switch's state once the request settles.
    await expect.poll(async () => await hasTools(spawn.value!.thread_id), { timeout: 30_000 })
      .toBe(false);
  } finally {
    await remote.rpc('UpdateSettings', { threadToolsEnabled: true });
  }
});

test('a cancel stops a turn the caller started on the other computer', async () => {
  test.setTimeout(180_000);
  const caller = await seed(home, 'remote-cancel-caller', ['Remote cancel caller']);
  const there = await seed(remote, 'remote-cancel-target', ['Remote cancel target'], true);
  const target = there.threadIds[0];

  await setScenario(
    remote,
    there.path,
    threadToolsScenario({
      name: 'remote-cancel-target-script',
      provider: 'claude',
      turns: [{ steps: [{ gate: 'hold-remote-cancel' }], text: 'Long remote job done.' }],
    }),
  );
  await setScenario(
    home,
    caller.path,
    threadToolsScenario({
      name: 'remote-cancel-caller-script',
      provider: 'claude',
      turns: [
        {
          steps: [
            {
              call: {
                tool: 'thread_send',
                args: {
                  thread_id: target,
                  computer_id: remoteID,
                  message: 'start the long remote job',
                  wait_seconds: 0,
                },
                timeoutMs: 120_000,
              },
            },
            { capture: { var: 'TOKEN', from: '${MCP_RESULT}', pattern: RESULT_TOKEN_PATTERN } },
          ],
          text: 'Started it.',
        },
        {
          steps: [
            { call: { tool: 'thread_cancel', args: { token: '${TOKEN}' }, timeoutMs: 120_000 } },
          ],
          text: 'Stopped it.',
        },
      ],
    }),
  );

  await home.rpc('StartSession', caller.threadIds[0]);
  await home.rpc('SendMessage', caller.threadIds[0], 'give the GPU computer the long job', null);
  await awaitGate(remote, 'hold-remote-cancel', there.path);

  const interrupted = remote.waitForEvent<HarnessMockEventData>(
    'harness:mock',
    (ev) => ev.report.kind === 'turn_interrupted' && ev.cwd === there.path,
    90_000,
  );
  await home.rpc('SendMessage', caller.threadIds[0], 'now stop it', null);

  interface CancelAnswer {
    token?: string;
    computer_id?: string;
    state: string;
    effect: string;
  }
  const cancel = await awaitToolAnswer<CancelAnswer>(home, {
    tool: 'thread_cancel',
    timeoutMs: 120_000,
  });
  expect(cancel.isError, cancel.text).toBe(false);
  expect(cancel.value!.state).toBe('cancelled');
  expect(cancel.value!.effect).toBe('turn_interrupted');
  expect(cancel.value!.computer_id).toBe(remoteID);
  await interrupted;

  // The interrupted turn never reached its own completion text there.
  const items = await remote.rpc<Array<{ summary?: string }>>('ListItems', target, true);
  expect(items.some((item) => (item.summary ?? '').includes('Long remote job done.'))).toBe(false);
});

// Last: this test forgets the pairing the whole file is built on.
test('forgetting a computer with open requests refuses once, then abandons them', async () => {
  test.setTimeout(180_000);
  const caller = await seed(home, 'remote-forget-caller', ['Forget caller']);
  const there = await seed(remote, 'remote-forget-work', []);

  // The work never finishes over there, so the request is still open when
  // the user asks to forget the computer.
  await setScenario(
    remote,
    there.path,
    threadToolsScenario({
      name: 'remote-forget-worker',
      provider: 'claude',
      turns: [{ steps: [{ gate: 'hold-forget' }], text: 'Never reached.' }],
    }),
  );
  await setScenario(
    home,
    caller.path,
    threadToolsScenario({
      name: 'remote-forget-caller-script',
      provider: 'claude',
      turns: [
        {
          steps: [
            {
              call: {
                tool: 'thread_spawn',
                args: {
                  prompt: 'Start something long over there.',
                  title: 'Open request',
                  computer_id: remoteID,
                  project_id: there.projectId,
                  notify: true,
                },
                timeoutMs: 120_000,
              },
            },
            { capture: { var: 'TOKEN', from: '${MCP_RESULT}', pattern: RESULT_TOKEN_PATTERN } },
          ],
          text: 'It is running over there.',
        },
        {
          steps: [
            { call: { tool: 'thread_status', args: { tokens: ['${TOKEN}'] }, timeoutMs: 60_000 } },
            { call: { tool: 'thread_options', args: {}, timeoutMs: 60_000 } },
          ],
          text: 'That computer is gone.',
        },
      ],
    }),
  );

  await home.rpc('StartSession', caller.threadIds[0]);
  await home.rpc('SendMessage', caller.threadIds[0], 'start something long over there', null);

  interface SpawnAnswer {
    token: string;
    thread_id: string;
    outcome: string;
  }
  const spawn = await awaitToolAnswer<SpawnAnswer>(home, {
    tool: 'thread_spawn',
    timeoutMs: 120_000,
  });
  expect(spawn.isError, spawn.text).toBe(false);
  expect(spawn.value!.outcome).toBe('backgrounded');
  await awaitGate(remote, 'hold-forget', there.path);

  // Without confirmation the removal refuses and names the open request.
  let refusal = '';
  try {
    await home.rpc('RemoveBackend', remoteID, false);
  } catch (error) {
    refusal = (error as Error).message;
  }
  expect(refusal).toContain('thread_requests_open');
  expect(refusal).toContain(spawn.value!.token);
  expect(await home.rpc<Array<{ id: string }>>('ListBackends')).toEqual(
    expect.arrayContaining([expect.objectContaining({ id: remoteID })]),
  );

  // Confirmed, the pairing goes and the requests it held settle here.
  await home.rpc('RemoveBackend', remoteID, true);
  expect(await home.rpc<Array<{ id: string }>>('ListBackends')).toHaveLength(0);

  interface StatusAnswer {
    requests?: Array<{ token: string; state: string; answer?: string; notify: boolean }>;
  }
  await home.rpc('SendMessage', caller.threadIds[0], 'what happened to that request?', null);
  const status = await awaitToolAnswer<StatusAnswer>(home, {
    tool: 'thread_status',
    timeoutMs: 90_000,
  });
  expect(status.isError, status.text).toBe(false);
  const abandoned = status.value!.requests![0];
  expect(abandoned.state).toBe('errored');
  expect(abandoned.answer).toContain('was not told');
  expect(abandoned.notify).toBe(false);

  // With nothing paired the tools are back in the single-computer shape,
  // inside the same running session.
  const options = await awaitToolAnswer(home, { tool: 'thread_options', timeoutMs: 90_000 });
  expect(options.isError, options.text).toBe(false);
  expect(options.value!).not.toHaveProperty('computers');

  // The work it started keeps running there, untold.
  expect((await threadRows(remote)).some((row) => row.id === spawn.value!.thread_id)).toBe(true);
});
