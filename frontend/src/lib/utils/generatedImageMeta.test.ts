import { describe, expect, it } from 'vitest';

import { generatedImageRow } from './generatedImageMeta';
import type { Item } from '../types/models';

function item(overrides: Partial<Item> = {}): Item {
  return {
    id: 'image:img-1',
    threadId: 'thread-1',
    turnIndex: 0,
    itemIndex: 3,
    kind: 'assistant_text',
    role: 'assistant',
    status: 'completed',
    summary: 'A quiet dashboard',
    createdAt: 1,
    updatedAt: 1,
    ...overrides,
  } as Item;
}

function meta(generatedImage: unknown, attachments?: unknown[]): string {
  return JSON.stringify(attachments ? { generatedImage, attachments } : { generatedImage });
}

const attachment = {
  id: 'att-1',
  threadId: 'thread-1',
  filename: 'render.png',
  mimeType: 'image/png',
  size: 4096,
  kind: 'image',
};

describe('generatedImageRow', () => {
  it('reads a successful import as its images plus provenance', () => {
    const row = generatedImageRow(item({
      meta: meta({ sourceItemId: 'img-1', provider: 'codex', prompt: 'A quiet dashboard' }, [attachment]),
    }));

    expect(row).not.toBeNull();
    expect(row?.sourceItemId).toBe('img-1');
    expect(row?.prompt).toBe('A quiet dashboard');
    expect(row?.error).toBe('');
    expect(row?.images).toHaveLength(1);
    expect(row?.images[0]).toMatchObject({ id: 'att-1', mimeType: 'image/png' });
  });

  it('reads a failed import as its reason, with no images', () => {
    const row = generatedImageRow(item({
      status: 'errored',
      meta: meta({ sourceItemId: 'img-1', provider: 'codex', error: 'path is outside the permitted directory' }),
    }));

    expect(row?.error).toBe('path is outside the permitted directory');
    expect(row?.images).toEqual([]);
  });

  // The discriminator is the meta key, never the row's text: a prose row whose
  // summary reads like a caption stays prose.
  it('is null for an ordinary assistant row', () => {
    expect(generatedImageRow(item({ meta: '' }))).toBeNull();
    expect(generatedImageRow(item({ meta: '{}' }))).toBeNull();
    expect(generatedImageRow(item({ meta: 'not json' }))).toBeNull();
    expect(generatedImageRow(item({ meta: JSON.stringify({ attachments: [attachment] }) }))).toBeNull();
  });

  it('is null when generatedImage is not an object', () => {
    for (const value of ['yes', 4, true, null, ['img-1']]) {
      expect(generatedImageRow(item({ meta: meta(value, [attachment]) }))).toBeNull();
    }
  });

  // Neither bytes nor a reason has nothing to show; the ordinary renderer
  // handles it rather than an empty frame.
  it('is null with neither an image nor an error', () => {
    expect(generatedImageRow(item({ meta: meta({ sourceItemId: 'img-1' }) }))).toBeNull();
    expect(generatedImageRow(item({ meta: meta({ sourceItemId: 'img-1' }, []) }))).toBeNull();
  });

  it('is null for any kind other than assistant_text', () => {
    const full = meta({ sourceItemId: 'img-1' }, [attachment]);
    for (const kind of ['user_text', 'tool_call', 'tool_completion', 'thinking']) {
      expect(generatedImageRow(item({ kind, meta: full }))).toBeNull();
    }
  });

  // A non-image attachment cannot be rendered as a picture, so it is not one.
  it('ignores an attachment that is not a renderable image', () => {
    expect(generatedImageRow(item({
      meta: meta({ sourceItemId: 'img-1' }, [
        { ...attachment, kind: 'file', filename: 'report.pdf', mimeType: 'application/pdf' },
      ]),
    }))).toBeNull();
  });

  it('trims stray whitespace out of the provenance strings', () => {
    const row = generatedImageRow(item({
      meta: meta({ sourceItemId: '  img-1  ', prompt: '  a prompt \n' }, [attachment]),
    }));
    expect(row?.sourceItemId).toBe('img-1');
    expect(row?.prompt).toBe('a prompt');
  });
});
