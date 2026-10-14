import { describe, expect, it } from 'vitest';
import agentMessageMetas from '../../../../internal/usermessage/testdata/agent_message_metas.json';
import personParts from '../../../../internal/usermessage/testdata/person_parts.json';

import {
  isAgentMessageMeta,
  isReaderAuthoredUserText,
  parseUserMessageAttachments,
  personTextOfUserMessage,
  parseUserMessageMeta,
  userMessageOriginThread,
  userMessageOriginWake,
  agentWakeOriginLabel,
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
  it('reads every wake kind and labels each', () => {
    const loop = userMessageOriginWake({ originWake: { kind: 'loop', label: 'board' } });
    expect(loop).toEqual({ kind: 'loop', label: 'board', threadId: '', computer: '' });
    expect(agentWakeOriginLabel(loop!)).toBe('Loop · board');
    const watch = userMessageOriginWake({ originWake: { kind: 'watch', label: 'mrs' } });
    expect(agentWakeOriginLabel(watch!)).toBe('Watch · mrs');
    const send = userMessageOriginWake({ originWake: { kind: 'send' } });
    expect(agentWakeOriginLabel(send!)).toBe('Sent by a script');
    const question = userMessageOriginWake({ originWake: { kind: 'question', label: 'Deploy worker', threadId: 't1' } });
    expect(question?.threadId).toBe('t1');
    expect(agentWakeOriginLabel(question!)).toBe('Question · Deploy worker');
    const approval = userMessageOriginWake({ originWake: { kind: 'approval', label: 'Deploy worker' } });
    expect(agentWakeOriginLabel(approval!)).toBe('Approval · Deploy worker');
    const job = userMessageOriginWake({ originWake: { kind: 'completion', label: 'build', computer: 'GPU box' } });
    expect(job).toEqual({ kind: 'completion', label: 'build', threadId: '', computer: 'GPU box' });
    expect(agentWakeOriginLabel(job!)).toBe('Remote job · build');
    const run = userMessageOriginWake({ originWake: { kind: 'workflow', label: 'release' } });
    expect(agentWakeOriginLabel(run!)).toBe('Workflow · release');
  });

  it('refuses a kind it does not know, which is not attribution', () => {
    expect(userMessageOriginWake({ originWake: { kind: 'cron', label: 'x' } })).toBeNull();
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

  // The same rows Go's IsAgentMessage, the store's SQL filter and Stop's
  // un-send predicate are held to (TestAgentMessageClassifierAgreesInGoAndSQL).
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

describe('personTextOfUserMessage', () => {
  // The rows Go's usermessage.SplitPersonParts is held to
  // (TestSplitPersonPartsFixture): Stop's un-send gives the composer the
  // same text on both sides.
  for (const tc of personParts) {
    it(`agrees with Go: ${tc.name}`, () => {
      const meta = tc.meta === '' ? {} : (JSON.parse(tc.meta) as Record<string, unknown>);
      expect(personTextOfUserMessage(tc.text, meta)).toBe(tc.person);
    });
  }
});
