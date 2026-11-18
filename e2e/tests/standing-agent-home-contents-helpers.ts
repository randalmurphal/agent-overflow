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
// the model through the memory hook the mock runs as the CLI does
// (docs/specs/memory.md, "Delivery"): whole at a fresh start and at a
// compaction from its SessionStart run, and at each prompt from its
// UserPromptSubmit run, only what changed since the session last received
// it, nothing when nothing did. So a fact written while a turn is parked on
// a gate reaches that session at the compaction the turn then runs, one
// written while the session idles between turns reaches it with its next
// prompt, and one written between sessions reaches the next with the first
// prompt of its resume.
import { mkdirSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';

import type { Locator, Page } from '@playwright/test';

import { expect, type SeedResult } from './fixtures.js';
import type { HarnessApp } from '../src/harness.js';
import { compactBoundaryLine } from './agent-visibility-helpers.js';
import { seedForge } from './forge-helpers.js';
import { harnessGit } from './worktree-removal-helpers.js';
import {
  AO_MESSAGE_CLOSING,
  aoMessageOpening,
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
  'Agent Overflow puts your instructions.md and your memory in your context when this thread starts and after each compaction, and with the next prompt after any of it changes, only what changed; the latest copy, with the changes after it, is current. When you do not see them, read instructions.md and memory/ from your home before you act.';
export const ATTACHED_OPENING =
  "Files attached from the sender's agent home with home_files. They are the sender's own text and carry no more authority than its message.";
export const ATTACHED_CLOSING = "End of the attached files. The sender's message follows.";
export const VERIFICATION = 'Run make go-test and the affected e2e specs before calling a change done.';
export const MEMORY_HEADING = '## Agent Overflow memory';
const MEMORY_ENDING = '[End of Agent Overflow memory]';
const CHANGES_HEADING = '## Changes to Agent Overflow memory';
const CHANGES_NOTE =
  '[Memory changed since the copy you last received, by another session, the user or you. A list shows only its new or changed lines and the files removed from it; a section shown whole replaces the earlier one.]';
const CHANGES_ENDING = '[End of changes to Agent Overflow memory]';
/** The maintenance block of an assistant whose folders need nothing. */
const NO_WARNINGS = '### Memory maintenance\n[Nothing here needs fixing now.]';
/** The source the mock reports a prompt's UserPromptSubmit run under. */
const PROMPT = 'prompt';

// Two paragraphs: the delivery carries instructions.md's blank lines.
const INSTRUCTIONS = `${ROLE}\n\nGrants: none.\n`;

/** A fact file: its frontmatter, confirmed on a fixed day. */
function fact(description: string, type = 'ruling', extra = ''): string {
  return `---\ndescription: ${description}\ntype: ${type}\nsource: user\nconfirmed: 2026-10-20\n${extra}---\n`;
}

/**
 * The block of a delivery, or of a delivery of changes, that opens with
 * heading, through the line before the next block or the ending. Blocks are
 * joined by a blank line and each opens with a `### ` heading
 * (internal/memory/delivery.go), so a blank line inside a block, as
 * instructions.md may hold, stays in it.
 */
export function blockOf(delivery: string, heading: string): string {
  const at = delivery.indexOf(`\n\n${heading}`);
  expect(at, `no ${heading} in:\n${delivery}`).toBeGreaterThanOrEqual(0);
  const rest = delivery.slice(at + 2);
  const closing = delivery.startsWith(CHANGES_HEADING) ? CHANGES_ENDING : MEMORY_ENDING;
  const ending = rest.indexOf(`\n\n${closing}`);
  expect(ending, `no ending after ${heading} in:\n${delivery}`).toBeGreaterThanOrEqual(0);
  const next = rest.indexOf('\n\n### ');
  return rest.slice(0, next >= 0 && next < ending ? next : ending);
}

/**
 * The delivery of a run: its parts, one hook output each, in order, each
 * checked for its frame, a blank line apart.
 */
export function deliveryOf(run: MockSessionStartRun): string {
  expect(run.contexts.length).toBeGreaterThan(0);
  run.contexts.forEach((part, i) => {
    const heading = run.contexts.length === 1 ? MEMORY_HEADING : `${MEMORY_HEADING}, part ${i + 1} of ${run.contexts.length}`;
    expect(part.startsWith(`${heading}\n\n`), part).toBe(true);
    expect(part.endsWith(`\n\n${MEMORY_ENDING}`), part).toBe(true);
  });
  return run.contexts.join('\n\n');
}

/**
 * The deliveries of changes of a prompt's run, one per part that changed,
 * each checked for its frame, a blank line apart.
 */
export function changesOf(run: MockSessionStartRun): string {
  expect(run.source).toBe(PROMPT);
  expect(run.contexts.length).toBeGreaterThan(0);
  for (const changes of run.contexts) {
    expect(changes.startsWith(`${CHANGES_HEADING}\n${CHANGES_NOTE}\n`), changes).toBe(true);
    expect(changes.endsWith(`\n\n${CHANGES_ENDING}`), changes).toBe(true);
  }
  return run.contexts.join('\n\n');
}

/** A delivery of changes holding blocks, as the hook writes one under the size limit. */
export function changesWith(...blocks: string[]): string {
  return `${CHANGES_HEADING}\n${CHANGES_NOTE}\n\n${blocks.join('\n\n')}\n\n${CHANGES_ENDING}`;
}

/** A session's runs of its SessionStart hooks, unstamped, leaving out its prompts' runs. */
export function startRunsOf(runs: MockSessionStartRun[]): MockSessionStartRun[] {
  return runs.filter((run) => run.source !== PROMPT).map(unstamped);
}

/** A session's runs of its prompts' UserPromptSubmit hooks. */
export function promptRunsOf(runs: MockSessionStartRun[]): MockSessionStartRun[] {
  return runs.filter((run) => run.source === PROMPT);
}

/** Checks that each of a session's `count` prompts ran the hook, and that none delivered anything. */
export function expectQuietPrompts(runs: MockSessionStartRun[], count: number): void {
  expect(promptRunsOf(runs)).toEqual(Array.from({ length: count }, () => ({ source: PROMPT, contexts: [] })));
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
    `\n\n## How you work\n\n- Serve the role in your instructions.md.`
  );
}

/** The stamp each part of a delivery carries under its heading: a part of several calls itself one. */
const STAMP =
  /^(## Agent Overflow memory(?:, part \d+ of \d+)?)\n\[Delivered at \d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z; it replaces any earlier copy of this (?:memory|part) in this conversation\.\]\n/;

/** run with its delivery as the folders read, after checking that each part carries its stamp. */
export function unstamped(run: MockSessionStartRun): MockSessionStartRun {
  return {
    ...run,
    contexts: run.contexts.map((context) => {
      expect(STAMP.test(context), context).toBe(true);
      return context.replace(STAMP, '$1\n');
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
 * block and the start's delivery of instructions.md and the agent's facts,
 * and a first prompt that delivers nothing; a resume of unchanged memory
 * that runs no SessionStart hook, keeps the same block and whose prompt
 * delivers nothing; a fact written while a turn runs reaching the session
 * at the compaction the turn then runs; a fact written while the session
 * idles reaching it with its next prompt as that one line, and the prompt
 * after it delivering nothing; an expired and a malformed fact written
 * between sessions reaching the next with its first prompt as the
 * maintenance warnings alone; a worker spawned with home_files; the
 * footer's role line both ways between the agent and an ordinary thread;
 * and sections over the size limit together reaching the model whole, at
 * the prompt after they grew and at the compaction, then one section over
 * it opening with its size, whole on Codex and its start on Claude, named
 * by the maintenance warning, with one warning row for it and no other.
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
  // The first prompt finds the start's record and delivers nothing.
  await setScenario(harness, home, agentScenario(provider, `${tag}-setup-${provider}`, [{ text: 'Home set up.' }]));
  await harness.rpc('SendMessage', agent, 'Set up your home.', null);
  await awaitTurnCompleted(harness, agent);
  const first = await sessionIn(harness, home, 0);
  const block = appended(provider, first.config);
  expect(block).toContain(opening);
  expect(block).not.toContain(MEMORY_HEADING);
  expect(block).not.toContain('### Your standing instructions');
  const firstRuns = startRunsOf(first.runs);
  expect(firstRuns.map((run) => run.source)).toEqual(['startup']);
  expectQuietPrompts(first.runs, 1);
  const started = deliveryOf(firstRuns[0]);
  expect(firstRuns[0].contexts).toHaveLength(5);
  expect(started.startsWith(`${MEMORY_HEADING}, part 1 of 5\n\n${instructionsHeading}\n`), started).toBe(true);
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
  expect(blockOf(started, '### Memory maintenance')).toBe(NO_WARNINGS);
  expect(started).not.toContain('[This part is');
  await harness.rpc('StopSession', agent);

  // ---- A resume of unchanged memory carries the same block and runs no
  // SessionStart hook: the resumed history already holds the delivery, and
  // its prompt delivers nothing.
  await setScenario(harness, home, agentScenario(provider, `${tag}-again-${provider}`, [{ text: 'Ready.' }]));
  await harness.rpc('SendMessage', agent, 'Are you ready?', null);
  await awaitTurnCompleted(harness, agent);
  const second = await sessionIn(harness, home, 1);
  expect(appended(provider, second.config)).toBe(block);
  expect(startRunsOf(second.runs)).toEqual([]);
  expectQuietPrompts(second.runs, 1);
  await harness.rpc('StopSession', agent);

  // ---- The agent writes a fact while its session runs. The session keeps
  // its block, and its next compaction delivers the new fact. Another
  // session writes one while this one idles: the next prompt delivers that
  // line alone, and the prompt after it nothing.
  await setScenario(
    harness,
    home,
    agentScenario(provider, `${tag}-remember-${provider}`, [
      { steps: [{ gate: 'remember' }, { emitLines: compactionLines(provider) }], text: 'Noted.' },
      { text: 'Seen it.' },
      { text: 'Nothing new.' },
    ]),
  );
  await harness.rpc('SendMessage', agent, 'Screenshots go to a secret gist from now on.', null);
  const gate = await awaitGate(harness, 'remember', home);
  expectQuietPrompts((await sessionIn(harness, home, 2)).runs, 1);
  writeFileSync(join(memoryDir, 'evidence-in-gists.md'), fact('Screenshots go to a secret gist.'));
  await advanceGate(harness, gate.mockId, 'remember');
  await awaitTurnCompleted(harness, agent);
  const rewritten = await sessionIn(harness, home, 2);
  expect(appended(provider, rewritten.config)).toBe(block);
  const rewrittenRuns = startRunsOf(rewritten.runs);
  expect(rewrittenRuns.map((run) => run.source)).toEqual(['compact']);
  expect(blockOf(deliveryOf(rewrittenRuns[0]), homeHeading)).toBe(
    `${homeHeading}\nRulings:\n- Screenshots go to a secret gist. (evidence-in-gists.md)\n- Never use em dashes in anything committed. (no-em-dashes.md)`,
  );
  writeFileSync(join(memoryDir, 'release-branch.md'), fact('Release branches are cut on Mondays.'));
  await harness.rpc('SendMessage', agent, 'Anything new?', null);
  await awaitTurnCompleted(harness, agent);
  await harness.rpc('SendMessage', agent, 'And now?', null);
  await awaitTurnCompleted(harness, agent);
  const live = promptRunsOf((await sessionIn(harness, home, 2)).runs);
  expect(live).toHaveLength(3);
  expect(changesOf(live[1])).toBe(changesWith(`${homeHeading}\nNew or changed:\n- ruling: Release branches are cut on Mondays. (release-branch.md)`));
  expect(live[2].contexts).toEqual([]);
  await harness.rpc('StopSession', agent);

  // ---- An expired and a malformed fact land between sessions: the first
  // prompt of the resume delivers the maintenance warnings alone, neither
  // fact in the index, and the block holds.
  writeFileSync(join(memoryDir, 'release-dates.md'), fact('The train leaves on Tuesdays.', 'context', 'expires: 2026-01-01\n'));
  writeFileSync(join(memoryDir, 'broken.md'), 'Remember the release train.\n');
  await setScenario(harness, home, agentScenario(provider, `${tag}-maintain-${provider}`, [{ text: 'Cleaning up next.' }]));
  await harness.rpc('SendMessage', agent, 'Check your memory.', null);
  await awaitTurnCompleted(harness, agent);
  const maintained = await sessionIn(harness, home, 3);
  expect(appended(provider, maintained.config)).toBe(block);
  expect(startRunsOf(maintained.runs)).toEqual([]);
  const maintainedPrompts = promptRunsOf(maintained.runs);
  expect(maintainedPrompts).toHaveLength(1);
  const fixThese = '### Memory maintenance\nKeeping these folders sound is your job. Fix these:';
  const fileWarnings =
    `- ${join(memoryDir, 'broken.md')}: it does not open with a --- frontmatter line. Fix its frontmatter or delete it.\n` +
    `- ${join(memoryDir, 'release-dates.md')} expired on 2026-01-01. Delete it, or move expires if it still holds.`;
  expect(changesOf(maintainedPrompts[0])).toBe(changesWith(`${fixThese}\n${fileWarnings}`));
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
  expect(workerFirstMessage.startsWith(`${aoMessageOpening(`the agent in thread "Release lead" (${agent})`)}${ATTACHED_OPENING}\n\n### verification.md\n\n${VERIFICATION}\n\n${ATTACHED_CLOSING}\n\nReview the parser change on this branch.\n\n---\n`), workerFirstMessage).toBe(true);
  expect(workerFirstMessage.endsWith(AO_MESSAGE_CLOSING), workerFirstMessage).toBe(true);
  expect(workerFirstMessage).toContain(
    `The sender is a standing agent. Its role, in its own words: "${ROLE}" It spawned this thread, so your questions and approval requests go to it, not to the user.`,
  );
  await awaitTurnCompleted(harness, agent);
  const work = await sessionIn(harness, home, 4);
  expect(appended(provider, work.config)).toBe(block);
  expect(startRunsOf(work.runs)).toEqual([]);

  // Agent to ordinary thread: the role line, and no route.
  const toOrdinary = awaitMockInput(harness, { cwd: plain.path, includes: 'Please confirm the release notes.' });
  await harness.rpc('SendMessage', agent, 'Ask the other thread about the release notes.', null);
  const ordinaryInput = (await toOrdinary).report.input ?? '';
  expect(ordinaryInput.startsWith(aoMessageOpening(`the agent in thread "Release lead" (${agent})`)), ordinaryInput).toBe(true);
  expect(ordinaryInput).toContain(`Agent request from thread "Release lead" (${agent}).`);
  expect(ordinaryInput).toContain(`Its role, in its own words: "${ROLE}"`);
  expect(ordinaryInput).not.toContain('It spawned this thread');
  await awaitTurnCompleted(harness, agent);
  await awaitTurnCompleted(harness, ordinary);

  // Ordinary thread to agent: its footer is unchanged.
  const toAgent = awaitMockInput(harness, { cwd: home, includes: 'The release notes are ready for your review.' });
  await harness.rpc('SendMessage', ordinary, 'Tell the lead the notes are ready.', null);
  const agentInput = (await toAgent).report.input ?? '';
  expect(agentInput.startsWith(aoMessageOpening(`the agent in thread "Ordinary" (${ordinary})`)), agentInput).toBe(true);
  expect(agentInput).toContain(`Agent request from thread "Ordinary" (${ordinary}).`);
  expect(agentInput).not.toContain('standing agent');
  await awaitTurnCompleted(harness, agent);
  // Nothing changed, so none of the session's prompts delivered anything.
  expect(promptRunsOf((await sessionIn(harness, home, 4)).runs).every((run) => run.contexts.length === 0)).toBe(true);
  await harness.rpc('StopSession', agent);

  // ---- Past the size limit nothing is cut. Each section is a hook output
  // of its own, so sections over 10,000 bytes together, each under it,
  // reach the model whole on both providers: at the first prompt after the
  // memory grew, as what changed, and at the compaction. A section itself
  // over the limit opens with its size: Claude shows the model the start,
  // which carries the size, and Codex, with its spill off, all of it. The
  // maintenance warning names that section and how to fix it, the thread
  // gets one warning row for it, and the block does not change.
  const line = `${'x'.repeat(99)}\n`;
  const longInstructions = `${ROLE}\n${line.repeat(60)}INSTRUCTIONS TAIL\n`;
  const bulk = (from: number, to: number) => {
    for (let i = from; i < to; i++) {
      writeFileSync(join(memoryDir, `bulk-${String(i).padStart(3, '0')}.md`), fact(`Bulk fact ${i}: ${'y'.repeat(90)}`, 'reference'));
    }
  };
  writeFileSync(join(home, 'instructions.md'), longInstructions);
  bulk(0, 60);
  await setScenario(
    harness,
    home,
    agentScenario(provider, `${tag}-large-${provider}`, [
      { steps: [{ emitLines: compactionLines(provider) }], text: 'All of it arrived.' },
      { steps: [{ emitLines: compactionLines(provider) }], text: 'Read the rest myself.' },
    ]),
  );
  await harness.rpc('SendMessage', agent, 'Your memory grew.', null);
  await awaitTurnCompleted(harness, agent);
  const grown = await sessionIn(harness, home, 5);
  expect(appended(provider, grown.config)).toBe(block);
  const grownChanges = changesOf(promptRunsOf(grown.runs)[0]);
  const grownRuns = startRunsOf(grown.runs);
  expect(grownRuns.map((run) => run.source)).toEqual(['compact']);
  const grownWhole = deliveryOf(grownRuns[0]);
  for (const delivered of [grownChanges, grownWhole]) {
    expect(Buffer.byteLength(delivered)).toBeGreaterThan(10000);
    expect(delivered).not.toContain('[This ');
    expect(blockOf(delivered, instructionsHeading)).toBe(`${instructionsHeading}\n${longInstructions.trimEnd()}`);
    expect((blockOf(delivered, homeHeading).match(/^- (reference: )?Bulk fact /gm) ?? []).length).toBe(60);
  }
  expect(blockOf(grownWhole, '### Memory maintenance')).toBe(`${fixThese}\n${fileWarnings}`);

  bulk(60, 160);
  await harness.rpc('SendMessage', agent, 'It grew again.', null);
  await awaitTurnCompleted(harness, agent);
  const over = await sessionIn(harness, home, 5);
  const overRuns = startRunsOf(over.runs);
  expect(overRuns.map((run) => run.source)).toEqual(['compact', 'compact']);
  const part = (run: MockSessionStartRun, n: number) => run.contexts.find((context) => context.startsWith(`${MEMORY_HEADING}, part ${n} of 5\n`)) ?? '';
  const homeIndex = part(overRuns[1], 2);
  const partNotice = /^\[This part is (\d+) bytes, more than the 10000 a hook's output reaches you whole\. If you see only its start, read all of it from the file the provider saved it to before you act\.\]$/;
  const size = homeIndex.split('\n')[2].match(partNotice);
  expect(size, homeIndex.slice(0, 400)).not.toBeNull();
  expect(homeIndex).toContain(`\n\n${homeHeading}\n`);
  if (provider === 'codex') {
    expect(Number(size![1])).toBe(Buffer.byteLength(over.runs.filter((run) => run.source === 'compact')[1].contexts[1]));
    expect(homeIndex.endsWith(`\n\n${MEMORY_ENDING}`), homeIndex.slice(-200)).toBe(true);
    expect((homeIndex.match(/^- Bulk fact /gm) ?? []).length).toBe(160);
  } else {
    expect(Number(size![1])).toBeGreaterThan(10000);
    expect(homeIndex.endsWith('Full hook output saved to: (ao-mockprovider keeps no file)'), homeIndex.slice(-200)).toBe(true);
    expect(homeIndex).not.toContain('Bulk fact 159');
  }
  // The other parts are whole on both, the warnings naming the index.
  const overWarning = `- The index of ${memoryDir}/ is more than the 10000 bytes a hook's output reaches every provider's model whole, so a session may see only its start until it reads the file the provider saved it to. Merge or delete facts, or move detail from their descriptions into their bodies.`;
  for (const n of [1, 3, 4, 5]) {
    expect(part(overRuns[1], n).endsWith(`\n\n${MEMORY_ENDING}`), part(overRuns[1], n).slice(0, 200)).toBe(true);
  }
  expect(blockOf(part(overRuns[1], 5), '### Memory maintenance')).toBe(`${fixThese}\n${overWarning}\n${fileWarnings}`);
  expect(promptRunsOf(over.runs)[1].contexts.some((context) => context.includes(overWarning))).toBe(true);
  await harness.rpc('StopSession', agent);

  // Every start, resume and compaction delivered what it ran: no error row,
  // and the one warning row is the size's, which the compaction reports off
  // its event.
  const rows = async (kind: string) =>
    (await harness.rpc<Array<{ kind: string; summary: string }>>('ListItems', agent, true))
      .filter((item) => item.kind === kind)
      .map((item) => item.summary);
  await expect
    .poll(() => rows('notification'), { timeout: 15_000 })
    .toEqual([
      `Part of this conversation's memory is longer than a provider may show the model of one hook output, so its sessions may see only that part's start and the path of a file holding all of it: the assistant's own memory index is ${size![1]} bytes, over the 10000 a hook's output reaches every provider's model whole.`,
    ]);
  expect(await rows('error')).toEqual([]);

  return { agent, home, worker, ordinary, workerFirstMessage };
}

/** The warning row of a start or a compaction whose hook did not deliver. */
export function undeliveredWarning(after: 'start' | 'compaction'): string {
  return `Agent Overflow memory did not reach this session after its ${after}: no SessionStart hook output carried it. A hook failed, or hooks are turned off in the provider's own settings. The next prompt delivers it whole.`;
}

/**
 * Runs a standing agent whose provider runs no hook, then runs them, then
 * not again. A start and a compaction whose hook did not deliver each leave
 * one warning row, and the turns complete; Codex checks a compaction inside
 * a turn when the next turn completes, so a turn follows each compaction. A
 * resume expects no delivery and leaves no row. With no record of what the
 * session was shown, a prompt delivers the whole memory; a compaction whose
 * hook did not deliver forgets the record its prompt wrote, so the first
 * prompt after it delivers the whole memory again.
 */
export async function runHomeHooksOff(harness: HarnessApp, provider: ProviderName, tag: string): Promise<{ agent: string }> {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{ name: `${tag}-${provider}`, repo: {}, threads: [{ title: 'Hookless lead', provider }] }],
  });
  const project = seed.projects[0];
  const agent = project.threadIds[0];
  const record = await harness.rpc<{ home: string }>('HarnessMakeAssistant', agent);
  const instructionsHeading = `### Your standing instructions (${join(record.home, 'instructions.md')})`;
  writeFileSync(join(record.home, 'instructions.md'), INSTRUCTIONS);
  const session = async (name: string, hooksOff: boolean, turns: ScriptedTurn[], messages: string[]) => {
    await setScenario(harness, record.home, { ...agentScenario(provider, `${tag}-${name}-${provider}`, turns), sessionStartHooksOff: hooksOff });
    for (const message of messages) {
      await harness.rpc('SendMessage', agent, message, null);
      await awaitTurnCompleted(harness, agent);
    }
  };
  const compacting: ScriptedTurn[] = [{ steps: [{ emitLines: compactionLines(provider) }], text: 'Compacted.' }, { text: 'Still here.' }];
  const errors = async () =>
    (await harness.rpc<Array<{ kind: string; summary: string }>>('ListItems', agent, true))
      .filter((item) => item.kind === 'error')
      .map((item) => item.summary);
  /** The one whole delivery the first prompt of session `index` made. */
  const wholeAtPrompt = async (index: number) => {
    const runs = (await sessionIn(harness, record.home, index)).runs;
    expect(startRunsOf(runs)).toEqual([]);
    const [prompt] = promptRunsOf(runs);
    expect(blockOf(deliveryOf(unstamped(prompt)), instructionsHeading)).toBe(`${instructionsHeading}\n${INSTRUCTIONS.trimEnd()}`);
  };

  await session('off', true, [{ text: 'Started.' }, ...compacting], ['Start up.']);
  expect(await errors()).toEqual([undeliveredWarning('start')]);
  for (const message of ['Compact now.', 'Carry on.']) {
    await harness.rpc('SendMessage', agent, message, null);
    await awaitTurnCompleted(harness, agent);
  }
  expect(await errors()).toEqual([undeliveredWarning('start'), undeliveredWarning('compaction')]);
  expect((await sessionIn(harness, record.home, 0)).runs).toEqual([]);
  await harness.rpc('StopSession', agent);

  // Hooks on: the resume expects nothing, and its prompt, with no record,
  // delivers the whole memory.
  await session('on', false, [{ text: 'Resumed.' }], ['Resume.']);
  const resumed = (await mocksIn(harness, record.home, 2))[1];
  if (provider === 'codex') expect(resumed.sessionConfig?.resumed).toBe(true);
  else expect(resumed.registration.resumeRef ?? '').not.toBe('');
  await wholeAtPrompt(1);
  await harness.rpc('StopSession', agent);

  // Hooks off again: the compaction's miss forgets the record the prompt
  // wrote, so the next session's first prompt delivers the whole memory.
  await session('off-again', true, compacting, ['Compact again.', 'Carry on again.']);
  await harness.rpc('StopSession', agent);
  await session('on-again', false, [{ text: 'Back.' }], ['Back again.']);
  await wholeAtPrompt(3);
  await harness.rpc('StopSession', agent);
  expect(await errors()).toEqual([undeliveredWarning('start'), undeliveredWarning('compaction'), undeliveredWarning('compaction')]);
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

/** Where a thread row lives in the sidebar: an assistant's, or a thread in a project. */
export type SidebarSection = 'assistants' | 'projects';

/**
 * The banner on an assistant that runs with two memories
 * (docs/specs/standing-agents.md, "Two memories"). By default the assistant runs
 * without its provider's own memory and shows none. With that provider's
 * choice at "never" while its own memory is on, the assistant's thread
 * shows it, with one action that opens the memory settings and nothing
 * that dismisses it; its worker and an ordinary thread never do. It goes
 * when the choice changes, and when the provider's own memory is turned
 * off and the window comes back. `open` shows a thread by its sidebar row.
 */
export async function expectOwnMemoryBanner(
  harness: HarnessApp,
  page: Page,
  provider: ProviderName,
  open: (section: SidebarSection, title: string) => Promise<void>,
): Promise<void> {
  const name = provider === 'codex' ? 'Codex' : 'Claude';
  const key = provider === 'codex' ? 'codexOwnMemoryOff' : 'claudeOwnMemoryOff';
  const ownMemory = (on: boolean) =>
    provider === 'codex'
      ? { path: '.codex/config.toml', content: `[features]\nmemories = ${on}\n` }
      : { path: '.claude/settings.json', content: `${JSON.stringify({ autoMemoryEnabled: on })}\n` };
  await harness.rpc('HarnessSeed', { providerHome: [ownMemory(true)] });
  const banner = page.getByTestId('own-memory-banner');

  await open('assistants', 'Release lead');
  await expect(page.getByText('Noted.', { exact: true })).toBeVisible();
  await expect(banner).toHaveCount(0);
  await harness.rpc('UpdateSettings', { [key]: 'never' });
  await expect(banner).toContainText(
    `${name}'s own memory is also on for this assistant, so it may save what it learns there instead of in Agent Overflow memory.`,
  );
  await expect(banner.getByRole('button')).toHaveText(['Memory settings']);

  await open('projects', 'Parser review');
  await expect(page.getByTestId('agent-message-row').first()).toBeVisible();
  await expect(banner).toHaveCount(0);
  await open('projects', 'Ordinary');
  await expect(page.getByTestId('own-memory-banner')).toHaveCount(0);

  await open('assistants', 'Release lead');
  await expect(banner).toBeVisible();
  await banner.getByRole('button', { name: 'Memory settings' }).click();
  await expect(page.getByTestId('settings-memory')).toBeVisible();
  await expect(page.getByTestId(`own-memory-${key}-never`).locator('input')).toBeChecked();
  await page.getByRole('button', { name: 'Close Settings', exact: true }).click();
  await expect(page.getByTestId('settings-overlay')).toHaveCount(0);

  // The choice changes, then the provider's own memory goes off outside
  // the app and the window comes back.
  await harness.rpc('UpdateSettings', { [key]: 'agentOverflowMemory' });
  await expect(banner).toHaveCount(0);
  await harness.rpc('UpdateSettings', { [key]: 'never' });
  await expect(banner).toBeVisible();
  await harness.rpc('HarnessSeed', { providerHome: [ownMemory(false)] });
  const focus = () => page.evaluate(() => window.dispatchEvent(new Event('focus')));
  await focus();
  await expect(banner).toHaveCount(0);

  // A change to the computer's settings reads the provider's own memory
  // again with no focus: on Claude, the variable that turns its memory off.
  await harness.rpc('HarnessSeed', { providerHome: [ownMemory(true)] });
  await focus();
  await expect(banner).toBeVisible();
  if (provider === 'claude') {
    await harness.rpc('SetProviderCustomEnvVar', 'claude', 'CLAUDE_CODE_DISABLE_AUTO_MEMORY', '1', false);
    await expect(banner).toHaveCount(0);
    await harness.rpc('DeleteProviderCustomEnvVar', 'claude', 'CLAUDE_CODE_DISABLE_AUTO_MEMORY');
  } else {
    await harness.rpc('HarnessSeed', { providerHome: [ownMemory(false)] });
    await harness.rpc('SetProviderCustomEnvVar', 'codex', 'E2E_UNRELATED', '1', false);
    await expect(banner).toHaveCount(0);
    await harness.rpc('HarnessSeed', { providerHome: [ownMemory(true)] });
    await harness.rpc('DeleteProviderCustomEnvVar', 'codex', 'E2E_UNRELATED');
  }
  await expect(banner).toBeVisible();

  // The other provider's configuration that cannot be read leaves this
  // one's answer; this one's own shows no banner.
  const unreadable = (codex: boolean) =>
    codex ? { path: '.codex/config.toml', content: '[features\n' } : { path: '.claude/settings.json', content: '{\n' };
  await harness.rpc('HarnessSeed', { providerHome: [ownMemory(false)] });
  await focus();
  await expect(banner).toHaveCount(0);
  await harness.rpc('HarnessSeed', { providerHome: [ownMemory(true), unreadable(provider !== 'codex')] });
  await focus();
  await expect(banner).toBeVisible();
  await harness.rpc('HarnessSeed', { providerHome: [unreadable(provider === 'codex')] });
  await focus();
  await expect(banner).toHaveCount(0);
}
