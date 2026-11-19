import { describe, expect, it } from 'vitest';
import agentMessageMetas from '../../../../internal/usermessage/testdata/agent_message_metas.json';
import labels from '../../../../internal/app/testdata/agent_message_labels.json';

import {
  isAgentMessageMeta,
  isReaderAuthoredUserText,
  parseUserMessageAttachments,
  parseUserMessageMeta,
  restoredDraftSnapshotAttachmentsFromUserItem,
  userMessageOriginThread,
  userMessageOriginWake,
  agentWakeOriginLabel,
  factsBody,
  userMessageFacts,
  userMessageOriginTokens,
} from './userMessageMeta';
import {
  DEFAULT_MAX_ATTACHMENT_SIZE,
  DEFAULT_MAX_FILE_ATTACHMENT_SIZE,
} from '../types/attachment';

function meta(attachments: unknown[]): string {
  return JSON.stringify({ attachments });
}

function entry(overrides: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    id: 'att-1',
    threadId: 'thread-1',
    filename: 'hero.png',
    mimeType: 'image/png',
    size: 128,
    ...overrides,
  };
}

// The meta blob is untrusted wire content the timeline renders straight from,
// so what it is allowed to say is the whole contract here.
describe('parseUserMessageAttachments — attachment kind', () => {
  it('requests inherited attachments through the viewing thread ownership', () => {
    const [attachment] = parseUserMessageAttachments(meta([entry()]), 'fork');
    expect(attachment).toMatchObject({ id: 'att-1', threadId: 'fork' });
  });

  it('reads an absent kind as image, which is what every pre-kind row carried', () => {
    const [attachment] = parseUserMessageAttachments(meta([entry()]), 'thread-1');
    expect(attachment?.kind).toBe('image');
  });

  it('reads the file kind and keeps a MIME the image allowlist would refuse', () => {
    const [attachment] = parseUserMessageAttachments(
      meta([entry({ kind: 'file', filename: 'report.pdf', mimeType: 'application/pdf' })]),
      'thread-1',
    );
    expect(attachment).toMatchObject({ kind: 'file', mimeType: 'application/pdf' });
  });

  it('reads a shell command off a file and bounds what it trusts', () => {
    const shell = {
      command: 'make test', commandLength: 640, exitCode: 2, durationMs: 340,
      truncated: true, timedOut: true, preview: 'FAIL\n', more: true, unknown: 'dropped',
    };
    const [attachment] = parseUserMessageAttachments(
      meta([entry({ kind: 'file', filename: 'output.txt', mimeType: 'text/plain', shell })]),
      'thread-1',
    );
    expect(attachment?.shell).toEqual({
      command: 'make test', commandLength: 640, exitCode: 2, durationMs: 340,
      truncated: true, timedOut: true, preview: 'FAIL\n', more: true,
    });

    // Absent flags stay absent; malformed numbers read as zero.
    const [plain] = parseUserMessageAttachments(
      meta([entry({ kind: 'file', mimeType: 'text/plain', shell: { command: 'ls', exitCode: 'x', durationMs: null } })]),
      'thread-1',
    );
    expect(plain?.shell).toEqual({ command: 'ls', exitCode: 0, durationMs: 0 });

    // A whole length that is not longer than the command says nothing.
    const [whole] = parseUserMessageAttachments(
      meta([entry({ kind: 'file', mimeType: 'text/plain', shell: { command: 'ls', commandLength: 2, exitCode: 0, durationMs: 1 } })]),
      'thread-1',
    );
    expect(whole?.shell).toEqual({ command: 'ls', exitCode: 0, durationMs: 1 });
  });

  it('never reads a shell command off an image or without a command', () => {
    const [image] = parseUserMessageAttachments(
      meta([entry({ shell: { command: 'ls', exitCode: 0, durationMs: 1 } })]),
      'thread-1',
    );
    expect(image?.kind).toBe('image');
    expect(image?.shell).toBeUndefined();
    const [file] = parseUserMessageAttachments(
      meta([entry({ kind: 'file', mimeType: 'text/plain', shell: { command: '  ', exitCode: 0 } })]),
      'thread-1',
    );
    expect(file?.shell).toBeUndefined();
  });

  it('carries a shell command back into a restored draft', () => {
    const item = {
      id: 'user-1', threadId: 'thread-1', createdAt: 5,
      meta: meta([entry({ kind: 'file', mimeType: 'text/plain', shell: { command: 'ls', exitCode: 0, durationMs: 1 } })]),
    } as unknown as Parameters<typeof restoredDraftSnapshotAttachmentsFromUserItem>[0];
    expect(restoredDraftSnapshotAttachmentsFromUserItem(item)[0]?.shell).toEqual({ command: 'ls', exitCode: 0, durationMs: 1 });
  });

  it('still refuses an IMAGE whose MIME is not one we can render', () => {
    expect(parseUserMessageAttachments(
      meta([entry({ mimeType: 'application/pdf' })]),
      'thread-1',
    )).toEqual([]);
  });

  it('treats an unrecognised kind as an image, so it faces the strict branch', () => {
    expect(parseUserMessageAttachments(
      meta([entry({ kind: 'video', mimeType: 'video/mp4' })]),
      'thread-1',
    )).toEqual([]);
  });

  it('caps each kind at its own ceiling', () => {
    const overImage = { size: DEFAULT_MAX_ATTACHMENT_SIZE + 1 };
    expect(parseUserMessageAttachments(meta([entry(overImage)]), 'thread-1')).toEqual([]);

    // The same byte count is fine for a file, which is bounded 5x higher.
    const [file] = parseUserMessageAttachments(
      meta([entry({ ...overImage, kind: 'file', mimeType: 'application/pdf' })]),
      'thread-1',
    );
    expect(file?.size).toBe(DEFAULT_MAX_ATTACHMENT_SIZE + 1);

    expect(parseUserMessageAttachments(
      meta([entry({
        kind: 'file',
        mimeType: 'application/pdf',
        size: DEFAULT_MAX_FILE_ATTACHMENT_SIZE + 1,
      })]),
      'thread-1',
    )).toEqual([]);
  });

  it('keeps a mixed list in order, so the reader sees what was sent', () => {
    const parsed = parseUserMessageAttachments(meta([
      entry({ id: 'img-1' }),
      entry({ id: 'doc-1', kind: 'file', filename: 'report.pdf', mimeType: 'application/pdf' }),
      entry({ id: 'img-2' }),
    ]), 'thread-1');

    expect(parsed.map((attachment) => [attachment.id, attachment.kind]))
      .toEqual([['img-1', 'image'], ['doc-1', 'file'], ['img-2', 'image']]);
  });
});

// The origin of an agent-written row is untrusted wire content too, and the
// chip it feeds is a link: a value that cannot name a thread is not
// attribution and must not render one.
describe('userMessageOriginThread', () => {
  function parse(originThread: unknown) {
    return userMessageOriginThread(
      parseUserMessageMeta(JSON.stringify({ origin: 'agent-thread', originThread })),
    );
  }

  it('reads the whole shape the tools stamp', () => {
    expect(parse({
      computerId: 'studio',
      computerName: 'Studio',
      threadId: 'thread-9',
      title: 'Auth rewrite',
      token: 'tok-1',
      kind: 'send',
    })).toEqual({
      computerId: 'studio',
      computerName: 'Studio',
      threadId: 'thread-9',
      title: 'Auth rewrite',
      token: 'tok-1',
      kind: 'send',
    });
  });

  it('keeps a row with only a thread id, which is the one field the chip needs', () => {
    expect(parse({ threadId: '  thread-9  ' })).toEqual({
      computerId: '',
      computerName: '',
      threadId: 'thread-9',
      title: '',
      token: '',
      kind: '',
    });
  });

  it('refuses anything that names no thread', () => {
    expect(parse({ title: 'Auth rewrite' })).toBeNull();
    expect(parse({ threadId: '   ' })).toBeNull();
    expect(parse({ threadId: 42 })).toBeNull();
    expect(parse(['thread-9'])).toBeNull();
    expect(parse('thread-9')).toBeNull();
    expect(parse(null)).toBeNull();
    expect(userMessageOriginThread({})).toBeNull();
  });

  it('drops fields that are not strings rather than rendering them', () => {
    expect(parse({ threadId: 'thread-9', title: { text: 'Auth' }, computerName: 7 })).toEqual({
      computerId: '',
      computerName: '',
      threadId: 'thread-9',
      title: '',
      token: '',
      kind: '',
    });
  });

  it('survives meta that is not JSON at all', () => {
    expect(userMessageOriginThread(parseUserMessageMeta('{not json'))).toBeNull();
  });
});

describe('userMessageOriginWake', () => {
  // Each report a Go writer wrote, as Go serves its attribution.
  const served = (name: string) => {
    const tc = labels.find((candidate) => candidate.name === name);
    if (!tc) throw new Error(`no served row for ${name}`);
    return parseUserMessageMeta(tc.meta);
  };

  it('reads every report kind and badges each as Go serves it', () => {
    for (const [name, badge] of [
      ['a loop', 'Loop · nightly'],
      ['a watch', 'Watch · ci'],
      ["a script's message", 'Sent by a script'],
      ["a worker's approval request", 'Approval · Worker'],
      ["an untitled worker's question", 'Question'],
      ['a remote job', 'Remote job · make test'],
      ['a workflow wake', 'Workflow · release'],
      ["an assistant's opening message", 'Opening · Ops'],
      ["an assistant's handoff block", 'New thread · Ops'],
    ] as const) {
      expect(agentWakeOriginLabel(userMessageOriginWake(served(name))!), name).toBe(badge);
    }
    const job = userMessageOriginWake(served('a remote job'));
    expect(job).toMatchObject({ kind: 'completion', label: 'make test', threadId: '', computer: 'desk' });
    expect(userMessageOriginWake(served("a worker's approval request"))?.threadId).toBe('t-worker');
  });

  it('reads a kind it has never heard of from what Go serves', () => {
    const report = { category: 'probe', label: 'Probe', title: 'Probe report', sender: 'p', badge: 'Probe · p', body: 'data' };
    expect(userMessageOriginWake({ originWake: { kind: 'probe', label: 'p', report } })).toMatchObject({ kind: 'probe', report });
  });

  it('refuses an attribution Go did not describe, which is not one it can read', () => {
    expect(userMessageOriginWake({ originWake: { kind: 'loop', label: 'x' } })).toBeNull();
    expect(userMessageOriginWake({ originWake: { kind: 'loop', report: { category: 'loop' } } })).toBeNull();
    expect(userMessageOriginWake({ originWake: 'loop' })).toBeNull();
    expect(userMessageOriginWake({})).toBeNull();
  });
});

describe('isAgentMessageMeta and isReaderAuthoredUserText', () => {
  const row = (meta: Record<string, unknown>, parentId = '') =>
    ({ kind: 'user_text', parentId, meta: JSON.stringify(meta) }) as Parameters<typeof isReaderAuthoredUserText>[0];

  it('calls an agent thread\'s or a wake\'s row an agent message, and no other', () => {
    expect(isAgentMessageMeta({ origin: 'agent-thread' })).toBe(true);
    expect(isAgentMessageMeta({ origin: 'agent-wake' })).toBe(true);
    for (const origin of ['', 'external-queue', 'scheduled', 'peer-session']) {
      expect(isAgentMessageMeta({ origin })).toBe(false);
    }
  });

  // Headless Claude joins the person's queued message after an agent's into
  // one row attributed to the agent; the composer flag says the person is in it.
  it('keeps a joined row holding the person\'s composer message as theirs', () => {
    expect(isAgentMessageMeta({ origin: 'agent-wake', expandComposerCommands: true })).toBe(false);
    expect(isReaderAuthoredUserText(row({ origin: 'agent-thread', expandComposerCommands: true }))).toBe(true);
  });

  // The same rows Go's IsAgentMessage, the store's SQL filter and
  // isReaderAuthoredUserItem are held to (TestAgentMessageClassifierAgreesInGoAndSQL).
  for (const tc of agentMessageMetas) {
    it(`agrees with Go and SQL: ${tc.name}`, () => {
      const meta = tc.meta === '' ? {} : (JSON.parse(tc.meta) as Record<string, unknown>);
      expect(isAgentMessageMeta(meta)).toBe(tc.agent);
      expect(isReaderAuthoredUserText(row(meta))).toBe(!tc.agent);
    });
  }

  it('counts only the person\'s top-level rows', () => {
    expect(isReaderAuthoredUserText(row({}))).toBe(true);
    expect(isReaderAuthoredUserText(row({ origin: 'agent-thread' }))).toBe(false);
    expect(isReaderAuthoredUserText(row({ origin: 'agent-wake' }))).toBe(false);
    expect(isReaderAuthoredUserText(row({ wire_only: true }))).toBe(false);
    expect(isReaderAuthoredUserText(row({}, 'tool-1'))).toBe(false);
  });
});

describe('userMessageFacts and factsBody', () => {
  const bytes = (s: string) => new TextEncoder().encode(s).length;
  const head = 'Reply from "Café" [token k]:\n';
  const text = `${head}naïve answer\nThat thread is a standing agent.\nrest`;
  const answer = { start: bytes(head), end: bytes(head) + bytes('naïve answer') };
  const rest = { start: bytes(text) - bytes('\nrest'), end: bytes(text) };
  const facts = { form: 'answer', bytes: bytes(text), outcome: 'reply', body: [answer, rest], tokens: ['k'], role: '', headline: 'naïve answer' };
  const thread = (value: unknown) => ({ origin: 'agent-thread', originThread: { threadId: 't', facts: value } });

  it('reads the facts of the attribution the row is from when they describe its text', () => {
    expect(userMessageFacts(text, thread(facts))).toMatchObject({ form: 'answer', outcome: 'reply', tokens: ['k'], role: '', headline: 'naïve answer' });
    const report = { form: 'report', bytes: 2, body: [{ start: 0, end: 2 }], headline: 'go' };
    expect(userMessageFacts('go', { origin: 'agent-wake', originWake: { kind: 'loop', facts: report } })?.headline).toBe('go');
    // Not this row's attribution, or none at all.
    expect(userMessageFacts(text, { origin: 'agent-wake', originThread: { threadId: 't', facts } })).toBeNull();
    expect(userMessageFacts(text, { origin: 'agent-thread', originThread: { threadId: 't' } })).toBeNull();
  });

  it('refuses facts that do not describe the text', () => {
    for (const refused of [
      { ...facts, form: '' },
      { ...facts, form: 'note' },
      { ...facts, bytes: bytes(text) - 1 },
      { ...facts, bytes: undefined },
      { ...facts, body: [{ start: 'a', end: 2 }] },
      { ...facts, body: [answer, { start: rest.start, end: rest.end + 1 }] },
      { ...facts, body: [{ start: -1, end: 2 }] },
      { ...facts, body: [{ start: 2, end: 2 }] },
      { ...facts, body: [rest, answer] },
      { ...facts, body: [answer, { start: answer.end - 1, end: rest.end }] },
      // Cutting the ï of naïve.
      { ...facts, body: [{ start: answer.start, end: answer.start + 3 }] },
      { ...facts, body: [{ start: answer.start + 3, end: answer.end }] },
    ]) {
      expect(userMessageFacts(text, thread(refused))).toBeNull();
    }
    // The same facts over text changed behind their back.
    expect(userMessageFacts(`${text}!`, thread(facts))).toBeNull();
  });

  it('reads the body spans cover, joined, by UTF-8 bytes', () => {
    expect(factsBody(text, userMessageFacts(text, thread(facts))!)).toBe('naïve answer\nrest');
    const ascii = { form: 'message', bytes: 10, body: [{ start: 6, end: 10 }] };
    expect(factsBody('ascii only', userMessageFacts('ascii only', thread(ascii))!)).toBe('only');
    expect(factsBody('anything', userMessageFacts('anything', thread({ form: 'answer', bytes: 8, outcome: 'settled' }))!)).toBe('');
  });

  it('counts every request a joined or digest answer says it answers', () => {
    const digest = { origin: 'agent-thread', originThread: { threadId: 't-a', token: 'tok-1', kind: 'wake', facts: { outcome: 'reply', tokens: ['tok-1', 'tok-2', 'tok-3'] } } };
    expect(userMessageOriginTokens(digest)).toEqual(['tok-1', 'tok-2', 'tok-3']);
    // main's rows carry their token alone.
    expect(userMessageOriginTokens({ origin: 'agent-thread', originThread: { threadId: 't-a', token: 'tok-1' } })).toEqual(['tok-1']);
    expect(userMessageOriginTokens({
      origin: 'agent-wake', originWake: { kind: 'loop' },
      joinedParts: [
        { origin: 'agent-wake', originWake: { kind: 'loop' } },
        digest,
        { origin: 'agent-thread', originThread: { threadId: 't-b', token: 'tok-4' } },
      ],
    })).toEqual(['tok-1', 'tok-2', 'tok-3', 'tok-4']);
    expect(userMessageOriginTokens({ origin: 'agent-wake', originWake: { kind: 'loop', facts: { tokens: ['x'] } } })).toEqual([]);
  });
});
