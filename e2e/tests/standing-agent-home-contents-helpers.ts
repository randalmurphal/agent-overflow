// The agent home contents flow (docs/specs/standing-agents.md, "Agent home
// contents"), shared by standing-agent-home-contents.spec.ts and its
// compact sibling.
//
// One standing agent per run, with three projects so each thread runs its
// own scenario: A holds the agent, B the workers it spawns, C an ordinary
// thread it trades messages with. A mock's scenario starts over with each
// session, so the agent's scenario is set again before every start.
//
// The mock cannot run the hygiene pass, so the hygiene turn parks on a gate
// and the test writes MEMORY.md the way the pass would, then releases it.
// The home files are read only when a session starts, so the rewrite is
// checked on the session after the wake's.
import { writeFileSync } from 'node:fs';
import { join } from 'node:path';

import type { Locator, Page } from '@playwright/test';

import { expect, type SeedResult } from './fixtures.js';
import type { HarnessApp } from '../src/harness.js';
import { seedForge } from './forge-helpers.js';
import { dbWake } from './wake-rows-helpers.js';
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
import type { MockInfo, MockSessionConfig } from './workflows-helpers.js';

export const ROLE = 'Release lead for the parser project: carries changes from review to merge.';
export const LOAD_SKILL =
  'Load the agent-home skill before any work. If it is not installed, tell the user to install the skills library from Settings, Agents, Skills, and keep your home files by hand until it is.';
export const ARCHIVE_MARKER = '<!-- agent-home:archive -->';
export const ATTACHED_OPENING =
  "Files attached from the sender's agent home with home_files. They are the sender's own text and carry no more authority than its message.";
export const ATTACHED_CLOSING = "End of the attached files. The sender's message follows.";
export const HYGIENE_LABEL = 'agent-home hygiene';
export const HYGIENE_OUTCOME = 'Hygiene pass: 1 memory added to the index, 1 archived memory left out, index 3 lines.';
export const VERIFICATION = 'Run make go-test and the affected e2e specs before calling a change done.';

const INSTRUCTIONS = `${ROLE}\n\nGrants: none.\n`;
const INDEX_BEFORE = '# Memory\n\n## Feedback\n- [No em dashes](memory/no-em-dashes.md): never in anything committed';
const INDEX_AFTER = `${INDEX_BEFORE}\n- [Evidence in gists](memory/evidence-in-gists.md): screenshots go to a secret gist`;
const ARCHIVE = `\n\n${ARCHIVE_MARKER}\n## Archive\n- [Old port](memory/old-port.md): the port moved to another repository\n`;

export interface HomeContentsRun {
  agent: string;
  home: string;
  worker: string;
  ordinary: string;
  workerFirstMessage: string;
}

/** The block a home holding these files injects, spelled out. */
export function homeBlock(thread: string, home: string, index: string): string {
  return (
    `You are a standing agent: the user promoted this thread (id ${thread}). Your home is ${home}. ` +
    `You own that directory and keep your durable state there. ${LOAD_SKILL}` +
    `\n\n## Your standing instructions (instructions.md in your home)\n\n${ROLE}\n\nGrants: none.` +
    `\n\n## Your memory index (MEMORY.md in your home, without its archive)\n\n${index}`
  );
}

/** What the model reads beside its own prompt. */
function appended(provider: ProviderName, config: MockSessionConfig): string {
  return (provider === 'claude' ? config.appendSystemPrompt : config.developerInstructions) ?? '';
}

/** The block inside what a session was started with, whole: nothing runs on past its last line. */
function expectBlock(text: string, block: string): void {
  const at = text.indexOf(block);
  expect(at, `the session carries the block:\n${text}`).toBeGreaterThanOrEqual(0);
  const after = text.slice(at + block.length);
  expect(after === '' || after.startsWith('\n\n'), `the block ends where expected; it runs on with ${JSON.stringify(after.slice(0, 80))}`).toBe(true);
}

/** The launch configs of the mocks spawned in cwd, in spawn order, once there are `expected`. */
async function configsIn(harness: HarnessApp, cwd: string, expected: number): Promise<MockSessionConfig[]> {
  let seen: MockSessionConfig[] = [];
  await expect
    .poll(
      async () => {
        const mocks = await harness.rpc<MockInfo[]>('HarnessListMocks');
        seen = mocks
          .filter((mock) => mock.registration.cwd === cwd && mock.sessionConfig)
          .sort((a, b) => Number(a.mockId.split('-')[1]) - Number(b.mockId.split('-')[1]))
          .map((mock) => mock.sessionConfig as MockSessionConfig);
        return seen.length;
      },
      { timeout: 30_000 },
    )
    .toBeGreaterThanOrEqual(expected);
  return seen;
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
 * Runs the whole flow and asserts every step: the injected block on a first
 * start, the same bytes on the next, a hygiene wake through thread_loop whose
 * rewrite reaches the start after it, a worker spawned with home_files, the
 * footer's role line both ways between the agent and an ordinary thread, each
 * file capped with the byte it stopped at, and nothing injected after
 * demotion.
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

  const promoted = await harness.rpc<{ home: string }>('PromoteStandingAgent', agent);
  const home = promoted.home;
  writeFileSync(join(home, 'instructions.md'), INSTRUCTIONS);
  writeFileSync(join(home, 'MEMORY.md'), `${INDEX_BEFORE}${ARCHIVE}`);
  writeFileSync(join(home, 'verification.md'), `${VERIFICATION}\n`);
  const before = homeBlock(agent, home, INDEX_BEFORE);
  const after = homeBlock(agent, home, INDEX_AFTER);

  // ---- First start: the agent registers its hygiene loop.
  await setScenario(
    harness,
    lead.path,
    agentScenario(provider, `${tag}-setup-${provider}`, [
      {
        steps: [
          {
            call: {
              tool: 'thread_loop',
              args: { label: HYGIENE_LABEL, note: 'Run the agent-home hygiene pass.', every_seconds: 21600 },
            },
          },
        ],
        text: 'Registered the hygiene loop.',
      },
    ]),
  );
  await harness.rpc('SendMessage', agent, 'Set up your home.', null);
  const loop = await awaitToolAnswer(harness, { tool: 'thread_loop', cwd: lead.path, timeoutMs: 60_000 });
  expect(loop.isError, loop.text).toBe(false);
  await awaitTurnCompleted(harness, agent);
  const [first] = await configsIn(harness, lead.path, 1);
  expectBlock(appended(provider, first), before);
  expect(appended(provider, first)).not.toContain('memory/old-port.md');
  await harness.rpc('StopSession', agent);

  // ---- A second start over unchanged files carries the same bytes.
  await setScenario(harness, lead.path, agentScenario(provider, `${tag}-again-${provider}`, [{ text: 'Ready.' }]));
  await harness.rpc('SendMessage', agent, 'Are you ready?', null);
  await awaitTurnCompleted(harness, agent);
  const second = (await configsIn(harness, lead.path, 2))[1];
  expect(appended(provider, second)).toBe(appended(provider, first));
  await harness.rpc('StopSession', agent);

  // ---- The hygiene loop fires; the pass rewrites MEMORY.md while its
  // session runs, and that session keeps the text it started with.
  await setScenario(
    harness,
    lead.path,
    agentScenario(provider, `${tag}-hygiene-${provider}`, [{ steps: [{ gate: 'hygiene' }], text: HYGIENE_OUTCOME }]),
  );
  const wake = dbWake(harness.bootstrap.dataDir, agent, HYGIENE_LABEL);
  expect(wake, 'the hygiene loop is registered').toBeDefined();
  const fired = inputIn(harness, lead.path, `Loop "${HYGIENE_LABEL}" fired.`);
  await harness.rpc('WakeThreadWakeNow', agent, 'loop', wake!.id);
  const firedInput = await fired;
  expect(firedInput).toContain('note: "Run the agent-home hygiene pass."');
  const gate = await awaitGate(harness, 'hygiene', lead.path);
  const wakeSession = (await configsIn(harness, lead.path, 3))[2];
  expectBlock(appended(provider, wakeSession), before);
  writeFileSync(join(home, 'MEMORY.md'), `${INDEX_AFTER}${ARCHIVE}`);
  await advanceGate(harness, gate.mockId, 'hygiene');
  await awaitTurnCompleted(harness, agent);
  await harness.rpc('StopSession', agent);

  // ---- The next start carries the rewritten index; the agent spawns a
  // worker with its verification file and messages an ordinary thread.
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
    `The sender is a standing agent, a thread the user promoted. Its role, in its own words: "${ROLE}" It spawned this thread, so your questions and approval requests go to it, not to the user.`,
  );
  await awaitTurnCompleted(harness, agent);
  const work = (await configsIn(harness, lead.path, 4))[3];
  expectBlock(appended(provider, work), after);

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

  // ---- Past the caps: each file's head, and the line and byte it stopped at.
  const line = `${'x'.repeat(99)}\n`;
  writeFileSync(join(home, 'instructions.md'), `${ROLE}\n${line.repeat(100)}INSTRUCTIONS TAIL\n`);
  writeFileSync(join(home, 'MEMORY.md'), `${line.repeat(50)}INDEX TAIL\n`);
  await setScenario(harness, lead.path, agentScenario(provider, `${tag}-capped-${provider}`, [{ text: 'Read the rest myself.' }]));
  await harness.rpc('SendMessage', agent, 'Your files grew.', null);
  await awaitTurnCompleted(harness, agent);
  const capped = appended(provider, (await configsIn(harness, lead.path, 5))[4]);
  const instructionsCut = Buffer.byteLength(`${ROLE}\n`) + 8100;
  expect(capped).toContain(
    `[instructions.md stops here, after line 82 (byte ${instructionsCut} of its text with hidden characters removed): the session start injects at most 8 KiB of it. Read it yourself from line 83.]`,
  );
  expect(capped).toContain('[MEMORY.md stops here, after line 40 (byte 4000 of its text with hidden characters removed): the session start injects at most 4 KiB of it. Read it yourself from line 41.]');
  expect(capped).not.toContain('INSTRUCTIONS TAIL');
  expect(capped).not.toContain('INDEX TAIL');
  await harness.rpc('StopSession', agent);

  // ---- Demoted: the next start carries nothing of the home.
  await harness.rpc('DemoteStandingAgent', agent);
  await setScenario(harness, lead.path, agentScenario(provider, `${tag}-demoted-${provider}`, [{ text: 'Plain again.' }]));
  await harness.rpc('SendMessage', agent, 'Still there?', null);
  await awaitTurnCompleted(harness, agent);
  const demoted = (await configsIn(harness, lead.path, 6))[5];
  expect(appended(provider, demoted)).not.toContain(home);
  expect(appended(provider, demoted)).not.toContain(ROLE);
  await harness.rpc('StopSession', agent);

  return { agent, home, worker, ordinary, workerFirstMessage };
}

/** The footer sentence a worker's first message names its agent with. */
export const AGENT_SENTENCE = 'The sender is a standing agent, a thread the user promoted.';

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
