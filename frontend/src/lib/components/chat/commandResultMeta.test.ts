import { describe, expect, it } from 'vitest';
import { makeItem } from '../../../test/helpers/chat';
import { readCommandResultView } from './commandResultMeta';

function commandResultItem(meta: unknown, overrides = {}) {
  return makeItem({
    kind: 'command_result',
    role: 'system',
    status: 'completed',
    summary: 'summary text',
    meta: typeof meta === 'string' ? meta : JSON.stringify(meta),
    ...overrides,
  });
}

describe('readCommandResultView', () => {
  it('treats an untruncated preview as the whole output', () => {
    const view = readCommandResultView(
      commandResultItem({ kind: 'command_result', preview: 'Current session\n  Tokens: 12' }),
    );
    expect(view).toEqual({
      preview: 'Current session\n  Tokens: 12',
      truncated: false,
      totalBytes: 0,
      agentResult: null,
    });
  });

  it('reports truncation and size when a payload backs the rest', () => {
    const view = readCommandResultView(
      commandResultItem(
        { kind: 'command_result', preview: 'head', truncated: true, totalBytes: 12_000 },
        { payloadId: 'payload-1' },
      ),
    );
    expect(view).toEqual({ preview: 'head', truncated: true, totalBytes: 12_000, agentResult: null });
  });

  it('refuses to claim truncation when no payload is linked', () => {
    // The affordance the flag drives is a fetch. Without a payload id that
    // fetch cannot resolve, so the row must render inline-only rather than
    // offer a button that always errors.
    const view = readCommandResultView(
      commandResultItem({ kind: 'command_result', preview: 'head', truncated: true, totalBytes: 9 }),
    );
    expect(view.truncated).toBe(false);
    expect(view.totalBytes).toBe(0);
  });

  it('drops a non-numeric or non-positive totalBytes', () => {
    const bad = readCommandResultView(
      commandResultItem(
        { kind: 'command_result', preview: 'head', truncated: true, totalBytes: '12000' },
        { payloadId: 'payload-1' },
      ),
    );
    expect(bad.totalBytes).toBe(0);

    const zero = readCommandResultView(
      commandResultItem(
        { kind: 'command_result', preview: 'head', truncated: true, totalBytes: 0 },
        { payloadId: 'payload-1' },
      ),
    );
    expect(zero.totalBytes).toBe(0);
  });

  it('falls back to the row summary when meta is absent or unparseable', () => {
    const missing = readCommandResultView(
      makeItem({ kind: 'command_result', summary: 'plain summary', meta: undefined }),
    );
    expect(missing.preview).toBe('plain summary');
    expect(missing.truncated).toBe(false);

    const garbage = readCommandResultView(commandResultItem('{not json'));
    expect(garbage.preview).toBe('summary text');
    expect(garbage.truncated).toBe(false);
  });

  it('falls back to the summary when preview is present but empty', () => {
    const view = readCommandResultView(
      commandResultItem({ kind: 'command_result', preview: '' }),
    );
    expect(view.preview).toBe('summary text');
  });

  it('returns a complete forked-agent source and rejects partial source metadata', () => {
    const sourced = readCommandResultView(commandResultItem({
      kind: 'command_result',
      preview: 'findings',
      agentResult: {
        launchId: 'claude-command:cmd-1',
        sourceKind: 'skill',
        sourceName: 'code-review',
      },
    }));
    expect(sourced.agentResult).toEqual({
      launchId: 'claude-command:cmd-1',
      sourceKind: 'skill',
      sourceName: 'code-review',
    });

    const partial = readCommandResultView(commandResultItem({
      kind: 'command_result', preview: 'findings',
      agentResult: { launchId: 'claude-command:cmd-1', sourceKind: 'skill' },
    }));
    expect(partial.agentResult).toBeNull();
  });
});
