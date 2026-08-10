// Operator-run driver for the recurring WebView2 renderer main-thread freeze.
//
// NOT part of `make e2e`: the base playwright config testIgnores `*.manual.spec.ts`,
// and this file only runs through `pnpm test:freeze-repro`
// (playwright.manual.config.ts). Two reasons it is excluded rather than
// skipped-by-env: it needs a locally generated fixture that is gitignored real
// session content, and it deliberately loads ~950 timeline items into one pane,
// which is not a state the shared worker backend should ever be left in.
//
// What it does: replays the exact recorded incident stream through the real
// backend and the real SPA — seeded turns as completed history, the dense turns
// live through the mock provider with their real payload_chunks boundaries —
// while an out-of-band monitor watches renderer liveness. On a wedge it
// captures the spinning stack (profiler-first, Debugger.pause second) and fails
// naming the top frames. Without a wedge it reports the longest evaluate gap,
// which is the near-miss signal.
//
// Generate the fixture first:
//   node e2e/scripts/generate-freeze-repro.mjs --thread <id> \
//     --seed-turns 220-243 --live-turns 244-247

import { existsSync, readFileSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { expect, test } from '@playwright/test';

import { launchHarness, type HarnessApp } from '../src/harness.js';
import {
  captureConsole,
  captureWedgeEvidence,
  startCaptureSessions,
  startRendererMonitor,
} from './freeze-repro-probe.js';

const HERE = path.dirname(fileURLToPath(import.meta.url));
const FIXTURE_DIR = path.join(HERE, '..', 'fixtures', 'freeze-repro');

interface Manifest {
  threadTitle: string;
  seedTurns: [number, number];
  liveTurns: [number, number];
  pacing: { gapMs: number; deltaMs: number; scale: number };
  seed: { turns: number; items: number; payloadChars: number };
  live: Array<{ turn: number; userText: string; recordedItems: number; emittedItems: number }>;
}

interface SeedResult {
  projects: Array<{ projectId: string; path: string; threadIds: string[] }>;
}

interface HarnessMockEvent {
  mockId: string;
  report: { kind: string; detail?: string };
}

/** No probe answered for this long ⇒ the renderer main thread is wedged. */
const WEDGE_MS = 12_000;
/** A live turn replays hundreds of items; give each one plenty of room. */
const TURN_TIMEOUT_MS = 10 * 60_000;
/** Cap on "Load older messages" presses while paging the seeded history in. */
const MAX_PAGE_IN_ROUNDS = 40;

function readFixture<T>(name: string): T {
  return JSON.parse(readFileSync(path.join(FIXTURE_DIR, name), 'utf8')) as T;
}

test.describe('freeze repro', () => {
  test('replays the recorded incident stream and watches for a renderer wedge', async ({
    browser,
  }, testInfo) => {
    test.skip(
      !existsSync(path.join(FIXTURE_DIR, 'manifest.json')),
      `no fixture at ${FIXTURE_DIR} — generate one first:\n` +
        '  node e2e/scripts/generate-freeze-repro.mjs --thread <id> ' +
        '--seed-turns 220-243 --live-turns 244-247',
    );

    const manifest = readFixture<Manifest>('manifest.json');
    const seedSpec = readFixture<unknown>('seed.json');
    const scenarioDoc = readFixture<unknown>('scenario.json');

    // This spec owns its backend: ~950 items in one thread is not state the
    // worker-shared fixture should inherit.
    const harness: HarnessApp = await launchHarness();
    const context = await browser.newContext();
    const page = await context.newPage();
    const consoleLines = captureConsole(page);
    const timeline: string[] = [];
    const note = (line: string) => {
      timeline.push(`${new Date().toISOString()} ${line}`);
      console.log(`[freeze-repro] ${line}`);
    };

    let monitor: ReturnType<typeof startRendererMonitor> | undefined;
    try {
      note(
        `fixture: seed turns ${manifest.seedTurns.join('-')} (${manifest.seed.items} items), ` +
          `live turns ${manifest.liveTurns.join('-')}, ` +
          `pacing gap=${manifest.pacing.gapMs}ms delta=${manifest.pacing.deltaMs}ms`,
      );

      const seed = await harness.rpc<SeedResult>('HarnessSeed', seedSpec);
      const threadId = seed.projects[0].threadIds[0];
      await harness.rpc('HarnessSetScenario', { scenario: scenarioDoc });
      note(`seeded thread ${threadId}`);

      await page.goto(harness.url);
      await page.getByText(manifest.threadTitle, { exact: false }).first().click();
      const scroller = page.getByTestId('message-timeline-scroll');
      await expect(scroller).toBeVisible();

      // The initial load is tail-only; the incident pane had the seeded range
      // resident (oldestLoadedTurnIndex well below the tail, ~800 items — the
      // window's own ACTIVE_TIMELINE_WINDOW_MAX_ITEMS). "Load older messages"
      // rides inside the FIRST virtualized row, so each round has to scroll to
      // the top before the button exists in the DOM at all.
      const loadOlder = page.getByTestId('load-older-messages');
      const toTop = () => scroller.evaluate((el) => void (el.scrollTop = 0));
      // Find-and-click inside ONE evaluate. The button lives in a row the
      // virtualizer mounts and unmounts as the window moves, so a locator
      // click has a real detach window between resolving and acting; this has
      // none, and simply reports false when the row is not mounted right now.
      const clickLoadOlder = () =>
        page.evaluate(() => {
          const btn = document.querySelector('[data-testid="load-older-messages"]');
          if (!(btn instanceof HTMLElement) || btn.hasAttribute('disabled')) return false;
          btn.click();
          return true;
        });

      const scrollHeight = () => scroller.evaluate((el) => el.scrollHeight);
      let rounds = 0;
      let attempts = 0;
      let stoppedBecause = 'button gone (all history resident)';
      while (rounds < MAX_PAGE_IN_ROUNDS && attempts < MAX_PAGE_IN_ROUNDS * 3) {
        attempts += 1;
        await toTop();
        // The virtualizer needs a frame or two to mount row 0 after the jump.
        const appeared = await loadOlder
          .first()
          .waitFor({ state: 'attached', timeout: 3_000 })
          .then(() => true, () => false);
        if (!appeared) break;
        const before = await scrollHeight();
        if (!(await clickLoadOlder())) continue;
        // Content growth is the load's own completion signal, and it does not
        // depend on the button surviving the re-anchor that follows a load.
        const grew = await expect
          .poll(scrollHeight, { timeout: 30_000, intervals: [150] })
          .toBeGreaterThan(before)
          .then(() => true, () => false);
        if (!grew) {
          stoppedBecause = `no content growth after round ${rounds + 1}`;
          break;
        }
        rounds += 1;
      }
      if (rounds >= MAX_PAGE_IN_ROUNDS) stoppedBecause = 'hit the round cap';
      if (attempts >= MAX_PAGE_IN_ROUNDS * 3) stoppedBecause = 'hit the attempt cap';
      note(
        `paged in older history in ${rounds} rounds (${await scrollHeight()}px of content); ` +
          `stopped: ${stoppedBecause}`,
      );
      await scroller.evaluate((el) => {
        el.scrollTop = el.scrollHeight;
      });

      await harness.rpc('StartSession', threadId);
      await harness.waitForEvent<HarnessMockEvent>(
        'harness:mock',
        (ev) => ev.report.kind === 'registered',
      );

      // Both CDP capture channels are armed while the renderer is still
      // healthy — see the header of freeze-repro-probe.ts for why on-demand
      // arming does not work against a wedge that never clears.
      const sessions = await startCaptureSessions(context, page);
      monitor = startRendererMonitor(page, { wedgeMs: WEDGE_MS });

      for (const turn of manifest.live) {
        const startedAt = Date.now();
        note(`turn ${turn.turn}: sending (${turn.emittedItems} items to replay)`);
        await harness.rpc('SendMessage', threadId, turn.userText, null);

        // The turn's completion and the wedge race each other. The loser's
        // rejection is swallowed on purpose — a timed-out waitForEvent after a
        // wedge is expected, not a second failure.
        const completed = harness
          .waitForEvent('provider:turn_completed', undefined, TURN_TIMEOUT_MS)
          .then(() => 'completed' as const)
          .catch((err: Error) => `timeout: ${err.message}` as const);
        const outcome = await Promise.race([
          completed,
          monitor.wedged.then(() => 'wedged' as const),
        ]);
        const wallMs = Date.now() - startedAt;

        if (outcome === 'wedged') {
          const wedge = await monitor.wedged;
          note(
            `turn ${turn.turn}: WEDGED after ${wallMs}ms ` +
              `(no renderer answer for ${wedge.gapMs}ms)`,
          );
          const dir = path.join(FIXTURE_DIR, `evidence-${new Date().toISOString().replace(/[:.]/g, '-')}`);
          const evidence = await captureWedgeEvidence({
            sessions,
            dir,
            consoleLines,
            wedge,
            monitor,
          });
          await testInfo.attach('freeze-repro-timeline', {
            body: timeline.join('\n'),
            contentType: 'text/plain',
          });
          throw new Error(
            `renderer wedged on live turn ${turn.turn} after ${wallMs}ms ` +
              `(${wedge.gapMs}ms with no answer; longest gap ${monitor.longestGapMs}ms; ` +
              // A wedge verdict built on silence has to say whether the
              // silence was the renderer's. Rejected probes mean the page
              // died or navigated, which produces the same silence and none
              // of the same conclusions.
              `${monitor.probesRejected} probe(s) REJECTED${
                monitor.lastRejection ? ` — last: ${monitor.lastRejection}` : ''
              }; ` +
              `freeze ${
                monitor.recoveredAfterMs === null
                  ? 'had not cleared by capture time'
                  : `lasted ${monitor.recoveredAfterMs}ms`
              }).\n` +
              `evidence: ${evidence.dir}\n` +
              (evidence.profileSaved ? '' : `profile NOT captured: ${evidence.profileError}\n`) +
              `top frames:\n  ${evidence.topFrames.join('\n  ') || '(none captured)'}`,
          );
        }

        if (outcome !== 'completed') {
          throw new Error(`turn ${turn.turn} never completed and never wedged — ${outcome}`);
        }
        note(`turn ${turn.turn}: completed in ${wallMs}ms (longest gap so far ${monitor.longestGapMs}ms)`);
      }

      monitor.stop();
      await sessions.profiler.send('Profiler.stop').catch(() => undefined);
      note(
        `all ${manifest.live.length} live turns completed without a wedge; ` +
          `LONGEST evaluate gap ${monitor.longestGapMs}ms ` +
          `(wedge threshold ${WEDGE_MS}ms, ${monitor.probesResolved} probes answered` +
          `${monitor.probesRejected > 0 ? `, ${monitor.probesRejected} rejected` : ''})`,
      );
      await testInfo.attach('freeze-repro-timeline', {
        body: timeline.join('\n'),
        contentType: 'text/plain',
      });
      expect(monitor.isWedged).toBe(false);
    } finally {
      monitor?.stop();
      await page.close().catch(() => undefined);
      await context.close().catch(() => undefined);
      await harness.close();
    }
  });
});
