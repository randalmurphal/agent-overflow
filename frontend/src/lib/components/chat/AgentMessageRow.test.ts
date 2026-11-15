import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render } from '@testing-library/svelte';
import { tick } from 'svelte';
import fixture from '../../../../../internal/threadtools/testdata/agent_messages.json';
import { buildPane, makeItem, makeThread } from '../../../test/helpers/chat';
import { resetBindingMocks } from '../../../test/mocks/bindings-app';
import { stageBackend, resetStagedBackends } from '../../../test/helpers/backends';
import { HOME_BACKEND } from '../../transport/backendKey';
import type { Item } from '../../types/models';
import TimelineLeaf from './TimelineLeaf.svelte';

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

const fixtureCase = (name: string) => (fixture as { name: string; text: string; members: number[] }[]).find((c) => c.name === name)!;
const text = (name: string) => fixtureCase(name).text;
/** The joinedParts a join records for a fixture case's members, each attributed `origins[i]`. */
const joinedParts = (name: string, origins: Record<string, unknown>[]) =>
  fixtureCase(name).members.map((bytes, index) => ({ bytes, origin: 'agent-thread', originThread: origins[index] }));

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
      originThread: { threadId: 't-plan', title: 'Planner', token: 'tok', kind: 'spawn' },
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
    expect(fact('Token')).toBe('0b5c7f2e-1111-4a2b-9c3d-5e6f7a8b9c0d');
    expect(body.textContent).not.toContain('call thread_reply');
  });

  it('keeps its fold across a remount, as virtualization does', async () => {
    const pane = await buildPane();
    const item = agentRow(text('reply'), { origin: 'agent-thread', originThread: { threadId: 't-work', title: 'Worker', kind: 'wake' } });
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
      originThread: { threadId: 't-plan', title: 'Planner', kind: 'send' },
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
      originThread: { computerId: 'home-uuid', computerName: 'workstation-7', threadId: 't-plan', title: 'Planner', kind: 'send' },
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
      originThread: { computerId: 'gone', computerName: 'desk', threadId: 't-work', title: 'Worker', kind: 'wake' },
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
      originThread: { threadId: 't-a', title: 'A', kind: 'wake' },
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
    const item = agentRow(`${requests.text}\n\n---\n\n${first}`, {
      origin: 'agent-thread',
      originThread: origins[0],
      sendId: 'thread-request:tok-a',
      joinedSendIds: origins.map((origin) => `thread-request:${origin.token}`),
      joinedParts: [...requests.members, requests.members[0]].map((bytes, index) => ({
        sendId: `thread-request:${origins[index].token}`, bytes, origin: 'agent-thread', originThread: origins[index],
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
    expect(parts.map((part) => part.querySelector('[data-fact="Token"]')?.textContent)).toEqual([
      '0b5c7f2e-1111-4a2b-9c3d-5e6f7a8b9c0d', 'tok-2', '0b5c7f2e-1111-4a2b-9c3d-5e6f7a8b9c0d',
    ]);
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
      originThread: { threadId: 't-lead', title: 'Lead', kind: 'send' },
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
        originThread: { threadId: 't-work', title: 'Worker', token: 'tok', kind: 'wake' },
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
            origin: 'agent-thread', originThread: { threadId: 't-work', title: 'Worker', token: 'tok', kind: 'wake' },
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
          origin: 'agent-thread', originThread: { threadId: 't-work', title: 'Worker', token: 'tok', kind: 'wake' },
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

    const digest = render(TimelineLeaf, {
      props: {
        pane,
        item: agentRow(text('digest: the same reply, a standing agent and an age'), {
          origin: 'agent-thread', originThread: { threadId: 't-work', title: 'Worker', token: 'tok', kind: 'wake' },
        }, { id: 'msg-2' }),
      },
    });
    expect(digest.getByTestId('agent-message-sender').textContent).toBe('2 threads');
    expect(digest.getByTestId('agent-message-sender').tagName).not.toBe('BUTTON');
    await fireEvent.click(digest.getByTestId('agent-message-toggle'));
    await tick();
    expect(digest.getAllByTestId('agent-message-part')).toHaveLength(1);
    expect(digest.getByTestId('agent-message-text').textContent).toBe('Done.\nSecond line.');
    // Each thread it speaks for is named the way a joined part's sender is,
    // with its own facts; only the one on this computer opens.
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
  });

  it('renders a loop wake decoded and a provider agent\'s message as agent rows', async () => {
    const pane = await buildPane();
    const loop = render(TimelineLeaf, {
      props: {
        pane,
        item: agentRow('Loop "nightly" fired. x\nnote: "\\u003cgo\\u003e"', {
          origin: 'agent-wake', originWake: { kind: 'loop', label: 'nightly' }, sendId: 'agent-wake:1',
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
          origin: 'agent-wake', originWake: { kind: 'loop', label: 'x' }, expandComposerCommands: true,
        }),
      },
    });
    expect(mixed.getByTestId('user-message-bubble')).toBeTruthy();
    expect(mixed.getByTestId('user-message-wake-origin').textContent).toContain('Loop · x');
  });
});
