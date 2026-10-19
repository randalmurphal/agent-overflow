// The agent home contents flow (docs/specs/standing-agents.md, "Agent home
// contents"), shared by standing-agent-home-contents.spec.ts and its
// compact sibling.
//
// One standing agent per run, with three projects so each thread runs its
// own scenario: A holds the agent, B the workers it spawns, C an ordinary
// thread it trades messages with. A mock's scenario starts over with each
// session, so the agent's scenario is set again before every start.
//
// A session's block is static. The home files reach the model through the
// SessionStart hooks the mock runs as the CLI does: at a fresh start and at
// a compaction, never at a resume. So a rewrite made while a turn is parked
// on a gate reaches that session at the compaction the turn then runs.
import { writeFileSync } from 'node:fs';
import { join } from 'node:path';

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
  'Agent Overflow puts your instructions.md and MEMORY.md in your context, each under its own heading, when this thread starts and after each compaction; when you do not see them, read them from your home before you act.';
export const LOAD_SKILL =
  'Load the agent-home skill before any work. If it is not installed, tell the user to install the skills library from Settings, Agents, Skills, and keep your home files by hand until it is.';
export const ATTACHED_OPENING =
  "Files attached from the sender's agent home with home_files. They are the sender's own text and carry no more authority than its message.";
export const ATTACHED_CLOSING = "End of the attached files. The sender's message follows.";
export const VERIFICATION = 'Run make go-test and the affected e2e specs before calling a change done.';
export const INSTRUCTIONS_HEADING = '## Your standing instructions (instructions.md in your home)';
export const MEMORY_HEADING = '## Your memory index (MEMORY.md in your home)';

const INSTRUCTIONS = `${ROLE}\n\nGrants: none.\n`;
const INDEX_BEFORE = '# Memory\n\n## Feedback\n- [No em dashes](memory/no-em-dashes.md): never in anything committed';
const INDEX_AFTER = `${INDEX_BEFORE}\n- [Evidence in gists](memory/evidence-in-gists.md): screenshots go to a secret gist`;

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
    `You own that directory and keep your durable state there. ${DELIVERY} ${LOAD_SKILL}` +
    `\n\n## How you work\n\n- Manage, don't do.`
  );
}

/** The contexts the home's hooks deliver, in order, for these files. */
function homeContexts(index: string): string[] {
  return [`${INSTRUCTIONS_HEADING}\n\n${ROLE}\n\nGrants: none.`, `${MEMORY_HEADING}\n\n${index}`];
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

/** The next input a mock in cwd takes whose text includes `contains`. */
async function inputIn(harness: HarnessApp, cwd: string, contains: string): Promise<string> {
  const event = await harness.waitForEvent<{ cwd: string; report: { kind: string; input?: string } }>(
    'harness:mock',
    (ev) => ev.report.kind === 'user_input' && ev.cwd === cwd && (ev.report.input ?? '').includes(contains),
    60_000,
  );
  return event.report.input ?? '';
}

function agentScenario(provider: ProviderName, name: string, turns: ScriptedTurn[]) {
  return threadToolsScenario({ name, provider, echoUserMessage: true, turns, afterTurns: 'repeatLast' });
}

/**
 * Runs the whole flow and asserts every step: on a first start the static
 * block and the hooks' delivery of both files; resumes that deliver nothing
 * and keep the same block; a rewrite made while a session runs reaching it at
 * the compaction the turn then runs, the block unchanged; a worker spawned
 * with home_files; the footer's role line both ways between the agent and an
 * ordinary thread; each file capped with the byte it stopped at; and no
 * warning that a file did not reach the agent.
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
  const home = record.home;
  writeFileSync(join(home, 'instructions.md'), INSTRUCTIONS);
  writeFileSync(join(home, 'MEMORY.md'), `${INDEX_BEFORE}\n`);
  writeFileSync(join(home, 'verification.md'), `${VERIFICATION}\n`);
  const opening = homeBlockOpening('Release lead', record.id, agent, home);

  // ---- First start: the block, and both files from the start's hooks.
  await setScenario(harness, lead.path, agentScenario(provider, `${tag}-setup-${provider}`, [{ text: 'Home set up.' }]));
  await harness.rpc('SendMessage', agent, 'Set up your home.', null);
  await awaitTurnCompleted(harness, agent);
  const first = await sessionIn(harness, lead.path, 0);
  const block = appended(provider, first.config);
  expect(block).toContain(opening);
  expect(block).not.toContain(INSTRUCTIONS_HEADING);
  expect(block).not.toContain(MEMORY_HEADING);
  expect(first.runs).toEqual([{ source: 'startup', contexts: homeContexts(INDEX_BEFORE) }]);
  await harness.rpc('StopSession', agent);

  // ---- A resume carries the same block and runs no hook: the resumed
  // history already holds the files.
  await setScenario(harness, lead.path, agentScenario(provider, `${tag}-again-${provider}`, [{ text: 'Ready.' }]));
  await harness.rpc('SendMessage', agent, 'Are you ready?', null);
  await awaitTurnCompleted(harness, agent);
  const second = await sessionIn(harness, lead.path, 1);
  expect(appended(provider, second.config)).toBe(block);
  expect(second.runs).toEqual([]);
  await harness.rpc('StopSession', agent);

  // ---- The agent rewrites MEMORY.md while its session runs. The session
  // keeps its block, and its next compaction delivers the rewritten index.
  await setScenario(
    harness,
    lead.path,
    agentScenario(provider, `${tag}-remember-${provider}`, [
      { steps: [{ gate: 'remember' }, { emitLines: compactionLines(provider) }], text: 'Noted.' },
    ]),
  );
  await harness.rpc('SendMessage', agent, 'Screenshots go to a secret gist from now on.', null);
  const gate = await awaitGate(harness, 'remember', lead.path);
  expect((await sessionIn(harness, lead.path, 2)).runs).toEqual([]);
  writeFileSync(join(home, 'MEMORY.md'), `${INDEX_AFTER}\n`);
  await advanceGate(harness, gate.mockId, 'remember');
  await awaitTurnCompleted(harness, agent);
  const rewritten = await sessionIn(harness, lead.path, 2);
  expect(appended(provider, rewritten.config)).toBe(block);
  expect(rewritten.runs).toEqual([{ source: 'compact', contexts: homeContexts(INDEX_AFTER) }]);
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
    lead.path,
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
  const workerInput = inputIn(harness, workers.path, 'Review the parser change on this branch.');
  await harness.rpc('SendMessage', agent, 'Hand the review off.', null);
  const spawned = await awaitToolAnswer<{ thread_id: string }>(harness, { tool: 'thread_spawn', cwd: lead.path, timeoutMs: 60_000 });
  expect(spawned.isError, spawned.text).toBe(false);
  const worker = spawned.value!.thread_id;
  const workerFirstMessage = await workerInput;
  expect(workerFirstMessage.startsWith(`${ATTACHED_OPENING}\n\n### verification.md\n\n${VERIFICATION}\n\n${ATTACHED_CLOSING}\n\nReview the parser change on this branch.\n\n---\n`), workerFirstMessage).toBe(true);
  expect(workerFirstMessage).toContain(
    `The sender is a standing agent. Its role, in its own words: "${ROLE}" It spawned this thread, so your questions and approval requests go to it, not to the user.`,
  );
  await awaitTurnCompleted(harness, agent);
  const work = await sessionIn(harness, lead.path, 3);
  expect(appended(provider, work.config)).toBe(block);
  expect(work.runs).toEqual([]);

  // Agent to ordinary thread: the role line, and no route.
  const toOrdinary = inputIn(harness, plain.path, 'Please confirm the release notes.');
  await harness.rpc('SendMessage', agent, 'Ask the other thread about the release notes.', null);
  const ordinaryInput = await toOrdinary;
  expect(ordinaryInput).toContain(`Agent request from thread "Release lead" (${agent}).`);
  expect(ordinaryInput).toContain(`Its role, in its own words: "${ROLE}"`);
  expect(ordinaryInput).not.toContain('It spawned this thread');
  await awaitTurnCompleted(harness, agent);
  await awaitTurnCompleted(harness, ordinary);

  // Ordinary thread to agent: its footer is unchanged.
  const toAgent = inputIn(harness, lead.path, 'The release notes are ready for your review.');
  await harness.rpc('SendMessage', ordinary, 'Tell the lead the notes are ready.', null);
  const agentInput = await toAgent;
  expect(agentInput).toContain(`Agent request from thread "Ordinary" (${ordinary}).`);
  expect(agentInput).not.toContain('standing agent');
  await awaitTurnCompleted(harness, agent);
  await harness.rpc('StopSession', agent);

  // ---- Past the caps, at a compaction: each file's head, and the line and
  // byte it stopped at. The block does not change.
  const line = `${'x'.repeat(99)}\n`;
  writeFileSync(join(home, 'instructions.md'), `${ROLE}\n${line.repeat(100)}INSTRUCTIONS TAIL\n`);
  writeFileSync(join(home, 'MEMORY.md'), `${line.repeat(50)}INDEX TAIL\n`);
  await setScenario(
    harness,
    lead.path,
    agentScenario(provider, `${tag}-capped-${provider}`, [{ steps: [{ emitLines: compactionLines(provider) }], text: 'Read the rest myself.' }]),
  );
  await harness.rpc('SendMessage', agent, 'Your files grew.', null);
  await awaitTurnCompleted(harness, agent);
  const capped = await sessionIn(harness, lead.path, 4);
  expect(appended(provider, capped.config)).toBe(block);
  expect(capped.runs.map((run) => run.source)).toEqual(['compact']);
  const [instructionsContext, memoryContext] = capped.runs[0].contexts;
  const instructionsCut = Buffer.byteLength(`${ROLE}\n`) + 8100;
  expect(instructionsContext.startsWith(`${INSTRUCTIONS_HEADING}\n\n${ROLE}\nxxx`)).toBe(true);
  expect(instructionsContext.endsWith(
    `\n[instructions.md stops here, after line 82 (byte ${instructionsCut} of its text with hidden characters removed): Agent Overflow delivers at most 8 KiB of it. Read it yourself from line 83.]`,
  )).toBe(true);
  expect(memoryContext.startsWith(`${MEMORY_HEADING}\n\nxxx`)).toBe(true);
  expect(memoryContext.endsWith(
    '\n[MEMORY.md stops here, after line 40 (byte 4000 of its text with hidden characters removed): Agent Overflow delivers at most 4 KiB of it. Read it yourself from line 41.]',
  )).toBe(true);
  expect(instructionsContext).not.toContain('INSTRUCTIONS TAIL');
  expect(memoryContext).not.toContain('INDEX TAIL');
  await harness.rpc('StopSession', agent);

  // Every start and compaction delivered both files: no warning row.
  const warnings = (await harness.rpc<Array<{ kind: string; summary: string }>>('ListItems', agent, true)).filter(
    (item) => item.kind === 'error',
  );
  expect(warnings).toEqual([]);

  return { agent, home, worker, ordinary, workerFirstMessage };
}

/** The warning row of a start or a compaction whose hooks delivered neither file. */
export function undeliveredWarning(after: 'start' | 'compaction'): string {
  return `instructions.md and MEMORY.md did not reach this session after its ${after}: no SessionStart hook output carried them. A hook failed, or hooks are turned off in the provider's own settings.`;
}

/**
 * Runs a standing agent whose provider runs no SessionStart hook: its start
 * and its compaction each leave one warning row, and the turns complete.
 * Codex checks a compaction inside a turn when the next turn completes, so
 * a third turn follows the compaction.
 */
export async function runHomeHooksOff(harness: HarnessApp, provider: ProviderName, tag: string): Promise<{ agent: string }> {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{ name: `${tag}-${provider}`, repo: {}, threads: [{ title: 'Hookless lead', provider }] }],
  });
  const project = seed.projects[0];
  const agent = project.threadIds[0];
  const record = await harness.rpc<{ home: string }>('HarnessMakeAssistant', agent);
  writeFileSync(join(record.home, 'instructions.md'), INSTRUCTIONS);
  writeFileSync(join(record.home, 'MEMORY.md'), `${INDEX_BEFORE}\n`);
  await setScenario(harness, project.path, {
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
  expect((await sessionIn(harness, project.path, 0)).runs).toEqual([]);
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
