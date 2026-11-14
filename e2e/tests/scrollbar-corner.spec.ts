// Classic scrollbars, which the rest of the suite runs without (Playwright
// hides them by default), in the shipped stylesheet. Covered: a box that
// scrolls both ways shows its own background where its two bars meet, not
// the opaque square the browser paints for an unstyled corner.
import { expect, test } from './fixtures.js';

test.use({ launchOptions: { ignoreDefaultArgs: ['--hide-scrollbars'] } });

const BACKGROUND = [10, 20, 30];

test('the corner where two scrollbars meet is not painted', async ({ harness, page }) => {
  await harness.open(page);
  await expect(page.getByTestId('sidebar')).toBeVisible();

  const corner = await page.evaluate((rgb) => {
    const box = document.createElement('div');
    box.style.cssText = `position:fixed;left:40px;top:40px;width:200px;height:120px;z-index:2147483647;overflow:auto;background:rgb(${rgb.join(',')})`;
    const content = document.createElement('div');
    content.style.cssText = 'width:600px;height:400px';
    box.append(content);
    document.body.append(box);
    const bounds = box.getBoundingClientRect();
    return {
      barWidth: box.offsetWidth - box.clientWidth,
      barHeight: box.offsetHeight - box.clientHeight,
      right: bounds.right,
      bottom: bounds.bottom,
    };
  }, BACKGROUND);
  // Both bars take layout space, so this run has classic scrollbars and
  // the corner exists.
  expect(corner.barWidth).toBeGreaterThan(0);
  expect(corner.barHeight).toBeGreaterThan(0);

  const shot = await page.screenshot({
    clip: { x: corner.right - corner.barWidth, y: corner.bottom - corner.barHeight, width: corner.barWidth, height: corner.barHeight },
  });
  const pixel = await page.evaluate(async (base64) => {
    const bytes = Uint8Array.from(atob(base64), (char) => char.charCodeAt(0));
    const bitmap = await createImageBitmap(new Blob([bytes], { type: 'image/png' }));
    const canvas = new OffscreenCanvas(bitmap.width, bitmap.height);
    const context = canvas.getContext('2d')!;
    context.drawImage(bitmap, 0, 0);
    const { data } = context.getImageData(Math.floor(bitmap.width / 2), Math.floor(bitmap.height / 2), 1, 1);
    return [data[0], data[1], data[2]];
  }, shot.toString('base64'));
  expect(pixel).toEqual(BACKGROUND);
});
