import { test, expect, type Browser } from '@playwright/test';
import { startCompositorTrace, summarizeCompositorWindow } from '../src/compositorTrace.js';

const DPR_VALUES = [1, 1.25, 1.5, 2] as const;
const SCROLL_WRITES = [0, 1, 2, 3, 4] as const;

interface GeometrySample {
  readonly requested: number;
  readonly scrollTop: number;
  readonly featureTop: number;
  readonly viewportTop: number;
  readonly featureHeight: number;
  readonly featureWidth: number;
  readonly frames: number;
}

interface GeometryRun {
  readonly measured: readonly GeometrySample[];
  readonly compositor: ReturnType<typeof summarizeCompositorWindow>;
}

async function measureAtDpr(browser: Browser, dpr: number): Promise<GeometryRun> {
  const context = await browser.newContext({
    viewport: { width: 300, height: 200 },
    deviceScaleFactor: dpr,
  });
  try {
    const page = await context.newPage();
    await page.setContent(`
      <style>
        * { box-sizing: border-box; }
        html, body { margin: 0; background: #000; }
        #scroller {
          position: fixed;
          left: 20px;
          top: 20px;
          width: 200px;
          height: 100px;
          overflow: auto;
          scrollbar-width: none;
          background: #000;
        }
        #scroller::-webkit-scrollbar { display: none; }
        #content { position: relative; height: 500px; }
        #hairline {
          position: absolute;
          left: 10px;
          right: 10px;
          top: 60px;
          height: 1px;
          background: #fff;
        }
      </style>
      <div id="scroller"><div id="content"><div id="hairline"></div></div></div>
    `);

    const compositorTrace = await startCompositorTrace(page);
    const startMark = `ao-scroll-dpr-${String(dpr)}-start`;
    const endMark = `ao-scroll-dpr-${String(dpr)}-end`;
    await page.evaluate((mark) => performance.mark(mark), startMark);
    const measured: GeometrySample[] = [];
    for (const requested of SCROLL_WRITES) {
      const sample = await page.locator('#scroller').evaluate((element, top) => {
        element.scrollTop = top;
        const scroller = element.getBoundingClientRect();
        const feature = element.querySelector<HTMLElement>('#hairline');
        if (!feature) throw new Error('hairline fixture is missing');
        let frames = 0;
        return new Promise<GeometrySample>((resolve) => {
          const tick = () => {
            frames += 1;
            if (frames < 2) {
              requestAnimationFrame(tick);
              return;
            }
            const rect = feature.getBoundingClientRect();
            resolve({
              requested: top,
              scrollTop: element.scrollTop,
              featureTop: rect.top,
              viewportTop: scroller.top,
              featureHeight: rect.height,
              featureWidth: rect.width,
              frames,
            });
          };
          requestAnimationFrame(tick);
        });
      }, requested);
      measured.push(sample);
    }
    await page.evaluate((mark) => performance.mark(mark), endMark);
    const compositor = summarizeCompositorWindow(
      await compositorTrace.stop(),
      startMark,
      endMark,
    );
    return { measured, compositor };
  } finally {
    await context.close();
  }
}

test('integer scrollTop preserves thin-feature layout geometry at every DPR', async ({ browser }) => {
  for (const dpr of DPR_VALUES) {
    const { measured, compositor } = await measureAtDpr(browser, dpr);
    expect(compositor.eventCount, `trace events at DPR ${dpr}`).toBeGreaterThan(0);
    expect(compositor.renderPasses, `render passes at DPR ${dpr}`).toBeGreaterThan(0);
    expect(compositor.prepareDraws, `compositor draws at DPR ${dpr}`).toBeGreaterThan(0);
    expect(compositor.layerTreeSnapshots, `layer tree snapshots at DPR ${dpr}`).toBeGreaterThan(0);
    expect(compositor.missingTileSignals, `missing tiles at DPR ${dpr}`).toBe(0);
    expect(compositor.checkerboardSignals, `checkerboard signals at DPR ${dpr}`).toBe(0);
    expect(compositor.blankRenderPasses, `blank render passes at DPR ${dpr}`).toBe(0);
    expect(measured).toHaveLength(SCROLL_WRITES.length);
    for (const [index, frame] of measured.entries()) {
      expect(frame.scrollTop, `scrollTop ${frame.requested}px at DPR ${dpr}`).toBe(frame.requested);
      expect(frame.frames, `rAF continuity at DPR ${dpr}`).toBeGreaterThanOrEqual(2);
      expect(frame.featureHeight, `feature height at DPR ${dpr}`).toBe(1);
      expect(frame.featureWidth, `feature width at DPR ${dpr}`).toBe(180);
      expect(frame.featureTop, `feature position at DPR ${dpr}`).toBe(
        frame.viewportTop + 60 - frame.requested,
      );
      if (index > 0) {
        expect(frame.featureTop - measured[index - 1].featureTop).toBe(-1);
      }
    }
  }
});
