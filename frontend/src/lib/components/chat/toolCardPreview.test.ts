import { describe, expect, it } from 'vitest';
import type { Item } from '../../types/models';
import {
  decodeToolCardPreview,
  presentToolCardInputPreview,
  toolCardInputPreview,
} from './toolCardPreview';

function makeItem(overrides: Partial<Item>): Item {
  return {
    rev: 0,
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
  it.each([
    ['the raw custom tool name', 'SomeCustomTool', 'SomeCustomTool'],
    ['the MCP suffix for prefixed MCP tools', 'MCP/filesystem.read', 'filesystem.read'],
    ['MCP tool for a bare MCP name', 'MCP', 'MCP tool'],
    ['MCP tool for an empty-suffix MCP name', 'MCP/', 'MCP tool'],
    // Command fallback text stays human-readable after category-label trimming.
    ['the command name for Bash', 'Bash', 'Bash'],
  ])('falls back to %s', (_name, toolName, expected) => {
    expect(toolCardInputPreview(makeItem({ toolName }), null, null)).toBe(expected);
  });

  it('preserves leading whitespace in MCP fallback suffixes so they do not become path links', () => {
    const item = makeItem({ toolName: 'MCP/ /tmp/foo.ts' });
    const preview = toolCardInputPreview(item, null, null);
    expect(preview).toBe(' /tmp/foo.ts');
    expect(decodeToolCardPreview(preview).path).toBeUndefined();
  });

  describe('MCP body synthesis', () => {
    // The redesigned MCP row composes its body as `server.tool(args)`
    // from `meta.mcp` + `meta.input`, regardless of the item's
    // `summary` (which still carries the triage-built fallback like
    // `MCP/lookup: query`). The synthesizer must win over the
    // summary; otherwise the row would display the raw fallback
    // instead of the spec-specified shape.

    it.each([
      ['composes server.tool(args) from meta.mcp + meta.input', 'docs', 'lookup', 'MCP/lookup: query',
        { q: 'wails' }, 'docs.lookup(q="wails")'],
      ['renders multi-arg dicts as a compact key=value, key=value list', 'playwright', 'browser_click', '',
        { selector: '#submit', force: true, retries: 2 }, 'playwright.browser_click(selector="#submit", force=true, retries=2)'],
      ['renders empty parens when the call took no arguments', 'docs', 'ping', '', {}, 'docs.ping()'],
    ])('%s', (_name, server, tool, summary, input, expected) => {
      const item = makeItem({ toolName: `MCP/${tool}`, summary });
      expect(toolCardInputPreview(item, null, { mcp: { server, tool }, input })).toBe(expected);
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

  describe('Skill preview', () => {
    it('reads meta.input.skill as the inline preview', () => {
      const item = makeItem({ toolName: 'Skill', summary: 'Skill' });
      const itemMeta = { input: { skill: 'code-review' } };
      expect(toolCardInputPreview(item, null, itemMeta)).toBe('code-review');
    });

    it('falls back to summary when meta.input is absent', () => {
      const item = makeItem({ toolName: 'Skill', summary: 'Skill' });
      expect(toolCardInputPreview(item, null, null)).toBe('Skill');
    });

    it('does not activate for non-Skill tools with a skill key in meta', () => {
      const item = makeItem({ toolName: 'Bash', summary: 'Bash: echo hi' });
      const itemMeta = { input: { skill: 'code-review' } };
      expect(toolCardInputPreview(item, null, itemMeta)).toBe('Bash: echo hi');
    });
  });
});

describe('presentToolCardInputPreview', () => {
  it('prefers structured file_path metadata over a truncated read summary', () => {
    const item = makeItem({
      toolName: 'Read',
      summary: 'Read: frontend/src/lib/components/composer/some/deeply/nested/Compose…',
      meta: JSON.stringify({
        input: {
          file_path: '/home/me/repo/frontend/src/lib/components/composer/some/deeply/nested/Composer.svelte',
        },
      }),
    });
    const result = presentToolCardInputPreview(item, null, JSON.parse(item.meta!), '/home/me/repo');
    expect(result.text).toBe('Composer.svelte');
    expect(result.path).toEqual({
      path: 'frontend/src/lib/components/composer/some/deeply/nested/Composer.svelte',
      line: undefined,
      col: undefined,
    });
  });

  it('uses structured notebook_path metadata for notebook edits', () => {
    const item = makeItem({
      toolName: 'NotebookEdit',
      summary: 'NotebookEdit: /home/me/repo/notebooks/analysis.ipynb',
      meta: JSON.stringify({
        input: {
          notebook_path: '/home/me/repo/notebooks/analysis.ipynb',
        },
      }),
    });
    const result = presentToolCardInputPreview(item, null, JSON.parse(item.meta!), '/home/me/repo');
    expect(result.text).toBe('analysis.ipynb');
    expect(result.path?.path).toBe('notebooks/analysis.ipynb');
  });

  it('strips the redundant `<toolName>: ` prefix triage embeds in the summary', () => {
    const item = makeItem({ toolName: 'Read', summary: 'Read: foo.go' });
    expect(presentToolCardInputPreview(item, null, null, '').text).toBe('foo.go');
  });

  it('collapses the displayed text to the basename while keeping a workspace-relative click target', () => {
    const item = makeItem({
      toolName: 'Read',
      summary: 'Read: /home/me/repo/src/foo.ts',
    });
    const result = presentToolCardInputPreview(item, null, null, '/home/me/repo');
    expect(result.text).toBe('foo.ts');
    expect(result.path).toEqual({
      path: 'src/foo.ts',
      line: undefined,
      col: undefined,
    });
  });

  it('preserves trailing line/col on the displayed basename and the click target', () => {
    // The relativized form is what EditorLink targets; EditorLink's
    // workspacePath prop joins it back to absolute when invoking
    // OpenInEditor, so the click target still resolves correctly.
    // The displayed text shows just the basename so a row in a long
    // directory tree stays compact.
    const item = makeItem({
      toolName: 'Read',
      summary: 'Read: /home/me/repo/src/foo.ts:42:7',
    });
    const result = presentToolCardInputPreview(item, null, null, '/home/me/repo');
    expect(result.text).toBe('foo.ts:42:7');
    expect(result.path).toEqual({ path: 'src/foo.ts', line: 42, col: 7 });
  });

  it('displays paths outside the workspace fully while preserving the absolute click target', () => {
    // The relativizer leaves a non-workspace path alone, but the
    // display label must keep the full absolute path so outside-repo
    // reads are visibly distinct from project files with the same name.
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

  it('shows absolute paths fully when workspacePath is empty', () => {
    // No workspace means an absolute path cannot be proven repo-local,
    // so the UI must show the full path.
    const item = makeItem({
      toolName: 'Read',
      summary: 'Read: /home/me/repo/test.go',
    });
    const result = presentToolCardInputPreview(item, null, null, '');
    expect(result.text).toBe('/home/me/repo/test.go');
    expect(result.path).toEqual({
      path: '/home/me/repo/test.go',
      line: undefined,
      col: undefined,
    });
  });

  it('refuses to strip the workspace root with no separator following', () => {
    // Bare-root strip would yield an empty preview. Keep the original
    // text instead so the row has something to render.
    const item = makeItem({ toolName: 'Bash', summary: 'Bash: /home/me/repo' });
    expect(presentToolCardInputPreview(item, null, null, '/home/me/repo').text).toBe(
      '/home/me/repo',
    );
  });

  it('does not over-strip the click target when the text starts with a workspace-lookalike prefix', () => {
    // `/home/me/repository` shares a prefix with `/home/me/repo`. The
    // relativizer must check for a path separator at the root boundary,
    // not just a startsWith match, or it would mangle paths under a
    // similarly-named sibling repo. The displayed text and `path` both
    // stay absolute so the row does not hide that the read is outside
    // the workspace.
    const item = makeItem({
      toolName: 'Read',
      summary: 'Read: /home/me/repository/foo.go',
    });
    const result = presentToolCardInputPreview(item, null, null, '/home/me/repo');
    expect(result.text).toBe('/home/me/repository/foo.go');
    expect(result.path).toEqual({
      path: '/home/me/repository/foo.go',
      line: undefined,
      col: undefined,
    });
  });

  it.each([
    ['shows absolute paths fully when lexical normalization escapes the workspace',
      '/home/me/repo/../outside/secret.txt', '/home/me/repo/../outside/secret.txt'],
    ['cleans repo-local dot segments before displaying and opening', '/home/me/repo/src/../events.ts', 'events.ts'],
    ['shows relative paths fully when dot segments escape the workspace', '../outside/secret.txt', '../outside/secret.txt'],
    ['cleans repo-local relative dot segments before displaying and opening', 'src/../events.ts', 'events.ts'],
  ])('%s', (_name, filePath, expected) => {
    const meta = { input: { file_path: filePath } };
    const item = makeItem({ toolName: 'Read', summary: `Read: ${filePath}`, meta: JSON.stringify(meta) });
    const result = presentToolCardInputPreview(item, null, meta, '/home/me/repo');
    expect(result.text).toBe(expected);
    expect(result.path?.path).toBe(expected);
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

  describe('ToolSearch preview', () => {
    // Claude Code 2.1.150+ deferred-tool schema loader. Without the
    // dedicated preview branch, the body ends up displaying the raw
    // `select:Foo` query string as if it were a tool name, which
    // looks like a meaningless call to the user.
    it.each([
      ['renames a select: query into a "Loaded schema: <names>" body',
        { query: 'select:TaskCreate', max_results: 1 }, 'Loaded schema: TaskCreate'],
      ['lists multiple comma-separated names in the select: body',
        { query: 'select:TaskCreate,TaskUpdate,TaskList', max_results: 6 }, 'Loaded schema: TaskCreate, TaskUpdate, TaskList'],
      ['renders a keyword query verbatim', { query: 'notebook jupyter', max_results: 5 }, 'notebook jupyter'],
      ['handles select: with no names by returning "Loaded schema"', { query: 'select:' }, 'Loaded schema'],
    ])('%s', (_name, input, expected) => {
      const item = makeItem({ toolName: 'ToolSearch', summary: input.query });
      expect(toolCardInputPreview(item, null, { input })).toBe(expected);
    });

    it('falls back to the standard preview path when input.query is missing', () => {
      // A malformed ToolSearch shape lands in fallbackPreviewForToolName,
      // which returns the bare tool name. No crash, no silent empty
      // preview.
      const item = makeItem({ toolName: 'ToolSearch' });
      expect(toolCardInputPreview(item, null, null)).toBe('ToolSearch');
    });
  });

  it('presents an AO tool by its own argument, untouched by the path passes', () => {
    const item = makeItem({ toolName: 'MCP/remote_run', summary: 'MCP/remote_run: ...' });
    const itemMeta = {
      mcp: { server: 'ao-remote-tools', tool: 'remote_run' },
      input: { computer_id: 'mac', argv: ['ls', '/workspace/src'] },
    };
    expect(toolCardInputPreview(item, null, itemMeta)).toBe('ls /workspace/src');
    expect(presentToolCardInputPreview(item, null, itemMeta, '/workspace')).toEqual({ text: 'ls /workspace/src' });
    const open = makeItem({ toolName: 'MCP/browser_open_file' });
    const openMeta = { mcp: { server: 'ao-browser-tools', tool: 'browser_open_file' }, input: { path: '/workspace/out/report.html' } };
    expect(presentToolCardInputPreview(open, null, openMeta, '/workspace')).toEqual({ text: '/workspace/out/report.html' });
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
  it.each([
    ['input with no path', 'Waiting on agents'],
    ['empty input', ''],
    ['a path that appears mid-string', 'Wrote 3 files (a.ts, b.ts, src/c.ts)'],
    ['a URL', 'https://example.com/foo.bar'],
  ])('returns plain text for %s', (_name, text) => {
    expect(decodeToolCardPreview(text)).toEqual({ text });
  });

  it.each([
    ['with line + col', 'src/lib/foo.ts:12:7 read OK', 12, 7],
    ['with only a line', 'src/lib/foo.ts:12', 12, undefined],
    ['bare', 'src/lib/foo.ts', undefined, undefined],
  ])('extracts a leading path %s', (_name, text, line, col) => {
    const decoded = decodeToolCardPreview(text);
    expect(decoded.text).toBe(text);
    expect(decoded.path).toEqual({ path: 'src/lib/foo.ts', line, col });
  });
});

describe('SendMessage preview', () => {
  it('names the recipient launch by its description when triage resolved one', () => {
    const item = makeItem({
      toolName: 'SendMessage',
      meta: JSON.stringify({
        input: { to: 'ab487a02304913d06', message: 'status?' },
        recipient_task_id: 'ab487a02304913d06',
        recipient_description: 'Frontend transitive suppression fix',
      }),
    });
    expect(toolCardInputPreview(item, null, JSON.parse(item.meta!))).toBe(
      'To Frontend transitive suppression fix',
    );
  });

  it('falls back to the recipient as typed when nothing was resolved', () => {
    const item = makeItem({
      toolName: 'SendMessage',
      meta: JSON.stringify({ input: { to: 'reviewer', message: 'status?' } }),
    });
    expect(toolCardInputPreview(item, null, JSON.parse(item.meta!))).toBe('To reviewer');
  });
});
