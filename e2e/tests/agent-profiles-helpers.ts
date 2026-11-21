// Shared body of agent-profiles.spec.ts and its compact twin
// (docs/specs/agent-thread-tools.md, "Agent profiles" and "Sub-thread
// depth"). A profile added on the Agent profiles page, with the sub-thread
// levels set to 1, is what the lead's thread_options lists; the page keeps
// each list's draft and an entry typed here when the roster changes
// elsewhere; a spawn by that profile lands on its provider, model and
// effort with its instructions and the call's appended to the worker's
// system prompt; the spawn row shows them from the spawn's reply, also
// after the worker is deleted and the page reloads; the worker, and
// the copy behind the lead's thread_ask, at the limit, list no
// thread_spawn; and Restore puts back the roster the computer starts with.
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
  model: 'gpt-sol',
  instructions: 'You only look; you never edit.',
};
/** The newest gpt-sol model the mock Codex lists (cmd/ao-mockprovider/codex.go). */
const PROFILE_RUNS = 'gpt-5.6-sol';
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
  // A family runs the newest model of it this computer lists.
  await expect(card.getByTestId('agent-profile-runs')).toHaveText(
    `Runs ${PROFILE_RUNS} here, the newest ${PROFILE.model} model this computer lists.`,
  );
  await card.getByLabel('Effort').selectOption('low');
  await card.getByLabel('Instructions, appended to the sub-thread\'s system prompt (optional)').fill(PROFILE.instructions);
  await card.getByLabel('Instructions, appended to the sub-thread\'s system prompt (optional)').blur();
  await page.getByTestId('agent-profiles-levels').selectOption('1');
}

/**
 * Restore puts back the roster the computer itself starts with, which the
 * page asks it for: the edited Scout is gone and the computer's seeds are
 * saved and shown.
 */
async function restoreRoster(harness: HarnessApp, page: Page, layout: Layout): Promise<void> {
  if ((await page.locator('html').getAttribute('data-compact-screen')) === 'thread') {
    await page.getByTestId('compact-back').click();
  }
  await page.getByTestId('sidebar-settings-button').click();
  await page.getByRole('tablist', { name: 'Settings Sections' }).getByRole('tab', { name: 'Agent profiles', exact: true }).click();
  const seeds = await harness.rpc<{ profiles: Array<Record<string, string>>; notes: Array<Record<string, string>> }>('AgentRosterSeeds');
  expect(seeds.profiles.length).toBeGreaterThan(0);
  expect(seeds.profiles.some((profile) => profile.name === PROFILE.name)).toBe(false);
  await expect(page.locator(`[data-testid="agent-profile"][data-profile-name="${PROFILE.name}"]`)).toHaveCount(1);
  const restore = page.getByTestId('agent-profiles-restore');
  if (layout === 'desktop') await restore.click();
  else await restore.tap();
  await expect(page.getByTestId('agent-profile')).toHaveCount(seeds.profiles.length);
  expect(await page.getByTestId('agent-profile').evaluateAll((rows) => rows.map((row) => (row as HTMLElement).dataset.profileName)))
    .toEqual(seeds.profiles.map((profile) => profile.name));
  await expect.poll(async () => {
    const saved = await harness.rpc<RosterSettings>('GetSettings');
    return [saved.agentProfiles, saved.agentNotes];
  }).toEqual([seeds.profiles, seeds.notes]);
  await expect(page.getByTestId('agent-profiles-restore-refusal')).toHaveCount(0);
}

/**
 * Each roster list keeps its own draft: a note saved elsewhere leaves a
 * profile's unsaved typing alone, and the profile changed elsewhere too is
 * marked, keeping the typing unsaved until the person saves theirs or takes
 * the other version, by pointer or keyboard. Leaves Scout saved as PROFILE.
 */
async function editWhileSavedElsewhere(harness: HarnessApp, page: Page, layout: Layout): Promise<void> {
  const press = async (target: ReturnType<Page['locator']>) => {
    if (layout === 'desktop') await target.click();
    else await target.tap();
  };
  const card = page.locator(`[data-testid="agent-profile"][data-profile-name="${PROFILE.name}"]`);
  const useFor = card.getByLabel('Use for');
  await useFor.fill('Half typed');

  const saved = await harness.rpc<RosterSettings>('GetSettings');
  const notes = [...saved.agentNotes, { text: 'Saved elsewhere.', when: '' }];
  await harness.rpc('UpdateSettings', { agentNotes: notes });
  await expect(page.getByTestId('agent-note')).toHaveCount(notes.length);
  await expect(useFor).toHaveValue('Half typed');

  const theirs = saved.agentProfiles.map((profile) =>
    profile.name === PROFILE.name ? { ...profile, useFor: 'Changed elsewhere.' } : profile,
  );
  await harness.rpc('UpdateSettings', { agentProfiles: theirs, agentNotes: saved.agentNotes });
  await expect(page.getByTestId('agent-note')).toHaveCount(saved.agentNotes.length);
  const conflict = card.getByTestId('agent-profile-conflict');
  await expect(conflict).toContainText('This was also changed elsewhere. Your version is kept here and not saved until you choose.');
  await expect(useFor).toHaveValue('Half typed');
  await press(conflict.getByRole('button', { name: 'Use the other version' }));
  await expect(conflict).toHaveCount(0);
  await expect(useFor).toHaveValue('Changed elsewhere.');
  await useFor.fill(PROFILE.useFor);
  await useFor.blur();
  await savedUseFor(harness, PROFILE.useFor);

  // By keyboard: leaving the marked entry's fields saves nothing, and its
  // two choices follow them in tab order. Were the field saved on the way,
  // the mark and its buttons would be gone.
  const instructions = card.getByLabel('Instructions, appended to the sub-thread\'s system prompt (optional)');
  const markedElsewhere = async (mine: string, theirs: string) => {
    await useFor.fill(mine);
    await savedElsewhere(harness, theirs);
    await expect(conflict).toBeVisible();
    await page.keyboard.press('Tab');
    await expect(instructions).toBeFocused();
    await page.keyboard.press('Tab');
    await expect(conflict.getByRole('button', { name: 'Save mine' })).toBeFocused();
  };
  await markedElsewhere('Mine, by keyboard.', 'Changed again elsewhere.');
  await page.keyboard.press('Tab');
  await expect(conflict.getByRole('button', { name: 'Use the other version' })).toBeFocused();
  await page.keyboard.press('Enter');
  await expect(conflict).toHaveCount(0);
  await expect(useFor).toHaveValue('Changed again elsewhere.');
  await savedUseFor(harness, 'Changed again elsewhere.');

  await markedElsewhere('Mine, by keyboard.', 'Changed a third time.');
  await page.keyboard.press('Enter');
  await expect(conflict).toHaveCount(0);
  await savedUseFor(harness, 'Mine, by keyboard.');

  await useFor.fill(PROFILE.useFor);
  await useFor.blur();
}

/** Saves Scout's use-for line as another window would. */
async function savedElsewhere(harness: HarnessApp, useFor: string): Promise<void> {
  const saved = await harness.rpc<RosterSettings>('GetSettings');
  await harness.rpc('UpdateSettings', {
    agentProfiles: saved.agentProfiles.map((profile) => (profile.name === PROFILE.name ? { ...profile, useFor } : profile)),
  });
}

async function savedUseFor(harness: HarnessApp, useFor: string): Promise<void> {
  await expect
    .poll(async () => (await harness.rpc<RosterSettings>('GetSettings')).agentProfiles.find((profile) => profile.name === PROFILE.name)?.useFor)
    .toBe(useFor);
}

/** Opens the lead's thread and its spawn row, and returns the row's instructions fact. */
async function spawnRowInstructions(harness: HarnessApp, page: Page) {
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
  return row.locator('[data-testid="tool-call-card-ao-result-fact"][data-fact="instructions"]');
}

export async function runAgentProfiles(harness: HarnessApp, page: Page, layout: Layout): Promise<void> {
  const tag = layout === 'desktop' ? 'profiles' : 'compact-profiles';
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      { name: `${tag}-lead`, repo: {}, threads: [{ title: 'Profiles lead', provider: 'claude' }] },
      { name: `${tag}-worker`, repo: {}, threads: [] },
      {
        name: `${tag}-asked`,
        repo: {},
        threads: [{
          title: 'Asked',
          provider: 'claude',
          turns: [{ userText: 'look into the build', items: [{ kind: 'assistant_text', summary: 'The build is green.' }] }],
        }],
      },
    ],
  });
  const [lead, worker, asked] = seed.projects;
  const leadId = lead.threadIds[0];
  const before = await harness.rpc<RosterSettings>('GetSettings');

  try {
    await harness.open(page);
    const savedAsEdited = () => expect
      .poll(async () => {
        const saved = await harness.rpc<RosterSettings>('GetSettings');
        return [saved.subThreadLevels, saved.agentProfiles.find((profile) => profile.name === PROFILE.name)];
      })
      .toEqual([1, { ...PROFILE, provider: 'codex', effort: 'low', runtimeMode: '' }]);
    await editRoster(page, layout);
    await savedAsEdited();
    await editWhileSavedElsewhere(harness, page, layout);
    await savedAsEdited();

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
          // A session for the asked thread, so a copy of it can be made.
          { call: { tool: 'thread_send', args: { thread_id: asked.threadIds[0], message: 'Check the build.', wait_seconds: 30 } } },
        ],
        text: 'Sent the scout.',
      }, {
        steps: [
          { call: { tool: 'thread_ask', args: { thread_id: asked.threadIds[0], question: 'Is the build green?', wait_seconds: 30 } } },
        ],
        text: 'Asked.',
      }],
      afterTurns: 'silent',
    }));
    await setScenario(harness, worker.path, threadToolsScenario({
      name: `${tag}-worker`, provider: 'codex', echoUserMessage: true,
      turns: [{ steps: [{ list: {} }], text: 'Looked.' }],
      afterTurns: 'silent',
    }));
    // The asked thread and then the ask's hidden copy of it run here.
    await setScenario(harness, asked.path, threadToolsScenario({
      name: `${tag}-asked`, provider: 'claude',
      turns: [{ steps: [{ list: {} }], text: 'Green.' }],
      afterTurns: 'repeatLast',
    }));

    await harness.rpc('GetProviderStatuses');
    await harness.rpc('StartSession', leadId);
    await harness.rpc('SendMessage', leadId, 'send a scout', null);

    // The lead's thread_options lists the profile as this computer runs it.
    const options = await awaitToolAnswer<{ profiles?: ProfileOption[]; profile_notes?: string[] }>(harness, { tool: 'thread_options', cwd: lead.path });
    expect(options.isError, options.text).toBe(false);
    expect(options.value?.profiles?.find((profile) => profile.name === PROFILE.name)).toEqual({
      name: PROFILE.name, use_for: PROFILE.useFor, provider: 'codex', model: PROFILE_RUNS, effort: 'low', instructions: true,
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
      .toEqual(['codex', PROFILE_RUNS, 'low']);

    // The worker reads the profile's instructions, then the call's, last in
    // its developer instructions, and at the limit lists no thread_spawn.
    const listing = await harness.awaitMcpTools({ server: THREAD_TOOLS_SERVER, cwd: worker.path });
    expect(listing.isError).toBe(false);
    expect(listing.tools).toContain('thread_send');
    expect(listing.tools).not.toContain('thread_spawn');
    expect(listing.instructions).toContain('the deepest allowed, so you cannot open new threads');
    const [codexConfig] = await sessionConfigs(harness, 'codex', 1);
    expect(codexConfig.developerInstructions?.endsWith(`${PROFILE.instructions}\n\n${CALL_INSTRUCTIONS}`)).toBe(true);

    // The asked thread is the person's, level 0, and can spawn; the copy
    // behind the lead's ask of it is one level below the lead, so at the
    // limit: it cannot.
    const send = await awaitToolAnswer(harness, { tool: 'thread_send', cwd: lead.path });
    expect(send.isError, send.text).toBe(false);
    const own = await harness.awaitMcpTools({ server: THREAD_TOOLS_SERVER, cwd: asked.path });
    expect(own.tools).toContain('thread_spawn');
    await harness.waitForEvent('provider:turn_completed', (data: { threadId?: string }) => data.threadId === leadId);
    await harness.rpc('SendMessage', leadId, 'ask it', null);
    const ask = await awaitToolAnswer(harness, { tool: 'thread_ask', cwd: lead.path });
    expect(ask.isError, ask.text).toBe(false);
    const copy = await harness.awaitMcpTools({ server: THREAD_TOOLS_SERVER, cwd: asked.path });
    expect(copy.isError).toBe(false);
    expect(copy.tools).toContain('thread_show');
    expect(copy.tools).not.toContain('thread_spawn');
    expect(copy.instructions).toContain('the deepest allowed, so you cannot open new threads');

    // The lead's spawn row shows the instructions the worker runs with, from
    // the spawn's own reply: the same once the worker is deleted and the
    // page reloads.
    const whole = `${PROFILE.instructions}\n\n${CALL_INSTRUCTIONS}`;
    await expect(await spawnRowInstructions(harness, page)).toHaveText(whole, { useInnerText: true });
    await harness.rpc('DeleteThread', workerId);
    await expect.poll(async () => (await threadRows(harness)).some((r) => r.id === workerId)).toBe(false);
    await expect(await spawnRowInstructions(harness, page)).toHaveText(whole, { useInnerText: true });

    await restoreRoster(harness, page, layout);
  } finally {
    await harness.rpc('UpdateSettings', {
      agentProfiles: before.agentProfiles,
      agentNotes: before.agentNotes,
      subThreadLevels: before.subThreadLevels,
    });
  }
}
