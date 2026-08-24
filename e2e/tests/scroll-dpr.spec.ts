import { test, expect, type Browser, type Page } from '@playwright/test';

const DPR_VALUES = [1, 1.25, 1.5, 2] as const;
const SCROLL_WRITES = [0, 1, 2, 3, 4] as const;

interface RasterMetrics {
  readonly total: number;
  readonly peakRow: number;
  readonly signature: readonly number[];
}

async function rasterMetrics(analyzer: Page, png: Buffer, dpr: number): Promise<RasterMetrics> {
  return analyzer.evaluate(async ({ base64, dpr: scale }) => {
    const image = new Image();
    image.src = `data:image/png;base64,${base64}`;
    await image.decode();
    const canvas = document.createElement('canvas');
    canvas.width = image.naturalWidth;
    canvas.height = image.naturalHeight;
    const context = canvas.getContext('2d', { willReadFrequently: true });
    if (!context) throw new Error('2D canvas unavailable while reading raster probe');
    context.drawImage(image, 0, 0);
    const pixels = context.getImageData(0, 0, canvas.width, canvas.height).data;
    const rowEnergy: number[] = [];
    const xStart = Math.round(10 * scale);
    const xEnd = Math.round(190 * scale);
    for (let y = 0; y < canvas.height; y += 1) {
      let energy = 0;
      for (let x = xStart; x < xEnd; x += 1) {
        energy += pixels[(y * canvas.width + x) * 4];
      }
      rowEnergy.push(energy);
    }
    const peak = Math.max(...rowEnergy);
    return {
      total: rowEnergy.reduce((sum, value) => sum + value, 0),
      peakRow: rowEnergy.indexOf(peak),
      signature: rowEnergy.filter((value) => value > 0).map((value) => value / peak),
    };
  }, { base64: png.toString('base64'), dpr });
}

async function measureAtDpr(browser: Browser, dpr: number): Promise<readonly RasterMetrics[]> {
  const context = await browser.newContext({
    viewport: { width: 300, height: 200 },
    deviceScaleFactor: dpr,
  });
  try {
    const page = await context.newPage();
    const analyzer = await context.newPage();
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

    const measured: RasterMetrics[] = [];
    for (const requested of SCROLL_WRITES) {
      const readback = await page.locator('#scroller').evaluate((element, top) => {
        element.scrollTop = top;
        return element.scrollTop;
      }, requested);
      expect(readback, `scrollTop ${requested}px at DPR ${dpr}`).toBe(requested);
      await page.evaluate(() => new Promise(requestAnimationFrame));
      const screenshot = await page.screenshot({
        clip: { x: 20, y: 20, width: 200, height: 100 },
        scale: 'device',
      });
      measured.push(await rasterMetrics(analyzer, screenshot, dpr));
    }
    return measured;
  } finally {
    await context.close();
  }
}

test('integer scrollTop preserves thin-feature raster energy at fractional DPR', async ({ browser }) => {
  for (const dpr of DPR_VALUES) {
    const measured = await measureAtDpr(browser, dpr);
    const baseline = measured[0];
    expect(baseline.total, `non-vacuous raster at DPR ${dpr}`).toBeGreaterThan(0);
    for (const frame of measured.slice(1)) {
      expect(frame.total, `hairline energy at DPR ${dpr}`).toBe(baseline.total);
      expect(frame.signature, `hairline coverage at DPR ${dpr}`).toEqual(baseline.signature);
    }

    const devicePixelSteps = measured.slice(1).map((frame, index) =>
      Math.abs(frame.peakRow - measured[index].peakRow));
    if (!Number.isInteger(dpr)) {
      expect(
        new Set(devicePixelSteps).size,
        `fractional DPR ${dpr} must exercise alternating device-pixel displacement`,
      ).toBeGreaterThan(1);
    }
  }
});
