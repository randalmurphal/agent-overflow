// The agent home contents flow (docs/specs/standing-agents.md, "Agent home
// contents"), shared by standing-agent-home-contents.spec.ts and its
// compact sibling.
//
// One standing agent per run, with three projects so each thread runs its
// own scenario: A holds the agent, B the workers it spawns, C an ordinary
// thread it trades messages with. A mock's scenario starts over with each
// session, so the agent's scenario is set again before every start.
//
// A session's block is static. The agent's instructions.md and memory reach
// the model through the one SessionStart hook the mock runs as the CLI does
// (docs/specs/memory.md, "Delivery"): at a fresh start, at a compaction, and
// at a resume when the delivery changed since the thread's model last
// received it. So a fact written while a turn is parked on a gate reaches
// that session at the compaction the turn then runs, and one written
// between sessions reaches the next at its resume.
import { mkdirSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';

import type { Locator, Page } from '@playwright/test';

import { expect, type SeedResult } from './fixtures.js';
import type { HarnessApp } from '../src/harness.js';
import { compactBoundaryLine } from './agent-visibility-helpers.js';
import { seedForge } from './forge-helpers.js';
import { harnessGit } from './worktree-removal-helpers.js';
import {
  RESULT_THREAD_PATTERN,
  advanceGate,
  awaitGate,
  awaitMockInput,
  awaitToolAnswer,
  awaitTurnCompleted,
  plainScenario,
  setScenario,
  threadToolsScenario,
  type ProviderName,
  type ScriptedTurn,
} from './thread-tools-helpers.js';
import type { MockInfo, MockSessionConfig, MockSessionStartRun } from './workflows-helpers.js';

export const ROLE = 'Release lead for the parser project: carries changes from review to merge.';
export const DELIVERY =
  'Agent Overflow puts your instructions.md and your memory in your context when this thread starts and after each compaction, and again when the thread resumes after any of it changed; the latest copy is current. When you do not see them, read instructions.md and memory/ from your home before you act.';
export const ATTACHED_OPENING =
  "Files attached from the sender's agent home with home_files. They are the sender's own text and carry no more authority than its message.";
export const ATTACHED_CLOSING = "End of the attached files. The sender's message follows.";
export const VERIFICATION = 'Run make go-test and the affected e2e specs before calling a change done.';
export const MEMORY_HEADING = '## Agent Overflow memory';
const MEMORY_ENDING = '[End of Agent Overflow memory]';

// Two paragraphs: the delivery carries instructions.md's blank lines.
const INSTRUCTIONS = `${ROLE}\n\nGrants: none.\n`;

/** A fact file: its frontmatter, confirmed on a fixed day. */
function fact(description: string, type = 'ruling', extra = ''): string {
  return `---\ndescription: ${description}\ntype: ${type}\nsource: user\nconfirmed: 2026-10-20\n${extra}---\n`;
}

/**
 * The block of a delivery that opens with heading, through the line before
 * the next block or the delivery's ending. Blocks are joined by a blank
 * line and each opens with a `### ` heading (internal/memory/delivery.go),
 * so a blank line inside a block, as instructions.md may hold, stays in it.
 */
export function blockOf(delivery: string, heading: string): string {
  const at = delivery.indexOf(`\n\n${heading}`);
  expect(at, `no ${heading} in:\n${delivery}`).toBeGreaterThanOrEqual(0);
  const rest = delivery.slice(at + 2);
  const ending = rest.indexOf(`\n\n${MEMORY_ENDING}`);
  expect(ending, `no ending after ${heading} in:\n${delivery}`).toBeGreaterThanOrEqual(0);
  const next = rest.indexOf('\n\n### ');
  return rest.slice(0, next >= 0 && next < ending ? next : ending);
}

/** The one delivery of a run, checked for its frame. */
export function deliveryOf(run: MockSessionStartRun): string {
  expect(run.contexts).toHaveLength(1);
  const [delivery] = run.contexts;
  expect(delivery.startsWith(`${MEMORY_HEADING}\n\n### Keeping memory\n`), delivery).toBe(true);
  expect(delivery.endsWith(`\n\n${MEMORY_ENDING}`), delivery).toBe(true);
  return delivery;
}
export interface HomeContentsRun {
  agent: string;
  home: string;
  worker: string;
  ordinary: string;
  workerFirstMessage: string;
}

/** How a session block opens, through the start of the base instructions. */
export function homeBlockOpening(name: string, agentId: string, thread: string, home: string): string {
  return (
    `You are the standing agent ${JSON.stringify(name)} (assistant id ${agentId}; this thread is ${thread}). Your home is ${home}. ` +
    `You own that directory and keep your durable state there. ${DELIVERY}` +
    `\n\n## How you work\n\n- Orchestrate and delegate; never implement.`
  );
}

/** The stamp each delivery carries under its heading. */
const STAMP =
  /^## Agent Overflow memory\n\[Delivered at \d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z; it replaces any earlier copy of this memory in this conversation\.\]\n/;

/** run with its delivery as the folders read, after checking that it carries its stamp. */
export function unstamped(run: MockSessionStartRun): MockSessionStartRun {
  return {
    ...run,
    contexts: run.contexts.map((context) => {
      expect(STAMP.test(context), context).toBe(true);
      return context.replace(STAMP, `${MEMORY_HEADING}\n`);
    }),
  };
}

/** What the model reads beside its own prompt. */
function appended(provider: ProviderName, config: MockSessionConfig): string {
  return (provider === 'claude' ? config.appendSystemPrompt : config.developerInstructions) ?? '';
}

/** The frames a provider writes for a compaction inside a turn. */
function compactionLines(provider: ProviderName): string[] {
  if (provider === 'claude') return [compactBoundaryLine('compact-${TURN}')];
  return ['item/started', 'item/completed'].map(
    (method) =>
      `{"jsonrpc":"2.0","method":"${method}","params":{"threadId":"\${THREAD_ID}","turnId":"\${TURN_ID}","item":{"type":"contextCompaction","id":"compaction-\${TURN}"}}}`,
  );
}

/** The mocks spawned in cwd, in spawn order, once there are `expected`. */
async function mocksIn(harness: HarnessApp, cwd: string, expected: number): Promise<MockInfo[]> {
  let seen: MockInfo[] = [];
  await expect
    .poll(
      async () => {
        const mocks = await harness.rpc<MockInfo[]>('HarnessListMocks');
        seen = mocks
          .filter((mock) => mock.registration.cwd === cwd && mock.sessionConfig)
          .sort((a, b) => Number(a.mockId.split('-')[1]) - Number(b.mockId.split('-')[1]));
        return seen.length;
      },
      { timeout: 30_000 },
    )
    .toBeGreaterThanOrEqual(expected);
  return seen;
}

/** The session `index` (0-based) of the mocks spawned in cwd: what it was started with and what its hooks delivered. */
async function sessionIn(harness: HarnessApp, cwd: string, index: number): Promise<{ config: MockSessionConfig; runs: MockSessionStartRun[] }> {
  const mock = (await mocksIn(harness, cwd, index + 1))[index];
  return { config: mock.sessionConfig as MockSessionConfig, runs: mock.sessionStartRuns ?? [] };
}

function agentScenario(provider: ProviderName, name: string, turns: ScriptedTurn[]) {
  return threadToolsScenario({ name, provider, echoUserMessage: true, turns, afterTurns: 'repeatLast' });
}

/**
 * Runs the whole flow and asserts every step: on a first start the static
 * block and the hook's delivery of instructions.md and the agent's facts; a
 * resume of unchanged memory that delivers nothing and keeps the same block;
 * a fact written while a session runs reaching it at the compaction the turn
 * then runs, the block unchanged; an expired and a malformed fact written
 * between sessions reaching the next at its resume as maintenance warnings,
 * and the resume after it delivering nothing; a worker spawned with
 * home_files; the footer's role line both ways between the agent and an
 * ordinary thread; an instructions.md past the budget given way to its path
 * and an index cut to fit, both delivered again at the resume after they
 * changed and at the compaction; and no warning that the delivery did not
 * reach the agent.
 */
export async function runHomeContents(harness: HarnessApp, provider: ProviderName, tag: string): Promise<HomeContentsRun> {
  const names = [`${tag}-lead-${provider}`, `${tag}-workers-${provider}`, `${tag}-ordinary-${provider}`];
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      { name: names[0], repo: {}, threads: [{ title: 'Release lead', provider }] },
      { name: names[1], repo: {}, threads: [{ title: 'Workers inbox', provider }] },
      { name: names[2], repo: {}, threads: [{ title: 'Ordinary', provider }] },
    ],
  });
  // Each project has a forge origin the fake forge verifies, as a real
  // checkout does, so no project row carries an identity warning.
  await seedForge(harness, names.map((name, i) => ({ forge: 'github', project: `me/${name}`, id: 301 + i })));
  for (const [i, project] of seed.projects.entries()) {
    harnessGit(harness, project.path, 'remote', 'add', 'origin', `https://github.com/me/${names[i]}`);
    const refreshed = await harness.rpc<{ identityError?: string }>('RefreshProjectIdentity', project.projectId);
    expect(refreshed.identityError ?? '').toBe('');
  }
  const [lead, workers, plain] = seed.projects;
  const agent = lead.threadIds[0];
  const ordinary = plain.threadIds[0];
  await setScenario(harness, workers.path, plainScenario({ name: `${tag}-workers-${provider}`, provider, texts: ['Reviewing it.'] }));
  await harness.rpc('GetProviderStatuses');

  const record = await harness.rpc<{ id: string; home: string }>('HarnessMakeAssistant', agent);
  // The lead runs in its home, serving its own project and the workers'.
  const home = record.home;
  await harness.rpc('SetAssistantProjects', agent, false, [lead.projectId, workers.projectId]);
  const memoryDir = join(home, 'memory');
  writeFileSync(join(home, 'instructions.md'), INSTRUCTIONS);
  mkdirSync(memoryDir);
  writeFileSync(join(memoryDir, 'no-em-dashes.md'), fact('Never use em dashes in anything committed.'));
  writeFileSync(join(home, 'verification.md'), `${VERIFICATION}\n`);
  const opening = homeBlockOpening('Release lead', record.id, agent, home);
  const instructionsHeading = `### Your standing instructions (${join(home, 'instructions.md')})`;
  const homeHeading = `### Your memory (${memoryDir}/)`;

  // ---- First start: the block, and from the start's hook the
  // instructions, the agent's facts and the list of the projects it
  // serves, each with its memory folder under <configDir>/memory/projects.
  await setScenario(harness, home, agentScenario(provider, `${tag}-setup-${provider}`, [{ text: 'Home set up.' }]));
  await harness.rpc('SendMessage', agent, 'Set up your home.', null);
  await awaitTurnCompleted(harness, agent);
  const first = await sessionIn(harness, home, 0);
  const block = appended(provider, first.config);
  expect(block).toContain(opening);
  expect(block).not.toContain(MEMORY_HEADING);
  expect(block).not.toContain('### Your standing instructions');
  const firstRuns = first.runs.map(unstamped);
  expect(firstRuns.map((run) => run.source)).toEqual(['startup']);
  const started = deliveryOf(firstRuns[0]);
  expect(blockOf(started, instructionsHeading)).toBe(`${instructionsHeading}\n${INSTRUCTIONS.trimEnd()}`);
  expect(blockOf(started, homeHeading)).toBe(`${homeHeading}\nRulings:\n- Never use em dashes in anything committed. (no-em-dashes.md)`);
  const projectsMemory = join(dirname(dirname(home)), 'memory', 'projects');
  const listed = blockOf(started, '### Your projects').split('\n').slice(2);
  expect(listed).toHaveLength(2);
  for (const [i, project] of [lead, workers].entries()) {
    expect(listed[i].startsWith(`- ${names[i]} (${project.path}): ${projectsMemory}/`), listed[i]).toBe(true);
    expect(listed[i].endsWith('/, 0 facts'), listed[i]).toBe(true);
  }
  expect(started).not.toContain('### Project memory');
  expect(started).not.toContain('### Memory maintenance');
  await harness.rpc('StopSession', agent);

  // ---- A resume of unchanged memory carries the same block and runs no
  // hook: the resumed history already holds the delivery as it reads now.
  await setScenario(harness, home, agentScenario(provider, `${tag}-again-${provider}`, [{ text: 'Ready.' }]));
  await harness.rpc('SendMessage', agent, 'Are you ready?', null);
  await awaitTurnCompleted(harness, agent);
  const second = await sessionIn(harness, home, 1);
  expect(appended(provider, second.config)).toBe(block);
  expect(second.runs).toEqual([]);
  await harness.rpc('StopSession', agent);

  // ---- The agent writes a fact while its session runs. The session keeps
  // its block, and its next compaction delivers the new fact.
  await setScenario(
    harness,
    home,
    agentScenario(provider, `${tag}-remember-${provider}`, [
      { steps: [{ gate: 'remember' }, { emitLines: compactionLines(provider) }], text: 'Noted.' },
    ]),
  );
  await harness.rpc('SendMessage', agent, 'Screenshots go to a secret gist from now on.', null);
  const gate = await awaitGate(harness, 'remember', home);
  expect((await sessionIn(harness, home, 2)).runs).toEqual([]);
  writeFileSync(join(memoryDir, 'evidence-in-gists.md'), fact('Screenshots go to a secret gist.'));
  await advanceGate(harness, gate.mockId, 'remember');
  await awaitTurnCompleted(harness, agent);
  const rewritten = await sessionIn(harness, home, 2);
  expect(appended(provider, rewritten.config)).toBe(block);
  const rewrittenRuns = rewritten.runs.map(unstamped);
  expect(rewrittenRuns.map((run) => run.source)).toEqual(['compact']);
  expect(blockOf(deliveryOf(rewrittenRuns[0]), homeHeading)).toBe(
    `${homeHeading}\nRulings:\n- Screenshots go to a secret gist. (evidence-in-gists.md)\n- Never use em dashes in anything committed. (no-em-dashes.md)`,
  );
  await harness.rpc('StopSession', agent);

  // ---- An expired and a malformed fact land between sessions: the next
  // resume delivers neither in the index and warns about both, and the
  // block holds.
  writeFileSync(join(memoryDir, 'release-dates.md'), fact('The train leaves on Tuesdays.', 'context', 'expires: 2026-01-01\n'));
  writeFileSync(join(memoryDir, 'broken.md'), 'Remember the release train.\n');
  await setScenario(harness, home, agentScenario(provider, `${tag}-maintain-${provider}`, [{ text: 'Cleaning up next.' }]));
  await harness.rpc('SendMessage', agent, 'Check your memory.', null);
  await awaitTurnCompleted(harness, agent);
  const maintained = await sessionIn(harness, home, 3);
  expect(appended(provider, maintained.config)).toBe(block);
  const maintainedRuns = maintained.runs.map(unstamped);
  expect(maintainedRuns.map((run) => run.source)).toEqual(['resume']);
  const maintenance = deliveryOf(maintainedRuns[0]);
  expect(maintenance).not.toContain('The train leaves on Tuesdays.');
  expect(blockOf(maintenance, '### Memory maintenance')).toBe(
    '### Memory maintenance\nKeeping these folders sound is your job. Fix these:\n' +
      `- ${join(memoryDir, 'broken.md')}: it does not open with a --- frontmatter line. Fix its frontmatter or delete it.\n` +
      `- ${join(memoryDir, 'release-dates.md')} expired on 2026-01-01. Delete it, or move expires if it still holds.`,
  );
  await harness.rpc('StopSession', agent);

  // ---- The agent spawns a worker with its verification file and messages
  // an ordinary thread.
  await setScenario(
    harness,
    plain.path,
    threadToolsScenario({
      name: `${tag}-ordinary-${provider}`,
      provider,
      echoUserMessage: true,
      turns: [
        { text: 'Release notes confirmed.' },
        {
          steps: [
            {
              call: {
                tool: 'thread_send',
                args: { thread_id: agent, message: 'The release notes are ready for your review.' },
              },
            },
          ],
          text: 'Told the lead.',
        },
      ],
      afterTurns: 'repeatLast',
    }),
  );
  await setScenario(
    harness,
    home,
    agentScenario(provider, `${tag}-work-${provider}`, [
      {
        steps: [
          {
            call: {
              tool: 'thread_spawn',
              args: {
                prompt: 'Review the parser change on this branch.',
                title: 'Parser review',
                project_id: workers.projectId,
                home_files: ['verification.md'],
              },
            },
          },
          { capture: { var: 'WORKER', from: 'lastResult', pattern: RESULT_THREAD_PATTERN } },
        ],
        text: 'Spawned a reviewer with my verification checklist.',
      },
      {
        steps: [
          {
            call: { tool: 'thread_send', args: { thread_id: ordinary, message: 'Please confirm the release notes.' } },
          },
        ],
        text: 'Asked the ordinary thread.',
      },
      { text: 'Thanks, noted.' },
    ]),
  );
  const workerInput = awaitMockInput(harness, { cwd: workers.path, includes: 'Review the parser change on this branch.' });
  await harness.rpc('SendMessage', agent, 'Hand the review off.', null);
  const spawned = await awaitToolAnswer<{ thread_id: string }>(harness, { tool: 'thread_spawn', cwd: home, timeoutMs: 60_000 });
  expect(spawned.isError, spawned.text).toBe(false);
  const worker = spawned.value!.thread_id;
  const workerFirstMessage = (await workerInput).report.input ?? '';
  expect(workerFirstMessage.startsWith(`${ATTACHED_OPENING}\n\n### verification.md\n\n${VERIFICATION}\n\n${ATTACHED_CLOSING}\n\nReview the parser change on this branch.\n\n---\n`), workerFirstMessage).toBe(true);
  expect(workerFirstMessage).toContain(
    `The sender is a standing agent. Its role, in its own words: "${ROLE}" It spawned this thread, so your questions and approval requests go to it, not to the user.`,
  );
  await awaitTurnCompleted(harness, agent);
  const work = await sessionIn(harness, home, 4);
  expect(appended(provider, work.config)).toBe(block);
  expect(work.runs).toEqual([]);

  // Agent to ordinary thread: the role line, and no route.
  const toOrdinary = awaitMockInput(harness, { cwd: plain.path, includes: 'Please confirm the release notes.' });
  await harness.rpc('SendMessage', agent, 'Ask the other thread about the release notes.', null);
  const ordinaryInput = (await toOrdinary).report.input ?? '';
  expect(ordinaryInput).toContain(`Agent request from thread "Release lead" (${agent}).`);
  expect(ordinaryInput).toContain(`Its role, in its own words: "${ROLE}"`);
  expect(ordinaryInput).not.toContain('It spawned this thread');
  await awaitTurnCompleted(harness, agent);
  await awaitTurnCompleted(harness, ordinary);

  // Ordinary thread to agent: its footer is unchanged.
  const toAgent = awaitMockInput(harness, { cwd: home, includes: 'The release notes are ready for your review.' });
  await harness.rpc('SendMessage', ordinary, 'Tell the lead the notes are ready.', null);
  const agentInput = (await toAgent).report.input ?? '';
  expect(agentInput).toContain(`Agent request from thread "Ordinary" (${ordinary}).`);
  expect(agentInput).not.toContain('standing agent');
  await awaitTurnCompleted(harness, agent);
  await harness.rpc('StopSession', agent);

  // ---- Past the budget: instructions.md gives way to its path and the
  // agent's index is cut from its end. Both changed, so the resume delivers
  // them, and so does the compaction. The block does not change.
  const line = `${'x'.repeat(99)}\n`;
  const longInstructions = `${ROLE}\n${line.repeat(100)}INSTRUCTIONS TAIL\n`;
  writeFileSync(join(home, 'instructions.md'), longInstructions);
  for (let i = 0; i < 120; i++) {
    writeFileSync(join(memoryDir, `bulk-${String(i).padStart(3, '0')}.md`), fact(`Bulk fact ${i}: ${'y'.repeat(90)}`, 'reference'));
  }
  await setScenario(
    harness,
    home,
    agentScenario(provider, `${tag}-capped-${provider}`, [{ steps: [{ emitLines: compactionLines(provider) }], text: 'Read the rest myself.' }]),
  );
  await harness.rpc('SendMessage', agent, 'Your memory grew.', null);
  await awaitTurnCompleted(harness, agent);
  const capped = await sessionIn(harness, home, 5);
  expect(appended(provider, capped.config)).toBe(block);
  const cappedRuns = capped.runs.map(unstamped);
  expect(cappedRuns.map((run) => run.source)).toEqual(['resume', 'compact']);
  expect(cappedRuns[0].contexts).toEqual(cappedRuns[1].contexts);
  const cut = deliveryOf(cappedRuns[1]);
  const bytes = Buffer.byteLength(longInstructions);
  expect(blockOf(cut, instructionsHeading)).toBe(
    `${instructionsHeading}\n[instructions.md is ${bytes} bytes, more than fits beside your memory. Read it from ${join(home, 'instructions.md')} before you act.]`,
  );
  expect(cut).not.toContain('INSTRUCTIONS TAIL');
  const index = blockOf(cut, homeHeading);
  const shown = (index.match(/^- Bulk fact /gm) ?? []).length;
  expect(shown).toBeGreaterThan(0);
  expect(index.endsWith(`\n[${120 - shown} more facts in ${memoryDir}/]`), index).toBe(true);
  // The delivery, its stamp included, fits in the 9,000 UTF-16 units a
  // hook's output is held to.
  expect(capped.runs[1].contexts[0].length).toBeLessThanOrEqual(9000);
  const warned = blockOf(cut, '### Memory maintenance');
  expect(warned).toContain(`- ${join(home, 'instructions.md')} (${bytes} bytes) did not fit in this delivery beside your memory and was left out. Shorten it.`);
  expect(warned).toContain(`- The index of ${memoryDir}/ was cut to fit this delivery: ${120 - shown} facts are not shown. Merge, shorten or delete facts there.`);
  await harness.rpc('StopSession', agent);

  // Every start, resume and compaction delivered what it ran: no warning
  // row.
  const warnings = (await harness.rpc<Array<{ kind: string; summary: string }>>('ListItems', agent, true)).filter(
    (item) => item.kind === 'error',
  );
  expect(warnings).toEqual([]);

  return { agent, home, worker, ordinary, workerFirstMessage };
}

/** The warning row of a start, a resume or a compaction whose hook did not deliver. */
export function undeliveredWarning(after: 'start' | 'resume' | 'compaction'): string {
  return `Agent Overflow memory did not reach this session after its ${after}: no SessionStart hook output carried it. A hook failed, or hooks are turned off in the provider's own settings.`;
}

/**
 * Runs a standing agent whose provider runs no SessionStart hook: its start
 * and its compaction each leave one warning row, and the turns complete.
 * Codex checks a compaction inside a turn when the next turn completes, so
 * a third turn follows the compaction. Nothing reached the thread's model,
 * so its resume runs the hook again, and leaves a third row.
 */
export async function runHomeHooksOff(harness: HarnessApp, provider: ProviderName, tag: string): Promise<{ agent: string }> {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{ name: `${tag}-${provider}`, repo: {}, threads: [{ title: 'Hookless lead', provider }] }],
  });
  const project = seed.projects[0];
  const agent = project.threadIds[0];
  const record = await harness.rpc<{ home: string }>('HarnessMakeAssistant', agent);
  writeFileSync(join(record.home, 'instructions.md'), INSTRUCTIONS);
  await setScenario(harness, record.home, {
    ...agentScenario(provider, `${tag}-${provider}`, [
      { text: 'Started.' },
      { steps: [{ emitLines: compactionLines(provider) }], text: 'Compacted.' },
      { text: 'Still here.' },
    ]),
    sessionStartHooksOff: true,
  });
  const errors = async () =>
    (await harness.rpc<Array<{ kind: string; summary: string }>>('ListItems', agent, true))
      .filter((item) => item.kind === 'error')
      .map((item) => item.summary);

  await harness.rpc('SendMessage', agent, 'Start up.', null);
  await awaitTurnCompleted(harness, agent);
  expect(await errors()).toEqual([undeliveredWarning('start')]);
  for (const message of ['Compact now.', 'Carry on.']) {
    await harness.rpc('SendMessage', agent, message, null);
    await awaitTurnCompleted(harness, agent);
  }
  expect(await errors()).toEqual([undeliveredWarning('start'), undeliveredWarning('compaction')]);
  expect((await sessionIn(harness, record.home, 0)).runs).toEqual([]);
  await harness.rpc('StopSession', agent);

  await setScenario(harness, record.home, {
    ...agentScenario(provider, `${tag}-resumed-${provider}`, [{ text: 'Resumed.' }]),
    sessionStartHooksOff: true,
  });
  await harness.rpc('SendMessage', agent, 'Resume.', null);
  await awaitTurnCompleted(harness, agent);
  expect(await errors()).toEqual([undeliveredWarning('start'), undeliveredWarning('compaction'), undeliveredWarning('resume')]);
  const resumed = (await mocksIn(harness, record.home, 2))[1];
  if (provider === 'codex') expect(resumed.sessionConfig?.resumed).toBe(true);
  else expect(resumed.registration.resumeRef ?? '').not.toBe('');
  expect(resumed.sessionStartRuns ?? []).toEqual([]);
  await harness.rpc('StopSession', agent);
  return { agent };
}

/** The footer sentence a worker's first message names its agent with. */
export const AGENT_SENTENCE = 'The sender is a standing agent.';

/**
 * Scroll the timeline to its top, polling: a windowed timeline mounts its
 * oldest rows only once the scroll reaches them.
 */
export async function scrollTimelineToTop(page: Page, until: Locator): Promise<void> {
  await expect
    .poll(
      async () => {
        await page.getByTestId('message-timeline-scroll').first().evaluate((el) => {
          el.scrollTop = 0;
        });
        return until.count();
      },
      { timeout: 15_000 },
    )
    .toBe(1);
  await page.getByTestId('message-timeline-scroll').first().evaluate((el) => {
    el.scrollTop = 0;
  });
}

/** Scroll target to the middle of the timeline. */
export async function centerInTimeline(target: Locator): Promise<void> {
  await target.evaluate((el) => el.scrollIntoView({ block: 'center' }));
}

/**
 * Nothing (the composer, a banner) covers target: the element at the top and
 * the bottom of its middle is target or inside it.
 */
export async function expectUncovered(target: Locator): Promise<void> {
  await expect
    .poll(() =>
      target.evaluate((el) => {
        const box = el.getBoundingClientRect();
        const x = box.left + box.width / 2;
        return [box.top + 1, box.bottom - 1].every((y) => {
          const hit = document.elementFromPoint(x, y);
          return hit !== null && (el === hit || el.contains(hit));
        });
      }),
    )
    .toBe(true);
}

/**
 * Scroll the timeline until `text` inside scope sits mid-view, then require
 * every line of it to be in the viewport and uncovered. For a sentence of a
 * message taller than the viewport, where no element bounds it.
 */
export async function showTextUncovered(page: Page, scope: Locator, text: string): Promise<void> {
  const scroller = page.getByTestId('message-timeline-scroll').first();
  await expect
    .poll(async () => {
      const handle = await scroller.elementHandle();
      return scope.evaluate(
        (root, [wanted, scrollEl]) => {
          const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
          for (let node = walker.nextNode(); node; node = walker.nextNode()) {
            const at = (node.textContent ?? '').indexOf(wanted);
            if (at < 0) continue;
            const range = document.createRange();
            range.setStart(node, at);
            range.setEnd(node, at + wanted.length);
            const view = (scrollEl as HTMLElement).getBoundingClientRect();
            const box = range.getBoundingClientRect();
            (scrollEl as HTMLElement).scrollTop += box.top + box.height / 2 - (view.top + view.height / 2);
            return Array.from(range.getClientRects()).every((line) => {
              if (line.top < 0 || line.bottom > window.innerHeight) return false;
              const hit = document.elementFromPoint(line.left + Math.min(4, line.width / 2), line.top + line.height / 2);
              return hit !== null && root.contains(hit);
            });
          }
          return false;
        },
        [text, handle] as const,
      );
    })
    .toBe(true);
}
