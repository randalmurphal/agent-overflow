// ao-thread-tools across two paired computers, both of them real harness
// backends with their own stores, settings and mock providers.
//
// Coverage: the paired (grouped) shape of thread_options and thread_search,
// with each computer's per-model efforts and a spawn refused for a model
// the destination lacks; a spawn that runs on the other computer and wakes
// the caller through the source-side poller; an ask that forks and answers
// there with only the answer crossing, and a write inside that fork refused
// there; thread_status and a thread-id prefix resolved across computers; a thread moved between the two resolving to
// its new owner; a 38k-item thread over there windowed, paged and exported
// to a path here; a multi-megabyte tool output over there read by query,
// range and negative offset; the origin chip naming the other computer on
// both ends; a destination whose own switch is off still serving a
// forwarded request and replying through thread_reply; a cancel that stops
// work there; a deleted caller stopping the scratch ask it left there; a
// destination too old to carry the tools; a reply written while the calling
// computer was off arriving once it is back; and forgetting a computer with
// open requests.
// Spec: docs/specs/agent-thread-tools.md.
import { readFile } from 'node:fs/promises';
import { randomUUID } from 'node:crypto';
import { test, expect } from '@playwright/test';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { launchHarness, type HarnessApp, type HarnessMockEventData } from '../src/harness.js';
import { headlessPairing } from './headless-pairing-helpers.js';
import type { ProviderOptionRow } from './thread-tools-helpers.js';
import {
  FOOTER_TOKEN_PATTERN,
  RESULT_TOKEN_PATTERN,
  SHOW_CURSOR_PATTERN,
  advanceGate,
  awaitGate,
  awaitToolAnswer,
  awaitTurnCompleted,
  bigItemFixture,
  bigThreadFixture,
  expectEffortsPerModel,
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
// The calling computer is launched on a data directory this file owns, so a
// test can stop it and start it again on the same state: an agent whose
// computer was off when its answer was written is a case only a restart of
// the same computer can show.
let homeRoot = '';
let homeDataDir = '';

test.beforeAll(async () => {
  homeRoot = await mkdtemp(join(tmpdir(), 'ao-thread-tools-home-'));
  homeDataDir = join(homeRoot, 'state');
  home = await launchHarness({ dataDir: homeDataDir });
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

// The two backends live for the whole file, so an answer one test never
// awaited would otherwise be the next test's match for the same tool.
test.beforeEach(() => {
  home.clearEvents();
  remote.clearEvents();
});

test.afterAll(async () => {
  try {
    await home?.close();
    await remote?.close();
  } finally {
    if (homeRoot) await rm(homeRoot, { recursive: true, force: true });
  }
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
            // A model the destination does not have is refused by the
            // destination, with the models the destination does have.
            {
              call: {
                tool: 'thread_spawn',
                args: {
                  prompt: 'try this',
                  computer_id: remoteID,
                  project_id: there.projectId,
                  provider: 'codex',
                  model: 'gpt-9000-imaginary',
                },
                timeoutMs: 60_000,
              },
            },
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
      providers: ProviderOptionRow[];
      runtime_modes?: Array<{ runtime_mode: string; meaning?: string }>;
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

  // Every model of both providers states the efforts it offers, on the
  // caller's own computer and on the paired one, which is what a spawn
  // picks an effort from. Each row carries that computer's runtime modes
  // with what each one means.
  for (const row of [options.value!.computers[0], paired]) {
    expectEffortsPerModel(row.providers);
    expect(row.runtime_modes?.map((mode) => mode.runtime_mode)).toEqual(
      expect.arrayContaining(['read-only', 'approval-required', 'full-access']),
    );
    expect(row.runtime_modes?.every((mode) => (mode.meaning ?? '') !== '')).toBe(true);
  }

  // The destination refuses a model it does not offer and answers with
  // the ones it does, so discovery is never a second round trip.
  const codex = paired.providers.find((row) => row.provider === 'codex')!;
  const refused = await awaitToolAnswer(home, { tool: 'thread_spawn', timeoutMs: 60_000 });
  expect(refused.isError).toBe(true);
  expect(refused.text).toContain('does not offer model "gpt-9000-imaginary"');
  for (const model of codex.models) expect(refused.text).toContain(model.model);
  // A spawn refused on its model created nothing over there.
  expect(await threadRows(remote)).toHaveLength(1);
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

test('a write inside the ask fork on the other computer is refused there and only the answer crosses', async () => {
  test.setTimeout(180_000);
  const caller = await seed(home, 'remote-refuse-caller', ['Remote refusal caller']);
  // full-access is the contrast: the fork is read-only whatever the thread
  // it was cut from may do, and the mode is the destination computer's to
  // impose on a thread the caller cannot see.
  const seeded = await remote.rpc<{ projects: SeededProject[] }>('HarnessSeed', {
    projects: [
      {
        name: 'remote-refuse-target',
        repo: {},
        threads: [{ title: 'Remote migration work', provider: 'claude', runtimeMode: 'full-access' }],
      },
    ],
  });
  const there = seeded.projects[0];
  const target = there.threadIds[0];
  const notePath = `${there.path}/rollback.md`;

  // Turn one settles so the fork has a session there to cut from; turn two
  // parks, so the ask forks a thread that is mid-turn on that computer.
  await setScenario(
    remote,
    there.path,
    threadToolsScenario({
      name: 'remote-refuse-target-script',
      provider: 'claude',
      turns: [
        { text: 'The migration renames two columns.' },
        { steps: [{ gate: 'remote-target-busy' }], text: 'Finished the long job.' },
      ],
    }),
  );
  await remote.rpc('StartSession', target);
  await remote.rpc('SendMessage', target, 'describe the migration', null);
  // This file keeps one backend for every test, so the wait names the
  // thread: an earlier test's completion is still in the event log.
  await remote.waitForEvent<{ threadId: string }>(
    'provider:turn_completed',
    (ev) => ev.threadId === target,
    60_000,
  );
  await remote.rpc('SendMessage', target, 'now run the long job', null);
  const targetMock = await awaitGate(remote, 'remote-target-busy', there.path);

  await setScenario(
    remote,
    there.path,
    threadToolsScenario({
      name: 'remote-refuse-fork',
      provider: 'claude',
      turns: [
        {
          steps: [
            { capture: { var: 'TOKEN', from: '${USER_INPUT}', pattern: FOOTER_TOKEN_PATTERN } },
            {
              emitLines: [
                JSON.stringify({
                  type: 'assistant',
                  message: {
                    id: 'msg-remote-fork-write',
                    role: 'assistant',
                    model: 'claude-mock-1',
                    content: [
                      {
                        type: 'tool_use',
                        id: 'tu-remotewrite',
                        name: 'Write',
                        input: { file_path: notePath, content: 'rollback plan' },
                      },
                    ],
                  },
                }),
                // What a read-only session returns for a stripped tool: a
                // pre-ask refusal, so nothing waits on a person who is not
                // even at this computer.
                JSON.stringify({
                  type: 'system',
                  subtype: 'permission_denied',
                  tool_name: 'Write',
                  tool_use_id: 'tu-remotewrite',
                  decision_reason_type: 'mode',
                  decision_reason: 'Write is not available in a read-only session',
                  message: 'Permission to use Write has been denied.',
                  uuid: 'pd-tu-remotewrite',
                }),
                JSON.stringify({
                  type: 'user',
                  message: {
                    role: 'user',
                    content: [
                      {
                        type: 'tool_result',
                        tool_use_id: 'tu-remotewrite',
                        is_error: true,
                        content: 'Permission to use Write has been denied.',
                      },
                    ],
                  },
                }),
              ],
            },
            { gate: 'remote-fork-refused' },
            {
              call: {
                tool: 'thread_reply',
                args: {
                  token: '${TOKEN}',
                  text: 'It renames two columns. I could not write the rollback note: Write was denied in this read-only copy.',
                },
              },
            },
          ],
          text: 'Answered without writing.',
        },
      ],
    }),
  );
  await setScenario(
    home,
    caller.path,
    threadToolsScenario({
      name: 'remote-refuse-caller-script',
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
                  question: 'what does the migration do, and write me a rollback note',
                  wait_seconds: 120,
                },
                timeoutMs: 180_000,
              },
            },
          ],
          text: 'It answered and told me what it could not do.',
        },
      ],
    }),
  );

  await home.rpc('StartSession', caller.threadIds[0]);
  await home.rpc('SendMessage', caller.threadIds[0], 'ask the GPU computer about rollback', null);

  const forkMock = await awaitGate(remote, 'remote-fork-refused', there.path);

  // The fork lives on the destination computer only, read-only, with the
  // refusal recorded in it.
  const fork = (await threadRows(remote)).find((row) => row.mode === 'scratch');
  expect(fork, 'the forwarded ask made no scratch fork there').toBeDefined();
  expect(fork!.runtimeMode).toBe('read-only');
  expect(fork!.id).not.toBe(target);
  expect((await threadRows(home)).some((row) => row.mode === 'scratch')).toBe(false);

  interface TimelineItem {
    id: string;
    kind: string;
    decision?: string;
    summary?: string;
  }
  await expect
    .poll(async () => {
      const items = await remote.rpc<TimelineItem[]>('ListItems', fork!.id, true);
      const notice = items.find((item) => item.id === 'permission-denied:tu-remotewrite');
      if (!notice) return null;
      return {
        noticeKind: notice.kind,
        writeDecision: items.find((item) => item.id === 'tu-remotewrite')?.decision ?? '',
      };
    })
    .toEqual({ noticeKind: 'notification', writeDecision: 'declined' });

  // The refusal is the session that computer gave the fork, not the
  // script's good manners: the write tools are stripped from it, and the
  // thread it was cut from kept its own.
  interface MockRow {
    mockId: string;
    registration: { cwd: string };
    sessionConfig?: { disallowedTools?: string[]; permissionMode?: string };
  }
  const mocks = await remote.rpc<MockRow[]>('HarnessListMocks');
  expect(mocks.find((mock) => mock.mockId === forkMock.mockId)?.sessionConfig?.disallowedTools).toEqual(
    expect.arrayContaining(['Write', 'Edit', 'NotebookEdit']),
  );
  expect(mocks.find((mock) => mock.mockId === targetMock.mockId)?.sessionConfig?.disallowedTools ?? []).toEqual(
    [],
  );

  await advanceGate(remote, forkMock.mockId, 'remote-fork-refused');

  interface AskAnswer {
    token: string;
    thread_id: string;
    computer_id?: string;
    state: string;
    outcome: string;
    answer?: string;
  }
  // Scoped to this test's workspace: the backend lives for the whole file,
  // so the previous test's ask answer is still in the event log.
  const ask = await awaitToolAnswer<AskAnswer>(home, {
    tool: 'thread_ask',
    cwd: caller.path,
    timeoutMs: 150_000,
  });
  expect(ask.isError, ask.text).toBe(false);
  expect(ask.value!.outcome).toBe('settled');
  expect(ask.value!.state).toBe('replied');
  expect(ask.value!.computer_id).toBe(remoteID);
  expect(ask.value!.answer).toContain('renames two columns');
  // The caller learns the write was refused rather than reading an answer
  // that quietly left it out.
  expect(ask.value!.answer).toContain('Write was denied in this read-only copy');

  // Only the answer crossed: no copy of the fork here, nothing written
  // there, and the thread that was asked never saw the question.
  expect((await threadRows(home)).some((row) => row.id === ask.value!.thread_id)).toBe(false);
  const targetItems = await remote.rpc<TimelineItem[]>('ListItems', target, true);
  expect(
    targetItems.some((item) => (item.summary ?? '').includes('what does the migration do')),
  ).toBe(false);
  await expect
    .poll(async () => (await threadRows(remote)).filter((row) => row.mode === 'scratch').length, {
      timeout: 30_000,
    })
    .toBe(0);
  await advanceGate(remote, targetMock.mockId, 'remote-target-busy');
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
      status: string;
    }
    const hasTools = async (threadID: string) =>
      (await remote.rpc<McpRow[]>('ListThreadMcpServers', threadID)).some(
        (server) => server.name === 'ao-thread-tools' && !server.disabled && server.status !== 'disabled',
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

test('a thread moved to the other computer resolves there by its bare id', async () => {
  test.setTimeout(180_000);
  const caller = await seed(home, 'remote-moved-caller', ['Moved caller']);
  const landing = await seed(remote, 'remote-moved-landing', []);

  // A move carries the thread's native provider session, so the source
  // needs one: an inert transcript in the harness's own provider home,
  // the fixture shape conversation-transfer-flow uses. No real provider.
  const nativeID = randomUUID();
  const source = (
    await home.rpc<{ projects: SeededProject[] }>('HarnessSeed', {
      projects: [
        {
          name: 'remote-moved-source',
          repo: {},
          threads: [
            {
              title: 'Portable thread',
              provider: 'claude',
              sessionRef: nativeID,
              turns: [
                {
                  userText: 'remember this before you travel',
                  items: [{ kind: 'assistant_text', summary: 'This thread is portable.' }],
                },
              ],
            },
          ],
        },
      ],
    })
  ).projects[0];
  const moved = source.threadIds[0];
  await home.rpc('HarnessSeed', {
    providerHome: [
      {
        path: `.claude/projects/transfer-fixture/${nativeID}.jsonl`,
        content:
          JSON.stringify({
            type: 'user',
            sessionId: nativeID,
            uuid: randomUUID(),
            parentUuid: null,
            message: { role: 'user', content: 'remember this before you travel' },
          }) + '\n',
      },
    ],
  });

  // The real move: BeginThreadTransfer here, the offer there, the bind
  // back here, and then the two hosts own it (conversation-transfer.md).
  const backend = (await home.rpc<Array<{ id: string; backendId: string }>>('ListBackends')).find(
    (row) => row.id === remoteID,
  )!;
  const operation = randomUUID();
  const intent = await home.rpc<Record<string, unknown>>(
    'BeginThreadTransfer',
    moved,
    operation,
    backend.backendId,
    'move',
    false,
  );
  const offer = await remote.rpc<Record<string, unknown>>(
    'CreateThreadTransferOffer',
    intent,
    landing.projectId,
    '',
    '',
  );
  const transfer = await home.rpc<{ id: string }>('BindThreadTransferDestination', moved, offer);
  interface TransferRow {
    id: string;
    kind: string;
    phase: string;
    error?: string;
  }
  await expect
    .poll(
      async () => {
        const rows = await home.rpc<TransferRow[]>('GetThreadTransfers');
        const row = rows.find((candidate) => candidate.id === transfer.id);
        if (row?.phase === 'complete') return 'complete';
        const received = (await remote.rpc<TransferRow[]>('GetThreadTransfers')).find(
          (candidate) => candidate.id === transfer.id,
        );
        // Both sides' errors, so a stall reports why instead of a phase.
        return [row?.error, received?.error, row?.phase].filter(Boolean).join(' | ');
      },
      { timeout: 60_000 },
    )
    .toBe('complete');

  // The thread is gone from this computer and keeps its id on the other.
  expect((await threadRows(home)).some((row) => row.id === moved)).toBe(false);
  expect((await threadRows(remote)).some((row) => row.id === moved)).toBe(true);

  await setScenario(
    home,
    caller.path,
    threadToolsScenario({
      name: 'remote-moved-caller-script',
      provider: 'claude',
      turns: [
        {
          steps: [{ call: { tool: 'thread_show', args: { thread_id: moved }, timeoutMs: 60_000 } }],
          text: 'Followed it to its new computer.',
        },
      ],
    }),
  );
  await home.rpc('StartSession', caller.threadIds[0]);
  await home.rpc('SendMessage', caller.threadIds[0], 'read the thread I moved', null);

  // The id this computer handed away is a move, not a miss: resolution
  // reads the transfer record and asks the new owner.
  interface ShowAnswer {
    thread_id: string;
    computer_id?: string;
    computer?: string;
    title?: string;
  }
  const show = await awaitToolAnswer<ShowAnswer>(home, { tool: 'thread_show', timeoutMs: 90_000 });
  expect(show.isError, show.text).toBe(false);
  expect(show.value!.thread_id).toBe(moved);
  expect(show.value!.computer_id).toBe(remoteID);
  expect(show.value!.computer).toBe('GPU computer');
  expect(show.value!.title).toBe('Portable thread');
});

test('the origin chip names the other computer on both ends of a spawn', async ({ page }) => {
  test.setTimeout(180_000);
  page.setDefaultTimeout(20_000);
  const caller = await seed(home, 'remote-chip-caller', ['Chip caller']);
  const there = await seed(remote, 'remote-chip-work', []);
  // The chip renders the name the VIEWER's pairing profile gives the other
  // computer, so the remote needs its own name for this one.
  await remote.rpc('RenameBackend', homeID, 'Laptop computer');

  await setScenario(
    remote,
    there.path,
    plainScenario({
      name: 'remote-chip-worker',
      provider: 'claude',
      texts: ['The chip run finished.'],
    }),
  );
  await setScenario(
    home,
    caller.path,
    threadToolsScenario({
      name: 'remote-chip-caller-script',
      provider: 'claude',
      turns: [
        {
          steps: [
            {
              call: {
                tool: 'thread_spawn',
                args: {
                  prompt: 'Run the chip job.',
                  title: 'Chip work',
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
  await home.rpc('SendMessage', caller.threadIds[0], 'have the GPU computer run the chip job', null);

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
  // Wait for the wake, which is the caller-side row that carries a chip.
  await home.waitForEvent<HarnessMockEventData>(
    'harness:mock',
    (ev) =>
      ev.report.kind === 'user_input' &&
      ev.cwd === caller.path &&
      (ev.report.input ?? '').includes(spawn.value!.token),
    90_000,
  );

  const sidebarRow = (title: string) =>
    page.getByTestId('thread-row-title').filter({ hasText: title }).first();

  // The destination's first message says which computer asked for it.
  await remote.open(page);
  await sidebarRow('Chip work').click();
  await expect(page.getByTestId('user-message-thread-origin').first()).toHaveText(
    'from Chip caller on Laptop computer',
  );

  // The caller's wake says which computer answered.
  await home.open(page);
  await sidebarRow('Chip caller').click();
  await expect(page.getByTestId('user-message-thread-origin').first()).toHaveText(
    'from Chip work on GPU computer',
  );
});

test('deleting the caller cancels the scratch ask it left running on the other computer', async () => {
  test.setTimeout(180_000);
  const caller = await seed(home, 'remote-delete-caller', ['Delete caller']);
  const there = await seed(remote, 'remote-delete-target', ['Ask target']);
  const target = there.threadIds[0];

  // One real turn first, so the ask's hidden fork has a provider session
  // on that computer to be cut from.
  await setScenario(
    remote,
    there.path,
    plainScenario({
      name: 'remote-delete-source',
      provider: 'claude',
      texts: ['The import path is hot.'],
    }),
  );
  await remote.rpc('StartSession', target);
  await remote.rpc('SendMessage', target, 'profile the import path', null);
  await remote.waitForEvent<{ threadId: string }>(
    'provider:turn_completed',
    (ev) => ev.threadId === target,
    60_000,
  );

  // The fork never answers: it parks, so the ask is still open when the
  // caller is deleted.
  await setScenario(
    remote,
    there.path,
    threadToolsScenario({
      name: 'remote-delete-fork',
      provider: 'claude',
      turns: [{ steps: [{ gate: 'hold-remote-delete' }], text: 'Never answered.' }],
    }),
  );
  await setScenario(
    home,
    caller.path,
    threadToolsScenario({
      name: 'remote-delete-caller-script',
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
                  wait_seconds: 0,
                  notify: true,
                },
                timeoutMs: 120_000,
              },
            },
          ],
          text: 'Asked it; it is thinking.',
        },
      ],
    }),
  );

  await home.rpc('StartSession', caller.threadIds[0]);
  await home.rpc('SendMessage', caller.threadIds[0], 'ask the GPU computer what it profiled', null);

  interface AskAnswer {
    token: string;
    thread_id: string;
    outcome: string;
  }
  const ask = await awaitToolAnswer<AskAnswer>(home, { tool: 'thread_ask', timeoutMs: 120_000 });
  expect(ask.isError, ask.text).toBe(false);
  expect(ask.value!.outcome).toBe('backgrounded');

  // The scratch copy exists over there and is parked on its gate.
  await awaitGate(remote, 'hold-remote-delete', there.path);
  expect(
    (await threadRows(remote)).some(
      (row) => row.id === ask.value!.thread_id && row.mode === 'scratch',
    ),
  ).toBe(true);

  // Deleting the caller stops the ask wherever it is running.
  await home.rpc('DeleteThread', caller.threadIds[0]);
  await expect
    .poll(async () => (await threadRows(remote)).filter((row) => row.mode === 'scratch').length, {
      timeout: 60_000,
    })
    .toBe(0);

  // The thread it asked is untouched; only the copy was ever at risk.
  expect((await threadRows(remote)).some((row) => row.id === target)).toBe(true);
});

test('a responder on a switched-off destination answers with thread_reply', async () => {
  test.setTimeout(180_000);
  const caller = await seed(home, 'remote-reply-off-caller', ['Reply off caller']);
  const there = await seed(remote, 'remote-reply-off-target', ['Reply off target'], true);
  const target = there.threadIds[0];

  await remote.rpc('UpdateSettings', { threadToolsEnabled: false });
  try {
    // The thread tools are off on that computer, so this responder only
    // has thread_reply because an open request from here admits it.
    await setScenario(
      remote,
      there.path,
      threadToolsScenario({
        name: 'remote-reply-off-responder',
        provider: 'claude',
        turns: [
          {
            steps: [
              { capture: { var: 'TOKEN', from: '${USER_INPUT}', pattern: FOOTER_TOKEN_PATTERN } },
              {
                call: {
                  tool: 'thread_reply',
                  args: { token: '${TOKEN}', text: 'Replied with the switch off.' },
                  timeoutMs: 60_000,
                },
              },
            ],
            text: 'Answered through thread_reply.',
          },
        ],
      }),
    );
    await setScenario(
      home,
      caller.path,
      threadToolsScenario({
        name: 'remote-reply-off-caller-script',
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
                    message: 'answer me from over there',
                    wait_seconds: 120,
                  },
                  timeoutMs: 180_000,
                },
              },
            ],
            text: 'It replied.',
          },
        ],
      }),
    );

    await home.rpc('StartSession', caller.threadIds[0]);
    await home.rpc('SendMessage', caller.threadIds[0], 'ask the GPU computer to reply', null);

    // The responder's own tool call succeeded on the switched-off computer.
    const reply = await awaitToolAnswer<{ state: string }>(remote, {
      tool: 'thread_reply',
      cwd: there.path,
      timeoutMs: 150_000,
    });
    expect(reply.isError, reply.text).toBe(false);

    interface SendAnswer {
      state: string;
      outcome: string;
      answer_kind?: string;
      answer?: string;
    }
    // Filtered by this caller's own project: earlier tests in this file
    // leave their own thread_send answers in the log unread.
    const send = await awaitToolAnswer<SendAnswer>(home, {
      tool: 'thread_send',
      cwd: caller.path,
      timeoutMs: 180_000,
    });
    expect(send.isError, send.text).toBe(false);
    expect(send.value!.outcome).toBe('settled');
    // `replied` is the reply's own state; a turn that only rested would
    // settle `answered` with the assistant text instead.
    expect(send.value!.state).toBe('replied');
    expect(send.value!.answer).toContain('Replied with the switch off.');
  } finally {
    await remote.rpc('UpdateSettings', { threadToolsEnabled: true });
  }
});

test('a computer whose hello lacks the thread-tools capability fails as thread_unsupported', async () => {
  test.setTimeout(180_000);
  // A backend that advertises everything except thread-tools.v1, which is
  // what a build older than these tools looks like on the wire.
  const older = await launchHarness({ env: { AO_HARNESS_OLD_THREAD_TOOLS_PEER: '1' } });
  let olderID = '';
  try {
    const pairing = await headlessPairing(older);
    try {
      const attachment = await home.rpc<{ id: string; verificationNumber: string }>(
        'AddBackend',
        pairing.invite.url,
      );
      await pairing.confirm(attachment.verificationNumber);
      olderID = attachment.id;
    } finally {
      pairing.close();
    }
    await home.rpc('RenameBackend', olderID, 'Old computer');
    const project = await seed(older, 'remote-old-work', []);

    const caller = await seed(home, 'remote-old-caller', ['Old peer caller']);
    await setScenario(
      home,
      caller.path,
      threadToolsScenario({
        name: 'remote-old-caller-script',
        provider: 'claude',
        turns: [
          {
            steps: [
              {
                call: {
                  tool: 'thread_spawn',
                  args: {
                    prompt: 'Try to work on the old computer.',
                    title: 'Old work',
                    computer_id: olderID,
                    project_id: project.projectId,
                    notify: true,
                  },
                  timeoutMs: 120_000,
                },
              },
            ],
            text: 'That computer is too old.',
          },
        ],
      }),
    );
    await home.rpc('StartSession', caller.threadIds[0]);
    await home.rpc('SendMessage', caller.threadIds[0], 'spawn on the old computer', null);

    const spawn = await awaitToolAnswer(home, {
      tool: 'thread_spawn',
      cwd: caller.path,
      timeoutMs: 120_000,
    });
    expect(spawn.isError).toBe(true);
    expect(spawn.text).toContain('does not support agent thread tools');
    expect(spawn.text).toContain('Update Agent Overflow on that computer');

    // Nothing was started there, and nothing is left open here: the
    // refusal happened before the destination ever saw the call.
    expect((await threadRows(older)).some((row) => row.title === 'Old work')).toBe(false);
  } finally {
    if (olderID !== '') await home.rpc('RemoveBackend', olderID, true);
    await older.close();
  }
});

test('a reply written while the calling computer was off arrives when it comes back', async () => {
  test.setTimeout(240_000);
  const caller = await seed(home, 'away-caller', ['Away caller']);
  const there = await seed(remote, 'away-target', ['Nightly build']);
  const target = there.threadIds[0];

  // The answering thread holds its reply until the caller's computer is
  // gone, so the answer is written with nowhere to deliver it.
  await setScenario(
    remote,
    there.path,
    threadToolsScenario({
      name: 'away-target-script',
      provider: 'claude',
      turns: [
        {
          steps: [
            { capture: { var: 'TOKEN', from: '${USER_INPUT}', pattern: FOOTER_TOKEN_PATTERN } },
            { gate: 'caller-offline' },
            {
              call: {
                tool: 'thread_reply',
                args: { token: '${TOKEN}', text: 'The nightly finished at 03:14.' },
              },
            },
          ],
          text: 'Replied while they were away.',
        },
      ],
    }),
  );
  await setScenario(
    home,
    caller.path,
    threadToolsScenario({
      name: 'away-caller-script',
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
                  message: 'when did the nightly finish?',
                  wait_seconds: 0,
                  notify: true,
                },
                timeoutMs: 60_000,
              },
            },
          ],
          text: 'Asked and ended my turn.',
        },
      ],
    }),
  );

  await home.rpc('StartSession', caller.threadIds[0]);
  await home.rpc('SendMessage', caller.threadIds[0], 'ask the GPU computer about the nightly', null);

  interface SendAck {
    token: string;
    state: string;
    outcome: string;
    notify: boolean;
  }
  // Scoped to this test's workspace: the backend lives for the whole file,
  // so an earlier test's answer is still in the event log.
  const send = await awaitToolAnswer<SendAck>(home, {
    tool: 'thread_send',
    cwd: caller.path,
    timeoutMs: 120_000,
  });
  expect(send.isError, send.text).toBe(false);
  expect(send.value!.outcome).toBe('backgrounded');
  expect(send.value!.notify).toBe(true);
  const token = send.value!.token;

  const targetMock = await awaitGate(remote, 'caller-offline', there.path);

  // The caller's computer goes away with the request open.
  expect(await home.stop()).toBe(true);

  await advanceGate(remote, targetMock.mockId, 'caller-offline');
  const reply = await awaitToolAnswer<{ token: string; state: string }>(remote, {
    tool: 'thread_reply',
    cwd: there.path,
    timeoutMs: 60_000,
  });
  expect(reply.isError, reply.text).toBe(false);
  expect(reply.value!.token).toBe(token);
  expect(reply.value!.state).toBe('replied');

  // Back on the same data directory: the source-side poller collects the
  // answer that was written while nobody here could hear it, and the wake
  // reaches the thread that asked.
  home = await launchHarness({ dataDir: homeDataDir });
  await setScenario(
    home,
    caller.path,
    plainScenario({
      name: 'away-caller-back',
      provider: 'claude',
      texts: ['Read the late answer.'],
    }),
  );
  const wake = await home.waitForEvent<HarnessMockEventData>(
    'harness:mock',
    (ev) =>
      ev.report.kind === 'user_input' &&
      ev.cwd === caller.path &&
      (ev.report.input ?? '').includes(token),
    150_000,
  );
  expect(wake.report.input).toContain('The nightly finished at 03:14.');
  expect(wake.report.input).toContain('GPU computer');
});

test('a thread of 38k items on the other computer is windowed, paged and exported here', async () => {
  test.setTimeout(420_000);
  const big = bigThreadFixture('PAIRBIG', 'Remote kernel sweep');
  const caller = await seed(home, 'remote-window-caller', ['Remote window caller']);
  const seeded = await remote.rpc<{ projects: SeededProject[] }>('HarnessSeed', {
    projects: [{ name: 'remote-window-target', repo: {}, threads: [big.thread] }],
  });
  const target = seeded.projects[0].threadIds[0];

  await setScenario(
    home,
    caller.path,
    threadToolsScenario({
      name: 'remote-window-script',
      provider: 'claude',
      turns: [
        {
          steps: [
            {
              call: {
                tool: 'thread_search',
                args: { query: big.anchor, limit: 5 },
                timeoutMs: 90_000,
              },
            },
            // A search hit names the computer, the thread and the item, so
            // `around` opens across the pairing with nothing else looked up.
            { capture: { var: 'ITEM', from: '${MCP_RESULT}', pattern: '"item_id":"([^"]+)"' } },
            {
              call: {
                tool: 'thread_show',
                args: { thread_id: target, window: 'around', item_id: '${ITEM}', context: 2 },
                timeoutMs: 120_000,
              },
            },
            {
              call: {
                tool: 'thread_show',
                args: { thread_id: target, window: 'all', include: ['all'], to_file: true },
                timeoutMs: 300_000,
              },
            },
          ],
          text: 'Opened the middle over there and wrote the whole thing out here.',
        },
        {
          steps: [
            {
              call: {
                tool: 'thread_show',
                args: { thread_id: target, window: 'all', max_bytes: 1_048_576 },
                timeoutMs: 300_000,
              },
            },
            { capture: { var: 'CURSOR', from: '${MCP_RESULT}', pattern: SHOW_CURSOR_PATTERN } },
          ],
          text: 'First page.',
        },
        {
          steps: [
            {
              call: {
                tool: 'thread_show',
                args: { thread_id: target, cursor: '${CURSOR}', max_bytes: 1_048_576 },
                timeoutMs: 300_000,
              },
            },
            { capture: { var: 'CURSOR', from: '${MCP_RESULT}', pattern: SHOW_CURSOR_PATTERN } },
          ],
          text: 'Next page.',
        },
      ],
    }),
  );
  await home.rpc('StartSession', caller.threadIds[0]);
  await home.rpc('SendMessage', caller.threadIds[0], 'read the middle of the sweep over there', null);

  interface SearchAnswer {
    computers: Array<{ computer_id?: string; rows: Array<{ thread_id: string; item_id?: string }> }>;
  }
  const search = await awaitToolAnswer<SearchAnswer>(home, {
    tool: 'thread_search',
    timeoutMs: 90_000,
  });
  expect(search.isError, search.text).toBe(false);
  const group = search.value!.computers.find((row) => row.computer_id === remoteID)!;
  expect(group.rows.map((row) => row.thread_id)).toContain(target);

  interface ShowAnswer {
    window: string;
    computer_id?: string;
    transcript?: string;
    items: number;
    bytes: number;
    done: boolean;
    cursor?: string;
    file?: { path: string; size: number; sha256: string };
  }
  // `around` reads the turns surrounding one item of a thread this
  // computer does not hold, inside the default budget.
  const around = await awaitToolAnswer<ShowAnswer>(home, {
    tool: 'thread_show',
    timeoutMs: 120_000,
  });
  expect(around.isError, around.text).toBe(false);
  expect(around.value!.window).toBe('around');
  expect(around.value!.computer_id).toBe(remoteID);
  expect(around.value!.done).toBe(true);
  expect(around.value!.items).toBe(big.aroundItems);
  expect(around.value!.bytes).toBeLessThanOrEqual(64 * 1024);
  expect(around.value!.transcript).toContain(big.anchor);
  expect(around.value!.transcript).not.toContain(big.head);
  expect(around.value!.transcript).not.toContain(big.tail);

  // to_file renders over there and copies here, so the path the model
  // reads is one THIS computer can open.
  const exported = await awaitToolAnswer<ShowAnswer>(home, {
    tool: 'thread_show',
    timeoutMs: 300_000,
  });
  expect(exported.isError, exported.text).toBe(false);
  expect(exported.value!.transcript).toBeUndefined();
  expect(exported.value!.file!.sha256).toMatch(/^[0-9a-f]{64}$/);
  const rendered = await readFile(exported.value!.file!.path, 'utf8');
  expect(rendered.length).toBe(exported.value!.file!.size);
  expect(rendered).toContain(big.head);
  expect(rendered).toContain(big.anchor);
  expect(rendered).toContain(big.tail);
  expect(rendered.match(/^--- turn \d+ ---$/gm)!.length).toBe(big.turns);

  // `all` pages the remote thread to its end through the cursor, one page
  // per turn because a thread that is still working refuses a send.
  await awaitTurnCompleted(home, caller.threadIds[0], 300_000);
  const pages: ShowAnswer[] = [];
  let done = false;
  while (!done) {
    await home.rpc('SendMessage', caller.threadIds[0], `page ${pages.length + 1} over there`, null);
    const answer = await awaitToolAnswer<ShowAnswer>(home, {
      tool: 'thread_show',
      timeoutMs: 300_000,
    });
    expect(answer.isError, answer.text).toBe(false);
    const page = answer.value!;
    expect(page.window).toBe('all');
    expect(page.computer_id).toBe(remoteID);
    expect(page.bytes).toBeLessThanOrEqual(1_048_576);
    pages.push(page);
    done = page.done;
    expect(pages.length).toBeLessThan(12);
    await awaitTurnCompleted(home, caller.threadIds[0], 300_000);
  }

  expect(pages.length).toBeGreaterThan(1);
  expect(pages[0].transcript).toContain(big.head);
  expect(pages.reduce((sum, entry) => sum + entry.items, 0)).toBe(big.items);
  const last = pages[pages.length - 1];
  expect(last.transcript).toContain(big.tail);
  expect(last.cursor).toBeUndefined();
});

test('a multi-megabyte tool output on the other computer is searched and read in ranges', async () => {
  test.setTimeout(300_000);
  const bigItem = bigItemFixture('PAIRHUGE', 'Remote fuzzer crash');
  const caller = await seed(home, 'remote-item-caller', ['Remote item caller']);
  const seeded = await remote.rpc<{ projects: SeededProject[] }>('HarnessSeed', {
    projects: [{ name: 'remote-item-target', repo: {}, threads: [bigItem.thread] }],
  });
  const target = seeded.projects[0].threadIds[0];
  const lastChunk = 16 * 1024;

  await setScenario(
    home,
    caller.path,
    threadToolsScenario({
      name: 'remote-item-script',
      provider: 'claude',
      turns: [
        {
          steps: [
            {
              call: {
                tool: 'thread_show',
                args: { thread_id: target, window: 'tail' },
                timeoutMs: 120_000,
              },
            },
            { capture: { var: 'ITEM', from: '${MCP_RESULT}', pattern: 'item_id=([A-Za-z0-9-]+)' } },
            {
              call: {
                tool: 'thread_item',
                args: { thread_id: target, item_id: '${ITEM}', query: bigItem.needle.trim() },
                timeoutMs: 180_000,
              },
            },
            {
              call: {
                tool: 'thread_item',
                args: {
                  thread_id: target,
                  item_id: '${ITEM}',
                  offset: bigItem.needleOffset - 512,
                  max_bytes: 1024,
                },
                timeoutMs: 120_000,
              },
            },
            {
              call: {
                tool: 'thread_item',
                args: { thread_id: target, item_id: '${ITEM}', offset: -lastChunk },
                timeoutMs: 120_000,
              },
            },
          ],
          text: 'Found it over there.',
        },
      ],
    }),
  );
  await home.rpc('StartSession', caller.threadIds[0]);
  await home.rpc(
    'SendMessage',
    caller.threadIds[0],
    'what did the fuzzer crash on over there?',
    null,
  );

  interface ShowAnswer {
    computer_id?: string;
    transcript?: string;
  }
  // The row renders on the computer that holds it, so the megabytes never
  // cross: the transcript is one line naming the size and the reader.
  const collapsed = await awaitToolAnswer<ShowAnswer>(home, {
    tool: 'thread_show',
    timeoutMs: 120_000,
  });
  expect(collapsed.isError, collapsed.text).toBe(false);
  expect(collapsed.value!.computer_id).toBe(remoteID);
  expect(collapsed.value!.transcript).toContain('3.8 MB, not shown; read it with thread_item');
  expect(collapsed.value!.transcript!.length).toBeLessThan(64 * 1024);
  expect(collapsed.value!.transcript).not.toContain(bigItem.needle.trim());

  interface ItemAnswer {
    computer_id?: string;
    size: number;
    offset: number;
    bytes: number;
    text?: string;
    eof: boolean;
    match_count?: number;
    matches?: Array<{ offset: number; context: string }>;
  }
  // The query runs over there and only the matches cross.
  const matched = await awaitToolAnswer<ItemAnswer>(home, {
    tool: 'thread_item',
    timeoutMs: 180_000,
  });
  expect(matched.isError, matched.text).toBe(false);
  expect(matched.value!.computer_id).toBe(remoteID);
  expect(matched.value!.size).toBe(bigItem.size);
  expect(matched.value!.match_count).toBe(1);
  expect(matched.value!.matches![0].offset).toBe(bigItem.needleOffset);
  expect(matched.value!.matches![0].context).toContain(bigItem.needle.trim());

  const range = await awaitToolAnswer<ItemAnswer>(home, {
    tool: 'thread_item',
    timeoutMs: 120_000,
  });
  expect(range.isError, range.text).toBe(false);
  expect(range.value!.offset).toBe(bigItem.needleOffset - 512);
  expect(range.value!.bytes).toBe(1024);
  expect(range.value!.text).toContain(bigItem.needle.trim());
  expect(range.value!.text).toContain('pad before');
  expect(range.value!.text).toContain('pad after');
  expect(range.value!.eof).toBe(false);

  // A negative offset reads the last 16KB of a 3.8MB row held elsewhere.
  const tail = await awaitToolAnswer<ItemAnswer>(home, {
    tool: 'thread_item',
    timeoutMs: 120_000,
  });
  expect(tail.isError, tail.text).toBe(false);
  expect(tail.value!.offset).toBe(bigItem.size - lastChunk);
  expect(tail.value!.bytes).toBe(lastChunk);
  expect(tail.value!.eof).toBe(true);
  expect(tail.value!.text).toContain('pad after');
  expect(tail.value!.text).not.toContain(bigItem.needle.trim());
});

// Last in the file: this one removes the pairing, and the tests run in
// serial order against the two backends beforeAll launched.
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
