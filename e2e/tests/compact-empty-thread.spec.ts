// Empty-thread greetings stay centered above the docked composer and wrap
// long project names across phone and desktop viewport sizes.
import { test, expect } from './fixtures.js';

for (const projectName of ['agent-overflow', 'project' + 'WithAVeryLongUnbrokenName'.repeat(4)]) {
  test(`empty greeting fits and centers for ${projectName}`, async ({ harness, page }) => {
    await harness.rpc('HarnessSeed', {
      projects: [{ name: projectName, repo: {} }],
    });
    await harness.open(page);
    await page.getByTestId('project-item-new-thread').click();
    const greeting = page.getByTestId('empty-thread-greeting');
    const heading = greeting.getByRole('heading');
    await expect(heading).toHaveText(`What should we build in ${projectName}?`);

    for (const width of [412, 320, 1100, 412]) {
      await page.setViewportSize({ width, height: 850 });
      await expect(heading).toBeVisible();
      await expect(heading).toHaveCSS('text-align', 'center');
      await expect.poll(async () => greeting.evaluate((el) => {
        const scroll = el.closest('[data-testid="message-timeline-scroll"]')!;
        const heading = el.querySelector('h2')!;
        const bounds = heading.getBoundingClientRect();
        const viewport = scroll.getBoundingClientRect();
        const paddingBottom = Number.parseFloat(getComputedStyle(scroll).paddingBottom);
        return Math.max(
          Math.abs(bounds.x + bounds.width / 2 - (viewport.x + viewport.width / 2)),
          Math.abs(bounds.y + bounds.height / 2 - (viewport.y + (viewport.height - paddingBottom) / 2)),
        );
      })).toBeLessThanOrEqual(2);
      await expect.poll(() => heading.evaluate((el) => el.scrollWidth - el.clientWidth)).toBeLessThanOrEqual(1);
      const headingBox = (await heading.boundingBox())!;
      const scrollBox = (await page.getByTestId('message-timeline-scroll').boundingBox())!;
      const composerBox = (await page.getByTestId('composer-overlay').boundingBox())!;
      expect(headingBox.x - scrollBox.x).toBeGreaterThanOrEqual(23);
      expect(scrollBox.x + scrollBox.width - headingBox.x - headingBox.width).toBeGreaterThanOrEqual(23);
      expect(headingBox.y + headingBox.height).toBeLessThan(composerBox.y);
      expect(Math.abs(composerBox.y + composerBox.height - scrollBox.y - scrollBox.height)).toBeLessThanOrEqual(1);
    }
  });
}
