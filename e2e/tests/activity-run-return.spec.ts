// Warm switches and pane reopen must retain enough fresh history across
// byte-limited sync pages to keep the visible activity tail at the bottom.
import { test, expect, type SeedResult } from './fixtures.js';

for (const deviceScaleFactor of [1, 1.25, 1.5, 2]) {
  test.describe(`DPR ${deviceScaleFactor}`, () => {
    test.use({ deviceScaleFactor });
    for (const returnPath of ['switch', 'reopen'] as const) {
      test(`a byte-limited refresh does not shrink visible history on ${returnPath}`, async ({ harness, page }) => {
        await page.setViewportSize({ width: 1900, height: 1350 });
        let revalidating = false;
        let responses = 0;
        const syncRequests = new Set<unknown>();
        await page.routeWebSocket(/\/ws(?:\?|$)/, socket => {
          const server = socket.connectToServer();
          socket.onMessage(message => {
            const frame = JSON.parse(String(message));
            if (revalidating && frame.type === 'rpc' && frame.methodId === 3841902986) {
              frame.params[1] = { ...frame.params[1], haveEpoch: -1, haveRev: -1, haveWindow: null };
              syncRequests.add(frame.id);
              server.send(JSON.stringify(frame));
            } else server.send(message);
          });
          server.onMessage(message => {
            const frame = JSON.parse(String(message));
            if (syncRequests.delete(frame.id)) responses++;
            socket.send(message);
          });
        });
        const items = Array.from({ length: 35 }, (_, i) => [
          { kind: 'assistant_text', summary: `Response ${i}. ${'The work continued with additional checks. '.repeat(8)}` },
          ...Array.from({ length: i === 34 ? 3 : 12 }, (_, j) => ({
            kind: 'tool_call', toolName: 'command_execution', summary: `Inspection ${i}-${j}`,
            meta: JSON.stringify({ command: `echo inspection-${i}-${j} ${'x'.repeat(9000)}`, exit_code: 0 }),
          })),
        ]).flat();
        await harness.rpc<SeedResult>('HarnessSeed', { projects: [{ name: 'activity-return', repo: {}, threads: [
          { title: 'Long history', provider: 'codex', turns: [{ userText: 'Review', items }] },
          { title: 'Away history', turns: [{ userText: 'Other', items: [{ kind: 'assistant_text', summary: 'An unrelated answer.' }] }] },
        ] }] });
        await harness.open(page);
        const open = () => page.getByTestId('thread-row').getByText('Long history', { exact: true }).click();
        await open();
        const tail = page.getByTestId('activity-run').last();
        await expect(tail).toBeVisible();
        // Wait for the initial short page to fill the viewport and settle.
        await expect.poll(() => page.getByTestId('message-timeline-scroll').evaluate(el => el.scrollHeight - el.clientHeight)).toBeGreaterThan(500);
        const baseline = await tail.evaluate(async el => {
          let previous = -1;
          let stable = 0;
          for (let frame = 0; frame < 180; frame++) {
            await new Promise(requestAnimationFrame);
            const bottom = el.getBoundingClientRect().bottom;
            stable = bottom === previous ? stable + 1 : 0;
            if (stable === 12) return bottom;
            previous = bottom;
          }
          throw new Error('Initial activity tail did not settle');
        });
        for (let round = 0; round < 2; round++) {
          if (returnPath === 'switch') {
            await page.getByTestId('thread-row').getByText('Away history', { exact: true }).click();
            await expect(page.getByText('An unrelated answer.', { exact: true })).toBeVisible();
          } else {
            await page.getByTestId('pane-close').click();
            await expect(page.getByTestId('message-timeline-scroll')).toHaveCount(0);
          }
          revalidating = true;
          const before = responses;
          await page.evaluate(() => {
            const w = window as typeof window & { tailSamples?: Promise<number[]> };
            w.tailSamples = new Promise(resolve => {
              const samples: number[] = [];
              let frames = 0;
              function record() {
                const rows = document.querySelectorAll('[data-testid="activity-run"]');
                const tail = rows[rows.length - 1];
                if (tail && getComputedStyle(tail).visibility === 'visible') samples.push(tail.getBoundingClientRect().bottom);
              }
              function sample() {
                record();
                // Also observe after this frame's layout/ResizeObserver work.
                setTimeout(() => {
                  record();
                  if (++frames < 120) requestAnimationFrame(sample);
                  else resolve(samples);
                }, 0);
              }
              requestAnimationFrame(sample);
            });
          });
          await open();
          await expect.poll(() => responses).toBeGreaterThan(before);
          const samples = await page.evaluate(() => (window as typeof window & { tailSamples: Promise<number[]> }).tailSamples);
          expect(samples.length).toBeGreaterThan(30);
          expect(Math.max(...samples.map(bottom => Math.abs(bottom - baseline))), JSON.stringify({ baseline, samples })).toBe(0);
        }
      });
    }
  });
}
