// The Skills settings page (docs/specs/standing-agents.md, "Skills
// library") against the provider homes the harness owns.
//
// Coverage: the page lists the shipped tree with its upstream version and
// shows both homes not installed, and nothing installs on its own; Install
// is refused, with nothing written, when a skill directory the manifest did
// not write is in the way; Install copies the tree flattened, with the
// scripts' modes, into one home and leaves the other alone; a stale install
// offers Update, which makes it current; Remove deletes what the manifest
// names and keeps a file the user added; an unfinished install offers
// Update and Remove; a second client's page follows Install and Remove
// without a reload; screenshots before and after Install, of a refusal
// beside its row, and of the unfinished and update-available states.
import { mkdirSync, readFileSync, rmSync, writeFileSync, existsSync } from 'node:fs';
import { join } from 'node:path';

import { test, expect } from './fixtures.js';
import { capture } from './wake-rows-helpers.js';
import { MANIFEST, expectInstalled, openSkillsPage, scrubComputerName, skillsDir, status } from './skills-settings-helpers.js';

test('the Skills page installs, updates and removes the shipped tree per provider home', async ({ harness, page }) => {
  test.setTimeout(120_000);
  const claude = skillsDir(harness, 'claude');
  const codex = skillsDir(harness, 'codex');
  rmSync(claude, { recursive: true, force: true });
  rmSync(codex, { recursive: true, force: true });

  await harness.open(page);
  await openSkillsPage(page);
  const section = page.getByTestId('settings-skills');
  await expect(section).toContainText(/Version \d+\.\d+\.\d+, upstream [0-9a-f]{7}/);
  await expect(page.getByTestId('settings-skills-list')).toContainText('poteto-mode');
  // The install belongs to the computer whose provider homes it writes, not
  // to this browser.
  await expect(page.getByText('Saved on this device.')).toHaveCount(0);
  await expect(status(page, 'claude')).toHaveText('Not installed');
  await expect(status(page, 'codex')).toHaveText('Not installed');
  expect(existsSync(claude)).toBe(false);
  expect(existsSync(codex)).toBe(false);
  await scrubComputerName(page);
  await capture(page, 'skills-before-install.png', page.getByTestId('settings-skills-homes'));

  // ---- A skill of the user's own with a shipped name refuses the whole
  // install and writes nothing.
  mkdirSync(join(codex, 'tdd'), { recursive: true });
  writeFileSync(join(codex, 'tdd', 'SKILL.md'), 'my own tdd skill');
  await page.getByTestId('settings-skills-codex-install').click();
  await expect(page.getByTestId('settings-skills-codex-error')).toContainText('Nothing was installed');
  await scrubComputerName(page);
  await page.getByTestId('settings-skills-codex-error').evaluate((el) => {
    el.textContent = (el.textContent ?? '').replace(/\/\S*\/skills\//g, '<home>/skills/');
  });
  await capture(page, 'skills-install-refused.png', page.getByTestId('settings-skills-homes'));
  await expect(status(page, 'codex')).toHaveText('Not installed');
  expect(existsSync(join(codex, MANIFEST))).toBe(false);
  expect(existsSync(join(codex, 'poteto-mode'))).toBe(false);
  expect(readFileSync(join(codex, 'tdd', 'SKILL.md'), 'utf8')).toBe('my own tdd skill');
  rmSync(join(codex, 'tdd'), { recursive: true });

  // ---- Install into each home.
  await page.getByTestId('settings-skills-claude-install').click();
  await expect(status(page, 'claude')).toHaveText('Installed');
  expectInstalled(claude);
  expect(existsSync(join(codex, 'poteto-mode'))).toBe(false);
  await page.getByTestId('settings-skills-codex-install').click();
  await expect(status(page, 'codex')).toHaveText('Installed');
  await expect(page.getByTestId('settings-skills-codex-error')).toHaveCount(0);
  expectInstalled(codex);
  await scrubComputerName(page);
  await capture(page, 'skills-after-install.png', page.getByTestId('settings-skills-homes'));

  // ---- An install from an older tree offers Update, which replaces it.
  const manifestPath = join(claude, MANIFEST);
  const manifest = JSON.parse(readFileSync(manifestPath, 'utf8')) as { digest: string; version: string };
  writeFileSync(manifestPath, JSON.stringify({ ...manifest, digest: 'older', version: '0.0.1' }));
  writeFileSync(join(claude, 'poteto-mode', 'SKILL.md'), 'stale text');
  await page.reload();
  await openSkillsPage(page);
  await expect(status(page, 'claude')).toHaveText('0.0.1 installed, update available');
  await scrubComputerName(page);
  await capture(page, 'skills-update-available.png', page.getByTestId('settings-skills-homes'));
  await page.getByTestId('settings-skills-claude-update').click();
  await expect(status(page, 'claude')).toHaveText('Installed');
  expect(readFileSync(join(claude, 'poteto-mode', 'SKILL.md'), 'utf8')).not.toBe('stale text');

  // ---- An install that did not finish offers Update and Remove.
  const finished = JSON.parse(readFileSync(manifestPath, 'utf8')) as Record<string, unknown>;
  writeFileSync(manifestPath, JSON.stringify({ ...finished, complete: false }));
  await page.reload();
  await openSkillsPage(page);
  await expect(status(page, 'claude')).toHaveText('Install did not finish');
  await expect(page.getByTestId('settings-skills-claude-update')).toBeVisible();
  await expect(page.getByTestId('settings-skills-claude-remove')).toBeVisible();
  await expect(page.getByTestId('settings-skills-claude-install')).toHaveCount(0);
  await scrubComputerName(page);
  await capture(page, 'skills-install-unfinished.png', page.getByTestId('settings-skills-homes'));
  await page.getByTestId('settings-skills-claude-update').click();
  await expect(status(page, 'claude')).toHaveText('Installed');

  // ---- Remove deletes what the manifest names and nothing else.
  writeFileSync(join(claude, 'poteto-mode', 'my-notes.md'), 'mine');
  mkdirSync(join(claude, 'my-skill'));
  writeFileSync(join(claude, 'my-skill', 'SKILL.md'), 'mine');
  await page.getByTestId('settings-skills-claude-remove').click();
  await expect(status(page, 'claude')).toHaveText('Not installed');
  expect(existsSync(join(claude, MANIFEST))).toBe(false);
  expect(existsSync(join(claude, 'poteto-mode', 'SKILL.md'))).toBe(false);
  expect(existsSync(join(claude, 'tdd'))).toBe(false);
  expect(readFileSync(join(claude, 'poteto-mode', 'my-notes.md'), 'utf8')).toBe('mine');
  expect(readFileSync(join(claude, 'my-skill', 'SKILL.md'), 'utf8')).toBe('mine');
  await expect(status(page, 'codex')).toHaveText('Installed');
});

test('a second client follows Install and Remove without a reload', async ({ harness, page, browser }) => {
  rmSync(skillsDir(harness, 'claude'), { recursive: true, force: true });
  const other = await browser.newContext();
  try {
    const second = await other.newPage();
    await harness.open(page);
    await harness.open(second);
    await openSkillsPage(page);
    await openSkillsPage(second);
    await expect(status(second, 'claude')).toHaveText('Not installed');

    await page.getByTestId('settings-skills-claude-install').click();
    await expect(status(page, 'claude')).toHaveText('Installed');
    await expect(status(second, 'claude')).toHaveText('Installed');

    await second.getByTestId('settings-skills-claude-remove').click();
    await expect(status(second, 'claude')).toHaveText('Not installed');
    await expect(status(page, 'claude')).toHaveText('Not installed');
  } finally {
    await other.close();
  }
});
