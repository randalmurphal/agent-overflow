// thread_new_assistant (docs/specs/agent-thread-tools.md,
// "thread_new_assistant"), end to end on both mock providers.
//
// Coverage: an assistant creates another one, which has its own record
// naming its creator, its own home, the creator's projects and a card of
// its own in the Assistants section, not nested under the creator; its
// opening block, delivered in its home, names the creator and quotes
// the role text; its answer reaches the creator; a name in use is refused
// naming the holder; a thread that is not an assistant is refused.
import { join } from 'node:path';

import { test, expect, type SeedResult } from './fixtures.js';
import { captureAgentMessage } from './wake-rows-helpers.js';
import { cardRow, cardShell } from './sidebar-sections-helpers.js';
import { assistantHome, awaitMockInput, awaitToolAnswer, awaitTurnCompleted, setScenario, threadRows, threadToolsScenario, type ProviderName } from './thread-tools-helpers.js';

const ROLE = 'Watch the nightly build and tell me when it breaks.';

/** The next input a mock takes whose text includes `contains`, in cwd when one is named. */
for (const provider of ['claude', 'codex'] as const satisfies readonly ProviderName[]) {
  test(`${provider}: an assistant creates an assistant that reports to it, with a row of its own`, async ({ harness, page }) => {
    test.setTimeout(180_000);
    const seed = await harness.rpc<SeedResult>('HarnessSeed', {
      projects: [
        { name: `assistant-creates-${provider}`, repo: {}, threads: [{ title: 'Lead', provider, assistant: true }] },
        { name: `assistant-creates-plain-${provider}`, repo: {}, threads: [{ title: 'Plain', provider }] },
      ],
    });
    const [leadProject, plainProject] = seed.projects;
    const lead = leadProject.threadIds[0];
    const plain = plainProject.threadIds[0];
    const leadAgent = await harness.rpc<{ id: string }>('GetStandingAgent', lead);
    // The lead runs in its home; the new assistant in one not made yet.
    const cwd = await assistantHome(harness, lead);

    // The lead's own session takes the creating scenario; the new
    // assistant, in any other workspace, takes a reply.
    await setScenario(harness, cwd, threadToolsScenario({
      name: `assistant-creates-lead-${provider}`, provider, echoUserMessage: true,
      turns: [
        { steps: [
          { call: { tool: 'thread_new_assistant', args: { name: 'Scout', role: ROLE } } },
          { call: { tool: 'thread_new_assistant', args: { name: 'scout', role: ROLE } } },
        ], text: 'Created Scout.' },
        { text: 'Noted.' },
      ],
      afterTurns: 'repeatLast',
    }));
    await setScenario(harness, '', threadToolsScenario({
      name: `assistant-creates-scout-${provider}`, provider, echoUserMessage: true,
      turns: [{ text: 'I am Scout. I propose: watch the nightly build.' }],
      afterTurns: 'repeatLast',
    }));

    const opening = awaitMockInput(harness, { includes: 'You are "Scout", a new standing agent in Agent Overflow, created by the standing agent "Lead"' });
    const reported = awaitMockInput(harness, { cwd, includes: 'I am Scout. I propose: watch the nightly build.' });
    await harness.rpc('SendMessage', lead, 'Make an assistant for the nightly build.', null);
    const created = await awaitToolAnswer<{ token: string; thread_id: string; assistant_id: string; name: string; home: string }>(
      harness, { tool: 'thread_new_assistant', cwd, timeoutMs: 60_000 });
    expect(created.isError, created.text).toBe(false);
    const scout = created.value!;
    expect(scout.name).toBe('Scout');
    expect(scout.home).toBe(join(harness.bootstrap.dataDir, 'agents', scout.assistant_id));
    const taken = await awaitToolAnswer(harness, { tool: 'thread_new_assistant', cwd, timeoutMs: 60_000 });
    expect(taken.isError).toBe(true);
    expect(taken.text).toContain(`The assistant "Scout" (thread ${scout.thread_id}) already has that name`);

    // Its record names its creator; its opening names the creator and quotes the role.
    const record = await harness.rpc<{ id: string; threadId: string; creatorAgentId: string }>('GetStandingAgent', scout.thread_id);
    expect(record.id).toBe(scout.assistant_id);
    expect(record.creatorAgentId).toBe(leadAgent.id);
    const openingEvent = await opening;
    expect(openingEvent.cwd).toBe(scout.home);
    const input = openingEvent.report.input ?? '';
    expect(input).toContain(`(thread ${lead})`);
    expect(input).toContain(`> ${ROLE}`);
    expect(input).toContain(`Your home is ${scout.home}, empty but for a README.`);
    // Its answer reaches the lead.
    await reported;
    await awaitTurnCompleted(harness, lead);

    // A card of its own, not nested under the lead, serving the lead's project.
    await harness.open(page);
    const scoutRow = cardRow(page, scout.thread_id);
    await expect(scoutRow).toHaveAttribute('data-sidebar-agent', '');
    // The projects line sits under the row, in its shell.
    await expect(cardShell(page, scout.thread_id).getByTestId('thread-row-agent-projects')).toHaveText(`assistant-creates-${provider}`);
    await expect(page.locator(`[data-agent-member] [data-sidebar-thread-id="${scout.thread_id}"]`)).toHaveCount(0);
    await captureAgentMessage(page, `${provider}-assistant-created-by-assistant.png`, cardRow(page, lead), scoutRow);
    await scoutRow.click();
    const openingRow = page.locator('[data-testid="agent-message-row"]').filter({ hasText: 'created by the standing agent "Lead"' });
    await expect(openingRow.getByTestId('agent-message-sender')).toHaveText('Lead');
    await captureAgentMessage(page, `${provider}-assistant-created-opening.png`, openingRow);

    // ---- A thread that is not an assistant is refused.
    await setScenario(harness, plainProject.path, threadToolsScenario({
      name: `assistant-creates-plain-${provider}`, provider,
      turns: [{ steps: [{ call: { tool: 'thread_new_assistant', args: { name: 'Nobody', role: ROLE } } }], text: 'Refused.' }],
    }));
    await harness.rpc('StartSession', plain);
    await harness.rpc('SendMessage', plain, 'Make an assistant.', null);
    const refused = await awaitToolAnswer(harness, { tool: 'thread_new_assistant', cwd: plainProject.path, timeoutMs: 60_000 });
    expect(refused.isError).toBe(true);
    expect(refused.text).toContain("Only an assistant's current thread may create an assistant");
    expect((await threadRows(harness)).some((row) => row.title === 'Nobody')).toBe(false);
  });
}
