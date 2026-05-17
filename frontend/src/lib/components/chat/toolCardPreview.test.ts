import { describe, expect, it } from 'vitest';
import type { Item } from '../../types/models';
import {
  decodeToolCardPreview,
  presentToolCardInputPreview,
  toolCardInputPreview,
} from './toolCardPreview';

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

describe('presentToolCardInputPreview', () => {
  it('strips the redundant `<toolName>: ` prefix triage embeds in the summary', () => {
    const item = makeItem({ toolName: 'Read', summary: 'Read: foo.go' });
    expect(presentToolCardInputPreview(item, null, null, '').text).toBe('foo.go');
  });

  it('relativizes a leading workspace-rooted absolute path', () => {
    const item = makeItem({
      toolName: 'Read',
      summary: 'Read: /home/me/repo/src/foo.ts',
    });
    const result = presentToolCardInputPreview(item, null, null, '/home/me/repo');
    expect(result.text).toBe('src/foo.ts');
    expect(result.path).toEqual({
      path: 'src/foo.ts',
      line: undefined,
      col: undefined,
    });
  });

  it('relativizes the EditorLink path in sync with the displayed text', () => {
    // The relativized form is what EditorLink renders; EditorLink's
    // workspacePath prop joins it back to absolute when invoking
    // OpenInEditor, so the click target still resolves correctly.
    const item = makeItem({
      toolName: 'Read',
      summary: 'Read: /home/me/repo/src/foo.ts:42:7',
    });
    const result = presentToolCardInputPreview(item, null, null, '/home/me/repo');
    expect(result.text).toBe('src/foo.ts:42:7');
    expect(result.path).toEqual({ path: 'src/foo.ts', line: 42, col: 7 });
  });

  it('passes paths outside the workspace through untouched', () => {
    const item = makeItem({
      toolName: 'Read',
      summary: 'Read: /usr/local/share/foo.go',
    });
    const result = presentToolCardInputPreview(item, null, null, '/home/me/repo');
    expect(result.text).toBe('/usr/local/share/foo.go');
    expect(result.path).toEqual({
      path: '/usr/local/share/foo.go',
      line: undefined,
      col: undefined,
    });
  });

  it('tolerates a trailing slash on workspacePath', () => {
    const item = makeItem({
      toolName: 'Read',
      summary: 'Read: /home/me/repo/test.go',
    });
    expect(presentToolCardInputPreview(item, null, null, '/home/me/repo/').text).toBe(
      'test.go',
    );
  });

  it('does not strip a workspace prefix that appears mid-string', () => {
    // The strip path must only fire on the leading token — otherwise
    // a `cd <workspace>/sub && ls` style Bash preview would have its
    // path argument silently mangled.
    const item = makeItem({
      toolName: 'Bash',
      summary: 'Bash: cd /home/me/repo/src && ls',
    });
    expect(presentToolCardInputPreview(item, null, null, '/home/me/repo').text).toBe(
      'cd /home/me/repo/src && ls',
    );
  });

  it('leaves the preview alone when workspacePath is empty', () => {
    const item = makeItem({
      toolName: 'Read',
      summary: 'Read: /home/me/repo/test.go',
    });
    expect(presentToolCardInputPreview(item, null, null, '').text).toBe(
      '/home/me/repo/test.go',
    );
  });

  it('refuses to strip the workspace root with no separator following', () => {
    // Bare-root strip would yield an empty preview. Keep the original
    // text instead so the row has something to render.
    const item = makeItem({ toolName: 'Bash', summary: 'Bash: /home/me/repo' });
    expect(presentToolCardInputPreview(item, null, null, '/home/me/repo').text).toBe(
      '/home/me/repo',
    );
  });

  it('does not over-strip when the text starts with a workspace-lookalike prefix', () => {
    // `/home/me/repository` shares a prefix with `/home/me/repo`. The
    // strip must check for a path separator at the root boundary, not
    // just a startsWith match, or it would mangle paths under a
    // similarly-named sibling repo.
    const item = makeItem({
      toolName: 'Read',
      summary: 'Read: /home/me/repository/foo.go',
    });
    expect(
      presentToolCardInputPreview(item, null, null, '/home/me/repo').text,
    ).toBe('/home/me/repository/foo.go');
  });

  it('strips Windows-style workspacePath + backslash separator', () => {
    const item = makeItem({
      toolName: 'Read',
      summary: 'Read: C:\\Users\\me\\repo\\test.go',
    });
    expect(
      presentToolCardInputPreview(item, null, null, 'C:\\Users\\me\\repo').text,
    ).toBe('test.go');
  });

  it('passes the wait_agent synthesizer output through unchanged', () => {
    // wait_agent takes the early-return synthesizer path inside
    // toolCardInputPreview, so its preview never starts with a
    // `<toolName>: ` prefix or a leading absolute path. The presenter
    // must be a passthrough here.
    const item = makeItem({ toolName: 'wait_agent', status: 'running' });
    expect(presentToolCardInputPreview(item, null, null, '/home/me/repo').text).toBe(
      'Waiting on agents',
    );
  });

  it('strips the MCP/<tool>: prefix on legacy MCP rows that fall through to the summary', () => {
    // Pre-redesign Items don't carry meta.mcp, so the synthesizer
    // declines and the preview falls back to item.summary. That
    // summary is `MCP/<tool>: <args>`; the icon already conveys
    // "MCP", so stripping is an improvement here too.
    const item = makeItem({ toolName: 'MCP/lookup', summary: 'MCP/lookup: query' });
    expect(presentToolCardInputPreview(item, null, null, '').text).toBe('query');
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
