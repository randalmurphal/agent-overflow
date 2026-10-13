// The Skills settings page under the compact layout (docs/specs/
// standing-agents.md, "Skills library"): the page is reachable from the
// stacked Settings rail, every control stays inside the viewport, and
// Install works from a touch screen. Screenshots before and after Install.
import { existsSync, rmSync } from 'node:fs';

import { test, expect } from './fixtures.js';
import { capture } from './wake-rows-helpers.js';
import { expectInstalled, openSkillsPage, scrubComputerName, skillsDir, status } from './skills-settings-helpers.js';

test('compact: the Skills page fits the phone and installs from a tap', async ({ harness, page }) => {
  test.setTimeout(120_000);
  const claude = skillsDir(harness, 'claude');
  rmSync(claude, { recursive: true, force: true });
  rmSync(skillsDir(harness, 'codex'), { recursive: true, force: true });

  await harness.open(page);
  await openSkillsPage(page);
  const section = page.getByTestId('settings-skills');
  await expect(status(page, 'claude')).toHaveText('Not installed');
  const install = page.getByTestId('settings-skills-claude-install');
  await expect(install).toBeInViewport({ ratio: 1 });
  const clipped = await page.evaluate(() => {
    const vw = innerWidth;
    const out: string[] = [];
    for (const el of document.querySelectorAll<HTMLElement>('[data-testid="settings-skills"] button')) {
      const b = el.getBoundingClientRect();
      if (b.left < -1 || b.right > vw + 1) out.push(`${el.dataset.testid} l=${Math.round(b.left)} r=${Math.round(b.right)}`);
    }
    return out;
  });
  expect(clipped).toEqual([]);
  await scrubComputerName(page);
  await capture(page, 'compact-skills-before-install.png', install);

  await install.tap();
  await expect(status(page, 'claude')).toHaveText('Installed');
  expectInstalled(claude);
  expect(existsSync(skillsDir(harness, 'codex'))).toBe(false);
  await expect(page.getByTestId('settings-skills-claude-remove')).toBeInViewport({ ratio: 1 });
  await scrubComputerName(page);
  await capture(page, 'compact-skills-after-install.png', section.getByTestId('settings-skills-claude'));
});
