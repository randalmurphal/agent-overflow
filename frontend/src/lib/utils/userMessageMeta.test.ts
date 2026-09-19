import { describe, expect, it } from 'vitest';

import {
  parseUserMessageAttachments,
  parseUserMessageMeta,
  userMessageOriginThread,
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
    })).toEqual({
      computerId: 'studio',
      computerName: 'Studio',
      threadId: 'thread-9',
      title: 'Auth rewrite',
      token: 'tok-1',
    });
  });

  it('keeps a row with only a thread id, which is the one field the chip needs', () => {
    expect(parse({ threadId: '  thread-9  ' })).toEqual({
      computerId: '',
      computerName: '',
      threadId: 'thread-9',
      title: '',
      token: '',
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
    });
  });

  it('survives meta that is not JSON at all', () => {
    expect(userMessageOriginThread(parseUserMessageMeta('{not json'))).toBeNull();
  });
});
