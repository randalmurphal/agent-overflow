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

  describe('MCP body synthesis', () => {
    // The redesigned MCP row composes its body as `server.tool(args)`
    // from `meta.mcp` + `meta.input`, regardless of the item's
    // `summary` (which still carries the triage-built fallback like
    // `MCP/lookup: query`). The synthesizer must win over the
    // summary; otherwise the row would display the raw fallback
    // instead of the spec-specified shape.

    it('composes server.tool(args) from meta.mcp + meta.input', () => {
      const item = makeItem({
        toolName: 'MCP/lookup',
        summary: 'MCP/lookup: query',
      });
      const itemMeta = {
        mcp: { server: 'docs', tool: 'lookup' },
        input: { q: 'wails' },
      };
      expect(toolCardInputPreview(item, null, itemMeta)).toBe('docs.lookup(q="wails")');
    });

    it('renders multi-arg dicts as a compact key=value, key=value list', () => {
      const item = makeItem({ toolName: 'MCP/browser_click' });
      const itemMeta = {
        mcp: { server: 'playwright', tool: 'browser_click' },
        input: { selector: '#submit', force: true, retries: 2 },
      };
      expect(toolCardInputPreview(item, null, itemMeta)).toBe(
        'playwright.browser_click(selector="#submit", force=true, retries=2)',
      );
    });

    it('renders empty parens when the call took no arguments', () => {
      const item = makeItem({ toolName: 'MCP/ping' });
      const itemMeta = { mcp: { server: 'docs', tool: 'ping' }, input: {} };
      expect(toolCardInputPreview(item, null, itemMeta)).toBe('docs.ping()');
    });

    it('drops to the summary fallback when meta.mcp is absent', () => {
      // Both legacy MCP rows (pre-redesign Items without meta.mcp) and
      // arbitrary non-MCP rows should sail through to the summary
      // branch — the synthesizer is opt-in via the wire-typed
      // metadata, not a substring-based heuristic on the tool name.
      const item = makeItem({
        toolName: 'MCP/lookup',
        summary: 'MCP/lookup: query',
      });
      expect(toolCardInputPreview(item, null, null)).toBe('MCP/lookup: query');
      expect(toolCardInputPreview(item, null, { input: { q: 'wails' } })).toBe(
        'MCP/lookup: query',
      );
    });
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
