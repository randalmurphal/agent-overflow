import { describe, expect, it } from 'vitest';
import fixture from '../../../../internal/threadtools/testdata/agent_messages.json';
import labels from '../../../../internal/app/testdata/agent_message_labels.json';
import servedRows from '../../../../internal/agentmeta/testdata/served_rows.json';
import type { Item } from '../types/models';
import {
  agentMessageFromItem,
  digestThreadOrigin,
  type AgentMessagePart,
  type AgentMessageRowPart,
} from './agentMessage';

interface FixtureOrigin {
  computerId?: string;
  computerName?: string;
  threadId: string;
  title?: string;
  token?: string;
  kind: string;
  facts: Record<string, unknown>;
}

interface FixtureCase {
  name: string;
  text: string;
  /** Each joined member's UTF-8 length, as a stored row's joinedParts record it. */
  members: number[];
  /** main's templates wrote the same text, which a row from before the facts holds. */
  legacy: boolean;
  /** Each member's attribution as its writer stored it, facts included. */
  origins: FixtureOrigin[];
  parts: AgentMessagePart[];
}

const cases = fixture as FixtureCase[];

/** What a row's parts say, without who said it. */
function parsed(parts: AgentMessageRowPart[] | undefined): AgentMessagePart[] | undefined {
  return parts?.map(({ body, footer, wake }) => ({ body, footer, wake }));
}

function row(summary: string, meta: Record<string, unknown>): Pick<Item, 'kind' | 'meta' | 'summary'> {
  return { kind: 'user_text', summary, meta: JSON.stringify(meta) };
}

/**
 * A fixture case as a client receives the row its writer stored, with the
 * facts Go read of it (internal/agentmeta/testdata/served_rows.json).
 */
function fixtureRow(fixtureCase: FixtureCase): Pick<Item, 'kind' | 'meta' | 'summary'> {
  return { kind: 'user_text', summary: fixtureCase.text, meta: servedOf(fixtureCase).meta };
}

function servedOf(fixtureCase: FixtureCase): { meta: string; legacyMeta?: string } {
  const served = servedRows.find((candidate) => candidate.name === fixtureCase.name);
  if (!served) throw new Error(`no served row for ${fixtureCase.name}`);
  return served;
}

/**
 * A legacy case as a client receives the row main stored, with no kind and
 * no facts: with the facts Go's legacy reader found in its text.
 */
function legacyRow(fixtureCase: FixtureCase): Pick<Item, 'kind' | 'meta' | 'summary'> {
  return { kind: 'user_text', summary: fixtureCase.text, meta: servedOf(fixtureCase).legacyMeta ?? '' };
}

const utf8Length = (text: string) => new TextEncoder().encode(text).length;

describe('the Go templates (internal/threadtools/testdata/agent_messages.json)', () => {
  it('has the cases the row is held to', () => {
    expect(cases.length).toBeGreaterThan(15);
    expect(cases.filter((c) => c.legacy).length).toBeGreaterThan(10);
  });

  for (const fixtureCase of cases) {
    it(`renders from its facts: ${fixtureCase.name}`, () => {
      expect(parsed(agentMessageFromItem(fixtureRow(fixtureCase))?.parts)).toEqual(fixtureCase.parts);
    });
  }

  // A row main wrote has no facts; Go reads them from its text.
  for (const fixtureCase of cases.filter((c) => c.legacy)) {
    it(`renders the facts Go read in main's text: ${fixtureCase.name}`, () => {
      expect(parsed(agentMessageFromItem(legacyRow(fixtureCase))?.parts)).toEqual(fixtureCase.parts);
    });
  }

  it('renders from the facts, not the text', () => {
    const fixtureCase = cases.find((c) => c.name === 'reply')!;
    const origin = { ...fixtureCase.origins[0], title: 'Named by its attribution', facts: { ...fixtureCase.origins[0].facts, tokens: ['tok-facts'] } };
    const message = agentMessageFromItem(row(fixtureCase.text, { origin: 'agent-thread', originThread: origin, sendId: 'thread-wake:x' }))!;
    expect(message.sender).toBe('Named by its attribution');
    expect(message.parts[0].wake?.tokens).toEqual(['tok-facts']);
  });

  it('shows a message whose text changed behind its facts whole', () => {
    const fixtureCase = cases.find((c) => c.name === 'request waiting, local, quoting the user')!;
    const { meta } = fixtureRow(fixtureCase);
    for (const text of [`${fixtureCase.text} more`, `\n${fixtureCase.text}`, fixtureCase.text.slice(1)]) {
      const message = agentMessageFromItem({ kind: 'user_text', summary: text, meta })!;
      expect(parsed(message.parts)).toEqual([{ body: text, footer: null, wake: null }]);
      // Still the request its attribution records, from its sender.
      expect(message).toMatchObject({ kind: 'request', sender: 'Lead thread' });
    }
  });

  it('shows a message Go served whole as its attribution records it', () => {
    const text = 'One.\n\n---\n\nTwo.';
    const whole = { form: 'whole', bytes: utf8Length(text), body: [{ start: 0, end: utf8Length(text) }], headline: 'One.' };
    for (const [kind, expected] of [['send', 'request'], ['ask', 'question'], ['', 'request'], ['wake', 'message']] as const) {
      const message = agentMessageFromItem(row(text, { origin: 'agent-thread', originThread: { threadId: 't-l', title: 'L', kind, facts: whole } }))!;
      expect(message, kind).toMatchObject({ kind: expected, sender: 'L', firstLine: 'One.' });
      expect(parsed(message.parts), kind).toEqual([{ body: text, footer: null, wake: null }]);
    }
  });

  it('slices no span the text does not hold', () => {
    const fixtureCase = cases.find((c) => c.name === 'request waiting, local, quoting the user')!;
    const origin = fixtureCase.origins[0];
    const bytes = utf8Length(fixtureCase.text);
    for (const body of [[{ start: 0, end: bytes + 1 }], [{ start: -1, end: 3 }], [{ start: 3, end: 3 }], [{ start: 5, end: 9 }, { start: 2, end: 4 }]]) {
      const meta = { origin: 'agent-thread', originThread: { ...origin, facts: { ...origin.facts, body } }, sendId: 'thread-request:x' };
      expect(parsed(agentMessageFromItem(row(fixtureCase.text, meta))?.parts)).toEqual([{ body: fixtureCase.text, footer: null, wake: null }]);
    }
    // A span that cuts a character.
    const text = 'é, then more';
    const meta = { origin: 'agent-thread', originThread: { threadId: 't', title: 'T', kind: 'send', facts: { form: 'message', bytes: utf8Length(text), body: [{ start: 1, end: 4 }] } } };
    expect(agentMessageFromItem(row(text, meta))?.parts[0].body).toBe(text);
  });

  it("reads a spawner's answer to its worker's questions as the spawner's message", () => {
    const fixtureCase = cases.find((c) => c.name === "a spawner's answer to its worker's questions")!;
    const message = agentMessageFromItem(fixtureRow(fixtureCase))!;
    expect(message).toMatchObject({ kind: 'message', kindLabel: 'Msg', sender: fixtureCase.origins[0].title });
    expect(message.senderThread?.threadId).toBe(fixtureCase.origins[0].threadId);
    expect(parsed(message.parts)).toEqual(fixtureCase.parts);
  });

  it('never leaves footer text in a request body', () => {
    for (const fixtureCase of cases) {
      for (const part of agentMessageFromItem(fixtureRow(fixtureCase))?.parts ?? []) {
        expect(part.body).not.toMatch(/^Agent request from thread /m);
        expect(part.body).not.toContain('call thread_reply with token');
      }
    }
  });
});

describe('agentMessageFromItem', () => {
  const byName = (name: string) => cases.find((c) => c.name === name)!;
  const request = byName('request from a standing agent that spawned the receiver');
  const reply = byName('late reply with age, computer and a standing agent');
  // A case's served attribution, its headline included.
  const servedThread = (fixtureCase: FixtureCase) => (JSON.parse(servedOf(fixtureCase).meta) as { originThread: Record<string, unknown> }).originThread;
  const withKind = (fixtureCase: FixtureCase, kind: string, extra: Record<string, unknown> = {}) => ({
    origin: 'agent-thread', originThread: { ...servedThread(fixtureCase), kind, ...extra }, sendId: 'thread-request:k',
  });

  it("is null for the person's own message", () => {
    expect(agentMessageFromItem(row('hello', {}))).toBeNull();
    expect(agentMessageFromItem(row('hello', { sendId: 'abc', expandComposerCommands: true }))).toBeNull();
  });

  it("keeps a joined row that holds the person's composer message as theirs", () => {
    expect(agentMessageFromItem(row(request.text, { ...withKind(request, 'send'), expandComposerCommands: true }))).toBeNull();
  });

  it('names the sender, its role, the kind and the first line of a spawn prompt', () => {
    const message = agentMessageFromItem(row(request.text, withKind(request, 'spawn')))!;
    expect(message).toMatchObject({
      kind: 'spawn', kindLabel: 'Spawn', kindTitle: 'Spawn prompt', sender: 'Planner',
      senderRole: 'Keeps the "plan" honest.', firstLine: 'Do the task.',
    });
    expect(message.senderThread?.threadId).toBe('t-plan');
    expect(message.parts[0].footer?.routesPrompts).toBe(true);
  });

  it('reads send and ask kinds', () => {
    expect(agentMessageFromItem(row(request.text, withKind(request, 'send')))?.kind).toBe('request');
    expect(agentMessageFromItem(row(request.text, withKind(request, 'ask')))?.kindTitle).toBe('Question');
  });

  it('reads an answer coming back with the remote computer', () => {
    expect(agentMessageFromItem(row(reply.text, { origin: 'agent-thread', originThread: servedThread(reply), sendId: 'thread-wake-late:k' }))).toMatchObject({
      kind: 'reply', kindLabel: 'Reply', sender: 'Worker',
      senderThread: expect.objectContaining({ threadId: 't-work', computerName: 'desk', computerId: 'computer-desk' }),
      senderRole: 'Runs the builds.', firstLine: 'Done.',
    });
  });

  it('names a digest by its thread count and opens each thread, on its own computer too', () => {
    const digest = byName('digest: the same reply, a standing agent and an age');
    const message = agentMessageFromItem(fixtureRow(digest))!;
    expect(message).toMatchObject({ kind: 'reply', sender: '2 threads', senderThread: null, firstLine: 'Done.' });
    expect(message.parts[0].wake!.threads.map(digestThreadOrigin)).toEqual([
      { computerId: '', computerName: '', threadId: 't-work', title: 'Worker', token: '0b5c7f2e-1111-4a2b-9c3d-5e6f7a8b9c0d', kind: 'wake' },
      // On another computer: opened through that computer's id.
      { computerId: 'computer-desk', computerName: 'desk', threadId: 't-lex', title: 'Lexer "two"', token: 'tok-2', kind: 'wake' },
    ]);
    expect(agentMessageFromItem(fixtureRow(byName('digest: expired')))?.firstLine).toBe('Worker, Lexer');
  });

  it('opens no thread for a reminder: the thread reminded itself', () => {
    const message = agentMessageFromItem(fixtureRow(byName('reminder')))!;
    expect(message).toMatchObject({ kind: 'reminder', sender: 'This thread', senderThread: null });
  });

  it("reads the notice that the user stopped a worker as the user's stop", () => {
    const text = 'The user stopped thread "W" (t-w), which you spawned, from the Agents tray after it had answered you.';
    const message = agentMessageFromItem(row(text, {
      origin: 'agent-thread', sendId: 'thread-stopped:k',
      originThread: { threadId: 't-w', title: 'W', token: 'k', kind: 'wake', facts: { form: 'answer', bytes: text.length, outcome: 'worker-stopped', body: [{ start: 0, end: text.length }], headline: text, tokens: ['k'], stoppedBy: 'the user' } },
    }))!;
    expect(message).toMatchObject({ kind: 'worker-stopped', kindLabel: 'Stop', kindTitle: 'Worker stopped by the user', sender: 'W', firstLine: text });
    expect(message.parts[0].wake).toMatchObject({ stoppedBy: 'the user', tokens: ['k'] });
  });

  // A report a Go writer wrote, as Go serves it
  // (internal/app/testdata/agent_message_labels.json).
  const servedReport = (name: string) => {
    const tc = labels.find((candidate) => candidate.name === name);
    if (!tc) throw new Error(`no served row for ${name}`);
    return { kind: 'user_text' as const, summary: tc.text, meta: tc.meta };
  };

  it('names each report kind as Go serves it', () => {
    for (const [name, expected] of [
      ['a loop', { kind: 'loop', kindLabel: 'Loop', kindTitle: 'Loop wake', sender: 'nightly', senderThread: null, firstLine: 'Check the build.' }],
      ['an unlabelled loop with no note', { kind: 'loop', sender: 'Loop wake' }],
      ['a watch', { kind: 'watch', kindLabel: 'Watch', kindTitle: 'Watch wake', sender: 'ci', firstLine: 'build failed: exit 2' }],
      ["a script's message", { kind: 'script', kindLabel: 'Shell', kindTitle: 'Message from a script', sender: 'A script', firstLine: 'deploy finished' }],
      ["a worker's approval request", { kind: 'worker-approval', kindLabel: 'Allow', kindTitle: "Worker's approval request", sender: 'Worker' }],
      ["an untitled worker's question", { kind: 'worker-question', kindLabel: 'Ask', kindTitle: "Worker's question", sender: "Worker's question" }],
      ['a remote job', { kind: 'job', kindLabel: 'Job', kindTitle: 'Remote job finished', sender: 'make test on desk', senderThread: null }],
      ['an unlabelled remote job', { kind: 'job', sender: 'c-desk' }],
      ['a workflow wake', { kind: 'workflow', kindLabel: 'Run', kindTitle: 'Workflow wake', sender: 'release', senderThread: null }],
      ["an assistant's opening message", { kind: 'opening', kindLabel: 'Start', kindTitle: "Assistant's opening message", sender: 'Agent Overflow' }],
      ["an assistant's handoff block", { kind: 'handoff', kindLabel: 'Moved', kindTitle: 'New thread, same assistant', sender: 'Agent Overflow' }],
    ] as const) {
      expect(agentMessageFromItem(servedReport(name)), name).toMatchObject(expected);
    }
    // A worker's prompt opens the worker.
    expect(agentMessageFromItem(servedReport("a worker's approval request"))?.senderThread?.threadId).toBe('t-worker');
    // A worker on a paired computer is opened there.
    expect(agentMessageFromItem(servedReport("a worker's approval request on a paired computer"))?.senderThread)
      .toMatchObject({ threadId: 't-worker', computerId: 'c-desk', computerName: 'desk' });
  });

  it('shows a report its data without the notice, decoded when Go says it is quoted data', () => {
    const loop = servedReport('a loop');
    const message = agentMessageFromItem(loop)!;
    // The notice written for the agent is the header's to say.
    expect(message.parts[0].body).toBe('This fire stands in for the ones it missed: missed 2 since the previous wake.\nnote: "Check  the build."');
    const escaped = agentMessageFromItem(servedReport('a workflow wake whose data holds escapes'))!;
    expect(escaped.parts[0].body).not.toContain('\\u003c');
    expect(escaped.parts[0].body).toContain('<b>');
    // The same report served as text is shown as written.
    const meta = JSON.parse(servedReport('a workflow wake whose data holds escapes').meta);
    meta.originWake.report.body = 'text';
    const asWritten = agentMessageFromItem({ ...servedReport('a workflow wake whose data holds escapes'), meta: JSON.stringify(meta) })!;
    expect(asWritten.parts[0].body).toContain('\\u003cb\\u003e');
  });

  it('shows a wake whose attribution did not decode whole', () => {
    const message = agentMessageFromItem(row('Line one.\n"data"', { origin: 'agent-wake', originWake: { kind: 'nonsense' } }))!;
    expect(message).toMatchObject({ kind: 'script', sender: 'A wake', firstLine: 'Line one.' });
    expect(message.parts[0].body).toBe('Line one.\n"data"');
  });

  it('reads each joined member with its own attribution', () => {
    const loopNotice = 'Loop "nightly" fired. x';
    const loop = `${loopNotice}\nnote: "Go."`;
    const quoting = byName('two wakes joined, the first answer quoting a wake');
    const quotingBytes = new TextEncoder().encode(quoting.text);
    const answer = new TextDecoder().decode(quotingBytes.subarray(quotingBytes.length - quoting.members[1]));
    const loopWake = {
      kind: 'loop', label: 'nightly', report: JSON.parse(servedReport('a loop').meta).originWake.report,
      facts: { form: 'report', bytes: loop.length, body: [{ start: loopNotice.length + 1, end: loop.length }], headline: 'Go.' },
    };
    const members = [
      { text: loop, origin: 'agent-wake', originWake: loopWake },
      { text: answer, origin: 'agent-thread', originThread: quoting.origins[1] },
    ];
    const text = members.map((member) => member.text).join('\n\n---\n\n');
    const message = agentMessageFromItem(row(text, {
      origin: 'agent-wake', originWake: loopWake,
      joinedParts: members.map(({ text: memberText, ...attribution }) => ({ bytes: new TextEncoder().encode(memberText).length, ...attribution })),
    }))!;
    expect(message).toMatchObject({ kind: 'loop', sender: 'nightly', firstLine: 'Go.' });
    expect(parsed(message.parts)).toEqual([{ body: 'note: "Go."', footer: null, wake: null }, quoting.parts[1]]);
    expect(message.parts.map(({ source }) => [source.kind, source.sender, source.senderThread?.threadId ?? null])).toEqual([
      ['loop', 'nightly', null],
      [quoting.parts[1].wake!.kind, quoting.parts[1].wake!.title, quoting.parts[1].wake!.threadId],
    ]);
  });

  it('reads a joined row whole when its recorded members do not describe its text', () => {
    const quoting = byName('two wakes joined, the first answer quoting a wake');
    const [first, second] = quoting.members;
    const origin = { origin: 'agent-thread', originThread: quoting.origins[0] };
    for (const joinedParts of [
      undefined,
      [],
      [{ bytes: first - 1, ...origin }, { bytes: second + 1, ...origin }],
      [{ bytes: 'x', ...origin }, { bytes: second, ...origin }],
    ]) {
      const message = agentMessageFromItem(row(quoting.text, { ...origin, joinedSendIds: ['thread-wake:a', 'thread-wake:b'], joinedParts }))!;
      expect(message.parts).toHaveLength(1);
      // Every member's words, the last one's included: the first member's
      // facts describe that member alone.
      expect(message.parts[0].body).toBe(quoting.text);
      expect(message.parts[0].body).toContain(quoting.parts[1].body);
    }
  });

  it("reads a provider's own agent messages", () => {
    expect(agentMessageFromItem(row('Do X', {
      wire_only: true, agent_message: { sender: '/root', messageType: 'NEW_TASK' },
    }))).toMatchObject({ kind: 'task', sender: 'Main agent', firstLine: 'Do X' });
    expect(agentMessageFromItem(row('Hi', {
      wire_only: true, origin: 'peer-session', cross_session_from_name: 'BETA',
    }))).toMatchObject({ kind: 'message', sender: 'BETA (Claude session)' });
  });

  it("shows an agent row it cannot read whole rather than as the person's", () => {
    const message = agentMessageFromItem(row('free text', { origin: 'agent-thread' }))!;
    expect(parsed(message.parts)).toEqual([{ body: 'free text', footer: null, wake: null }]);
    expect(message.senderThread).toBeNull();
  });
});

describe('a report kind defined only in Go', () => {
  // agentmeta.TestReportKind: no TypeScript names it.
  const served = servedRows.find((candidate) => candidate.name === 'a report of a kind only Go defines') as { text: string; meta: string };

  it('renders with the label, sender and body Go serves, not as an unknown wake', () => {
    const message = agentMessageFromItem({ kind: 'user_text', summary: served.text, meta: served.meta })!;
    expect(message).toMatchObject({
      kind: 'test-only', kindLabel: 'Probe', kindTitle: 'Test-only report', sender: 'probe', firstLine: '"saw <err>"',
    });
    expect(message.senderThread?.threadId).toBe('t-probe');
    // The data without the notice written for the agent, decoded for the person.
    expect(parsed(message.parts)).toEqual([{ body: '"saw <err>"\n"second line"', footer: null, wake: null }]);
  });
});

describe('a first line under every white space class (agentmeta.FirstLine)', () => {
  const served = servedRows.find((candidate) => candidate.name === "a whole message's first line under every white space class") as { text: string; meta: string };
  const headline = (JSON.parse(served.meta) as { originThread: { facts: { headline: string } } }).originThread.facts.headline;

  it('is the headline Go serves for the same text, read without facts', () => {
    expect(headline).not.toBe('');
    const withoutFacts = JSON.parse(served.meta) as { originThread: Record<string, unknown> };
    delete withoutFacts.originThread.facts;
    for (const meta of [
      withoutFacts,
      { agent_message: { sender: 'peer', messageType: 'MESSAGE' } },
      { origin: 'peer-session', cross_session_from_name: 'BETA' },
      { origin: 'agent-wake' },
    ]) {
      expect(agentMessageFromItem(row(served.text, meta))?.firstLine, JSON.stringify(meta)).toBe(headline);
    }
  });
});

describe('the Edits selector labels (internal/app/testdata/agent_message_labels.json)', () => {
  // Go names a turn an agent's message started by the row's sender and
  // first line (agentMessageLabel); these are the row's own.
  for (const tc of labels) {
    it(`agrees with Go: ${tc.name}`, () => {
      const message = agentMessageFromItem({ kind: 'user_text', meta: tc.meta, summary: tc.text });
      expect(message).not.toBeNull();
      const label = message!.firstLine === '' ? message!.sender : `${message!.sender}: ${message!.firstLine}`;
      expect(label).toBe(tc.label);
    });
  }
});
