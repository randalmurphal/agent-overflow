// A following viewport must reach the physical tail above the composer after
// active work advances while the thread is switched away or its pane is closed.
import { test, expect, type SeedResult } from './fixtures.js';
import { advance, claudeScenario, emit, startMock, textLines, toolResultLine, toolUseLine, waitForGate } from './agent-visibility-helpers.js';

const batch = (start: number, count: number) => Array.from({ length: count }, (_, j) => {
  const i = start + j;
  return [toolUseLine(`message-${i}`, `tool-${i}`, 'Bash', { command: `echo active-return-${i}` }), toolResultLine(`tool-${i}`, 'done')];
}).flat();

for (const returnPath of ['switch', 'reopen'] as const) {
  for (const [initialCount, awayCount, backCount] of [[5, 60, 5], [39, 1, 1], [39, 60, 1]] as const) {
    test(`active activity reaches the visible bottom after ${returnPath} (${initialCount} initial, ${awayCount} new rows)`, async ({ harness, page }) => {
      await page.setViewportSize({ width: 1600, height: 1000 });
      await harness.rpc('HarnessSetScenario', { scenario: claudeScenario('active-return', [
        emit([...textLines('lead', 'Starting active work.'), ...batch(0, initialCount)]),
        { waitSignal: { name: 'away' } }, emit(batch(initialCount, awayCount)),
        { waitSignal: { name: 'back' } }, emit(batch(initialCount + awayCount, backCount)),
        { waitSignal: { name: 'hold' } },
      ]) });
      const seed = await harness.rpc<SeedResult>('HarnessSeed', { projects: [{ name: 'active-return', repo: {}, threads: [
        { title: 'Active return', provider: 'claude', turns: Array.from({ length: 20 }, (_, i) => ({ userText: `Question ${i}`, items: [{ kind: 'assistant_text', summary: `Earlier answer ${i}. ${'Some detailed history. '.repeat(20)}` }] })) },
        { title: 'Away', turns: [{ userText: 'Other', items: [{ kind: 'assistant_text', summary: 'Away answer.' }] }] },
      ] }] });
      const threadId = seed.projects[0].threadIds[0];
      await harness.open(page);
      const open = () => page.getByTestId('thread-row').getByText('Active return', { exact: true }).click();
      await open();
      const mockId = await startMock(harness, threadId);
      await harness.rpc('SendMessage', threadId, 'Start', null);
      await waitForGate(harness, 'away');
      const tail = page.getByTestId('activity-run').last();
      await expect(tail).toHaveAttribute('data-live', 'true');
      await expect(tail.getByTestId('activity-run-header-counts')).toContainText(`${initialCount} Bash`);
      if ((await tail.getAttribute('data-collapsed')) === 'true') await tail.getByTestId('activity-run-header').click();
      const scroll = page.getByTestId('message-timeline-scroll');
      const gap = () => scroll.evaluate(el => el.scrollHeight - el.clientHeight - el.scrollTop);
      const innerGap = () => tail.getByTestId('activity-run-clip').evaluate(el => el.scrollHeight - el.clientHeight - el.scrollTop);
      await expect.poll(gap).toBeLessThan(1);
      await expect.poll(innerGap).toBeLessThan(1);
      await expect(tail.getByTestId('activity-run-later')).toHaveCount(0);
      if (returnPath === 'switch') {
        await page.getByTestId('thread-row').getByText('Away', { exact: true }).click();
        await expect(page.getByText('Away answer.', { exact: true })).toBeVisible();
      } else {
        await page.getByTestId('pane-close').click();
        await expect(scroll).toHaveCount(0);
      }
      await advance(harness, mockId, 'away');
      await waitForGate(harness, 'back');
      const returnSamples = awayCount === 1 ? page.evaluate(() => new Promise<Array<{ counts: string; later: boolean }>>(resolve => {
        const samples: Array<{ counts: string; later: boolean }> = [];
        let frames = 0;
        function sample() {
          const runs = document.querySelectorAll<HTMLElement>('[data-testid="activity-run"]');
          const run = runs[runs.length - 1];
          if (run && getComputedStyle(run).visibility === 'visible') {
            samples.push({
              counts: run.querySelector('[data-testid="activity-run-header-counts"]')?.textContent ?? '',
              later: run.querySelector('[data-testid="activity-run-later"]') !== null,
            });
          }
          if (++frames < 120) requestAnimationFrame(sample);
          else resolve(samples);
        }
        requestAnimationFrame(sample);
      })) : null;
      await open();
      await expect(tail.getByTestId('activity-run-header-counts')).toContainText(`${initialCount + awayCount} Bash`);
      await expect(tail.getByTestId('activity-run-later')).toHaveCount(0);
      await expect.poll(gap).toBeLessThan(1);
      await expect.poll(async () => {
        const bottom = (await tail.boundingBox())!;
        const composer = (await page.getByTestId('composer-overlay').boundingBox())!;
        return bottom.y + bottom.height <= composer.y;
      }).toBe(true);
      await expect(page.getByRole('button', { name: 'Jump to bottom' })).toHaveCount(0);
      if (returnSamples) {
        const samples = await returnSamples;
        expect(samples.length).toBeGreaterThan(30);
        expect(samples.every(sample => sample.counts.includes(`${initialCount + awayCount} Bash`) && !sample.later), JSON.stringify(samples)).toBe(true);
      }
      await advance(harness, mockId, 'back');
      await waitForGate(harness, 'hold');
      await expect(tail.getByTestId('activity-run-header-counts')).toContainText(`${initialCount + awayCount + backCount} Bash`);
      await expect(tail.getByTestId('activity-run-later')).toHaveCount(0);
      await expect.poll(gap).toBeLessThan(1);
    });
  }
}

test('a reader-pinned run keeps its earlier window when its tail advances out of range', async ({ harness, page }) => {
  await page.setViewportSize({ width: 1600, height: 1000 });
  await harness.rpc('HarnessSetScenario', { scenario: claudeScenario('pinned-return', [
    emit([...textLines('lead', 'Starting active work.'), ...batch(0, 39)]),
    { waitSignal: { name: 'away' } }, emit(batch(39, 60)),
    { waitSignal: { name: 'hold' } },
  ]) });
  const seed = await harness.rpc<SeedResult>('HarnessSeed', { projects: [{ name: 'pinned-return', repo: {}, threads: [
    { title: 'Pinned return', provider: 'claude', turns: [
      { userText: 'Earlier', items: [{ kind: 'assistant_text', summary: 'Earlier answer.' }] },
    ] },
    { title: 'Away', turns: [{ userText: 'Other', items: [{ kind: 'assistant_text', summary: 'Away answer.' }] }] },
  ] }] });
  await harness.open(page);
  await page.getByTestId('thread-row').getByText('Pinned return', { exact: true }).click();
  const mockId = await startMock(harness, seed.projects[0].threadIds[0]);
  await harness.rpc('SendMessage', seed.projects[0].threadIds[0], 'Start', null);
  await waitForGate(harness, 'away');
  const run = page.getByTestId('activity-run').last();
  await expect(run.getByTestId('activity-run-header-counts')).toContainText('39 Bash');
  const clip = run.getByTestId('activity-run-clip');
  await expect.poll(() => clip.evaluate(el => el.scrollTop)).toBeGreaterThan(0);
  await clip.hover();
  await page.mouse.wheel(0, -500);
  await expect.poll(() => clip.evaluate(el => el.scrollHeight - el.clientHeight - el.scrollTop)).toBeGreaterThan(100);
  await page.getByTestId('thread-row').getByText('Away', { exact: true }).click();
  await expect(page.getByText('Away answer.', { exact: true })).toBeVisible();
  await advance(harness, mockId, 'away');
  await waitForGate(harness, 'hold');
  await page.getByTestId('thread-row').getByText('Pinned return', { exact: true }).click();
  await expect(run.getByTestId('activity-run-header-counts')).toContainText('99 Bash');
  await expect(run.getByTestId('activity-run-later')).toContainText('60 later');
  await expect(run.getByText('echo active-return-38', { exact: true })).toBeVisible();
});

test('a warm return keeps its cached activity hidden until the backend verifies it', async ({ harness, page }) => {
  let holdReturnSync = false;
  let releaseRequest: (() => void) | null = null;
  let sentWindow = false;
  let sentEpoch = -1;
  await page.routeWebSocket(/\/ws(?:\?|$)/, socket => {
    const server = socket.connectToServer();
    socket.onMessage(message => {
      const frame = JSON.parse(String(message));
      if (holdReturnSync && frame.type === 'rpc' && frame.methodId === 3841902986) {
        sentWindow = Boolean(frame.params?.[1]?.haveWindow);
        sentEpoch = frame.params?.[1]?.haveEpoch ?? -1;
        releaseRequest = () => server.send(message);
      } else {
        server.send(message);
      }
    });
    server.onMessage(message => socket.send(message));
  });
  await harness.rpc<SeedResult>('HarnessSeed', { projects: [{ name: 'verified-return', repo: {}, threads: [
    { title: 'Active return', provider: 'claude', turns: [{ userText: 'Earlier', items: [
      { kind: 'assistant_text', summary: 'Earlier response.' },
      { kind: 'tool_call', toolName: 'Bash', summary: 'echo verified-return' },
    ] }] },
    { title: 'Away', turns: [{ userText: 'Other', items: [{ kind: 'assistant_text', summary: 'Away answer.' }] }] },
  ] }] });
  await harness.open(page);
  const open = () => page.getByTestId('thread-row').getByText('Active return', { exact: true }).click();
  await open();
  await expect(page.getByTestId('activity-run').last().getByTestId('activity-run-header-counts')).toContainText('1 Bash');
  await page.getByTestId('thread-row').getByText('Away', { exact: true }).click();
  await expect(page.getByText('Away answer.', { exact: true })).toBeVisible();
  holdReturnSync = true;
  await page.evaluate(() => {
    const w = window as typeof window & { unverifiedPaints?: number };
    w.unverifiedPaints = 0;
    let frames = 0;
    function sample() {
      const run = document.querySelector<HTMLElement>('[data-testid="activity-run"]');
      if (run && getComputedStyle(run).visibility === 'visible') w.unverifiedPaints! += 1;
      if (++frames < 40) requestAnimationFrame(sample);
    }
    requestAnimationFrame(sample);
  });
  await open();
  await expect.poll(() => releaseRequest !== null).toBe(true);
  expect(sentWindow || sentEpoch >= 0, JSON.stringify({ sentWindow, sentEpoch })).toBe(true);
  await expect(page.getByTestId('activity-run')).toHaveCount(0);
  await expect(page.getByRole('status').getByText('Loading thread...')).toBeVisible();
  // Cross the warm-up failsafe while the response is held. Its expiry
  // must not reveal the unverified cached run on a slow connection.
  await page.waitForTimeout(2700);
  await expect(page.getByTestId('activity-run')).toHaveCount(0);
  expect(await page.evaluate(() => (window as typeof window & { unverifiedPaints: number }).unverifiedPaints)).toBe(0);
  await page.evaluate(() => {
    const w = window as typeof window & { returnSamples?: Promise<Array<{ count: string; gap: number; clearsComposer: boolean }>> };
    w.returnSamples = new Promise(resolve => {
      const samples: Array<{ count: string; gap: number; clearsComposer: boolean }> = [];
      let frames = 0;
      function sample() {
        const run = document.querySelector<HTMLElement>('[data-testid="activity-run"]');
        const scroll = document.querySelector<HTMLElement>('[data-testid="message-timeline-scroll"]');
        const composer = document.querySelector<HTMLElement>('[data-testid="composer-overlay"]');
        if (run && scroll && composer && getComputedStyle(run).visibility === 'visible') {
          samples.push({
            count: run.querySelector<HTMLElement>('[data-testid="activity-run-header-counts"]')?.textContent ?? '',
            gap: scroll.scrollHeight - scroll.clientHeight - scroll.scrollTop,
            clearsComposer: run.getBoundingClientRect().bottom <= composer.getBoundingClientRect().top,
          });
        }
        if (++frames < 120) requestAnimationFrame(sample);
        else resolve(samples);
      }
      requestAnimationFrame(sample);
    });
  });
  releaseRequest!();
  await expect(page.getByTestId('activity-run').last().getByTestId('activity-run-header-counts')).toContainText('1 Bash');
  const samples = await page.evaluate(() => (window as typeof window & { returnSamples: Promise<Array<{ count: string; gap: number; clearsComposer: boolean }>> }).returnSamples);
  expect(samples.length).toBeGreaterThan(30);
  expect(samples.every(sample => sample.count.includes('1 Bash') && sample.gap < 1 && sample.clearsComposer), JSON.stringify(samples)).toBe(true);
  const scroll = page.getByTestId('message-timeline-scroll');
  await expect.poll(() => scroll.evaluate(el => el.scrollHeight - el.clientHeight - el.scrollTop)).toBeLessThan(1);
  await expect(page.getByRole('button', { name: 'Jump to bottom' })).toHaveCount(0);
});
