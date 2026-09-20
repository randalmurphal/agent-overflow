// ao-thread-tools on one computer, through real MCP sessions.
//
// Coverage: the server in both providers' session configs and the
// composer's MCP menu; the real tools/list a session sees and the guide
// it carries; the settings switch removing and restoring the tools inside
// a running session; read tools (search, show, item, options), including a
// 38k-item thread windowed, paged and exported and a multi-megabyte tool
// output read in ranges; spawn with
// its origin chip, footer and wake; send, reply, late reply and status;
// ask on a hidden read-only fork that is deleted once it answers; cancel
// by token and by thread; reminders; organizing threads and groups; and
// the refusals (self-send, unrelated cancel, switch off).
//
// Everything a tool does here runs through a mock provider making REAL
// MCP calls (scenario `mcpCall` / `mcpList`), so the assertions are about
// what a provider session actually received.
import { readFile } from 'node:fs/promises';
import { test, expect, type HarnessMockEvent, type SeedResult } from './fixtures.js';
import { sessionConfigs } from './workflows-helpers.js';
import {
  FOOTER_TOKEN_PATTERN,
  RESULT_TOKEN_PATTERN,
  SHOW_CURSOR_PATTERN,
  THREAD_TOOLS_SERVER,
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

const TOOL_NAMES = [
  'thread_search',
  'thread_show',
  'thread_item',
  'thread_options',
  'thread_spawn',
  'thread_send',
  'thread_ask',
  'thread_reply',
  'thread_status',
  'thread_cancel',
  'thread_update',
  'thread_group',
  'thread_remind',
];

interface McpRow {
  name: string;
  disabled: boolean;
}

test('both providers get the thirteen tools and the guide, in every runtime mode', async ({
  harness,
}) => {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: 'tt-listing',
        repo: {},
        threads: [
          { title: 'Claude caller', provider: 'claude', runtimeMode: 'read-only' },
          { title: 'Codex caller', provider: 'codex', runtimeMode: 'read-only' },
        ],
      },
    ],
  });
  const { projectId, path, threadIds } = seed.projects[0];
  const [claudeThread, codexThread] = threadIds;
  expect(projectId).not.toBe('');

  // read-only is the runtime mode that auto-denies a prompting tool on
  // both providers. The tools are admitted by name (Claude's allowlist
  // argv, Codex's default_tools_approval_mode), so this is the mode worth
  // listing them in.
  await setScenario(
    harness,
    path,
    threadToolsScenario({
      name: 'tt-list-claude',
      provider: 'claude',
      turns: [{ steps: [{ list: {} }], text: 'Listed the thread tools.' }],
    }),
  );
  await setScenario(
    harness,
    path,
    threadToolsScenario({
      name: 'tt-list-codex',
      provider: 'codex',
      turns: [{ steps: [{ list: {} }], text: 'Listed the thread tools.' }],
    }),
  );

  await harness.rpc('StartSession', claudeThread);
  await harness.rpc('StartSession', codexThread);
  const [claudeConfig] = await sessionConfigs(harness, 'claude', 1);
  const [codexConfig] = await sessionConfigs(harness, 'codex', 1);
  expect(claudeConfig.mcpServers).toContain(THREAD_TOOLS_SERVER);
  expect(codexConfig.mcpServers).toContain(THREAD_TOOLS_SERVER);

  const rows = await harness.rpc<McpRow[]>('ListThreadMcpServers', claudeThread);
  expect(rows).toContainEqual(expect.objectContaining({ name: THREAD_TOOLS_SERVER, disabled: false }));

  await harness.rpc('SendMessage', claudeThread, 'list your thread tools', null);
  const claudeListing = await harness.awaitMcpTools({ server: THREAD_TOOLS_SERVER });
  expect(claudeListing.isError).toBe(false);
  expect(claudeListing.tools).toEqual(TOOL_NAMES);
  expect(claudeListing.instructions).toContain('These tools let you work with other Agent Overflow threads');
  // With no paired computer the guide drops the computers paragraph and
  // no schema offers a computer to address.
  expect(claudeListing.instructions).not.toContain('Other computers');
  expect(claudeListing.instructions).not.toContain('computer_id');

  await harness.rpc('SendMessage', codexThread, 'list your thread tools', null);
  const codexListing = await harness.awaitMcpTools({ server: THREAD_TOOLS_SERVER });
  expect(codexListing.tools).toEqual(TOOL_NAMES);
  // Codex never shows a server's instructions to the model, so the same
  // guide rides its developer instructions; the handshake still carries
  // it, which is what this listing reads.
  expect(codexListing.instructions).toBe(claudeListing.instructions);
});

test('the switch removes and restores the tools inside one running session', async ({
  harness,
}) => {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      { name: 'tt-switch', repo: {}, threads: [{ title: 'Switch caller', provider: 'claude' }] },
    ],
  });
  const { path, threadIds } = seed.projects[0];
  const caller = threadIds[0];

  await setScenario(
    harness,
    path,
    threadToolsScenario({
      name: 'tt-switch-caller',
      provider: 'claude',
      turns: [
        { steps: [{ list: {} }], text: 'Tools are here.' },
        {
          steps: [{ list: {} }, { call: { tool: 'thread_options', args: {} } }],
          text: 'Tools are gone.',
        },
        { steps: [{ list: {} }], text: 'Tools are back.' },
      ],
    }),
  );

  await harness.rpc('StartSession', caller);
  const registered = await harness.waitForEvent<HarnessMockEvent>(
    'harness:mock',
    (ev) => ev.report.kind === 'registered',
  );

  await harness.rpc('SendMessage', caller, 'first look', null);
  expect((await harness.awaitMcpTools({ mockId: registered.mockId })).tools).toEqual(TOOL_NAMES);
  await harness.waitForEvent('provider:turn_completed');

  try {
    await harness.rpc('UpdateSettings', { threadToolsEnabled: false });
    await harness.rpc('SendMessage', caller, 'second look', null);
    const off = await harness.awaitMcpTools({ mockId: registered.mockId });
    expect(off.isError).toBe(false);
    expect(off.tools).toEqual([]);
    // A call that races the flip is refused by name rather than running.
    const refused = await awaitToolAnswer(harness, { tool: 'thread_options' });
    expect(refused.isError).toBe(true);
    expect(refused.text).toContain('thread_tools_disabled');
    await harness.waitForEvent('provider:turn_completed');
  } finally {
    await harness.rpc('UpdateSettings', { threadToolsEnabled: true });
  }

  await harness.rpc('SendMessage', caller, 'third look', null);
  expect((await harness.awaitMcpTools({ mockId: registered.mockId })).tools).toEqual(TOOL_NAMES);
  await harness.waitForEvent('provider:turn_completed');

  // One session throughout: the switch is applied inside the running
  // provider, so nothing was restarted to make the tools come and go.
  const mocks = await harness.rpc<Array<{ mockId: string }>>('HarnessListMocks');
  expect(mocks.map((mock) => mock.mockId)).toEqual([registered.mockId]);
});

test('the composer MCP toggle turns the server off for one conversation', async ({
  harness,
  page,
}) => {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: 'tt-composer',
        repo: {},
        threads: [
          {
            title: 'Composer caller',
            provider: 'claude',
            turns: [{ userText: 'Ready?', items: [{ kind: 'assistant_text', summary: 'Ready.' }] }],
          },
        ],
      },
    ],
  });
  const { path, threadIds } = seed.projects[0];
  const caller = threadIds[0];

  // Each turn lists what the live session can see, which is the only
  // surface that says the toggle reached the running provider rather than
  // only the row the menu reads back.
  await setScenario(
    harness,
    path,
    threadToolsScenario({
      name: 'tt-composer-caller',
      provider: 'claude',
      turns: [
        { steps: [{ list: {} }], text: 'Tools are gone.' },
        { steps: [{ list: {} }], text: 'Tools are back.' },
      ],
    }),
  );
  await harness.rpc('StartSession', caller);

  await harness.open(page);
  await page.getByText('Composer caller').click();
  await page.getByTestId('composer-mcp-trigger').click();
  const row = page
    .getByRole('menu', { name: 'MCP servers' })
    .getByRole('menuitem')
    .filter({ hasText: THREAD_TOOLS_SERVER });
  await expect(row).toBeVisible();
  await row.click();
  await expect
    .poll(async () => {
      const rows = await harness.rpc<McpRow[]>('ListThreadMcpServers', caller);
      return rows.find((entry) => entry.name === THREAD_TOOLS_SERVER)?.disabled;
    })
    .toBe(true);

  // The session this conversation is already running offers nothing from
  // the server: the endpoint is still wired, and it lists no tools.
  await harness.rpc('SendMessage', caller, 'look after the toggle', null);
  const off = await harness.awaitMcpTools({ server: THREAD_TOOLS_SERVER });
  expect(off.isError, off.error).toBe(false);
  expect(off.tools).toEqual([]);
  await awaitTurnCompleted(harness, caller);

  await harness.rpc('SetThreadMcpServerEnabled', caller, THREAD_TOOLS_SERVER, true);
  await expect
    .poll(async () => {
      const rows = await harness.rpc<McpRow[]>('ListThreadMcpServers', caller);
      return rows.find((entry) => entry.name === THREAD_TOOLS_SERVER)?.disabled;
    })
    .toBe(false);

  await harness.rpc('SendMessage', caller, 'look once more', null);
  const back = await harness.awaitMcpTools({ server: THREAD_TOOLS_SERVER });
  expect(back.isError, back.error).toBe(false);
  expect(back.tools).toEqual(TOOL_NAMES);
});

test('the settings switch is a real toggle in the SPA', async ({ harness, page }) => {
  await harness.open(page);
  await page.getByTestId('sidebar-settings-button').click();
  await page.getByRole('tab', { name: 'Thread tools' }).click();
  const toggle = page.getByRole('switch', { name: 'Toggle Built-in Thread Tools' });
  await expect(toggle).toHaveAttribute('aria-checked', 'true');
  try {
    await toggle.click();
    await expect(toggle).toHaveAttribute('aria-checked', 'false');
    await expect
      .poll(async () => {
        const settings = await harness.rpc<{ threadToolsEnabled: boolean }>('GetSettings');
        return settings.threadToolsEnabled;
      })
      .toBe(false);
  } finally {
    const settings = await harness.rpc<{ threadToolsEnabled: boolean }>('GetSettings');
    if (!settings.threadToolsEnabled) {
      await harness.rpc('UpdateSettings', { threadToolsEnabled: true });
    }
  }
});

test('thread_options renders the catalogs with per-model efforts, and a model this computer lacks is refused with the list', async ({
  harness,
}) => {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      { name: 'tt-options', repo: {}, threads: [{ title: 'Options caller', provider: 'claude' }] },
      { name: 'tt-options-other', repo: {}, threads: [{ title: 'Elsewhere', provider: 'codex' }] },
    ],
  });
  const { path, threadIds } = seed.projects[0];
  const caller = threadIds[0];
  const callerRow = (await threadRows(harness)).find((row) => row.id === caller)!;

  await setScenario(
    harness,
    path,
    threadToolsScenario({
      name: 'tt-options-caller',
      provider: 'claude',
      turns: [
        {
          steps: [
            { call: { tool: 'thread_options' } },
            // A model this computer does not have is refused with what it
            // does have, so discovery is never a second round trip.
            {
              call: {
                tool: 'thread_spawn',
                args: { prompt: 'try this', model: 'gpt-9000-imaginary', provider: 'codex' },
              },
            },
          ],
          text: 'Read the options.',
        },
      ],
    }),
  );
  await harness.rpc('StartSession', caller);
  await harness.rpc('SendMessage', caller, 'what can you spawn?', null);

  // With no paired computer the answer inlines the one computer's fields
  // and says nothing about computers at all.
  interface OptionsAnswer {
    computers?: unknown;
    computer_id?: string;
    reachable?: boolean;
    os?: string;
    defaults?: { provider: string; model: string; mode: string; runtime_mode: string };
    providers: Array<{
      provider: string;
      default_model?: string;
      models: Array<{ model: string; efforts?: string[]; default_effort?: string }>;
    }>;
    projects: Array<{
      project_id: string;
      project: string;
      workspaces?: Array<{ workspace_path: string }>;
    }>;
    runtime_modes?: Array<{ runtime_mode: string; meaning?: string }>;
  }
  const answer = await awaitToolAnswer<OptionsAnswer>(harness, { tool: 'thread_options' });
  expect(answer.isError).toBe(false);
  const computer = answer.value!;
  expect(computer.computers).toBeUndefined();
  expect(computer.defaults).toEqual(
    expect.objectContaining({
      provider: callerRow.provider,
      model: callerRow.model,
      runtime_mode: callerRow.runtimeMode,
    }),
  );
  const providers = computer.providers.map((entry) => entry.provider);
  expect(providers).toEqual(expect.arrayContaining(['claude', 'codex']));
  const claude = computer.providers.find((entry) => entry.provider === 'claude')!;
  expect(claude.models.length).toBeGreaterThan(0);
  expect(claude.default_model).not.toBe('');
  expect(computer.runtime_modes?.map((mode) => mode.runtime_mode)).toEqual(
    expect.arrayContaining(['read-only', 'approval-required', 'full-access']),
  );
  expect(computer.runtime_modes?.every((mode) => (mode.meaning ?? '') !== '')).toBe(true);
  // Both seeded projects are offered with the checkout each one has.
  const projects = computer.projects.map((entry) => entry.project);
  expect(projects).toEqual(expect.arrayContaining(['tt-options', 'tt-options-other']));

  // Every model of both providers states the efforts it offers and marks
  // its default among them, which is what a spawn picks an effort from.
  expectEffortsPerModel(computer.providers);

  const codex = computer.providers.find((entry) => entry.provider === 'codex')!;
  const refused = await awaitToolAnswer(harness, { tool: 'thread_spawn' });
  expect(refused.isError).toBe(true);
  expect(refused.text).toContain('does not offer model "gpt-9000-imaginary"');
  for (const model of codex.models) expect(refused.text).toContain(model.model);
  // A spawn refused on its model created nothing: the two seeded threads
  // are still the only rows.
  expect(await threadRows(harness)).toHaveLength(2);
});

test('a self-send and a self-ask are refused by name', async ({ harness }) => {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      { name: 'tt-self', repo: {}, threads: [{ title: 'Self caller', provider: 'claude' }] },
    ],
  });
  const { path, threadIds } = seed.projects[0];
  const caller = threadIds[0];

  await setScenario(
    harness,
    path,
    threadToolsScenario({
      name: 'tt-self-caller',
      provider: 'claude',
      turns: [
        {
          steps: [
            { call: { tool: 'thread_send', args: { thread_id: caller, message: 'hello me' } } },
            { call: { tool: 'thread_ask', args: { thread_id: caller, question: 'well?' } } },
          ],
          text: 'Refused, as expected.',
        },
      ],
    }),
  );
  await harness.rpc('StartSession', caller);
  await harness.rpc('SendMessage', caller, 'talk to yourself', null);

  const send = await awaitToolAnswer(harness, { tool: 'thread_send' });
  expect(send.isError).toBe(true);
  expect(send.text).toContain('thread_self_send');
  const ask = await awaitToolAnswer(harness, { tool: 'thread_ask' });
  expect(ask.isError).toBe(true);
  expect(ask.text).toContain('thread_self_send');

  // Neither refusal may have written anything: still one thread.
  await harness.waitForEvent('provider:turn_completed');
  expect(await threadRows(harness)).toHaveLength(1);
});

test('thread_spawn opens a visible thread, delivers the prompt with its footer and wakes the caller', async ({
  harness,
  page,
}) => {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      { name: 'tt-spawn-caller', repo: {}, threads: [{ title: 'Spawn caller', provider: 'claude' }] },
      { name: 'tt-spawn-target', repo: {}, threads: [] },
    ],
  });
  const caller = seed.projects[0].threadIds[0];
  const callerPath = seed.projects[0].path;
  const targetProject = seed.projects[1].projectId;
  const targetPath = seed.projects[1].path;

  await setScenario(
    harness,
    callerPath,
    threadToolsScenario({
      name: 'tt-spawn-caller-script',
      provider: 'claude',
      turns: [
        {
          steps: [
            {
              call: {
                tool: 'thread_spawn',
                args: {
                  prompt: 'Audit the launcher startup path.',
                  title: 'Launcher audit',
                  project_id: targetProject,
                  notify: true,
                },
              },
            },
          ],
          text: 'Started the audit thread.',
        },
        { text: 'Read the wake.' },
      ],
    }),
  );
  // The spawned thread answers on its own project's workspace.
  await setScenario(
    harness,
    targetPath,
    plainScenario({
      name: 'tt-spawn-target-script',
      provider: 'claude',
      texts: ['The launcher starts in main.go.'],
    }),
  );

  await harness.rpc('StartSession', caller);
  await harness.rpc('SendMessage', caller, 'have another thread audit the launcher', null);

  interface SpawnAnswer {
    token: string;
    thread_id: string;
    state: string;
    outcome: string;
    notify: boolean;
  }
  const spawn = await awaitToolAnswer<SpawnAnswer>(harness, { tool: 'thread_spawn' });
  expect(spawn.isError).toBe(false);
  const spawned = spawn.value!;
  expect(spawned.outcome).toBe('backgrounded');
  expect(spawned.notify).toBe(true);
  expect(spawned.thread_id).not.toBe('');

  // The prompt reached the new thread as its first user message, with the
  // footer naming the sender, the token and what the user asked for.
  const delivered = await harness.waitForEvent<HarnessMockEvent>(
    'harness:mock',
    (ev) => ev.report.kind === 'user_input' && ev.cwd === targetPath,
  );
  expect(delivered.report.input).toContain('Audit the launcher startup path.');
  expect(delivered.report.input).toContain('Agent request from thread "Spawn caller"');
  expect(delivered.report.input).toContain(spawned.token);
  expect(delivered.report.input).toContain('have another thread audit the launcher');

  const rows = await threadRows(harness);
  const spawnedRow = rows.find((row) => row.id === spawned.thread_id)!;
  expect(spawnedRow.title).toBe('Launcher audit');
  expect(spawnedRow.projectId).toBe(targetProject);
  expect(spawnedRow.mode).toBe('chat');

  // The wake lands in the caller as a user row and starts its next turn.
  const wake = await harness.waitForEvent<HarnessMockEvent>(
    'harness:mock',
    (ev) =>
      ev.report.kind === 'user_input' &&
      ev.cwd === callerPath &&
      (ev.report.input ?? '').includes(spawned.token),
    30_000,
  );
  expect(wake.report.input).toContain('finished its turn without calling thread_reply');
  expect(wake.report.input).toContain('The launcher starts in main.go.');

  // Both ends carry the attribution chip: the spawned thread's first
  // message says where it came from.
  await harness.open(page);
  const sidebarRow = (title: string) =>
    page.getByTestId('thread-row-title').filter({ hasText: title }).first();
  await sidebarRow('Launcher audit').click();
  await expect(page.getByTestId('user-message-thread-origin')).toContainText('from Spawn caller');
  await sidebarRow('Spawn caller').click();
  await expect(page.getByTestId('user-message-thread-origin').first()).toContainText(
    'from Launcher audit',
  );
});

test('thread_send queues into a busy thread, thread_reply settles it, and a late reply still arrives', async ({
  harness,
}) => {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      { name: 'tt-send-caller', repo: {}, threads: [{ title: 'Send caller', provider: 'claude' }] },
      { name: 'tt-send-target', repo: {}, threads: [{ title: 'Send target', provider: 'claude' }] },
    ],
  });
  const caller = seed.projects[0].threadIds[0];
  const callerPath = seed.projects[0].path;
  const target = seed.projects[1].threadIds[0];
  const targetPath = seed.projects[1].path;

  // The target is mid-turn when the send arrives: its first turn parks on
  // a gate, so the queued message can only land at the turn boundary.
  await setScenario(
    harness,
    targetPath,
    threadToolsScenario({
      name: 'tt-send-target-script',
      provider: 'claude',
      turns: [
        { text: 'Still on the first task.' },
        {
          // Turn two is the queued agent message: read the token out of
          // its footer the way a model does, then answer it.
          steps: [
            { capture: { var: 'TOKEN', from: '${USER_INPUT}', pattern: FOOTER_TOKEN_PATTERN } },
            {
              call: {
                tool: 'thread_reply',
                args: { token: '${TOKEN}', text: 'The stall is in the watchdog.' },
              },
            },
          ],
          text: 'Answered the caller.',
        },
      ],
    }),
  );
  await setScenario(
    harness,
    callerPath,
    threadToolsScenario({
      name: 'tt-send-caller-script',
      provider: 'claude',
      turns: [
        {
          steps: [
            {
              call: {
                tool: 'thread_send',
                args: {
                  thread_id: target,
                  message: 'Where does the stall come from?',
                  wait_seconds: 120,
                },
                timeoutMs: 180_000,
              },
            },
          ],
          text: 'Got the answer inline.',
        },
      ],
    }),
  );

  await harness.rpc('StartSession', target);
  await harness.rpc('SendMessage', target, 'start on the first task', null);
  await harness.waitForEvent('provider:turn_completed');

  await harness.rpc('StartSession', caller);
  await harness.rpc('SendMessage', caller, 'ask the other thread about the stall', null);

  interface SendAnswer {
    token: string;
    state: string;
    outcome: string;
    answer_kind?: string;
    answer?: string;
    delivered?: string;
  }
  const send = await awaitToolAnswer<SendAnswer>(harness, {
    tool: 'thread_send',
    timeoutMs: 120_000,
  });
  expect(send.isError).toBe(false);
  expect(send.value!.outcome).toBe('settled');
  expect(send.value!.state).toBe('replied');
  expect(send.value!.answer_kind).toBe('reply');
  expect(send.value!.answer).toContain('The stall is in the watchdog.');
  expect(send.value!.delivered).toBe('inline');
});

test('the read tools find a thread, render its window and read inside one item', async ({
  harness,
}) => {
  // One seeded tool output big enough that the transcript collapses it and
  // thread_item is the only way to reach the line that matters.
  const bulk: string[] = [];
  for (let line = 1; line <= 400; line += 1) bulk.push(`line ${line}: scanning token ${line}`);
  bulk[200] = 'NEEDLE panic: tokenizer overran the escape';
  const output = bulk.join('\n');

  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      { name: 'tt-read', repo: {}, threads: [{ title: 'Read caller', provider: 'claude' }] },
      {
        name: 'tt-read-target',
        repo: {},
        threads: [
          {
            title: 'Tokenizer crash',
            provider: 'codex',
            turns: [
              {
                userText: 'investigate the tokenizer crash',
                items: [
                  {
                    kind: 'assistant_text',
                    summary: 'The crash is in scanIdent when the input ends mid-escape.',
                  },
                  {
                    kind: 'tool_call',
                    toolName: 'Bash',
                    summary: 'go test ./internal/lex',
                    payload: { kind: 'tool_call_result', data: output },
                  },
                ],
              },
            ],
          },
        ],
      },
    ],
  });
  const caller = seed.projects[0].threadIds[0];
  const callerPath = seed.projects[0].path;
  const target = seed.projects[1].threadIds[0];

  await setScenario(
    harness,
    callerPath,
    threadToolsScenario({
      name: 'tt-read-caller',
      provider: 'claude',
      turns: [
        {
          steps: [
            { call: { tool: 'thread_search', args: { query: 'scanIdent', limit: 5 } } },
            { call: { tool: 'thread_show', args: { thread_id: target, window: 'tail', turns: 5 } } },
            // The collapsed tool row names the item to read next, which is
            // the handoff the transcript exists to make.
            { capture: { var: 'ITEM', from: '${MCP_RESULT}', pattern: 'item_id=([A-Za-z0-9-]+)' } },
            {
              call: {
                tool: 'thread_show',
                args: { thread_id: target, include: ['tool_outputs'], max_bytes: 60000 },
              },
            },
            {
              call: {
                tool: 'thread_item',
                args: { thread_id: target, item_id: '${ITEM}', query: 'NEEDLE' },
              },
            },
            {
              call: {
                tool: 'thread_item',
                args: { thread_id: target, item_id: '${ITEM}', lines: '201' },
              },
            },
            {
              call: {
                tool: 'thread_show',
                args: { thread_id: target, window: 'all', include: ['all'], to_file: true },
              },
            },
          ],
          text: 'Read it all.',
        },
      ],
    }),
  );
  await harness.rpc('StartSession', caller);
  await harness.rpc('SendMessage', caller, 'find out what the tokenizer thread learned', null);

  interface SearchAnswer {
    rows: Array<{ thread_id: string; title: string; item_id?: string; snippet?: string }>;
    computers?: unknown;
  }
  const search = await awaitToolAnswer<SearchAnswer>(harness, { tool: 'thread_search' });
  expect(search.isError).toBe(false);
  const hit = search.value!.rows.find((row) => row.thread_id === target)!;
  expect(hit).toBeDefined();
  expect(hit.title).toBe('Tokenizer crash');
  expect(hit.item_id).toBeTruthy();
  expect(hit.snippet).toContain('scanIdent');
  // Unpaired, so a row carries no computer grouping.
  expect(search.value!.computers).toBeUndefined();

  interface ShowAnswer {
    transcript?: string;
    items: number;
    done: boolean;
    title?: string;
    state?: string;
    file?: { path: string; size: number; sha256: string };
  }
  const collapsed = await awaitToolAnswer<ShowAnswer>(harness, { tool: 'thread_show' });
  expect(collapsed.isError).toBe(false);
  expect(collapsed.value!.title).toBe('Tokenizer crash');
  expect(collapsed.value!.done).toBe(true);
  expect(collapsed.value!.transcript).toContain('investigate the tokenizer crash');
  expect(collapsed.value!.transcript).toContain('The crash is in scanIdent');
  // The tool row is one line naming its size and the tool that reads it.
  expect(collapsed.value!.transcript).toContain('not shown; read it with thread_item');
  expect(collapsed.value!.transcript).not.toContain('NEEDLE panic');

  const expanded = await awaitToolAnswer<ShowAnswer>(harness, { tool: 'thread_show' });
  expect(expanded.value!.transcript).toContain('NEEDLE panic: tokenizer overran the escape');

  interface ItemAnswer {
    item_id: string;
    size: number;
    offset: number;
    bytes: number;
    text?: string;
    eof: boolean;
    match_count?: number;
    matches?: Array<{ offset: number; context: string }>;
    first_line?: number;
    last_line?: number;
  }
  const matched = await awaitToolAnswer<ItemAnswer>(harness, { tool: 'thread_item' });
  expect(matched.isError).toBe(false);
  expect(matched.value!.match_count).toBe(1);
  expect(matched.value!.matches![0].offset).toBeGreaterThan(0);
  expect(matched.value!.matches![0].context).toContain('NEEDLE panic');
  expect(matched.value!.size).toBe(output.length);

  const lines = await awaitToolAnswer<ItemAnswer>(harness, { tool: 'thread_item' });
  // A line range carries its terminating newline, the way sed -n does.
  expect(lines.value!.text).toBe('NEEDLE panic: tokenizer overran the escape\n');
  expect(lines.value!.first_line).toBe(201);
  expect(lines.value!.last_line).toBe(201);

  const exported = await awaitToolAnswer<ShowAnswer>(harness, { tool: 'thread_show' });
  // include: all writes every item whole, so the file holds the payload
  // the inline windows collapsed or clipped.
  expect(exported.value!.transcript).toBeUndefined();
  expect(exported.value!.file!.path).toContain(target);
  expect(exported.value!.file!.size).toBeGreaterThan(output.length);
  expect(exported.value!.file!.sha256).toMatch(/^[0-9a-f]{64}$/);
});

test('thread_show windows, pages and exports a thread of 38k items', async ({ harness }) => {
  test.setTimeout(240_000);
  const big = bigThreadFixture('BIG', 'Kernel sweep');
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      { name: 'tt-window', repo: {}, threads: [{ title: 'Window caller', provider: 'claude' }] },
      { name: 'tt-window-target', repo: {}, threads: [big.thread] },
    ],
  });
  const caller = seed.projects[0].threadIds[0];
  const callerPath = seed.projects[0].path;
  const target = seed.projects[1].threadIds[0];

  await setScenario(
    harness,
    callerPath,
    threadToolsScenario({
      name: 'tt-window-caller',
      provider: 'claude',
      turns: [
        {
          steps: [
            { call: { tool: 'thread_search', args: { query: big.anchor, limit: 5 } } },
            // A search hit carries thread_id and item_id together, which is
            // exactly what `around` is opened with.
            { capture: { var: 'ITEM', from: '${MCP_RESULT}', pattern: '"item_id":"([^"]+)"' } },
            {
              call: {
                tool: 'thread_show',
                args: { thread_id: target, window: 'around', item_id: '${ITEM}', context: 2 },
              },
            },
            {
              call: {
                tool: 'thread_show',
                args: { thread_id: target, window: 'all', include: ['all'], to_file: true },
                timeoutMs: 120_000,
              },
            },
          ],
          text: 'Opened the middle and wrote the whole thing out.',
        },
        {
          steps: [
            {
              call: {
                tool: 'thread_show',
                args: { thread_id: target, window: 'all', max_bytes: 1_048_576 },
                timeoutMs: 120_000,
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
                timeoutMs: 120_000,
              },
            },
            { capture: { var: 'CURSOR', from: '${MCP_RESULT}', pattern: SHOW_CURSOR_PATTERN } },
          ],
          text: 'Next page.',
        },
      ],
    }),
  );
  await harness.rpc('StartSession', caller);
  await harness.rpc('SendMessage', caller, 'read the middle of the sweep thread', null);

  interface SearchAnswer {
    rows: Array<{ thread_id: string; item_id?: string }>;
  }
  const search = await awaitToolAnswer<SearchAnswer>(harness, {
    tool: 'thread_search',
    timeoutMs: 60_000,
  });
  expect(search.isError, search.text).toBe(false);
  expect(search.value!.rows[0].thread_id).toBe(target);

  interface ShowAnswer {
    window: string;
    transcript?: string;
    items: number;
    bytes: number;
    done: boolean;
    cursor?: string;
    file?: { path: string; size: number; sha256: string };
  }
  // `around` reads the turns surrounding one item, inside the default
  // budget, and never walks the rest of the thread to get there.
  const around = await awaitToolAnswer<ShowAnswer>(harness, {
    tool: 'thread_show',
    timeoutMs: 60_000,
  });
  expect(around.isError, around.text).toBe(false);
  expect(around.value!.window).toBe('around');
  expect(around.value!.done).toBe(true);
  expect(around.value!.items).toBe(big.aroundItems);
  expect(around.value!.bytes).toBeLessThanOrEqual(64 * 1024);
  expect(around.value!.transcript).toContain(big.anchor);
  expect(around.value!.transcript).not.toContain(big.head);
  expect(around.value!.transcript).not.toContain(big.tail);

  const exported = await awaitToolAnswer<ShowAnswer>(harness, {
    tool: 'thread_show',
    timeoutMs: 120_000,
  });
  expect(exported.isError, exported.text).toBe(false);
  expect(exported.value!.transcript).toBeUndefined();
  expect(exported.value!.file!.sha256).toMatch(/^[0-9a-f]{64}$/);
  const rendered = await readFile(exported.value!.file!.path, 'utf8');
  expect(rendered.length).toBe(exported.value!.file!.size);
  expect(rendered).toContain(big.head);
  expect(rendered).toContain(big.anchor);
  expect(rendered).toContain(big.tail);
  // Every turn of the thread is in the file, which is what "the whole
  // window, written whole" means for a thread this size.
  expect(rendered.match(/^--- turn \d+ ---$/gm)!.length).toBe(big.turns);

  // `all` walks to the end through the cursor, a page at a time, and every
  // page is a snapshot: the counts add up to the thread exactly once. One
  // page per turn, because a thread that is still working refuses a send.
  await awaitTurnCompleted(harness, caller);
  const pages: ShowAnswer[] = [];
  let done = false;
  while (!done) {
    await harness.rpc('SendMessage', caller, `page ${pages.length + 1} of the sweep thread`, null);
    const answer = await awaitToolAnswer<ShowAnswer>(harness, {
      tool: 'thread_show',
      timeoutMs: 120_000,
    });
    expect(answer.isError, answer.text).toBe(false);
    const page = answer.value!;
    expect(page.window).toBe('all');
    expect(page.bytes).toBeLessThanOrEqual(1_048_576);
    pages.push(page);
    done = page.done;
    // A budget this size pages a thread of this size in a handful of
    // calls; a loop that runs away is a cursor that stopped advancing.
    expect(pages.length).toBeLessThan(12);
    await awaitTurnCompleted(harness, caller);
  }

  expect(pages.length).toBeGreaterThan(1);
  expect(pages[0].transcript).toContain(big.head);
  expect(pages.reduce((sum, entry) => sum + entry.items, 0)).toBe(big.items);
  const last = pages[pages.length - 1];
  expect(last.transcript).toContain(big.tail);
  expect(last.cursor).toBeUndefined();
});

test('a multi-megabyte tool output is clipped in the transcript and read in ranges by thread_item', async ({
  harness,
}) => {
  test.setTimeout(180_000);
  const bigItem = bigItemFixture('HUGE', 'Fuzzer crash');
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      { name: 'tt-bigitem', repo: {}, threads: [{ title: 'Item caller', provider: 'claude' }] },
      { name: 'tt-bigitem-target', repo: {}, threads: [bigItem.thread] },
    ],
  });
  const caller = seed.projects[0].threadIds[0];
  const callerPath = seed.projects[0].path;
  const target = seed.projects[1].threadIds[0];
  const lastChunk = 16 * 1024;

  await setScenario(
    harness,
    callerPath,
    threadToolsScenario({
      name: 'tt-bigitem-caller',
      provider: 'claude',
      turns: [
        {
          steps: [
            { call: { tool: 'thread_show', args: { thread_id: target, window: 'tail' } } },
            { capture: { var: 'ITEM', from: '${MCP_RESULT}', pattern: 'item_id=([A-Za-z0-9-]+)' } },
            {
              call: {
                tool: 'thread_show',
                args: { thread_id: target, window: 'tail', include: ['tool_outputs'] },
              },
            },
            {
              call: {
                tool: 'thread_item',
                args: { thread_id: target, item_id: '${ITEM}', query: bigItem.needle.trim() },
                timeoutMs: 120_000,
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
              },
            },
            {
              call: {
                tool: 'thread_item',
                args: { thread_id: target, item_id: '${ITEM}', offset: -lastChunk },
              },
            },
          ],
          text: 'Found it.',
        },
      ],
    }),
  );
  await harness.rpc('StartSession', caller);
  await harness.rpc('SendMessage', caller, 'what did the fuzzer crash on?', null);

  interface ShowAnswer {
    transcript?: string;
    done: boolean;
  }
  // Collapsed: the row is one line naming its size and the tool that reads
  // it, and the transcript never carries megabytes for one row.
  const collapsed = await awaitToolAnswer<ShowAnswer>(harness, { tool: 'thread_show' });
  expect(collapsed.isError, collapsed.text).toBe(false);
  expect(collapsed.value!.transcript).toContain('3.8 MB, not shown; read it with thread_item');
  expect(collapsed.value!.transcript!.length).toBeLessThan(64 * 1024);
  expect(collapsed.value!.transcript).not.toContain(bigItem.needle.trim());

  // Included: the row is clipped at the per-item budget and still states
  // the whole size, with the pointer to read the rest.
  const clipped = await awaitToolAnswer<ShowAnswer>(harness, { tool: 'thread_show' });
  expect(clipped.isError, clipped.text).toBe(false);
  expect(clipped.value!.transcript).toMatch(/… clipped at [\d.]+ KB of 3\.8 MB; read the rest with thread_item/);
  expect(clipped.value!.transcript!.length).toBeLessThan(64 * 1024);

  interface ItemAnswer {
    size: number;
    offset: number;
    bytes: number;
    text?: string;
    eof: boolean;
    match_count?: number;
    matches?: Array<{ offset: number; context: string }>;
  }
  const matched = await awaitToolAnswer<ItemAnswer>(harness, {
    tool: 'thread_item',
    timeoutMs: 120_000,
  });
  expect(matched.isError, matched.text).toBe(false);
  expect(matched.value!.size).toBe(bigItem.size);
  expect(matched.value!.match_count).toBe(1);
  expect(matched.value!.matches![0].offset).toBe(bigItem.needleOffset);
  expect(matched.value!.matches![0].context).toContain(bigItem.needle.trim());

  // The range around the match, which is what the query's offset is for.
  const range = await awaitToolAnswer<ItemAnswer>(harness, { tool: 'thread_item' });
  expect(range.isError, range.text).toBe(false);
  expect(range.value!.offset).toBe(bigItem.needleOffset - 512);
  expect(range.value!.bytes).toBe(1024);
  expect(range.value!.text).toContain(bigItem.needle.trim());
  expect(range.value!.text).toContain('pad before');
  expect(range.value!.text).toContain('pad after');
  expect(range.value!.eof).toBe(false);

  // A negative offset reads from the end: the last 16KB of a 3.8MB row.
  const tail = await awaitToolAnswer<ItemAnswer>(harness, { tool: 'thread_item' });
  expect(tail.isError, tail.text).toBe(false);
  expect(tail.value!.offset).toBe(bigItem.size - lastChunk);
  expect(tail.value!.bytes).toBe(lastChunk);
  expect(tail.value!.eof).toBe(true);
  expect(tail.value!.text).toContain('pad after');
  expect(tail.value!.text).not.toContain(bigItem.needle.trim());
});

test('thread_ask answers from a hidden read-only fork that is deleted once it has answered', async ({
  harness,
}) => {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      { name: 'tt-ask-caller', repo: {}, threads: [{ title: 'Ask caller', provider: 'claude' }] },
      {
        name: 'tt-ask-target',
        repo: {},
        // full-access is the contrast: the fork is forced read-only
        // whatever the thread it was cut from runs as.
        threads: [{ title: 'Profiler run', provider: 'claude', runtimeMode: 'full-access' }],
      },
    ],
  });
  const caller = seed.projects[0].threadIds[0];
  const callerPath = seed.projects[0].path;
  const target = seed.projects[1].threadIds[0];
  const targetPath = seed.projects[1].path;

  // The thread being asked runs one real turn first, so the fork has a
  // provider session to be cut from, exactly as a live thread would.
  await setScenario(
    harness,
    targetPath,
    plainScenario({
      name: 'tt-ask-target-script',
      provider: 'claude',
      texts: ['The profiler blames decodeChunk for 68% of the time.'],
    }),
  );
  await harness.rpc('StartSession', target);
  await harness.rpc('SendMessage', target, 'profile the import path', null);
  await harness.waitForEvent('provider:turn_completed');
  const targetThread = await harness.rpc<{ sessionRef: string }>('GetThread', target);
  expect(targetThread.sessionRef).not.toBe('');

  // The fork runs in the source thread's workspace, so replacing that
  // workspace's rule now is what scripts the hidden copy. The source's own
  // mock keeps the script it registered with.
  await setScenario(
    harness,
    targetPath,
    threadToolsScenario({
      name: 'tt-ask-fork',
      provider: 'claude',
      turns: [
        {
          steps: [
            { capture: { var: 'TOKEN', from: '${USER_INPUT}', pattern: FOOTER_TOKEN_PATTERN } },
            {
              call: {
                tool: 'thread_reply',
                args: { token: '${TOKEN}', text: 'decodeChunk held 68% of the profile.' },
              },
            },
          ],
          text: 'Answered from the fork.',
        },
      ],
    }),
  );
  await setScenario(
    harness,
    callerPath,
    threadToolsScenario({
      name: 'tt-ask-caller-script',
      provider: 'claude',
      turns: [
        {
          steps: [
            {
              call: {
                tool: 'thread_ask',
                args: {
                  thread_id: target,
                  question: 'What did the profiler blame?',
                  wait_seconds: 120,
                },
                timeoutMs: 180_000,
              },
            },
          ],
          text: 'Got the answer without touching that thread.',
        },
      ],
    }),
  );

  await harness.rpc('StartSession', caller);
  await harness.rpc('SendMessage', caller, 'ask the profiler thread what it found', null);

  interface AskAnswer {
    token: string;
    kind: string;
    thread_id: string;
    state: string;
    outcome: string;
    answer_kind?: string;
    answer?: string;
  }
  const ask = await awaitToolAnswer<AskAnswer>(harness, {
    tool: 'thread_ask',
    timeoutMs: 120_000,
  });
  expect(ask.isError, ask.text).toBe(false);
  expect(ask.value!.kind).toBe('ask');
  expect(ask.value!.outcome).toBe('settled');
  expect(ask.value!.state).toBe('replied');
  expect(ask.value!.answer_kind).toBe('reply');
  expect(ask.value!.answer).toContain('decodeChunk held 68% of the profile.');
  // The question went to the copy, never to the thread that was asked.
  expect(ask.value!.thread_id).not.toBe(target);

  // The copy was forced read-only: Claude's write tools were stripped from
  // its session, which no settings allow rule can put back. The source's
  // own session, full-access, stripped nothing.
  interface MockRow {
    mockId: string;
    registration: { cwd: string };
    sessionConfig?: { disallowedTools?: string[]; permissionMode?: string };
  }
  const mocks = await harness.rpc<MockRow[]>('HarnessListMocks');
  const inTarget = mocks.filter((mock) => mock.registration.cwd === targetPath);
  expect(inTarget).toHaveLength(2);
  expect(inTarget[0].sessionConfig?.disallowedTools ?? []).toEqual([]);
  expect(inTarget[1].sessionConfig?.disallowedTools).toEqual(
    expect.arrayContaining(['Write', 'Edit', 'NotebookEdit']),
  );

  // The scratch thread is gone once its answer is stored, and the thread
  // it was cut from is untouched.
  await expect
    .poll(async () => (await threadRows(harness)).map((row) => row.id).sort())
    .toEqual([caller, target].sort());
  const rows = await threadRows(harness);
  expect(rows.some((row) => row.mode === 'scratch')).toBe(false);
});

test('thread_status lists the caller requests, watches a thread and carries a late reply', async ({
  harness,
}) => {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: 'tt-status-caller',
        repo: {},
        threads: [{ title: 'Status caller', provider: 'claude' }],
      },
      {
        name: 'tt-status-target',
        repo: {},
        threads: [{ title: 'Status target', provider: 'claude' }],
      },
    ],
  });
  const caller = seed.projects[0].threadIds[0];
  const callerPath = seed.projects[0].path;
  const target = seed.projects[1].threadIds[0];
  const targetPath = seed.projects[1].path;

  // The target ends its first turn without replying, which settles the
  // request as a final rather than a reply, and answers for real on the
  // next turn: the late reply the sender is told to expect.
  await setScenario(
    harness,
    targetPath,
    threadToolsScenario({
      name: 'tt-status-target-script',
      provider: 'claude',
      turns: [
        {
          steps: [{ capture: { var: 'TOKEN', from: '${USER_INPUT}', pattern: FOOTER_TOKEN_PATTERN } }],
          text: 'I will look into it.',
        },
        {
          // A capture binds for the rest of the process, so the token read
          // off turn one's footer is still spellable here.
          steps: [
            {
              call: {
                tool: 'thread_reply',
                args: { token: '${TOKEN}', text: 'The leak is in the cache.' },
              },
            },
          ],
          text: 'Answered late.',
        },
      ],
    }),
  );
  await setScenario(
    harness,
    callerPath,
    threadToolsScenario({
      name: 'tt-status-caller-script',
      provider: 'claude',
      turns: [
        {
          steps: [
            {
              call: {
                tool: 'thread_send',
                args: {
                  thread_id: target,
                  message: 'Where is the leak?',
                  wait_seconds: 0,
                  notify: true,
                },
              },
            },
            { capture: { var: 'TOKEN', from: '${MCP_RESULT}', pattern: RESULT_TOKEN_PATTERN } },
            { call: { tool: 'thread_status', args: {} } },
            {
              call: {
                tool: 'thread_status',
                args: { thread_ids: [target], wait_seconds: 30 },
                timeoutMs: 60_000,
              },
            },
          ],
          text: 'Sent it and went on.',
        },
        {
          // after_revision past every settlement this request can have is
          // how a caller says "only tell me about something new": the wait
          // runs out instead of re-reporting what it already read.
          steps: [
            {
              call: {
                tool: 'thread_status',
                args: { tokens: ['${TOKEN}'], wait_seconds: 3, after_revision: 999999 },
                timeoutMs: 30_000,
              },
            },
          ],
          text: 'Nothing new yet.',
        },
        {
          steps: [{ call: { tool: 'thread_status', args: { tokens: ['${TOKEN}'] } } }],
          text: 'Read the late reply.',
        },
      ],
    }),
  );

  await harness.rpc('StartSession', caller);
  await harness.rpc('SendMessage', caller, 'ask the other thread where the leak is', null);

  interface SendAnswer {
    token: string;
    outcome: string;
    notify: boolean;
  }
  const send = await awaitToolAnswer<SendAnswer>(harness, { tool: 'thread_send' });
  expect(send.isError, send.text).toBe(false);
  expect(send.value!.outcome).toBe('backgrounded');
  expect(send.value!.notify).toBe(true);
  const token = send.value!.token;

  interface StatusAnswer {
    requests?: Array<{
      token: string;
      kind: string;
      state: string;
      thread_id?: string;
      answer_kind?: string;
      answer?: string;
      revision: number;
      notify: boolean;
    }>;
    threads?: Array<{ thread_id: string; state: string; resting: boolean }>;
    woke_on?: string;
    timed_out?: boolean;
  }
  // No arguments: the listing that recovers a lost token.
  const listing = await awaitToolAnswer<StatusAnswer>(harness, { tool: 'thread_status' });
  expect(listing.isError, listing.text).toBe(false);
  const listed = listing.value!.requests!.find((row) => row.token === token)!;
  expect(listed).toBeDefined();
  expect(listed.kind).toBe('send');
  expect(listed.thread_id).toBe(target);

  // thread_ids watches a thread the caller never had to message.
  const watched = await awaitToolAnswer<StatusAnswer>(harness, {
    tool: 'thread_status',
    timeoutMs: 60_000,
  });
  expect(watched.isError, watched.text).toBe(false);
  expect(watched.value!.threads![0].thread_id).toBe(target);
  expect(watched.value!.requests).toBeUndefined();

  // The target ended its turn without replying, so the sender is woken
  // with its final text marked as not a reply.
  const finalWake = await harness.waitForEvent<HarnessMockEvent>(
    'harness:mock',
    (ev) =>
      ev.report.kind === 'user_input' &&
      ev.cwd === callerPath &&
      (ev.report.input ?? '').includes('finished its turn without calling thread_reply'),
    60_000,
  );
  expect(finalWake.report.input).toContain('I will look into it.');

  const suppressed = await awaitToolAnswer<StatusAnswer>(harness, {
    tool: 'thread_status',
    timeoutMs: 30_000,
  });
  expect(suppressed.isError, suppressed.text).toBe(false);
  expect(suppressed.value!.timed_out).toBe(true);
  expect(suppressed.value!.woke_on ?? '').toBe('');
  const seen = suppressed.value!.requests![0];
  expect(seen.state).toBe('finished');
  expect(seen.answer_kind).toBe('final');

  // The late reply: the responder answers a token whose request already
  // settled, and it still reaches the sender.
  await harness.rpc('SendMessage', target, 'now answer the caller properly', null);
  interface ReplyAnswer {
    token: string;
    accepted: boolean;
    state: string;
    late?: boolean;
    revision: number;
  }
  const reply = await awaitToolAnswer<ReplyAnswer>(harness, { tool: 'thread_reply' });
  expect(reply.isError, reply.text).toBe(false);
  expect(reply.value!.accepted).toBe(true);
  expect(reply.value!.late).toBe(true);
  // The request keeps the state it settled with; the reply rides on top
  // of it as a follow-up.
  expect(reply.value!.state).toBe('finished');

  const lateWake = await harness.waitForEvent<HarnessMockEvent>(
    'harness:mock',
    (ev) =>
      ev.report.kind === 'user_input' &&
      ev.cwd === callerPath &&
      (ev.report.input ?? '').includes('The leak is in the cache.'),
    60_000,
  );
  expect(lateWake.report.input).toContain(token);

  const settled = await awaitToolAnswer<StatusAnswer>(harness, { tool: 'thread_status' });
  expect(settled.isError, settled.text).toBe(false);
  const row = settled.value!.requests![0];
  expect(row.state).toBe('finished');
  // The newest revision is the answer: the late reply supersedes the
  // final message the caller was already told about.
  expect(row.answer_kind).toBe('reply');
  expect(row.answer).toContain('The leak is in the cache.');
  expect(row.revision).toBeGreaterThan(seen.revision);
});

test('thread_cancel takes a queued message back and interrupts a turn it started', async ({
  harness,
}) => {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: 'tt-cancel-caller',
        repo: {},
        threads: [
          { title: 'Cancel caller', provider: 'claude' },
          { title: 'Bystander', provider: 'claude' },
        ],
      },
      {
        // This thread is mid-turn with no session behind it, the state a
        // restart leaves: a message sent to it stays on the durable queue
        // instead of reaching a provider, which is the message a cancel
        // can still take back.
        name: 'tt-cancel-queued',
        repo: {},
        threads: [
          {
            title: 'Cancel queued target',
            provider: 'claude',
            turns: [{ userText: 'work that never finished', incomplete: true }],
          },
        ],
      },
      {
        name: 'tt-cancel-busy',
        repo: {},
        threads: [{ title: 'Cancel busy target', provider: 'claude' }],
      },
    ],
  });
  const caller = seed.projects[0].threadIds[0];
  const bystander = seed.projects[0].threadIds[1];
  const callerPath = seed.projects[0].path;
  const queuedTarget = seed.projects[1].threadIds[0];
  const busyTarget = seed.projects[2].threadIds[0];
  const busyPath = seed.projects[2].path;

  // The busy thread parks inside the turn the request starts, so the
  // interrupt has something to stop.
  await setScenario(
    harness,
    busyPath,
    threadToolsScenario({
      name: 'tt-cancel-busy-script',
      provider: 'claude',
      turns: [{ steps: [{ gate: 'hold-busy' }], text: 'Long job done.' }],
    }),
  );
  await setScenario(
    harness,
    callerPath,
    threadToolsScenario({
      name: 'tt-cancel-caller-script',
      provider: 'claude',
      turns: [
        {
          steps: [
            {
              call: {
                tool: 'thread_send',
                args: {
                  thread_id: queuedTarget,
                  message: 'queued work nobody wants',
                  wait_seconds: 0,
                },
              },
            },
            { capture: { var: 'TOKEN', from: '${MCP_RESULT}', pattern: RESULT_TOKEN_PATTERN } },
          ],
          text: 'Queued it.',
        },
        {
          steps: [
            { call: { tool: 'thread_cancel', args: { token: '${TOKEN}' } } },
            // A thread this caller never spawned, sent to or asked.
            { call: { tool: 'thread_cancel', args: { thread_id: bystander } } },
          ],
          text: 'Took it back.',
        },
        {
          steps: [
            {
              call: {
                tool: 'thread_send',
                args: { thread_id: busyTarget, message: 'start the long job', wait_seconds: 0 },
              },
            },
          ],
          text: 'Started it.',
        },
        {
          steps: [{ call: { tool: 'thread_cancel', args: { thread_id: busyTarget } } }],
          text: 'Stopped it.',
        },
      ],
    }),
  );

  await harness.rpc('StartSession', caller);
  await harness.rpc('SendMessage', caller, 'send something we can take back', null);

  interface SendAck {
    token: string;
    state: string;
    outcome: string;
  }
  const queuedAck = await awaitToolAnswer<SendAck>(harness, { tool: 'thread_send' });
  expect(queuedAck.isError, queuedAck.text).toBe(false);
  expect(queuedAck.value!.outcome).toBe('backgrounded');

  interface QueuedMessage {
    id: string;
    sendId?: string;
    message: string;
  }
  await expect
    .poll(async () => await harness.rpc<QueuedMessage[]>('GetQueueState', queuedTarget))
    .toHaveLength(1);
  const queued = await harness.rpc<QueuedMessage[]>('GetQueueState', queuedTarget);
  expect(queued[0].message).toContain('queued work nobody wants');
  expect(queued[0].sendId).toBe(`thread-request:${queuedAck.value!.token}`);

  interface CancelAnswer {
    token?: string;
    thread_id?: string;
    state: string;
    effect: string;
  }
  await awaitTurnCompleted(harness, caller);
  await harness.rpc('SendMessage', caller, 'take it back', null);
  const byToken = await awaitToolAnswer<CancelAnswer>(harness, { tool: 'thread_cancel' });
  expect(byToken.isError, byToken.text).toBe(false);
  expect(byToken.value!.token).toBe(queuedAck.value!.token);
  expect(byToken.value!.effect).toBe('queued_message_removed');
  expect(byToken.value!.state).toBe('cancelled');
  expect(
    (await harness.rpc<QueuedMessage[] | null>('GetQueueState', queuedTarget)) ?? [],
  ).toHaveLength(0);

  const refused = await awaitToolAnswer<CancelAnswer>(harness, { tool: 'thread_cancel' });
  expect(refused.isError).toBe(true);
  expect(refused.text).toContain('thread_not_yours');

  // The other half: a turn this caller's own message started, stopped by
  // thread id.
  await awaitTurnCompleted(harness, caller);
  await harness.rpc('SendMessage', caller, 'give the other thread the long job', null);
  const busyMock = await awaitGate(harness, 'hold-busy', busyPath);
  expect(busyMock.mockId).not.toBe('');
  const startedAck = await awaitToolAnswer<SendAck>(harness, { tool: 'thread_send' });
  expect(startedAck.isError, startedAck.text).toBe(false);

  // The interrupt reaches the provider process itself, so the wait is
  // registered before the cancel can land.
  const interrupted = harness.waitForEvent<HarnessMockEvent>(
    'harness:mock',
    (ev) => ev.report.kind === 'turn_interrupted' && ev.cwd === busyPath,
  );
  await awaitTurnCompleted(harness, caller);
  await harness.rpc('SendMessage', caller, 'now stop it', null);
  const byThread = await awaitToolAnswer<CancelAnswer>(harness, {
    tool: 'thread_cancel',
    timeoutMs: 60_000,
  });
  expect(byThread.isError, byThread.text).toBe(false);
  expect(byThread.value!.thread_id).toBe(busyTarget);
  expect(byThread.value!.effect).toBe('turn_interrupted');
  await interrupted;

  // The parked turn never reaches its own completion text.
  const busyItems = await harness.rpc<Array<{ summary?: string }>>('ListItems', busyTarget, true);
  expect(busyItems.some((item) => (item.summary ?? '').includes('Long job done.'))).toBe(false);
});

test('thread_remind wakes the caller later and a pending reminder can be cancelled', async ({
  harness,
}) => {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      { name: 'tt-remind', repo: {}, threads: [{ title: 'Deploy watcher', provider: 'claude' }] },
    ],
  });
  const caller = seed.projects[0].threadIds[0];
  const callerPath = seed.projects[0].path;

  await setScenario(
    harness,
    callerPath,
    threadToolsScenario({
      name: 'tt-remind-script',
      provider: 'claude',
      turns: [
        {
          steps: [
            // The far one is the reminder the next turn takes back; the
            // near one is the wake that starts that turn.
            {
              call: {
                tool: 'thread_remind',
                args: { after_seconds: 3600, note: 'next week, if the deploy is still out' },
              },
            },
            { capture: { var: 'LATER', from: '${MCP_RESULT}', pattern: RESULT_TOKEN_PATTERN } },
            {
              call: {
                tool: 'thread_remind',
                args: { after_seconds: 1, note: 'check whether the deploy finished' },
              },
            },
          ],
          text: 'Two reminders set.',
        },
        {
          steps: [
            { call: { tool: 'thread_status', args: { tokens: ['${LATER}'] } } },
            { call: { tool: 'thread_cancel', args: { token: '${LATER}' } } },
          ],
          text: 'Took the other one back.',
        },
        {
          steps: [{ call: { tool: 'thread_status', args: {} } }],
          text: 'Nothing else is outstanding.',
        },
      ],
    }),
  );

  await harness.rpc('StartSession', caller);
  await harness.rpc('SendMessage', caller, 'watch the deploy and remind me', null);

  interface RemindAck {
    token: string;
    kind?: string;
    state: string;
    outcome: string;
    notify: boolean;
  }
  const later = await awaitToolAnswer<RemindAck>(harness, { tool: 'thread_remind', timeoutMs: 60_000 });
  expect(later.isError, later.text).toBe(false);
  expect(later.value!.state).toBe('accepted');
  expect(later.value!.outcome).toBe('backgrounded');
  const soon = await awaitToolAnswer<RemindAck>(harness, { tool: 'thread_remind', timeoutMs: 60_000 });
  expect(soon.isError, soon.text).toBe(false);
  expect(soon.value!.token).not.toBe(later.value!.token);
  await awaitTurnCompleted(harness, caller);

  // The reminder fires into this same thread as a user message and starts
  // its next turn.
  const wake = await harness.waitForEvent<HarnessMockEvent>(
    'harness:mock',
    (ev) =>
      ev.report.kind === 'user_input' &&
      ev.cwd === callerPath &&
      (ev.report.input ?? '').includes('Reminder you set'),
    60_000,
  );
  expect(wake.report.input).toContain('check whether the deploy finished');
  expect(wake.report.input).toContain(soon.value!.token);

  interface StatusAnswer {
    requests?: Array<{ token: string; kind: string; state: string; target_thread_id?: string }>;
  }
  const status = await awaitToolAnswer<StatusAnswer>(harness, {
    tool: 'thread_status',
    timeoutMs: 60_000,
  });
  expect(status.isError, status.text).toBe(false);
  const pending = status.value!.requests![0];
  expect(pending.token).toBe(later.value!.token);
  expect(pending.kind).toBe('remind');
  expect(pending.state).toBe('accepted');

  interface CancelAnswer {
    token?: string;
    state: string;
    effect: string;
  }
  const cancelled = await awaitToolAnswer<CancelAnswer>(harness, {
    tool: 'thread_cancel',
    timeoutMs: 60_000,
  });
  expect(cancelled.isError, cancelled.text).toBe(false);
  expect(cancelled.value!.token).toBe(later.value!.token);
  // A reminder is only a row, so cancelling it removes it rather than
  // settling it.
  expect(cancelled.value!.effect).toBe('reminder_dropped');
  expect(cancelled.value!.state).toBe('cancelled');

  // The caller's own ledger is how it recovers its tokens: the fired
  // reminder is there, settled, and the cancelled one is gone. The wake's
  // turn has to end first: a composer send into a working thread is refused.
  await awaitTurnCompleted(harness, caller);
  await harness.rpc('SendMessage', caller, 'what is still outstanding?', null);
  const ledger = await awaitToolAnswer<StatusAnswer>(harness, {
    tool: 'thread_status',
    timeoutMs: 60_000,
  });
  expect(ledger.isError, ledger.text).toBe(false);
  const tokens = (ledger.value!.requests ?? []).map((row) => row.token);
  expect(tokens).toContain(soon.value!.token);
  expect(tokens).not.toContain(later.value!.token);
});

/** The five threads one thread_update call archives together. */
const SWEEP_TITLES = ['Sweep one', 'Sweep two', 'Sweep three', 'Sweep four', 'Sweep five'];

test('thread_update and thread_group organize the sidebar and refuse what the sidebar refuses', async ({
  harness,
  page,
}) => {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: 'tt-organize',
        repo: {},
        // Each organized thread carries history: a thread with no
        // messages is still a draft and the sidebar does not list it.
        threads: [
          ...[
            'Organize caller',
            'Alpha notes',
            'Beta notes',
            'Gamma notes',
            'Delta notes',
            ...SWEEP_TITLES,
          ].map((title) => ({
            title,
            provider: 'claude',
            turns: [
              { userText: `notes for ${title}`, items: [{ kind: 'assistant_text', summary: 'ok' }] },
            ],
          })),
        ],
      },
    ],
  });
  const { projectId, path: callerPath, threadIds } = seed.projects[0];
  const [caller, alpha, beta, gamma, delta] = threadIds;
  const sweep = threadIds.slice(5);
  expect(sweep).toHaveLength(SWEEP_TITLES.length);

  await setScenario(
    harness,
    callerPath,
    threadToolsScenario({
      name: 'tt-organize-script',
      provider: 'claude',
      turns: [
        {
          steps: [
            {
              call: {
                tool: 'thread_update',
                args: { thread_ids: [alpha], title: 'Renamed by the agent', pin: 'front' },
              },
            },
            {
              call: {
                tool: 'thread_update',
                args: { thread_ids: [beta, gamma], group: 'Release work' },
              },
            },
            // Its own id is refused per row; the other id still applies.
            {
              call: {
                tool: 'thread_update',
                args: { thread_ids: [caller, delta], archived: true },
              },
            },
            // One call covers "archive these five", which is the shape the
            // tool exists for.
            {
              call: {
                tool: 'thread_update',
                args: { thread_ids: sweep, archived: true },
              },
            },
            // A group carries the pin, so the two together are refused for
            // the whole call.
            {
              call: {
                tool: 'thread_update',
                args: { thread_ids: [beta], group: 'Release work', pin: 'front' },
              },
            },
            {
              call: {
                tool: 'thread_group',
                args: { group: 'Release work', project_id: projectId, pin: 'front' },
              },
            },
            { capture: { var: 'GROUP', from: '${MCP_RESULT}', pattern: '"group_id":"([^"]+)"' } },
            { call: { tool: 'thread_group', args: { group_id: '${GROUP}', rename: 'Release 27' } } },
          ],
          text: 'Sorted.',
        },
        {
          steps: [{ call: { tool: 'thread_group', args: { group_id: '${GROUP}', delete: true } } }],
          text: 'Group gone.',
        },
      ],
    }),
  );

  await harness.open(page);
  await harness.rpc('StartSession', caller);
  await harness.rpc('SendMessage', caller, 'tidy up the notes threads', null);

  interface UpdateAnswer {
    results: Array<{
      thread_id: string;
      title?: string;
      updated: boolean;
      error?: string;
      error_code?: string;
    }>;
    note?: string;
  }
  const renamed = await awaitToolAnswer<UpdateAnswer>(harness, { tool: 'thread_update' });
  expect(renamed.isError, renamed.text).toBe(false);
  expect(renamed.value!.results[0]).toMatchObject({
    thread_id: alpha,
    title: 'Renamed by the agent',
    updated: true,
  });

  const grouped = await awaitToolAnswer<UpdateAnswer>(harness, { tool: 'thread_update' });
  expect(grouped.value!.results.map((row) => row.updated)).toEqual([true, true]);

  const archived = await awaitToolAnswer<UpdateAnswer>(harness, { tool: 'thread_update' });
  expect(archived.value!.results[0]).toMatchObject({
    thread_id: caller,
    updated: false,
    error_code: 'thread_is_caller',
  });
  expect(archived.value!.results[1]).toMatchObject({ thread_id: delta, updated: true });
  expect(archived.value!.note).toContain('left untouched');

  const archivedFive = await awaitToolAnswer<UpdateAnswer>(harness, { tool: 'thread_update' });
  expect(archivedFive.isError, archivedFive.text).toBe(false);
  expect(archivedFive.value!.results).toHaveLength(5);
  expect(archivedFive.value!.results.map((row) => row.thread_id)).toEqual(sweep);
  expect(archivedFive.value!.results.every((row) => row.updated)).toBe(true);
  expect(archivedFive.value!.results.every((row) => !row.error)).toBe(true);

  const contradictory = await awaitToolAnswer<UpdateAnswer>(harness, { tool: 'thread_update' });
  expect(contradictory.isError).toBe(true);
  expect(contradictory.text).toContain('thread_grouped');

  interface GroupAnswer {
    group_id: string;
    group: string;
    action: string;
    pin?: string;
    ungrouped?: number;
    note?: string;
  }
  const pinned = await awaitToolAnswer<GroupAnswer>(harness, { tool: 'thread_group' });
  expect(pinned.isError, pinned.text).toBe(false);
  expect(pinned.value!.action).toBe('pinned');
  expect(pinned.value!.pin).toBe('front');
  const groupId = pinned.value!.group_id;

  const renamedGroup = await awaitToolAnswer<GroupAnswer>(harness, { tool: 'thread_group' });
  expect(renamedGroup.value!.action).toBe('renamed');
  expect(renamedGroup.value!.group).toBe('Release 27');

  // The sidebar is the same surface the user organizes by hand, and it
  // shows every one of those writes without a reload.
  const groupRow = page.getByTestId('thread-group-row');
  await expect(groupRow.getByTestId('thread-group-row-name')).toHaveText('Release 27');
  await expect(page.getByTestId('thread-row').filter({ hasText: 'Renamed by the agent' })).toBeVisible();
  await expect(page.getByTestId('thread-row').filter({ hasText: 'Alpha notes' })).toHaveCount(0);
  // Archived threads leave the list; the caller refused to archive itself
  // and is still there.
  await expect(page.getByTestId('thread-row').filter({ hasText: 'Delta notes' })).toHaveCount(0);
  await expect(page.getByTestId('thread-row').filter({ hasText: 'Organize caller' })).toBeVisible();

  const rows = await threadRows(harness);
  const byId = new Map(rows.map((row) => [row.id, row]));
  expect(byId.get(alpha)!.title).toBe('Renamed by the agent');
  expect(byId.get(alpha)!.pinnedAt).toBeTruthy();
  expect(byId.get(beta)!.groupId).toBe(groupId);
  expect(byId.get(gamma)!.groupId).toBe(groupId);
  // The row listing skips archived threads, so the archived one is simply
  // gone from it and the caller, which refused to archive itself, is not.
  expect(byId.has(delta)).toBe(false);
  expect(byId.get(caller)!.archived).toBe(false);
  // All five of the one call are gone from the listing and the sidebar.
  expect(sweep.filter((id) => byId.has(id))).toEqual([]);
  for (const title of SWEEP_TITLES) {
    await expect(page.getByTestId('thread-row').filter({ hasText: title })).toHaveCount(0);
  }

  // Deleting a group ungroups its threads, exactly as the sidebar does.
  await awaitTurnCompleted(harness, caller);
  await harness.rpc('SendMessage', caller, 'drop the group again', null);
  const deleted = await awaitToolAnswer<GroupAnswer>(harness, {
    tool: 'thread_group',
    timeoutMs: 60_000,
  });
  expect(deleted.isError, deleted.text).toBe(false);
  expect(deleted.value!.action).toBe('deleted');
  expect(deleted.value!.ungrouped).toBe(2);
  expect(deleted.value!.note).toContain('ungrouped, not deleted');

  await expect(page.getByTestId('thread-group-row')).toHaveCount(0);
  await expect(page.getByTestId('thread-row').filter({ hasText: 'Beta notes' })).toBeVisible();
  const after = await threadRows(harness);
  expect(after.find((row) => row.id === beta)!.groupId).toBeFalsy();
});

test('thread_spawn cuts a worktree and forks an existing thread when asked to', async ({
  harness,
}) => {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: 'tt-spawn-extra',
        repo: {},
        threads: [{ title: 'Spawn options caller', provider: 'claude' }],
      },
      {
        name: 'tt-spawn-source',
        repo: {},
        threads: [{ title: 'Fork source', provider: 'claude' }],
      },
    ],
  });
  const caller = seed.projects[0].threadIds[0];
  const callerPath = seed.projects[0].path;
  const sourceProject = seed.projects[1].projectId;
  const source = seed.projects[1].threadIds[0];
  const sourcePath = seed.projects[1].path;

  await setScenario(
    harness,
    sourcePath,
    plainScenario({
      name: 'tt-spawn-source-script',
      provider: 'claude',
      texts: ['The original approach used a mutex.'],
    }),
  );
  // The fork is cut from a live session, so the source runs one real turn
  // before anything forks it.
  await harness.rpc('StartSession', source);
  await harness.rpc('SendMessage', source, 'explain the approach you took', null);
  await awaitTurnCompleted(harness, source);
  expect((await harness.rpc<{ sessionRef: string }>('GetThread', source)).sessionRef).not.toBe('');

  await setScenario(
    harness,
    callerPath,
    threadToolsScenario({
      name: 'tt-spawn-extra-script',
      provider: 'claude',
      turns: [
        {
          steps: [
            {
              call: {
                tool: 'thread_spawn',
                args: {
                  prompt: 'Try the lock-free version on a branch of your own.',
                  title: 'Worktree work',
                  worktree: 'agent/experiment',
                  // The local head of the current branch, no fetch: the
                  // harness project has no origin to fetch from.
                  base_local: true,
                  group: 'Approaches',
                  wait_seconds: 0,
                },
              },
            },
          ],
          text: 'Branch thread opened.',
        },
        {
          steps: [
            {
              call: {
                tool: 'thread_spawn',
                args: {
                  prompt: 'Take the same history and try the channel version instead.',
                  title: 'Second approach',
                  from_thread: source,
                  group: 'Approaches',
                  wait_seconds: 0,
                },
              },
            },
          ],
          text: 'Fork opened.',
        },
      ],
    }),
  );

  await harness.rpc('StartSession', caller);
  await harness.rpc('SendMessage', caller, 'open a thread on its own branch', null);

  interface SpawnAnswer {
    token: string;
    thread_id: string;
    outcome: string;
  }
  const worktreeSpawn = await awaitToolAnswer<SpawnAnswer>(harness, { tool: 'thread_spawn' });
  expect(worktreeSpawn.isError, worktreeSpawn.text).toBe(false);
  const worktreeThread = worktreeSpawn.value!.thread_id;

  await expect
    .poll(async () => (await threadRows(harness)).find((row) => row.id === worktreeThread)?.branch)
    .toBe('agent/experiment');
  const worktreeRow = (await threadRows(harness)).find((row) => row.id === worktreeThread)!;
  expect(worktreeRow.title).toBe('Worktree work');
  // The thread's workspace IS the fresh checkout, not the project root.
  expect(worktreeRow.worktreePath).toBeTruthy();
  expect(worktreeRow.worktreePath).not.toBe(callerPath);
  expect(worktreeRow.workspacePath).toBe(worktreeRow.worktreePath);
  // The group the call named was created in the caller's project and
  // holds the new thread.
  expect(worktreeRow.groupId).toBeTruthy();

  // The new thread really runs in the checkout that was cut for it.
  const inWorktree = await harness.waitForEvent<HarnessMockEvent>(
    'harness:mock',
    (ev) => ev.report.kind === 'user_input' && ev.cwd === worktreeRow.worktreePath,
    60_000,
  );
  expect(inWorktree.report.input).toContain('Try the lock-free version');

  await awaitTurnCompleted(harness, caller);
  await harness.rpc('SendMessage', caller, 'now fork the source thread', null);
  const forkSpawn = await awaitToolAnswer<SpawnAnswer>(harness, {
    tool: 'thread_spawn',
    timeoutMs: 60_000,
  });
  expect(forkSpawn.isError, forkSpawn.text).toBe(false);
  const forked = forkSpawn.value!.thread_id;

  // A fork keeps its source's project and checkout, and carries its
  // history: the prompt lands on top of the conversation it was cut from.
  const forkRow = (await threadRows(harness)).find((row) => row.id === forked)!;
  expect(forkRow.forkedFromThreadId).toBe(source);
  expect(forkRow.projectId).toBe(sourceProject);
  expect(forkRow.workspacePath).toBe(sourcePath);
  expect(forkRow.title).toBe('Second approach');
  // The same group name in the fork's own project is a different group.
  expect(forkRow.groupId).toBeTruthy();
  expect(forkRow.groupId).not.toBe(worktreeRow.groupId);

  const items = await harness.rpc<Array<{ summary?: string }>>('ListItems', forked, true);
  expect(items.some((item) => (item.summary ?? '').includes('The original approach used a mutex.'))).toBe(
    true,
  );
  expect(
    items.some((item) => (item.summary ?? '').includes('Take the same history and try the channel version')),
  ).toBe(true);
});

test('thread_spawn forks a live Codex thread on a process of its own, and the fork runs', async ({
  harness,
}) => {
  // A Codex `thread/fork` loads the CHILD into the app-server that
  // answered and keeps its writer lock until that process exits. Cut on
  // the source's live session, the fork's first send would be refused as
  // "open in another Codex process". The mock holds the same lock, so this
  // spec fails the same way the product did if the cut ever moves back.
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: 'tt-fork-codex-caller',
        repo: {},
        threads: [{ title: 'Codex fork caller', provider: 'claude' }],
      },
      {
        name: 'tt-fork-codex-source',
        repo: {},
        threads: [{ title: 'Live Codex source', provider: 'codex' }],
      },
    ],
  });
  const caller = seed.projects[0].threadIds[0];
  const callerPath = seed.projects[0].path;
  const source = seed.projects[1].threadIds[0];
  const sourcePath = seed.projects[1].path;

  await setScenario(
    harness,
    sourcePath,
    plainScenario({
      name: 'tt-fork-codex-source-script',
      provider: 'codex',
      texts: ['The Codex source answered first.'],
    }),
  );
  await harness.rpc('StartSession', source);
  await harness.rpc('SendMessage', source, 'answer once, then stay up', null);
  await awaitTurnCompleted(harness, source);
  const sourceRef = (await harness.rpc<{ sessionRef: string }>('GetThread', source)).sessionRef;
  expect(sourceRef).not.toBe('');

  await setScenario(
    harness,
    callerPath,
    threadToolsScenario({
      name: 'tt-fork-codex-caller-script',
      provider: 'claude',
      turns: [
        {
          steps: [
            {
              call: {
                tool: 'thread_spawn',
                args: {
                  prompt: 'Carry on from the Codex history.',
                  title: 'Codex continuation',
                  from_thread: source,
                  wait_seconds: 0,
                },
              },
            },
          ],
          text: 'Codex fork opened.',
        },
      ],
    }),
  );

  await harness.rpc('StartSession', caller);
  await harness.rpc('SendMessage', caller, 'fork the live codex thread', null);
  const spawn = await awaitToolAnswer<{ thread_id: string }>(harness, {
    tool: 'thread_spawn',
    timeoutMs: 60_000,
  });
  expect(spawn.isError, spawn.text).toBe(false);
  const forked = spawn.value!.thread_id;

  // The prompt reaches the fork's OWN Codex thread: a fresh id minted from
  // the source's, resumed in a fresh process while the source stays live.
  const delivered = await harness.waitForEvent<HarnessMockEvent>(
    'harness:mock',
    (ev) =>
      ev.report.kind === 'user_input' &&
      ev.cwd === sourcePath &&
      (ev.report.input ?? '').includes('Carry on from the Codex history.'),
    60_000,
  );
  expect(delivered.report.sessionRef).toMatch(new RegExp(`^${sourceRef}-fork-\\d+-[0-9a-f]{4}$`));
  const forkRow = (await threadRows(harness)).find((row) => row.id === forked)!;
  expect(forkRow.forkedFromThreadId).toBe(source);
  const items = await harness.rpc<Array<{ kind: string; summary?: string }>>('ListItems', forked, true);
  expect(items.filter((item) => item.kind === 'error').map((item) => item.summary)).toEqual([]);
  expect(items.some((item) => (item.summary ?? '').includes('The Codex source answered first.'))).toBe(true);

  // The source kept its session and its thread: a later message still
  // runs there, on the same Codex thread id.
  await harness.rpc('SendMessage', source, 'and again', null);
  const again = await harness.waitForEvent<HarnessMockEvent>(
    'harness:mock',
    (ev) =>
      ev.report.kind === 'user_input' && ev.cwd === sourcePath && (ev.report.input ?? '').includes('and again'),
    30_000,
  );
  expect(again.report.sessionRef).toBe(sourceRef);
});

test('a wait ends as blocked when the target stops to ask the user', async ({ harness }) => {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      { name: 'tt-blocked-caller', repo: {}, threads: [{ title: 'Blocked caller', provider: 'claude' }] },
      {
        name: 'tt-blocked-target',
        repo: {},
        threads: [
          { title: 'Approval target', provider: 'claude', runtimeMode: 'approval-required' },
        ],
      },
    ],
  });
  const caller = seed.projects[0].threadIds[0];
  const callerPath = seed.projects[0].path;
  const target = seed.projects[1].threadIds[0];
  const targetPath = seed.projects[1].path;
  const notePath = `${targetPath}/note.txt`;

  // The target stops on a write it needs a person to allow.
  await setScenario(
    harness,
    targetPath,
    threadToolsScenario({
      name: 'tt-blocked-target-script',
      provider: 'claude',
      turns: [
        {
          steps: [
            {
              emitLines: [
                JSON.stringify({
                  type: 'assistant',
                  message: {
                    id: 'msg-write',
                    role: 'assistant',
                    model: 'claude-mock-1',
                    content: [
                      {
                        type: 'tool_use',
                        id: 'tu-write',
                        name: 'Write',
                        input: { file_path: notePath, content: 'hello' },
                      },
                    ],
                  },
                }),
              ],
            },
            {
              approval: {
                toolName: 'Write',
                input: { file_path: notePath, content: 'hello' },
                toolUseId: 'tu-write',
                onAllow: [
                  {
                    emitLines: [
                      JSON.stringify({
                        type: 'user',
                        message: {
                          role: 'user',
                          content: [
                            {
                              type: 'tool_result',
                              tool_use_id: 'tu-write',
                              content: 'File created successfully.',
                            },
                          ],
                        },
                      }),
                    ],
                  },
                ],
                onDeny: [],
              },
            },
          ],
          text: 'Wrote the note.',
        },
      ],
    }),
  );
  await setScenario(
    harness,
    callerPath,
    threadToolsScenario({
      name: 'tt-blocked-caller-script',
      provider: 'claude',
      turns: [
        {
          steps: [
            {
              call: {
                tool: 'thread_send',
                // Long enough that only the block can end it.
                args: { thread_id: target, message: 'write the note', wait_seconds: 45 },
                timeoutMs: 90_000,
              },
            },
            { capture: { var: 'TOKEN', from: '${MCP_RESULT}', pattern: RESULT_TOKEN_PATTERN } },
          ],
          text: 'It is waiting on you.',
        },
        {
          steps: [
            {
              call: {
                tool: 'thread_status',
                args: { tokens: ['${TOKEN}'], wait_seconds: 45 },
                timeoutMs: 90_000,
              },
            },
          ],
          text: 'It finished after you answered.',
        },
      ],
    }),
  );

  const asked = harness.waitForEvent<HarnessMockEvent>(
    'harness:mock',
    (ev) => ev.report.kind === 'approval_pending' && ev.cwd === targetPath,
    60_000,
  );
  const approval = harness.waitForEvent<{
    action: string;
    threadId?: string;
    request?: { requestId: string; threadId: string };
  }>('provider:approval', (ev) => ev.action === 'request' && ev.request?.threadId === target);

  await harness.rpc('StartSession', caller);
  await harness.rpc('SendMessage', caller, 'have the other thread write the note', null);

  interface SendAck {
    token: string;
    state: string;
    outcome: string;
    notify: boolean;
  }
  const blocked = await awaitToolAnswer<SendAck>(harness, {
    tool: 'thread_send',
    timeoutMs: 90_000,
  });
  expect(blocked.isError, blocked.text).toBe(false);
  // The person owns the answer now, so the wait ends without settling the
  // request. Like every other end of a positive wait, it arms the wake: the
  // answer arrives as a message unless a later wait takes delivery first.
  expect(blocked.value!.outcome).toBe('blocked');
  expect(blocked.value!.state).toBe('blocked');
  expect(blocked.value!.notify).toBe(true);
  await asked;

  // Answering the prompt lets the turn finish, and the caller's own watch
  // is what picks the settlement up.
  const pending = await approval;
  await awaitTurnCompleted(harness, caller);
  await harness.rpc('SendMessage', caller, 'watch it until it is done', null);
  await harness.rpc('RespondToApproval', target, {
    requestId: pending.request!.requestId,
    decision: 'allow',
  });

  interface StatusAnswer {
    requests?: Array<{ token: string; state: string; answer_kind?: string; answer?: string }>;
    woke_on?: string;
  }
  const settled = await awaitToolAnswer<StatusAnswer>(harness, {
    tool: 'thread_status',
    timeoutMs: 90_000,
  });
  expect(settled.isError, settled.text).toBe(false);
  const row = settled.value!.requests![0];
  expect(row.token).toBe(blocked.value!.token);
  expect(row.state).toBe('finished');
  expect(row.answer).toContain('Wrote the note.');
});

test('a backgrounded ask arrives as a message and thread_status returns the same answer', async ({
  harness,
}) => {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      { name: 'tt-bg-caller', repo: {}, threads: [{ title: 'Background caller', provider: 'claude' }] },
      { name: 'tt-bg-target', repo: {}, threads: [{ title: 'Retry budget', provider: 'claude' }] },
    ],
  });
  const caller = seed.projects[0].threadIds[0];
  const callerPath = seed.projects[0].path;
  const target = seed.projects[1].threadIds[0];
  const targetPath = seed.projects[1].path;

  // One real turn first, so the ask has a session to fork.
  await setScenario(
    harness,
    targetPath,
    plainScenario({
      name: 'tt-bg-target-script',
      provider: 'claude',
      texts: ['The retry budget is three attempts.'],
    }),
  );
  await harness.rpc('StartSession', target);
  await harness.rpc('SendMessage', target, 'look at the retry budget', null);
  await awaitTurnCompleted(harness, target);

  // The fork holds its answer past the caller's wait, so the ask
  // backgrounds and the answer is owed as a message instead.
  await setScenario(
    harness,
    targetPath,
    threadToolsScenario({
      name: 'tt-bg-fork',
      provider: 'claude',
      turns: [
        {
          steps: [
            { capture: { var: 'TOKEN', from: '${USER_INPUT}', pattern: FOOTER_TOKEN_PATTERN } },
            { gate: 'hold-answer' },
            {
              call: {
                tool: 'thread_reply',
                args: { token: '${TOKEN}', text: 'Three attempts, then it gives up.' },
              },
            },
          ],
          text: 'Answered late.',
        },
      ],
    }),
  );
  // The caller asks, backgrounds, and stays in the same turn while the
  // answer lands: the wake is still in its queue when it reads the token.
  await setScenario(
    harness,
    callerPath,
    threadToolsScenario({
      name: 'tt-bg-caller-script',
      provider: 'claude',
      turns: [
        {
          steps: [
            {
              call: {
                tool: 'thread_ask',
                args: { thread_id: target, question: 'what is the retry budget?', wait_seconds: 1 },
                timeoutMs: 60_000,
              },
            },
            { capture: { var: 'ASK', from: '${MCP_RESULT}', pattern: RESULT_TOKEN_PATTERN } },
            { gate: 'caller-reads' },
            {
              call: {
                tool: 'thread_status',
                args: { tokens: ['${ASK}'], wait_seconds: 30 },
                timeoutMs: 60_000,
              },
            },
          ],
          text: 'Read the answer off the token.',
        },
        // The wake message starts a turn of its own once this one ends.
        { text: 'Read the wake message as well.' },
      ],
    }),
  );

  await harness.rpc('StartSession', caller);
  await harness.rpc('SendMessage', caller, 'ask the other thread about the retry budget', null);

  interface AskAck {
    token: string;
    state: string;
    outcome: string;
    notify: boolean;
  }
  const ask = await awaitToolAnswer<AskAck>(harness, { tool: 'thread_ask', timeoutMs: 60_000 });
  expect(ask.isError, ask.text).toBe(false);
  // The wait ran out, so the answer is owed as a message and notify is on.
  expect(ask.value!.outcome).toBe('backgrounded');
  expect(ask.value!.notify).toBe(true);

  const forkMock = await awaitGate(harness, 'hold-answer', targetPath);
  const callerMock = await awaitGate(harness, 'caller-reads', callerPath);
  await advanceGate(harness, forkMock.mockId, 'hold-answer');

  interface TimelineItem {
    kind: string;
    summary?: string;
    meta?: string;
  }
  const wakeSendId = `thread-wake:${ask.value!.token}`;
  // The caller is mid-turn, so the wake takes the queued path: the answer
  // reaches the thread as a message of its own, attributed to the thread
  // that answered, without the caller having asked for it again.
  const findWakeRow = async () =>
    (await harness.rpc<TimelineItem[]>('ListItems', caller, true)).find(
      (item) => item.kind === 'user_text' && (item.meta ?? '').includes(wakeSendId),
    );
  // A turn boundary plus the queued write behind it is past expect's 10s
  // default, so every poll that waits on one names its own bound.
  await expect
    .poll(async () => (await findWakeRow()) !== undefined, { timeout: 30_000 })
    .toBe(true);
  const wakeRow = (await findWakeRow())!;
  expect(wakeRow.summary).toContain('Three attempts, then it gives up.');
  expect(wakeRow.summary).toContain(ask.value!.token);

  await advanceGate(harness, callerMock.mockId, 'caller-reads');

  interface StatusAnswer {
    requests: Array<{
      token: string;
      state: string;
      answer_kind?: string;
      answer?: string;
      delivered?: string;
      wake_queued?: boolean;
    }>;
    woke_on?: string;
    timed_out?: boolean;
    note?: string;
  }
  const status = await awaitToolAnswer<StatusAnswer>(harness, {
    tool: 'thread_status',
    timeoutMs: 60_000,
  });
  expect(status.isError, status.text).toBe(false);
  const request = status.value!.requests[0];
  expect(request.token).toBe(ask.value!.token);
  expect(request.state).toBe('replied');
  expect(request.answer_kind).toBe('reply');
  expect(request.answer).toContain('Three attempts, then it gives up.');
  expect(status.value!.timed_out).toBeFalsy();
  // The wake's own turn runs to its end, so the thread is left idle.
  await expect
    .poll(async () =>
      (await harness.rpc<TimelineItem[]>('ListItems', caller, true)).some((item) =>
        (item.summary ?? '').includes('Read the wake message as well.'),
      ),
      { timeout: 30_000 },
    )
    .toBe(true);
  // Whether the reply also says the message is still coming depends on
  // whether the provider has taken it up yet, and this mock picks every
  // queued message up the moment it is written (cmd/ao-mockprovider
  // claude.go). The notice itself is pinned in
  // TestThreadStatusSaysTheQueuedMessageIsStillComing, over both halves of
  // the queued path.
  expect(request.delivered).toBe('queued');
});

test('thread_send with notify into a mid-turn thread lands after the boundary with the draft intact', async ({
  harness,
}) => {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      { name: 'tt-draft-caller', repo: {}, threads: [{ title: 'Draft caller', provider: 'claude' }] },
      { name: 'tt-draft-target', repo: {}, threads: [{ title: 'Busy target', provider: 'claude' }] },
    ],
  });
  const caller = seed.projects[0].threadIds[0];
  const callerPath = seed.projects[0].path;
  const target = seed.projects[1].threadIds[0];
  const targetPath = seed.projects[1].path;
  const draft = 'half a sentence the person is still typing';

  // The target's first turn parks, so the send arrives mid-turn and the
  // only way into the thread is the turn boundary.
  await setScenario(
    harness,
    targetPath,
    threadToolsScenario({
      name: 'tt-draft-target-script',
      provider: 'claude',
      turns: [
        { steps: [{ gate: 'hold-target' }], text: 'Still on the first task.' },
        {
          steps: [
            { capture: { var: 'TOKEN', from: '${USER_INPUT}', pattern: FOOTER_TOKEN_PATTERN } },
            {
              call: {
                tool: 'thread_reply',
                args: { token: '${TOKEN}', text: 'The stall is in the watchdog.' },
              },
            },
          ],
          text: 'Answered at the boundary.',
        },
      ],
    }),
  );
  await setScenario(
    harness,
    callerPath,
    threadToolsScenario({
      name: 'tt-draft-caller-script',
      provider: 'claude',
      turns: [
        {
          steps: [
            {
              call: {
                tool: 'thread_send',
                args: {
                  thread_id: target,
                  message: 'look at the crash report too',
                  wait_seconds: 0,
                  notify: true,
                },
              },
            },
          ],
          text: 'Queued it for the boundary.',
        },
        // The wake the reply sends back starts a turn of its own here.
        { text: 'Read the answer when it arrived.' },
      ],
    }),
  );

  await harness.rpc('StartSession', target);
  await harness.rpc('SendMessage', target, 'start on the first task', null);
  const targetMock = await awaitGate(harness, 'hold-target', targetPath);

  // A person is half-way through a message in that thread's composer.
  await harness.rpc('SaveDraft', target, draft, [], [], null);

  await harness.rpc('StartSession', caller);
  await harness.rpc('SendMessage', caller, 'tell the busy thread about the crash report', null);

  interface SendAck {
    token: string;
    state: string;
    outcome: string;
    notify: boolean;
  }
  const send = await awaitToolAnswer<SendAck>(harness, { tool: 'thread_send', timeoutMs: 60_000 });
  expect(send.isError, send.text).toBe(false);
  expect(send.value!.outcome).toBe('backgrounded');
  expect(send.value!.state).toBe('accepted');
  expect(send.value!.notify).toBe(true);

  interface TimelineItem {
    id: string;
    kind: string;
    summary?: string;
  }
  const targetItems = async () => await harness.rpc<TimelineItem[]>('ListItems', target, true);
  const landedRequest = async () =>
    (await targetItems()).find((item) => (item.summary ?? '').includes('look at the crash report too'));
  // The request takes the queued path into a thread that is mid-turn: the
  // message is written for the target's agent to pick up at its boundary,
  // which is past expect's 10s default.
  await expect
    .poll(async () => (await landedRequest()) !== undefined, { timeout: 30_000 })
    .toBe(true);
  const landed = (await landedRequest())!;
  expect(landed.kind).toBe('user_text');
  expect(landed.summary).toContain('Agent request from thread');

  const composer = () => harness.rpc<{ content: string }>('GetDraft', target);
  // A message the person did not type must not take the message they were
  // typing, on either side of the boundary.
  expect((await composer()).content).toBe(draft);
  // The target is still on its first task: the request has not been
  // answered, and its own turn has not started.
  expect((await targetItems()).some((item) => (item.summary ?? '').includes('Answered at the boundary'))).toBe(
    false,
  );

  // The boundary is what lets the target act on it, and its reply wakes
  // the caller.
  await advanceGate(harness, targetMock.mockId, 'hold-target');
  const reply = await awaitToolAnswer<{ token: string; state: string }>(harness, {
    tool: 'thread_reply',
    timeoutMs: 60_000,
  });
  expect(reply.isError, reply.text).toBe(false);
  expect(reply.value!.token).toBe(send.value!.token);
  expect((await composer()).content).toBe(draft);

  const wake = await harness.waitForEvent<HarnessMockEvent>(
    'harness:mock',
    (ev) =>
      ev.report.kind === 'user_input' &&
      ev.cwd === callerPath &&
      (ev.report.input ?? '').includes(send.value!.token),
    60_000,
  );
  expect(wake.report.input).toContain('The stall is in the watchdog.');
  // The wake's own turn runs to its end, so both threads are left idle.
  await expect
    .poll(async () =>
      (await harness.rpc<TimelineItem[]>('ListItems', caller, true)).some((item) =>
        (item.summary ?? '').includes('Read the answer when it arrived.'),
      ),
      { timeout: 30_000 },
    )
    .toBe(true);
});

test('a write inside the ask fork is refused by its session and the refusal comes back in the answer', async ({
  harness,
}) => {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      { name: 'tt-refuse-caller', repo: {}, threads: [{ title: 'Refusal caller', provider: 'claude' }] },
      {
        name: 'tt-refuse-target',
        repo: {},
        // full-access is the contrast: the fork is read-only whatever the
        // thread it was cut from may do.
        threads: [{ title: 'Migration work', provider: 'claude', runtimeMode: 'full-access' }],
      },
    ],
  });
  const caller = seed.projects[0].threadIds[0];
  const callerPath = seed.projects[0].path;
  const target = seed.projects[1].threadIds[0];
  const targetPath = seed.projects[1].path;
  const notePath = `${targetPath}/rollback.md`;

  // Turn one settles so the fork has a session file; turn two parks, so
  // the ask below forks a thread that is mid-turn.
  await setScenario(
    harness,
    targetPath,
    threadToolsScenario({
      name: 'tt-refuse-target-script',
      provider: 'claude',
      turns: [
        { text: 'The migration renames two columns.' },
        { steps: [{ gate: 'target-busy' }], text: 'Finished the long job.' },
      ],
    }),
  );
  await harness.rpc('StartSession', target);
  await harness.rpc('SendMessage', target, 'describe the migration', null);
  await awaitTurnCompleted(harness, target);
  await harness.rpc('SendMessage', target, 'now run the long job', null);
  const targetMock = await awaitGate(harness, 'target-busy', targetPath);

  // The fork tries to write, is refused before any prompt (read-only
  // denies rather than asking), and says so in its answer.
  await setScenario(
    harness,
    targetPath,
    threadToolsScenario({
      name: 'tt-refuse-fork',
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
                    id: 'msg-fork-write',
                    role: 'assistant',
                    model: 'claude-mock-1',
                    content: [
                      {
                        type: 'tool_use',
                        id: 'tu-forkwrite',
                        name: 'Write',
                        input: { file_path: notePath, content: 'rollback plan' },
                      },
                    ],
                  },
                }),
                // What a read-only session returns for a stripped tool: a
                // pre-ask refusal, so nothing waits on a person.
                JSON.stringify({
                  type: 'system',
                  subtype: 'permission_denied',
                  tool_name: 'Write',
                  tool_use_id: 'tu-forkwrite',
                  decision_reason_type: 'mode',
                  decision_reason: 'Write is not available in a read-only session',
                  message: 'Permission to use Write has been denied.',
                  uuid: 'pd-tu-forkwrite',
                }),
                JSON.stringify({
                  type: 'user',
                  message: {
                    role: 'user',
                    content: [
                      {
                        type: 'tool_result',
                        tool_use_id: 'tu-forkwrite',
                        is_error: true,
                        content: 'Permission to use Write has been denied.',
                      },
                    ],
                  },
                }),
              ],
            },
            { gate: 'fork-refused' },
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
    harness,
    callerPath,
    threadToolsScenario({
      name: 'tt-refuse-caller-script',
      provider: 'claude',
      turns: [
        {
          steps: [
            {
              call: {
                tool: 'thread_ask',
                args: {
                  thread_id: target,
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

  await harness.rpc('StartSession', caller);
  await harness.rpc('SendMessage', caller, 'ask the migration thread about rollback', null);

  const forkMock = await awaitGate(harness, 'fork-refused', targetPath);

  // The fork is a live scratch thread at this point, and the refusal is
  // recorded in it: a notice row of its own and a declined tool row.
  const rows = await threadRows(harness);
  const fork = rows.find((row) => row.mode === 'scratch');
  expect(fork, 'the ask made no scratch fork').toBeDefined();
  expect(fork!.runtimeMode).toBe('read-only');
  expect(fork!.id).not.toBe(target);

  interface TimelineItem {
    id: string;
    kind: string;
    decision?: string;
    summary?: string;
  }
  await expect
    .poll(async () => {
      const items = await harness.rpc<TimelineItem[]>('ListItems', fork!.id, true);
      const notice = items.find((item) => item.id === 'permission-denied:tu-forkwrite');
      if (!notice) return null;
      return {
        noticeKind: notice.kind,
        writeDecision: items.find((item) => item.id === 'tu-forkwrite')?.decision ?? '',
      };
    })
    .toEqual({ noticeKind: 'notification', writeDecision: 'declined' });

  // The refusal is the session AO gave the fork, not the script's good
  // manners: Claude's write tools are stripped from it, which no allow
  // rule can put back, and the thread it was cut from kept its own.
  interface MockRow {
    mockId: string;
    registration: { cwd: string };
    sessionConfig?: { disallowedTools?: string[]; permissionMode?: string };
  }
  const mocks = await harness.rpc<MockRow[]>('HarnessListMocks');
  const forkConfig = mocks.find((mock) => mock.mockId === forkMock.mockId)?.sessionConfig;
  expect(forkConfig?.disallowedTools).toEqual(
    expect.arrayContaining(['Write', 'Edit', 'NotebookEdit']),
  );
  const targetConfig = mocks.find((mock) => mock.mockId === targetMock.mockId)?.sessionConfig;
  expect(targetConfig?.disallowedTools ?? []).toEqual([]);

  await advanceGate(harness, forkMock.mockId, 'fork-refused');

  interface AskAnswer {
    token: string;
    thread_id: string;
    state: string;
    outcome: string;
    answer_kind?: string;
    answer?: string;
  }
  const ask = await awaitToolAnswer<AskAnswer>(harness, { tool: 'thread_ask', timeoutMs: 120_000 });
  expect(ask.isError, ask.text).toBe(false);
  expect(ask.value!.outcome).toBe('settled');
  expect(ask.value!.state).toBe('replied');
  expect(ask.value!.answer).toContain('renames two columns');
  // The caller learns the write was refused rather than reading an answer
  // that quietly left it out.
  expect(ask.value!.answer).toContain('Write was denied in this read-only copy');

  // Nothing was written, the thread that was asked is still running its
  // own turn, and the fork is gone once its answer was stored.
  const noteExists = await harness.rpc<TimelineItem[]>('ListItems', target, true);
  expect(
    noteExists.some((item) => (item.summary ?? '').includes('what does the migration do')),
  ).toBe(false);
  await expect
    .poll(async () => (await threadRows(harness)).some((row) => row.mode === 'scratch'))
    .toBe(false);
  await advanceGate(harness, targetMock.mockId, 'target-busy');
});

test('cancelling a request blocked on an approval clears the prompt in the sidebar and the pane', async ({
  harness,
  page,
}) => {
  test.setTimeout(180_000);
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      { name: 'tt-cancel-caller', repo: {}, threads: [{ title: 'Cancel caller', provider: 'claude' }] },
      {
        name: 'tt-cancel-target',
        repo: {},
        threads: [
          {
            title: 'Cancel approval target',
            provider: 'claude',
            runtimeMode: 'approval-required',
            // A prior turn so the sidebar lists it as a thread, not a draft.
            turns: [{ userText: 'Ready?', items: [{ kind: 'assistant_text', summary: 'Ready.' }] }],
          },
        ],
      },
    ],
  });
  const caller = seed.projects[0].threadIds[0];
  const callerPath = seed.projects[0].path;
  const target = seed.projects[1].threadIds[0];
  const targetPath = seed.projects[1].path;
  const notePath = `${targetPath}/note.txt`;

  // The target stops on a write it needs a person to allow, exactly as the
  // blocked-wait spec above; nobody answers it this time.
  await setScenario(
    harness,
    targetPath,
    threadToolsScenario({
      name: 'tt-cancel-target-script',
      provider: 'claude',
      turns: [
        {
          steps: [
            {
              emitLines: [
                JSON.stringify({
                  type: 'assistant',
                  message: {
                    id: 'msg-write',
                    role: 'assistant',
                    model: 'claude-mock-1',
                    content: [
                      {
                        type: 'tool_use',
                        id: 'tu-write',
                        name: 'Write',
                        input: { file_path: notePath, content: 'hello' },
                      },
                    ],
                  },
                }),
              ],
            },
            {
              approval: {
                toolName: 'Write',
                input: { file_path: notePath, content: 'hello' },
                toolUseId: 'tu-write',
                onAllow: [],
                onDeny: [],
              },
            },
          ],
          text: 'Wrote the note.',
        },
      ],
    }),
  );
  await setScenario(
    harness,
    callerPath,
    threadToolsScenario({
      name: 'tt-cancel-caller-script',
      provider: 'claude',
      turns: [
        {
          steps: [
            {
              call: {
                tool: 'thread_send',
                args: { thread_id: target, message: 'write the note', wait_seconds: 45 },
                timeoutMs: 90_000,
              },
            },
            { capture: { var: 'TOKEN', from: '${MCP_RESULT}', pattern: RESULT_TOKEN_PATTERN } },
          ],
          text: 'It is waiting on you.',
        },
        {
          steps: [{ call: { tool: 'thread_cancel', args: { token: '${TOKEN}' }, timeoutMs: 60_000 } }],
          text: 'Stopped it.',
        },
      ],
    }),
  );

  const asked = harness.waitForEvent<HarnessMockEvent>(
    'harness:mock',
    (ev) => ev.report.kind === 'approval_pending' && ev.cwd === targetPath,
    60_000,
  );
  const approval = harness.waitForEvent<{
    action: string;
    request?: { requestId: string; threadId: string };
  }>(
    'provider:approval',
    (ev) => ev.action === 'request' && ev.request?.threadId === target,
    60_000,
  );

  // The person is looking at the target thread when the agent stops it.
  await harness.open(page);
  await page.getByText('Cancel approval target').click();
  const row = page.locator(`[data-sidebar-thread-id="${target}"]`);

  await harness.rpc('StartSession', caller);
  await harness.rpc('SendMessage', caller, 'have the other thread write the note', null);

  interface SendAck {
    token: string;
    state: string;
    outcome: string;
  }
  const blocked = await awaitToolAnswer<SendAck>(harness, {
    tool: 'thread_send',
    timeoutMs: 90_000,
  });
  expect(blocked.isError, blocked.text).toBe(false);
  expect(blocked.value!.outcome).toBe('blocked');
  await asked;
  const pending = await approval;
  await expect(page.getByTestId('composer-pending-approval')).toBeVisible();
  await expect(row).toHaveAttribute('data-effective-status', 'pending-approval');
  // The caller was seeded without a turn, so the sidebar listed it as a
  // draft. Its first message landed without any screen sending it, and
  // the row still has to stop being a draft everywhere.
  const callerRow = page.locator(`[data-sidebar-thread-id="${caller}"]`);
  await expect(callerRow).toBeVisible();
  await expect(callerRow.getByTestId('thread-row-draft-icon')).toHaveCount(0);

  // The cancel interrupts the target's turn while its prompt is open. The
  // CLI abandons the prompt on the wire, and everything that showed it
  // clears: the composer's prompt, the sidebar pill, and the live state
  // the tools report.
  const resolved = harness.waitForEvent<{ action: string; requestId?: string; threadId?: string }>(
    'provider:approval',
    (ev) => ev.action === 'resolve' && ev.requestId === pending.request!.requestId,
    20_000,
  );
  const targetDone = awaitTurnCompleted(harness, target);
  await harness.rpc('SendMessage', caller, 'stop it', null);

  interface CancelReport {
    token: string;
    state: string;
    effect: string;
  }
  const cancelled = await awaitToolAnswer<CancelReport>(harness, {
    tool: 'thread_cancel',
    timeoutMs: 60_000,
  });
  expect(cancelled.isError, cancelled.text).toBe(false);
  expect(cancelled.value!.effect).toBe('turn_interrupted');
  expect(cancelled.value!.state).toBe('cancelled');
  await resolved;
  await targetDone;

  await expect(page.getByTestId('composer-pending-approval')).toHaveCount(0);
  await expect(row).not.toHaveAttribute('data-effective-status', 'pending-approval');
  await expect(row).not.toHaveAttribute('data-effective-status', 'running');

  const live = await harness.rpc<{ interactive?: { approvals?: unknown[] } }>(
    'GetThreadLiveState',
    target,
  );
  expect(live.interactive?.approvals ?? []).toHaveLength(0);
});
