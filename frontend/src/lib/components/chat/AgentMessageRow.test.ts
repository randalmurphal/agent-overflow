import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render } from '@testing-library/svelte';
import { tick } from 'svelte';
import fixture from '../../../../../internal/threadtools/testdata/agent_messages.json';
import servedRows from '../../../../../internal/agentmeta/testdata/served_rows.json';
import labels from '../../../../../internal/app/testdata/agent_message_labels.json';
import { buildPane, makeItem, makeThread } from '../../../test/helpers/chat';
import { resetBindingMocks } from '../../../test/mocks/bindings-app';
import { stageBackend, resetStagedBackends } from '../../../test/helpers/backends';
import { HOME_BACKEND } from '../../transport/backendKey';
import type { Item } from '../../types/models';
import TimelineLeaf from './TimelineLeaf.svelte';
import MessageTimeline from './MessageTimeline.svelte';

const openThreadInPane = vi.hoisted(() => vi.fn(async () => {}));
vi.mock('../../stores/panes.svelte', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../../stores/panes.svelte')>()),
  openThreadInPane,
  findPaneShowingThread: () => null,
}));
vi.mock('../../stores/threads.svelte', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../../stores/threads.svelte')>()),
  getThreadById: (id: string) => (id === 't-plan' || id === 't-work' ? makeThread({ id }) : undefined),
}));

type Origin = Record<string, unknown>;
const fixtureCase = (name: string) =>
  (fixture as { name: string; text: string; members: number[]; origins: { facts: unknown }[] }[]).find((c) => c.name === name)!;
const text = (name: string) => fixtureCase(name).text;
/**
 * `attribution` with the facts a client receives for a fixture case's member
 * `index` (internal/agentmeta/testdata/served_rows.json).
 */
const withFacts = (name: string, attribution: Origin, index = 0): Origin => {
  const served = JSON.parse(servedRows.find((row) => row.name === name)!.meta) as {
    originThread: Origin; joinedParts?: { originThread: Origin }[];
  };
  const member = served.joinedParts ? served.joinedParts[index].originThread : served.originThread;
  return { ...attribution, facts: member.facts };
};
/** The joinedParts a join records for a fixture case's members, each attributed `origins[i]` with its facts. */
const joinedParts = (name: string, origins: Origin[]) =>
  fixtureCase(name).members.map((bytes, index) => ({ bytes, origin: 'agent-thread', originThread: withFacts(name, origins[index], index) }));

/** What Go serves a loop's report kind as (agentmeta.ReportDescriptor). */
const loopReport = JSON.parse(labels.find((tc) => tc.name === 'a loop')!.meta).originWake.report;

function agentRow(summary: string, meta: Record<string, unknown>, overrides: Partial<Item> = {}): Item {
  return makeItem({ id: 'msg-1', kind: 'user_text', role: 'user', summary, meta: JSON.stringify(meta), ...overrides });
}

describe('<AgentMessageRow> through TimelineLeaf', () => {
  beforeEach(() => {
    resetBindingMocks();
    openThreadInPane.mockClear();
    stageBackend({ id: HOME_BACKEND, name: 'This computer', backendId: 'home-uuid' });
  });
  afterEach(() => resetStagedBackends());

  it('renders a request as a folded row, not the user bubble, and expands into facts', async () => {
    const pane = await buildPane();
    const item = agentRow(text('request from a standing agent that spawned the receiver'), {
      origin: 'agent-thread',
      originThread: withFacts('request from a standing agent that spawned the receiver', { threadId: 't-plan', title: 'Planner', token: 'tok', kind: 'spawn' }),
      sendId: 'thread-request:tok',
    });
    const view = render(TimelineLeaf, { props: { pane, item } });

    expect(view.queryByTestId('user-message-bubble')).toBeNull();
    const row = view.getByTestId('agent-message-row');
    expect(row.dataset.agentMessageKind).toBe('spawn');
    expect(view.getByTestId('agent-message-kind').textContent).toBe('Spawn');
    expect(view.getByTestId('agent-message-sender').textContent).toBe('Planner');
    expect(view.getByTestId('agent-message-role').textContent).toBe('Keeps the "plan" honest.');
    expect(view.getByTestId('agent-message-first-line').textContent).toBe('Do the task.');
    // The routing never renders raw, folded or not.
    expect(row.textContent).not.toContain('Agent request from thread');
    expect(view.queryByTestId('agent-message-body')).toBeNull();

    const toggle = view.getByTestId('agent-message-toggle');
    expect(toggle.getAttribute('aria-expanded')).toBe('false');
    expect(toggle.getAttribute('aria-label')).toBe('Spawn prompt from Planner');
    await fireEvent.click(toggle);
    await tick();
    expect(toggle.getAttribute('aria-expanded')).toBe('true');
    expect(pane.isUserMessageExpanded('msg-1')).toBe(true);
    const body = view.getByTestId('agent-message-body');
    expect(toggle.getAttribute('aria-controls')).toBe(body.id);
    expect(view.getByTestId('agent-message-text').textContent).toBe('Do the task.');
    const fact = (label: string) => body.querySelector(`[data-fact="${label}"]`)?.textContent;
    expect(fact('Waiting')).toBe('Yes, for an answer');
    expect(fact('Reply')).toBe('This thread can read the sender and answer it');
    expect(fact('Prompts')).toBe('Go to the sender, which spawned this thread');
    // The token is the request's attribution's.
    expect(fact('Token')).toBe('tok');
    expect(body.textContent).not.toContain('call thread_reply');
  });

  it('keeps its fold across a remount, as virtualization does', async () => {
    const pane = await buildPane();
    const item = agentRow(text('reply'), { origin: 'agent-thread', originThread: withFacts('reply', { threadId: 't-work', title: 'Worker', kind: 'wake' }) });
    const first = render(TimelineLeaf, { props: { pane, item } });
    await fireEvent.click(first.getByTestId('agent-message-toggle'));
    await tick();
    first.unmount();
    const again = render(TimelineLeaf, { props: { pane, item } });
    expect(again.getByTestId('agent-message-toggle').getAttribute('aria-expanded')).toBe('true');
    expect(again.getByTestId('agent-message-text').textContent).toBe('All green.\nSecond line.');
  });

  it('opens the sender in a pane from the header', async () => {
    const pane = await buildPane();
    const item = agentRow(text('request waiting, local, quoting the user'), {
      origin: 'agent-thread',
      originThread: withFacts('request waiting, local, quoting the user', { threadId: 't-plan', title: 'Planner', kind: 'send' }),
    });
    const view = render(TimelineLeaf, { props: { pane, item } });
    const sender = view.getByTestId('agent-message-sender');
    expect(sender.tagName).toBe('BUTTON');
    await fireEvent.click(sender);
    await vi.waitFor(() => expect(openThreadInPane).toHaveBeenCalledWith(expect.objectContaining({ id: 't-plan' })));
    // Opening the sender is not a toggle of the row.
    expect(pane.isUserMessageExpanded('msg-1')).toBe(false);
  });

  it('names no computer for a sender on the computer showing the row', async () => {
    const pane = await buildPane();
    const item = agentRow(text('request waiting, local, quoting the user'), {
      origin: 'agent-thread',
      originThread: withFacts('request waiting, local, quoting the user', { computerId: 'home-uuid', computerName: 'workstation-7', threadId: 't-plan', title: 'Planner', kind: 'send' }),
    });
    const view = render(TimelineLeaf, { props: { pane, item } });
    const sender = view.getByTestId('agent-message-sender');
    expect(sender.textContent).toBe('Planner');
    expect(sender.tagName).toBe('BUTTON');
  });

  it('names a remote sender with its computer and offers no dead link to an unpaired one', async () => {
    const pane = await buildPane();
    const item = agentRow(text('late reply with age, computer and a standing agent'), {
      origin: 'agent-thread',
      originThread: withFacts('late reply with age, computer and a standing agent', { computerId: 'gone', computerName: 'desk', threadId: 't-work', title: 'Worker', kind: 'wake' }),
    });
    const view = render(TimelineLeaf, { props: { pane, item } });
    const sender = view.getByTestId('agent-message-sender');
    expect(sender.textContent).toBe('Worker on desk');
    expect(sender.tagName).not.toBe('BUTTON');
    expect(view.getByTestId('agent-message-kind').textContent).toBe('Reply');
    await fireEvent.click(view.getByTestId('agent-message-toggle'));
    await tick();
    const body = view.getByTestId('agent-message-body');
    expect(body.querySelector('[data-fact="Answered"]')?.textContent).toBe('5m ago');
    expect(body.querySelector('[data-fact="Role"]')?.textContent).toBe('Runs the builds.');
    expect(body.textContent).not.toContain('Reply from');
    // The header already names the sender.
    expect(body.querySelector('[data-fact="From"]')).toBeNull();
  });

  it('names the sender of a joined part from another thread', async () => {
    const pane = await buildPane();
    pane.setUserMessageExpanded('msg-1', true);
    const item = agentRow(text('two wakes joined, the first answer holding a rule'), {
      origin: 'agent-thread',
      originThread: withFacts('two wakes joined, the first answer holding a rule', { threadId: 't-a', title: 'A', kind: 'wake' }),
      joinedSendIds: ['thread-wake:a', 'thread-wake:b'],
      joinedParts: joinedParts('two wakes joined, the first answer holding a rule', [
        { threadId: 't-a', title: 'A', kind: 'wake' },
        { threadId: 't-b', title: 'B', kind: 'wake' },
      ]),
    });
    const view = render(TimelineLeaf, { props: { pane, item } });
    const parts = view.getAllByTestId('agent-message-part');
    const heading = (part: HTMLElement) => part.querySelector('[data-testid="agent-message-part-source"]')?.textContent?.replace(/\s+/g, ' ').trim();
    expect(parts.map(heading)).toEqual(['Reply A', 'Final message, no reply B']);
  });

  it('heads each message of a joined row with its own sender and kind, opening only that one', async () => {
    const pane = await buildPane();
    pane.setUserMessageExpanded('msg-1', true);
    const origins = [
      { threadId: 't-plan', title: 'Planner', token: 'tok-a', kind: 'send' },
      { threadId: 't-work', title: 'Worker', token: 'tok-b', kind: 'spawn' },
      { computerId: 'gone', computerName: 'desk', threadId: 't-far', title: 'Far', token: 'tok-c', kind: 'ask' },
    ];
    const requests = fixtureCase('two requests joined');
    // A third member: the first request again, from a thread on a computer this client is not paired with.
    const first = new TextDecoder().decode(new TextEncoder().encode(requests.text).subarray(0, requests.members[0]));
    const attributed = origins.map((origin, index) => withFacts('two requests joined', origin, index % 2));
    const item = agentRow(`${requests.text}\n\n---\n\n${first}`, {
      origin: 'agent-thread',
      originThread: attributed[0],
      sendId: 'thread-request:tok-a',
      joinedSendIds: origins.map((origin) => `thread-request:${origin.token}`),
      joinedParts: [...requests.members, requests.members[0]].map((bytes, index) => ({
        sendId: `thread-request:${origins[index].token}`, bytes, origin: 'agent-thread', originThread: attributed[index],
      })),
    });
    const view = render(TimelineLeaf, { props: { pane, item } });
    // The header names the first message's sender.
    expect(view.getByTestId('agent-message-sender').textContent).toBe('Planner');
    const parts = view.getAllByTestId('agent-message-part');
    expect(parts.map((part) => part.dataset.agentMessageKind)).toEqual(['request', 'spawn', 'question']);
    const senders = view.getAllByTestId('agent-message-part-source-sender');
    expect(senders.map((sender) => [sender.textContent, sender.tagName])).toEqual([
      ['Planner', 'BUTTON'],
      ['Worker', 'BUTTON'],
      ['Far on desk', 'SPAN'],
    ]);
    // Each request's token is its attribution's.
    expect(parts.map((part) => part.querySelector('[data-fact="Token"]')?.textContent)).toEqual(['tok-a', 'tok-b', 'tok-c']);
    await fireEvent.click(senders[1]);
    await vi.waitFor(() => expect(openThreadInPane).toHaveBeenCalledTimes(1));
    expect(openThreadInPane).toHaveBeenCalledWith(expect.objectContaining({ id: 't-work' }));
    // Opening a sender is not a toggle of the row.
    expect(pane.isUserMessageExpanded('msg-1')).toBe(true);
  });

  it('shows each message of a joined row', async () => {
    const pane = await buildPane();
    pane.setUserMessageExpanded('msg-1', true);
    const item = agentRow(text('two requests joined'), {
      origin: 'agent-thread',
      originThread: withFacts('two requests joined', { threadId: 't-lead', title: 'Lead', kind: 'send' }),
      joinedSendIds: ['thread-request:a', 'thread-request:b'],
      joinedParts: joinedParts('two requests joined', [
        { threadId: 't-lead', title: 'Lead', kind: 'send' },
        { threadId: 't-lead', title: 'Lead', kind: 'send' },
      ]),
    });
    const view = render(TimelineLeaf, { props: { pane, item } });
    expect(view.getAllByTestId('agent-message-part')).toHaveLength(2);
    expect(view.getAllByTestId('agent-message-text').map((p) => p.textContent)).toEqual(['First ask.', 'Second ask.']);
  });

  it('names the session\'s end as what stopped a request, and shows what its turn wrote', async () => {
    const wakeRow = async (name: string) => {
      const pane = await buildPane();
      const item = agentRow(text(name), {
        origin: 'agent-thread',
        originThread: withFacts(name, { threadId: 't-work', title: 'Worker', token: 'tok', kind: 'wake' }),
        sendId: 'thread-wake:tok',
      });
      const view = render(TimelineLeaf, { props: { pane, item } });
      await fireEvent.click(view.getByTestId('agent-message-toggle'));
      return view;
    };
    for (const [name, stoppedBy] of [
      ['interrupted by its session ending before it read the request', 'the session ended before it read the request'],
      ['interrupted by its session ending before it answered the request', 'the session ended before it answered the request'],
      ['interrupted by its session ending during the turn, having written nothing', 'the session ended during the turn that read the request'],
    ]) {
      const view = await wakeRow(name);
      expect(view.getByTestId('agent-message-row').dataset.agentMessageKind, name).toBe('interrupted');
      expect(view.container.querySelector('[data-fact="Stopped by"]')?.textContent, name).toBe(stoppedBy);
      expect(view.queryByTestId('agent-message-text'), name).toBeNull();
      view.unmount();
    }
    const wrote = await wakeRow('interrupted by its session ending during the turn, with what it wrote');
    expect(wrote.container.querySelector('[data-fact="Stopped by"]')?.textContent).toBe('the session ended during the turn that read the request');
    expect(wrote.getByTestId('agent-message-text').textContent).toBe('Half done.\nSecond line.');
    expect(wrote.getByTestId('agent-message-row').textContent).not.toContain('What it had written by then');
  });

  it('shows the past thread a request was made in as facts, never as its answer', async () => {
    for (const [name, answer] of [
      ['reply made in a past thread', 'All green.'],
      ['stopped by the person before it said anything, made in a past thread', null],
      ['digest: the same reply, made in a past thread', 'Done.'],
    ] as const) {
      const pane = await buildPane();
      pane.setUserMessageExpanded('msg-1', true);
      const view = render(TimelineLeaf, {
        props: {
          pane,
          item: agentRow(text(name), {
            origin: 'agent-thread', originThread: withFacts(name, { threadId: 't-work', title: 'Worker', token: 'tok', kind: 'wake' }),
          }),
        },
      });
      const row = view.getByTestId('agent-message-row');
      expect(row.textContent, name).not.toContain('Made in thread');
      expect(row.querySelector('[data-fact="Made in"]')?.textContent, name).toBe('t-old, before New Thread, Same Assistant');
      expect(row.querySelector('[data-fact="Now in"]')?.textContent, name).toBe('t-new');
      if (answer === null) expect(view.queryByTestId('agent-message-text'), name).toBeNull();
      else expect(view.getByTestId('agent-message-text').textContent, name).toBe(answer);
      if (answer !== null) expect(view.getByTestId('agent-message-first-line').textContent, name).toBe(answer);
      view.unmount();
    }
  });

  it('shows one card for a message answering several requests, naming every token', async () => {
    const pane = await buildPane();
    const several = render(TimelineLeaf, {
      props: {
        pane,
        item: agentRow(text('one turn answering several requests'), {
          origin: 'agent-thread', originThread: withFacts('one turn answering several requests', { threadId: 't-work', title: 'Worker', token: 'tok', kind: 'wake' }),
        }),
      },
    });
    await fireEvent.click(several.getByTestId('agent-message-toggle'));
    await tick();
    expect(several.getAllByTestId('agent-message-part')).toHaveLength(1);
    expect(several.getByTestId('agent-message-sender').textContent).toBe('Worker');
    expect(several.container.querySelector('[data-fact="Tokens"]')?.textContent)
      .toBe('0b5c7f2e-1111-4a2b-9c3d-5e6f7a8b9c0d, tok-2, tok-3');
    expect(several.container.querySelector('[data-fact="Token"]')).toBeNull();
    several.unmount();

    const digestRow = agentRow(text('digest: the same reply, a standing agent and an age'), {
      origin: 'agent-thread',
      originThread: withFacts('digest: the same reply, a standing agent and an age', { threadId: 't-work', title: 'Worker', token: 'tok', kind: 'wake' }),
    }, { id: 'msg-2' });
    const digest = render(TimelineLeaf, { props: { pane, item: digestRow } });
    expect(digest.getByTestId('agent-message-sender').textContent).toBe('2 threads');
    expect(digest.getByTestId('agent-message-sender').tagName).not.toBe('BUTTON');
    await fireEvent.click(digest.getByTestId('agent-message-toggle'));
    await tick();
    expect(digest.getAllByTestId('agent-message-part')).toHaveLength(1);
    expect(digest.getByTestId('agent-message-text').textContent).toBe('Done.\nSecond line.');
    // Each thread it speaks for is named the way a joined part's sender is,
    // with its own facts; one on a computer this client is not paired with
    // does not open.
    const threads = digest.getAllByTestId('agent-message-thread');
    const facts = (thread: HTMLElement) =>
      [...thread.querySelectorAll('[data-fact]')].map((fact) => `${fact.getAttribute('data-fact')}: ${fact.textContent}`);
    expect(threads.map(facts)).toEqual([
      ['Role: Runs the builds.', 'Token: 0b5c7f2e-1111-4a2b-9c3d-5e6f7a8b9c0d'],
      ['Answered: 3h ago', 'Tokens: tok-2, tok-3'],
    ]);
    const senders = digest.getAllByTestId('agent-message-thread-source-sender');
    expect(senders.map((sender) => [sender.textContent, sender.tagName])).toEqual([
      ['Worker', 'BUTTON'],
      ['Lexer "two" on desk', 'SPAN'],
    ]);
    expect(digest.container.querySelector('[data-fact="Token"]')?.closest('[data-testid="agent-message-thread"]')).toBe(threads[0]);
    expect(digest.getByTestId('agent-message-row').textContent).not.toContain('threads sent the same reply');
    await fireEvent.click(senders[0]);
    await vi.waitFor(() => expect(openThreadInPane).toHaveBeenCalledWith(expect.objectContaining({ id: 't-work' })));
    digest.unmount();

    // A member on a paired computer that is up opens there, named as this
    // client names that computer.
    stageBackend({ id: 'desk-key', backendId: 'computer-desk', name: 'Desk' });
    const paired = render(TimelineLeaf, { props: { pane, item: digestRow } });
    const pairedSenders = paired.getAllByTestId('agent-message-thread-source-sender');
    expect(pairedSenders.map((sender) => [sender.textContent, sender.tagName])).toEqual([
      ['Worker', 'BUTTON'],
      ['Lexer "two" on Desk', 'BUTTON'],
    ]);
  });

  it('renders a loop wake decoded and a provider agent\'s message as agent rows', async () => {
    const pane = await buildPane();
    const loop = render(TimelineLeaf, {
      props: {
        pane,
        item: agentRow('Loop "nightly" fired. x\nnote: "\\u003cgo\\u003e"', {
          origin: 'agent-wake', sendId: 'agent-wake:1',
          originWake: {
            kind: 'loop', label: 'nightly', report: loopReport,
            facts: { form: 'report', bytes: 46, body: [{ start: 24, end: 46 }], headline: '<go>' },
          },
        }),
      },
    });
    expect(loop.getByTestId('agent-message-first-line').textContent).toBe('<go>');
    expect(loop.getByTestId('agent-message-sender').tagName).not.toBe('BUTTON');
    loop.unmount();
    const codex = render(TimelineLeaf, {
      props: {
        pane,
        item: agentRow('Message content is encrypted by Codex.', {
          wire_only: true, agent_message: { sender: '/root', recipient: '/root/worker', encrypted: true },
        }, { parentId: 'collab-1' }),
      },
    });
    expect(codex.getByTestId('agent-message-sender').textContent).toBe('Main agent');
    expect(codex.queryByRole('button', { name: /edit/i })).toBeNull();
  });

  it('renders a report of a kind only Go defines from what Go serves, with the default icon', async () => {
    const pane = await buildPane();
    const served = servedRows.find((row) => row.name === 'a report of a kind only Go defines') as { text: string; meta: string };
    const view = render(TimelineLeaf, { props: { pane, item: makeItem({ id: 'msg-1', kind: 'user_text', role: 'user', summary: served.text, meta: served.meta }) } });
    const row = view.getByTestId('agent-message-row');
    expect(row.dataset.agentMessageKind).toBe('test-only');
    expect(view.getByTestId('agent-message-kind').textContent).toBe('Probe');
    expect(view.getByTestId('agent-message-sender').textContent).toBe('probe');
    expect(view.getByTestId('agent-message-first-line').textContent).toBe('"saw <err>"');
    expect(view.getByTestId('agent-message-toggle-icon-slot').childElementCount).toBeGreaterThan(0);
    const toggle = view.getByTestId('agent-message-toggle');
    expect(toggle.getAttribute('aria-label')).toBe('Test-only report from probe');
    await fireEvent.click(toggle);
    await tick();
    expect(view.getByTestId('agent-message-text').textContent).toBe('"saw <err>"\n"second line"');
    expect(row.textContent).not.toContain('Probe reported.');
  });

  it('keeps the person\'s own message, and a join holding it, in the user bubble', async () => {
    const pane = await buildPane();
    const own = render(TimelineLeaf, { props: { pane, item: agentRow('hello', { sendId: 's1' }) } });
    expect(own.getByTestId('user-message-bubble')).toBeTruthy();
    expect(own.queryByTestId('agent-message-row')).toBeNull();
    own.unmount();
    const mixed = render(TimelineLeaf, {
      props: {
        pane,
        item: agentRow('wake\n\n---\n\nmine', {
          origin: 'agent-wake', originWake: { kind: 'loop', label: 'x', report: { ...loopReport, badge: 'Loop · x' } }, expandComposerCommands: true,
        }),
      },
    });
    expect(mixed.getByTestId('user-message-bubble')).toBeTruthy();
    expect(mixed.getByTestId('user-message-wake-origin').textContent).toContain('Loop · x');
  });
});

// An agent message is an activity-run member (utils/activityRunSpans.ts): it
// renders inside the run's clip, folds with it, and stays readable and
// operable there; alone, the collapsed run's header names it.
describe('<AgentMessageRow> inside an activity run', () => {
  beforeEach(() => {
    resetBindingMocks();
    openThreadInPane.mockClear();
    stageBackend({ id: HOME_BACKEND, name: 'This computer', backendId: 'home-uuid' });
  });
  afterEach(() => resetStagedBackends());

  /** The person asks, a tool runs, the worker's reply opens the next turn, the work goes on. */
  function workAroundReply(): Item[] {
    const reply = agentRow(text('reply'), { origin: 'agent-thread', originThread: withFacts('reply', { threadId: 't-work', title: 'Worker', kind: 'wake' }) });
    return [
      makeItem({ id: 'ask', itemIndex: 0, kind: 'user_text', role: 'user', summary: 'have a worker look' }),
      makeItem({ id: 'spawn', itemIndex: 1, kind: 'tool_call', toolName: 'Bash', summary: 'Bash: ls' }),
      { ...reply, turnIndex: 1, itemIndex: 0 },
      makeItem({ id: 'think', turnIndex: 1, itemIndex: 1, kind: 'thinking', summary: 'reading it' }),
      makeItem({ id: 'bash', turnIndex: 1, itemIndex: 2, kind: 'tool_call', toolName: 'Bash', summary: 'Bash: pwd' }),
      makeItem({ id: 'done', turnIndex: 1, itemIndex: 3, kind: 'assistant_text', summary: 'Nothing to fix.' }),
    ];
  }

  it('collapsed, the run header counts the reply and the run holds the work on both sides', async () => {
    const pane = await buildPane(makeThread(), workAroundReply());
    const view = render(MessageTimeline, { props: { pane } });

    await vi.waitFor(() => expect(view.getAllByTestId('activity-run')).toHaveLength(1));
    expect(view.getByTestId('activity-run').dataset.collapsed).toBe('true');
    expect(view.getByTestId('activity-run-header-counts').textContent).toBe('2 Bash, 1 agent message, 1 thinking');
    // Folded with the run: no standalone agent row anywhere in the timeline.
    expect(view.queryByTestId('agent-message-row')).toBeNull();
  });

  /** The person asks, the worker's reply opens the next turn, prose answers it. */
  function loneReply(): Item[] {
    const reply = agentRow(text('reply'), { origin: 'agent-thread', originThread: withFacts('reply', { threadId: 't-work', title: 'Worker', kind: 'wake' }) });
    return [
      makeItem({ id: 'ask', itemIndex: 0, kind: 'user_text', role: 'user', summary: 'have a worker look' }),
      { ...reply, turnIndex: 1, itemIndex: 0 },
      makeItem({ id: 'done', turnIndex: 1, itemIndex: 1, kind: 'assistant_text', summary: 'Nothing to fix.' }),
    ];
  }

  it('a lone reply is a run of one whose collapsed header names its kind, sender and first line', async () => {
    const pane = await buildPane(makeThread(), loneReply());
    const view = render(MessageTimeline, { props: { pane } });

    await vi.waitFor(() => expect(view.getAllByTestId('activity-run')).toHaveLength(1));
    const run = view.getByTestId('activity-run');
    expect(run.dataset.collapsed).toBe('true');
    expect(view.queryByTestId('agent-message-row')).toBeNull();
    const header = view.getByTestId('activity-run-header');
    expect(view.queryByTestId('activity-run-header-counts')).toBeNull();
    expect(view.getByTestId('activity-run-header-message-kind').textContent).toBe('Reply');
    expect(view.getByTestId('activity-run-header-message-sender').textContent).toBe('Worker');
    expect(view.getByTestId('activity-run-header-message-first-line').textContent).toBe('All green.');
    expect(header.getAttribute('aria-label')).toBe('Expand Reply from Worker');

    // Opened, the header counts it like any run and the row carries the message.
    await fireEvent.click(header);
    const row = await vi.waitFor(() => view.getByTestId('agent-message-row'));
    expect(view.getByTestId('activity-run-clip').contains(row)).toBe(true);
    expect(view.getByTestId('activity-run-header-counts').textContent).toBe('1 agent message');
    expect(view.queryByTestId('activity-run-header-message')).toBeNull();
    expect(header.getAttribute('aria-label')).toBe('Collapse 1 activity row');
  });

  it('a lone reply from another computer names that computer in its collapsed header, as its row does', async () => {
    const reply = agentRow(text('late reply with age, computer and a standing agent'), {
      origin: 'agent-thread',
      originThread: withFacts('late reply with age, computer and a standing agent', { computerId: 'gone', computerName: 'desk', threadId: 't-work', title: 'Worker', kind: 'wake' }),
    });
    const [ask, , done] = loneReply();
    const pane = await buildPane(makeThread(), [ask, { ...reply, turnIndex: 1, itemIndex: 0 }, done]);
    const view = render(MessageTimeline, { props: { pane } });

    await vi.waitFor(() => expect(view.getByTestId('activity-run').dataset.collapsed).toBe('true'));
    expect(view.getByTestId('activity-run-header-message-sender').textContent).toBe('Worker on desk');
    expect(view.getByTestId('activity-run-header').getAttribute('aria-label')).toBe('Expand Reply from Worker on desk');
    await fireEvent.click(view.getByTestId('activity-run-header'));
    const row = await vi.waitFor(() => view.getByTestId('agent-message-row'));
    expect(row.querySelector('[data-testid="agent-message-sender"]')?.textContent).toBe('Worker on desk');
  });

  it('a jump to a reply inside a collapsed run opens the run on it', async () => {
    const pane = await buildPane(makeThread(), loneReply());
    const view = render(MessageTimeline, { props: { pane } });
    await vi.waitFor(() => expect(view.getByTestId('activity-run').dataset.collapsed).toBe('true'));

    pane.requestScrollToItem('msg-1');

    await vi.waitFor(() => expect(view.getByTestId('activity-run').dataset.collapsed).toBe('false'));
    const row = await vi.waitFor(() => view.getByTestId('agent-message-row'));
    expect(row.closest('[data-item-id]')?.getAttribute('data-item-id')).toBe('msg-1');
  });

  it('expanded, the reply shows its label, sender and text, and its sender opens', async () => {
    const pane = await buildPane(makeThread(), workAroundReply());
    const view = render(MessageTimeline, { props: { pane } });
    await vi.waitFor(() => expect(view.getAllByTestId('activity-run')).toHaveLength(1));
    await fireEvent.click(view.getByTestId('activity-run-header'));

    const row = await vi.waitFor(() => view.getByTestId('agent-message-row'));
    const clip = view.getByTestId('activity-run-clip');
    expect(clip.contains(row)).toBe(true);
    const members = Array.from(clip.querySelectorAll<HTMLElement>('[data-item-id]')).map((el) => el.dataset.itemId);
    expect(members).toEqual(['spawn', 'msg-1', 'think', 'bash']);
    const inRow = (testId: string) => row.querySelector<HTMLElement>(`[data-testid="${testId}"]`);
    expect(inRow('agent-message-kind')?.textContent).toBe('Reply');
    expect(inRow('agent-message-sender')?.textContent).toBe('Worker');
    expect(inRow('agent-message-first-line')?.textContent).toBe('All green.');

    await fireEvent.click(inRow('agent-message-toggle')!);
    await vi.waitFor(() => expect(inRow('agent-message-text')?.textContent).toBe('All green.\nSecond line.'));
    // Its body is a disclosure body inside the clip, which is how an
    // expanded body lifts the run's cap (utils/activityRunClip.ts).
    const toggle = inRow('agent-message-toggle')!;
    expect(toggle.getAttribute('aria-expanded')).toBe('true');
    expect(clip.querySelector(`#${CSS.escape(toggle.getAttribute('aria-controls')!)}`)).toBe(inRow('agent-message-body'));

    await fireEvent.click(inRow('agent-message-sender')!);
    await vi.waitFor(() => expect(openThreadInPane).toHaveBeenCalledWith(expect.objectContaining({ id: 't-work' })));
  });
});
