// Shared body of agent-profiles.spec.ts and its compact twin
// (docs/specs/agent-thread-tools.md, "Agent profiles" and "Sub-thread
// depth"). A profile added on the Agent profiles page, with the sub-thread
// levels set to 1, is what the lead's thread_options lists; a spawn by that
// profile lands on its provider, model and effort with its instructions
// and the call's appended to the worker's system prompt; the spawn row shows
// them; and the worker, at the limit, lists no thread_spawn.
import { expect, type Page } from '@playwright/test';

import type { SeedResult } from './fixtures.js';
import type { HarnessApp } from '../src/harness.js';
import { sessionConfigs } from './workflows-helpers.js';
import {
  THREAD_TOOLS_SERVER,
  awaitToolAnswer,
  setScenario,
  threadRows,
  threadToolsScenario,
} from './thread-tools-helpers.js';

export type Layout = 'desktop' | 'compact';

interface RosterSettings {
  agentProfiles: Array<Record<string, string>>;
  agentNotes: Array<Record<string, string>>;
  subThreadLevels: number;
}

interface ProfileOption {
  name: string;
  use_for: string;
  provider: string;
  model?: string;
  effort?: string;
  instructions?: boolean;
}

const PROFILE = {
  name: 'Scout',
  useFor: 'A quick look at one file.',
  model: 'gpt-5.6-sol',
  instructions: 'You only look; you never edit.',
};
const CALL_INSTRUCTIONS = 'Answer in one line.';

/** Adds the Scout profile and sets one sub-thread level through the page. */
async function editRoster(page: Page, layout: Layout): Promise<void> {
  if ((await page.locator('html').getAttribute('data-compact-screen')) === 'thread') {
    await page.getByTestId('compact-back').click();
  }
  await page.getByTestId('sidebar-settings-button').click();
  await page.getByRole('tablist', { name: 'Settings Sections' }).getByRole('tab', { name: 'Agent profiles', exact: true }).click();
  const press = async (target: ReturnType<Page['locator']>) => {
    if (layout === 'desktop') await target.click();
    else await target.tap();
  };

  await expect(page.getByTestId('agent-profile').first()).toBeVisible();
  await press(page.getByRole('button', { name: 'Add profile' }));
  const card = page.getByTestId('agent-profile').last();
  await card.getByLabel('Name').fill(PROFILE.name);
  await card.getByLabel('Name').blur();
  // Saved only once the entry passes the backend's checks: a profile needs
  // a use-for line, so the page says so and holds it back until then.
  await expect(card.getByTestId('agent-profile-error')).toHaveText('Say what the profile is for: agents choose a profile by it.');
  await card.getByLabel('Use for').fill(PROFILE.useFor);
  await card.getByLabel('Use for').blur();
  await expect(card.getByTestId('agent-profile-error')).toHaveCount(0);
  await card.getByLabel('Provider').selectOption('codex');
  await card.getByLabel('Model').fill(PROFILE.model);
  await card.getByLabel('Model').blur();
  await card.getByLabel('Effort').selectOption('low');
  await card.getByLabel('Instructions, appended to the sub-thread\'s system prompt (optional)').fill(PROFILE.instructions);
  await card.getByLabel('Instructions, appended to the sub-thread\'s system prompt (optional)').blur();
  await page.getByTestId('agent-profiles-levels').selectOption('1');
}

export async function runAgentProfiles(harness: HarnessApp, page: Page, layout: Layout): Promise<void> {
  const tag = layout === 'desktop' ? 'profiles' : 'compact-profiles';
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      { name: `${tag}-lead`, repo: {}, threads: [{ title: 'Profiles lead', provider: 'claude' }] },
      { name: `${tag}-worker`, repo: {}, threads: [] },
    ],
  });
  const [lead, worker] = seed.projects;
  const leadId = lead.threadIds[0];
  const before = await harness.rpc<RosterSettings>('GetSettings');

  try {
    await harness.open(page);
    await editRoster(page, layout);
    await expect
      .poll(async () => {
        const saved = await harness.rpc<RosterSettings>('GetSettings');
        return [saved.subThreadLevels, saved.agentProfiles.find((profile) => profile.name === PROFILE.name)];
      })
      .toEqual([1, { ...PROFILE, provider: 'codex', effort: 'low', runtimeMode: '' }]);

    await setScenario(harness, lead.path, threadToolsScenario({
      name: `${tag}-lead`, provider: 'claude',
      turns: [{
        steps: [
          { call: { tool: 'thread_options', args: {} } },
          {
            call: {
              tool: 'thread_spawn',
              args: { prompt: 'Look at README.md.', title: 'Scout look', project_id: worker.projectId, profile: 'scout', instructions: CALL_INSTRUCTIONS },
            },
          },
        ],
        text: 'Sent the scout.',
      }],
      afterTurns: 'silent',
    }));
    await setScenario(harness, worker.path, threadToolsScenario({
      name: `${tag}-worker`, provider: 'codex', echoUserMessage: true,
      turns: [{ steps: [{ list: {} }], text: 'Looked.' }],
      afterTurns: 'silent',
    }));

    await harness.rpc('GetProviderStatuses');
    await harness.rpc('StartSession', leadId);
    await harness.rpc('SendMessage', leadId, 'send a scout', null);

    // The lead's thread_options lists the profile as this computer runs it.
    const options = await awaitToolAnswer<{ profiles?: ProfileOption[]; profile_notes?: string[] }>(harness, { tool: 'thread_options', cwd: lead.path });
    expect(options.isError, options.text).toBe(false);
    expect(options.value?.profiles?.find((profile) => profile.name === PROFILE.name)).toEqual({
      name: PROFILE.name, use_for: PROFILE.useFor, provider: 'codex', model: PROFILE.model, effort: 'low', instructions: true,
    });
    // Both providers run here, so the council note names both.
    expect(options.value?.profile_notes?.some((note) => note.includes('both the Claude and the Codex profiles'))).toBe(true);

    // The spawn by profile lands on the profile's provider, model and effort.
    const spawn = await awaitToolAnswer<{ thread_id: string }>(harness, { tool: 'thread_spawn', cwd: lead.path });
    expect(spawn.isError, spawn.text).toBe(false);
    const workerId = spawn.value!.thread_id;
    await expect
      .poll(async () => {
        const row = (await threadRows(harness)).find((r) => r.id === workerId);
        return row && [row.provider, row.model, row.reasoningEffort];
      })
      .toEqual(['codex', PROFILE.model, 'low']);

    // The worker reads the profile's instructions, then the call's, last in
    // its developer instructions, and at the limit lists no thread_spawn.
    const listing = await harness.awaitMcpTools({ server: THREAD_TOOLS_SERVER, cwd: worker.path });
    expect(listing.isError).toBe(false);
    expect(listing.tools).toContain('thread_send');
    expect(listing.tools).not.toContain('thread_spawn');
    expect(listing.instructions).toContain('the deepest allowed, so you cannot open new threads');
    const [codexConfig] = await sessionConfigs(harness, 'codex', 1);
    expect(codexConfig.developerInstructions?.endsWith(`${PROFILE.instructions}\n\n${CALL_INSTRUCTIONS}`)).toBe(true);

    // The lead's spawn row shows the instructions the worker runs with.
    await harness.open(page);
    if ((await page.locator('html').getAttribute('data-compact-screen')) === 'thread') {
      await page.getByTestId('compact-back').click();
    }
    await page.getByTestId('thread-row-title').filter({ hasText: 'Profiles lead' }).first().click();
    const runs = page.getByTestId('activity-run');
    await expect(runs.first()).toBeAttached({ timeout: 30_000 });
    for (let index = 0; index < (await runs.count()); index++) {
      const run = runs.nth(index);
      if ((await run.getAttribute('data-collapsed')) === 'true') await run.getByTestId('activity-run-header').click();
    }
    const row = page.getByTestId('tool-call-card')
      .filter({ has: page.getByTestId('tool-call-card-label').filter({ hasText: /^spawn$/ }) })
      .first();
    await row.getByTestId('tool-call-card-toggle').click();
    const instructions = row.locator('[data-testid="tool-call-card-ao-fact"][data-fact="instructions"]');
    await expect(instructions).toHaveText(`${PROFILE.instructions}\n\n${CALL_INSTRUCTIONS}`, { useInnerText: true });
  } finally {
    await harness.rpc('UpdateSettings', {
      agentProfiles: before.agentProfiles,
      agentNotes: before.agentNotes,
      subThreadLevels: before.subThreadLevels,
    });
  }
}
