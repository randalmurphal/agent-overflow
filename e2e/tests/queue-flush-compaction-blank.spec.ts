// Reproduction probe (2026-09-19 field report): five background agents
// stream hundreds of child rows into a turn that already returned, their
// notifications wake the assistant again, an auto compaction lands, and a
// message queued around that moment opens the next turn. The timeline went
// blank at that point until the pane was left and re-entered. Samples the
// timeline every frame across the sequence and fails on any stretch where a
// populated window paints no row inside the viewport.
import { expect, test, type SeedResult } from './fixtures.js';
import {
  RESULT_LINE,
  advance,
  asyncAgentAckLine,
  backgroundTasksChangedLine,
  claudeScenario,
  emit,
  startMock,
  taskNotificationLine,
  taskStartedLine,
  taskUpdatedLine,
  textLines,
  thinkingLines,
  toolResultLine,
  toolUseLine,
  waitForGate,
} from './agent-visibility-helpers.js';

const QUEUED = 'Queued mid-turn: also consider the unpaired case.';
const AGENTS = 5;
const CHILDREN_PER_AGENT = 40;
const BLANK_LIMIT_MS = 400;

interface FrameSample {
  t: number;
  nodes: number;
  visibleNodes: number;
  hidden: boolean;
  bubbles: number;
  firstVisible: string;
  scrollTop: number;
  scrollHeight: number;
  clientHeight: number;
}

function toolRound(prefix: string, n: number, parent?: string): string[] {
  const lines: string[] = [];
  for (let i = 0; i < n; i++) {
    const id = `${prefix}-tool-${i}`;
    lines.push(toolUseLine(`${prefix}-msg-${i}`, id, i % 4 === 3 ? 'Read' : 'Bash', { command: `echo ${prefix} ${i}` }, parent));
    lines.push(toolResultLine(id, `${prefix} ${i} done`, { parentToolUseId: parent }));
  }
  return lines;
}

function agentId(i: number): string {
  return `tu-agent-${i}`;
}

function launchAgents(): string[] {
  const lines: string[] = [];
  for (let i = 0; i < AGENTS; i++) {
    lines.push(toolUseLine(`msg-agent-${i}`, agentId(i), 'Agent', { description: `Survey area ${i}`, prompt: 'Survey it.' }));
    lines.push(taskStartedLine(`task-${i}`, agentId(i), `Survey area ${i}`));
    lines.push(asyncAgentAckLine(agentId(i), `task-${i}`, `Survey area ${i}`));
  }
  lines.push(backgroundTasksChangedLine(
    Array.from({ length: AGENTS }, (_, i) => ({ task_id: `task-${i}`, task_type: 'local_agent', description: `Survey area ${i}` })),
  ));
  return lines;
}

function agentChildren(): string[] {
  // Interleave the five transcripts the way the real forward does.
  const per = Array.from({ length: AGENTS }, (_, i) => [
    ...textLines(`agent-${i}-lead`, `Agent ${i} starting the survey.`, agentId(i)),
    ...toolRound(`agent-${i}`, CHILDREN_PER_AGENT, agentId(i)),
  ]);
  const out: string[] = [];
  const max = Math.max(...per.map((p) => p.length));
  for (let k = 0; k < max; k++) {
    for (const p of per) if (p[k] !== undefined) out.push(p[k]);
  }
  return out;
}

function agentFinishes(): string[] {
  const lines: string[] = [];
  for (let i = 0; i < AGENTS; i++) {
    lines.push(taskUpdatedLine(`task-${i}`, { status: 'completed', end_time: 1787415964724 + i }));
    lines.push(taskNotificationLine(`task-${i}`, agentId(i), `Report ${i}: everything is in order.`));
    lines.push(...thinkingLines(`msg-think-${i}`, `Report ${i} is in.`));
    lines.push(...textLines(`msg-text-${i}`, `${i + 1} of ${AGENTS} reports in.`));
  }
  lines.push(backgroundTasksChangedLine([]));
  return lines;
}

const compactingLine = '{"type":"system","subtype":"status","status":"compacting"}';
const compactionDone = [
  '{"type":"system","subtype":"status","status":null,"compact_result":"success"}',
  JSON.stringify({
    type: 'system',
    subtype: 'compact_boundary',
    uuid: 'compact-1',
    content: 'Context compacted',
    compact_metadata: { trigger: 'auto', pre_tokens: 312377, post_tokens: 22074 },
  }),
];

function scenario(name: string) {
  return claudeScenario(name, [
    emit([
      ...textLines('msg-open', 'Spec committed. Now investigating the codebase areas.'),
      ...launchAgents(),
      ...thinkingLines('msg-think-launch', 'Five agents are running.'),
      ...textLines('msg-launched', 'Five investigation agents are running.'),
      RESULT_LINE,
    ]),
    { waitSignal: { name: 'children' } },
    emit(agentChildren()),
    { waitSignal: { name: 'notify' } },
    emit(agentFinishes()),
    { waitSignal: { name: 'compact' } },
    emit([compactingLine]),
    { waitSignal: { name: 'compacted' } },
    emit([
      ...compactionDone,
      ...thinkingLines('msg-think-after', 'All five reports are in, drafting now.'),
      ...textLines('msg-after', 'All five reports are in. Checking the plan-doc precedents.'),
      ...toolRound('post', 1),
    ]),
    { waitSignal: { name: 'finish' } },
    emit([
      ...thinkingLines('msg-think-final', 'Confirming two facts.'),
      ...toolRound('final', 5),
      ...textLines('msg-final', 'Confirming two facts before writing the plan.'),
      RESULT_LINE,
    ]),
  ]);
}

async function seedLongThread(harness: any, project: string, title: string): Promise<string> {
  const turns = Array.from({ length: 8 }, (_, i) => ({
    userText: `History question ${i}`,
    items: [
      { kind: 'thinking', summary: `thinking about ${i}` },
      { kind: 'tool_call', toolName: 'Bash', summary: `history call ${i} a` },
      { kind: 'tool_call', toolName: 'Bash', summary: `history call ${i} b` },
      { kind: 'tool_call', toolName: 'Read', summary: `history read ${i}` },
      { kind: 'assistant_text', summary: `History answer ${i}: a paragraph of prose that takes a few lines so the row has real height on screen.` },
    ],
  }));
  const seed = await harness.rpc('HarnessSeed', {
    projects: [{ name: project, repo: { commits: [{ message: 'init', files: { 'README.md': '# fixture\n' } }] }, threads: [{ title, provider: 'claude', turns }] }],
  }) as SeedResult;
  return seed.projects[0].threadIds[0];
}

async function installSampler(page: import('@playwright/test').Page): Promise<void> {
  await page.evaluate(() => {
    const w = window as unknown as { __aoFrames: FrameSample[]; __aoStop: () => void };
    w.__aoFrames = [];
    let running = true;
    const tick = () => {
      if (!running) return;
      const scroller = document.querySelector('[data-testid="message-timeline-scroll"]') as HTMLElement | null;
      const nodes = Array.from(document.querySelectorAll('[data-testid="message-timeline-node"]')) as HTMLElement[];
      let visibleNodes = 0;
      let hidden = false;
      let firstVisible = '';
      if (scroller) {
        const sr = scroller.getBoundingClientRect();
        for (const n of nodes) {
          const r = n.getBoundingClientRect();
          if (r.height > 0 && r.bottom > sr.top && r.top < sr.bottom) {
            visibleNodes++;
            if (!firstVisible) firstVisible = (n.querySelector('[data-item-id]') as HTMLElement | null)?.dataset.itemId ?? '?';
          }
        }
        const first = nodes[0];
        if (first) {
          let el: HTMLElement | null = first;
          while (el && el !== scroller) {
            if (getComputedStyle(el).visibility === 'hidden') { hidden = true; break; }
            el = el.parentElement;
          }
        }
      }
      w.__aoFrames.push({
        t: performance.now(),
        nodes: nodes.length,
        visibleNodes,
        hidden,
        firstVisible,
        bubbles: document.querySelectorAll('[data-testid="user-message-bubble"]').length,
        scrollTop: scroller?.scrollTop ?? -1,
        scrollHeight: scroller?.scrollHeight ?? -1,
        clientHeight: scroller?.clientHeight ?? -1,
      });
      requestAnimationFrame(tick);
    };
    w.__aoStop = () => { running = false; };
    requestAnimationFrame(tick);
  });
}

async function collectSamples(page: import('@playwright/test').Page): Promise<FrameSample[]> {
  return page.evaluate(() => {
    const w = window as unknown as { __aoFrames: FrameSample[]; __aoStop: () => void };
    w.__aoStop();
    return w.__aoFrames;
  });
}

// Resolves once the timeline's mounted rows and scroll geometry have held
// for `frames` consecutive animation frames, or after `capMs`. A settle
// point between scripted phases, not an assertion: the sampler judges
// every frame either way.
async function timelineSettled(page: import('@playwright/test').Page, frames: number, capMs = 6_000): Promise<void> {
  await page.evaluate(({ frames, capMs }) => new Promise<void>((resolve) => {
    const start = performance.now();
    let previous = '';
    let stable = 0;
    const tick = () => {
      const scroller = document.querySelector('[data-testid="message-timeline-scroll"]') as HTMLElement | null;
      const nodes = document.querySelectorAll('[data-testid="message-timeline-node"]').length;
      const key = scroller ? `${nodes}:${scroller.scrollTop}:${scroller.scrollHeight}` : 'none';
      stable = key === previous ? stable + 1 : 0;
      previous = key;
      if (stable >= frames || performance.now() - start > capMs) resolve();
      else requestAnimationFrame(tick);
    };
    requestAnimationFrame(tick);
  }), { frames, capMs });
}

function longestBlankRun(samples: FrameSample[]): { ms: number; at: FrameSample | null } {
  let best = 0;
  let bestAt: FrameSample | null = null;
  let runStart: FrameSample | null = null;
  for (const s of samples) {
    const blank = s.visibleNodes === 0 || s.hidden;
    if (blank) {
      if (!runStart) runStart = s;
      const ms = s.t - runStart.t;
      if (ms > best) { best = ms; bestAt = runStart; }
    } else {
      runStart = null;
    }
  }
  return { ms: best, at: bestAt };
}

for (const order of ['send-during-compaction', 'send-after-boundary'] as const) {
  test(`timeline stays painted when a queued message flushes around a compaction (${order})`, async ({ harness, page }) => {
    test.setTimeout(180_000);
    await harness.rpc('HarnessSetScenario', { scenario: scenario(`flush-compact-${order}`) });
    const threadId = await seedLongThread(harness, `flush-compact-${order}`, 'Flush after compaction');
    await harness.open(page);
    await page.getByTestId('thread-row').getByText('Flush after compaction', { exact: true }).click();
    await expect(page.getByText('History answer 7', { exact: false })).toBeVisible();
    const mockId = await startMock(harness, threadId);

    const input = page.getByLabel('Message Input');
    // The mock picks a mid-turn message up at once: its echo is what moves
    // the queued row into the timeline.
    const queuedPickedUp = () => harness.waitForEvent('harness:mock', (ev: any) =>
      ev.mockId === mockId && ev.report.kind === 'user_input' && (ev.report.input ?? '').includes(QUEUED));
    await input.fill('Plan the implementation.');
    await page.getByRole('button', { name: 'Send message', exact: true }).click();
    await harness.waitForEvent('provider:turn_completed');
    await expect(page.getByText('Five investigation agents are running.', { exact: true })).toBeVisible();

    await installSampler(page);

    await waitForGate(harness, 'children');
    await advance(harness, mockId, 'children');
    await waitForGate(harness, 'notify');
    // The backend has pushed every child row by the time the mock reports
    // this gate. Let the page apply them and run its folds and prunes.
    await timelineSettled(page, 30);
    await advance(harness, mockId, 'notify');
    await waitForGate(harness, 'compact');
    await expect(page.getByText(`${AGENTS} of ${AGENTS} reports in.`, { exact: true })).toBeVisible();
    await timelineSettled(page, 12);

    await advance(harness, mockId, 'compact');
    await harness.waitForEvent('provider:compacting');
    await waitForGate(harness, 'compacted');

    if (order === 'send-during-compaction') {
      await input.fill(QUEUED);
      await input.press('Enter');
      await queuedPickedUp();
    }
    await advance(harness, mockId, 'compacted');

    await waitForGate(harness, 'finish');
    await expect(page.getByText('All five reports are in. Checking the plan-doc precedents.', { exact: true })).toBeVisible({ timeout: 15_000 });

    if (order === 'send-after-boundary') {
      await input.fill(QUEUED);
      await input.press('Enter');
    }
    if (order === 'send-after-boundary') await queuedPickedUp();
    await timelineSettled(page, 12);
    await advance(harness, mockId, 'finish');
    // The queued message's own turn opens only after the running turn wrote
    // its last frame.
    await harness.waitForEvent('harness:mock', (ev: any) =>
      ev.mockId === mockId && ev.report.kind === 'turn_started' && ev.report.detail === 'afterTurns:silent');

    let samples: FrameSample[] = [];
    try {
      await expect(page.getByText('Confirming two facts before writing the plan.', { exact: true })).toBeVisible({ timeout: 20_000 });
      await timelineSettled(page, 12);
      // A blank that persists into the settled state runs past the limit.
      await page.waitForTimeout(BLANK_LIMIT_MS + 200);
    } finally {
      samples = await collectSamples(page);
      const blank = longestBlankRun(samples);
      console.log(`[probe ${order}] frames=${samples.length} longestBlankMs=${Math.round(blank.ms)} at=${JSON.stringify(blank.at)} last=${JSON.stringify(samples[samples.length - 1])}`);
    }
    await page.screenshot({ path: `test-results/flush-compact-${order}.png`, fullPage: false });
    const blank = longestBlankRun(samples);
    expect(blank.ms, `timeline painted no rows for ${Math.round(blank.ms)}ms starting at ${JSON.stringify(blank.at)}`).toBeLessThan(BLANK_LIMIT_MS);
    await expect(page.getByText(QUEUED, { exact: true })).toHaveCount(1);
  });
}
