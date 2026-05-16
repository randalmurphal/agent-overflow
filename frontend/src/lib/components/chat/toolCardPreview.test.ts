import { describe, expect, it } from 'vitest';
import type { Item } from '../../types/models';
import { decodeToolCardPreview, toolCardInputPreview } from './toolCardPreview';

function makeItem(overrides: Partial<Item>): Item {
  return {
    id: 'i1',
    threadId: 't1',
    turnIndex: 0,
    itemIndex: 0,
    kind: 'tool_call',
    role: 'assistant',
    status: 'completed',
    summary: '',
    createdAt: 0,
    updatedAt: 0,
    ...overrides,
  };
}

describe('toolCardInputPreview', () => {
  it('falls back to the raw custom tool name without storing it in the classification', () => {
    const item = makeItem({ toolName: 'SomeCustomTool' });
    expect(toolCardInputPreview(item, null, null)).toBe('SomeCustomTool');
  });

  it('falls back to the MCP suffix for prefixed MCP tools', () => {
    const item = makeItem({ toolName: 'MCP/filesystem.read' });
    expect(toolCardInputPreview(item, null, null)).toBe('filesystem.read');
  });

  it('falls back to MCP tool for bare and empty-suffix MCP names', () => {
    expect(toolCardInputPreview(makeItem({ toolName: 'MCP' }), null, null)).toBe('MCP tool');
    expect(toolCardInputPreview(makeItem({ toolName: 'MCP/' }), null, null)).toBe('MCP tool');
  });

  it('preserves leading whitespace in MCP fallback suffixes so they do not become path links', () => {
    const item = makeItem({ toolName: 'MCP/ /tmp/foo.ts' });
    const preview = toolCardInputPreview(item, null, null);
    expect(preview).toBe(' /tmp/foo.ts');
    expect(decodeToolCardPreview(preview).path).toBeUndefined();
  });

  it('keeps command fallback text human-readable after category-label trimming', () => {
    const item = makeItem({ toolName: 'Bash' });
    expect(toolCardInputPreview(item, null, null)).toBe('Bash');
  });
});

describe('decodeToolCardPreview', () => {
  it('returns plain text when the input has no path', () => {
    expect(decodeToolCardPreview('Waiting on agents')).toEqual({
      text: 'Waiting on agents',
    });
  });

  it('returns plain text for empty input', () => {
    expect(decodeToolCardPreview('')).toEqual({ text: '' });
  });

  it('extracts a leading path with line + col', () => {
    const decoded = decodeToolCardPreview('src/lib/foo.ts:12:7 read OK');
    expect(decoded.text).toBe('src/lib/foo.ts:12:7 read OK');
    expect(decoded.path).toEqual({ path: 'src/lib/foo.ts', line: 12, col: 7 });
  });

  it('extracts a leading path with only a line', () => {
    const decoded = decodeToolCardPreview('src/lib/foo.ts:12');
    expect(decoded.path).toEqual({ path: 'src/lib/foo.ts', line: 12, col: undefined });
  });

  it('extracts a bare leading path', () => {
    const decoded = decodeToolCardPreview('src/lib/foo.ts');
    expect(decoded.path).toEqual({ path: 'src/lib/foo.ts', line: undefined, col: undefined });
  });

  it('does not extract paths that appear mid-string', () => {
    const decoded = decodeToolCardPreview('Wrote 3 files (a.ts, b.ts, src/c.ts)');
    expect(decoded.path).toBeUndefined();
  });

  it('does not match URLs as paths', () => {
    const decoded = decodeToolCardPreview('https://example.com/foo.bar');
    expect(decoded.path).toBeUndefined();
  });
});
