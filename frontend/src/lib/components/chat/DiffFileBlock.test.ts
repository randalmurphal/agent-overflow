// DiffFileBlock is the unified per-file inline diff renderer used by
// both Claude (single-file tool calls) and Codex (multi-file
// apply_patch). Tests cover the header contract, the always-inline
// body render, the capped-file preview fallback, and the sidebar promote
// affordances.

import { describe, it, expect, vi, beforeEach } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import DiffFileBlock from './DiffFileBlock.svelte';
import type { PatchFile, PatchLine } from '../../utils/patchFiles';
import type { ThreadPane } from '../../stores/thread.svelte';
import { setBindingMock } from '../../../test/mocks/bindings-app';

function ctx(content: string): PatchLine {
  return { type: 'context', content: ' ' + content };
}
function add(content: string): PatchLine {
  return { type: 'add', content: '+' + content };
}
function del(content: string): PatchLine {
  return { type: 'del', content: '-' + content };
}
function meta(content: string): PatchLine {
  return { type: 'meta', content };
}

function makePatchFile(overrides: Partial<PatchFile> = {}): PatchFile {
  return {
    path: 'src/foo.ts',
    kind: 'modified',
    additions: 1,
    deletions: 1,
    lines: [
      meta('diff --git a/src/foo.ts b/src/foo.ts'),
      meta('--- a/src/foo.ts'),
      meta('+++ b/src/foo.ts'),
      meta('@@ -1,2 +1,2 @@'),
      ctx('const x = 1;'),
      del('const y = 2;'),
      add('const y = 3;'),
    ],
    ...overrides,
  };
}

function makeLongPatchFile(contextLineCount: number, path = 'src/big.ts'): PatchFile {
  const lines: PatchLine[] = [
    meta(`diff --git a/${path} b/${path}`),
    meta(`--- a/${path}`),
    meta(`+++ b/${path}`),
    meta(`@@ -1,${contextLineCount} +1,${contextLineCount} @@`),
  ];
  for (let i = 0; i < contextLineCount; i += 1) {
    lines.push(ctx(`line ${i + 1};`));
  }
  return {
    path,
    kind: 'modified',
    additions: 0,
    deletions: 0,
    lines,
  };
}

function makeRenamedPatchFile(): PatchFile {
  return {
    path: 'src/new.ts',
    kind: 'renamed',
    additions: 1,
    deletions: 1,
    lines: [
      meta('diff --git a/src/old.ts b/src/new.ts'),
      meta('rename from src/old.ts'),
      meta('rename to src/new.ts'),
      meta('--- a/src/old.ts'),
      meta('+++ b/src/new.ts'),
      meta('@@ -1,1 +1,1 @@'),
      del('old;'),
      add('new;'),
    ],
  };
}

function makeMultiHunkPatchFile(): PatchFile {
  return {
    path: 'src/two.ts',
    kind: 'modified',
    additions: 2,
    deletions: 0,
    lines: [
      meta('diff --git a/src/two.ts b/src/two.ts'),
      meta('--- a/src/two.ts'),
      meta('+++ b/src/two.ts'),
      meta('@@ -1,1 +1,2 @@'),
      ctx('first;'),
      add('inserted;'),
      meta('@@ -10,1 +11,2 @@'),
      ctx('tenth;'),
      add('next;'),
    ],
  };
}

function fakePane(openDiffSidebar = vi.fn()): Partial<ThreadPane> {
  return {
    openDiffSidebar,
    thread: { id: 'thread-1', workspacePath: '/tmp/workspace' } as ThreadPane['thread'],
  } as Partial<ThreadPane>;
}

describe('<DiffFileBlock>', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });

  it('renders lowercase tool label, path, and +/- counts in the header', () => {
    const file = makePatchFile();
    const { getByTestId } = render(DiffFileBlock, {
      props: { file, threadId: 'thread-1', toolName: 'Edit' },
    });

    expect(getByTestId('diff-file-label').textContent).toBe('edit');
    expect(getByTestId('diff-file-path').textContent).toBe('src/foo.ts');
    const counts = getByTestId('diff-file-counts').textContent ?? '';
    expect(counts).toContain('+1');
    expect(counts).toContain('-1');
  });

  it('falls back to a generic diff label when no toolName is provided', () => {
    const file = makePatchFile();
    const { getByTestId } = render(DiffFileBlock, {
      props: { file, threadId: 'thread-1' },
    });
    expect(getByTestId('diff-file-label').textContent).toBe('diff');
  });

  it('renders the diff body inline by default (no expand needed)', () => {
    const file = makePatchFile();
    const { getByTestId } = render(DiffFileBlock, {
      props: { file, threadId: 'thread-1' },
    });

    // The body region exists and contains the change lines. We don't
    // assert on Shiki tokens (they land asynchronously); just on the
    // raw text content.
    const body = getByTestId('diff-file-body');
    expect(body.className).not.toContain('whitespace-pre');
    expect(body.textContent).toContain('const x = 1;');
    expect(body.textContent).toContain('const y = 2;');
    expect(body.textContent).toContain('const y = 3;');

    const rows = Array.from(body.children);
    expect(rows).toHaveLength(3);
    expect(rows.every((row) => row.className.includes('whitespace-pre'))).toBe(true);
    expect(rows.map((row) => row.textContent)).toEqual([
      '1 const x = 1;',
      '2-const y = 2;',
      '2+const y = 3;',
    ]);
  });

  it('renders a hunk separator between hunks within one file', () => {
    const file = makeMultiHunkPatchFile();
    const { getAllByTestId } = render(DiffFileBlock, {
      props: { file, threadId: 'thread-1' },
    });

    // Two hunks → one separator (between them; the first hunk's
    // `@@` line is dropped silently).
    const separators = getAllByTestId('diff-file-hunk-separator');
    expect(separators).toHaveLength(1);
  });

  it('shows old → new in the header for renamed files', () => {
    const file = makeRenamedPatchFile();
    const { getByTestId } = render(DiffFileBlock, {
      props: { file, threadId: 'thread-1' },
    });
    expect(getByTestId('diff-file-path').textContent).toBe('src/old.ts → src/new.ts');
  });

  it('renders the body without scroll containers (no max-height, no overflow scroll)', () => {
    const file = makePatchFile();
    const { getByTestId } = render(DiffFileBlock, {
      props: { file, threadId: 'thread-1' },
    });
    const body = getByTestId('diff-file-body');
    const cls = body.className;
    expect(cls).not.toMatch(/max-h/);
    expect(cls).not.toMatch(/overflow-(auto|scroll|y-auto|y-scroll)/);
  });

  it('renders the full body when the file is at the inline preview cap', () => {
    const file = makeLongPatchFile(15);
    const { getByTestId, queryByTestId } = render(DiffFileBlock, {
      props: { file, threadId: 'thread-1' },
    });
    const bodyText = getByTestId('diff-file-body').textContent ?? '';
    expect(bodyText).toContain('line 15;');
    expect(queryByTestId('diff-file-fade')).toBeNull();
    expect(queryByTestId('diff-file-show-full')).toBeNull();
  });

  it('renders a fade + sidebar CTA when the file exceeds the inline preview cap', async () => {
    const open = vi.fn();
    const pane = fakePane(open) as ThreadPane;
    const file = makeLongPatchFile(16);
    const { getByTestId } = render(DiffFileBlock, {
      props: { pane, file, payloadId: 'p-long', threadId: 'thread-1' },
    });
    expect(getByTestId('diff-file-fade')).toBeInTheDocument();
    const cta = getByTestId('diff-file-show-full');
    expect(cta).toBeInTheDocument();
    expect(cta.textContent ?? '').toContain('Show full diff in side panel');
    const bodyText = getByTestId('diff-file-body').textContent ?? '';
    expect(bodyText).toContain('line 15;');
    expect(bodyText).not.toContain('line 16;');
    await fireEvent.click(cta);
    expect(open).toHaveBeenCalledWith({ payloadId: 'p-long', filePath: 'src/big.ts' });
  });

  it('renders header-only when file lines are empty (loading / summary-only)', () => {
    const file: PatchFile = {
      path: 'src/loading.ts',
      kind: 'modified',
      additions: 0,
      deletions: 0,
      lines: [],
    };
    const { queryByTestId, getByTestId } = render(DiffFileBlock, {
      props: { file, threadId: 'thread-1', toolName: 'Edit' },
    });
    // Header still renders (stable outer shell across the upgrade).
    expect(getByTestId('diff-file-label')).toBeInTheDocument();
    expect(getByTestId('diff-file-path')).toBeInTheDocument();
    // No body, fade, or CTA — header-only state until lines arrive.
    expect(queryByTestId('diff-file-body')).toBeNull();
    expect(queryByTestId('diff-file-fade')).toBeNull();
    expect(queryByTestId('diff-file-show-full')).toBeNull();
  });

  it('hides the sidebar trigger and CTA when no pane is provided', () => {
    const file = makePatchFile();
    const { queryByTestId } = render(DiffFileBlock, {
      props: { file, threadId: 'thread-1' },
    });
    expect(queryByTestId('diff-file-open-sidebar')).toBeNull();
  });

  it('does not render an inert long-file CTA when sidebar promotion is unavailable', () => {
    const file = makeLongPatchFile(16);
    const { queryByTestId } = render(DiffFileBlock, {
      props: { file, threadId: 'thread-1' },
    });
    expect(queryByTestId('diff-file-show-full')).toBeNull();
  });

  it('clicking the sidebar trigger calls pane.openDiffSidebar with payloadId + filePath', async () => {
    const open = vi.fn();
    const pane = fakePane(open) as ThreadPane;
    const file = makePatchFile();
    const { getByTestId } = render(DiffFileBlock, {
      props: { pane, file, payloadId: 'p-1', threadId: 'thread-1' },
    });
    await fireEvent.click(getByTestId('diff-file-open-sidebar'));
    expect(open).toHaveBeenCalledWith({ payloadId: 'p-1', filePath: 'src/foo.ts' });
  });

  it('clicking the long-file CTA calls pane.openDiffSidebar with payloadId + filePath', async () => {
    const open = vi.fn();
    const pane = fakePane(open) as ThreadPane;
    const file = makeLongPatchFile(500, 'src/long.ts');
    const { getByTestId } = render(DiffFileBlock, {
      props: { pane, file, payloadId: 'p-2', threadId: 'thread-1' },
    });
    await fireEvent.click(getByTestId('diff-file-show-full'));
    expect(open).toHaveBeenCalledWith({ payloadId: 'p-2', filePath: 'src/long.ts' });
  });

  it('mod-click on the header promotes to sidebar', async () => {
    const open = vi.fn();
    const pane = fakePane(open) as ThreadPane;
    const file = makePatchFile();
    const { getByTestId } = render(DiffFileBlock, {
      props: { pane, file, payloadId: 'p-3', threadId: 'thread-1' },
    });
    await fireEvent.click(getByTestId('diff-file-header'), { metaKey: true });
    expect(open).toHaveBeenCalledWith({ payloadId: 'p-3', filePath: 'src/foo.ts' });
  });

  it('plain click on the header does NOT promote (mod-only contract)', async () => {
    const open = vi.fn();
    const pane = fakePane(open) as ThreadPane;
    const file = makePatchFile();
    const { getByTestId } = render(DiffFileBlock, {
      props: { pane, file, payloadId: 'p-4', threadId: 'thread-1' },
    });
    await fireEvent.click(getByTestId('diff-file-header'));
    expect(open).not.toHaveBeenCalled();
  });

  it('renders the file path as the editor link without the pen icon', () => {
    const file = makePatchFile();
    const { getByTestId, queryByTestId } = render(DiffFileBlock, {
      props: { file, threadId: 'thread-1', workspacePath: '/tmp/workspace' },
    });
    const path = getByTestId('diff-file-path');
    const editorLink = getByTestId('editor-link');
    expect(path.textContent).toBe('src/foo.ts');
    expect(editorLink.textContent).toBe('src/foo.ts');
    expect(editorLink.getAttribute('aria-label') ?? '').toContain('src/foo.ts');
    expect(queryByTestId('editor-link-icon')).toBeNull();
  });

  it('clicking the file path opens the file in the editor without promoting to sidebar', async () => {
    const openSidebar = vi.fn();
    const openEditor = setBindingMock('OpenInEditor', vi.fn(async () => undefined));
    const pane = fakePane(openSidebar) as ThreadPane;
    const file = makePatchFile();
    const { getByTestId } = render(DiffFileBlock, {
      props: {
        pane,
        file,
        payloadId: 'p-file',
        threadId: 'thread-1',
        workspacePath: '/tmp/workspace',
      },
    });

    await fireEvent.click(getByTestId('editor-link'));

    await waitFor(() => {
      expect(openEditor).toHaveBeenCalledTimes(1);
    });
    expect(openEditor.mock.calls[0]).toEqual(['src/foo.ts', 0, 0, '/tmp/workspace']);
    expect(openSidebar).not.toHaveBeenCalled();
  });

  it('renders the body with line-tint backgrounds even before tokens land', () => {
    const file = makePatchFile();
    const { getByTestId } = render(DiffFileBlock, {
      props: { file, threadId: 'thread-1' },
    });
    const body = getByTestId('diff-file-body');
    // The line-tint classes are applied per-row regardless of
    // tokenization status — that's the "always usable" pre-render
    // pattern documented in DiffSidebarFile.
    expect(body.querySelectorAll('.bg-success\\/20').length).toBeGreaterThan(0);
    expect(body.querySelectorAll('.bg-error\\/20').length).toBeGreaterThan(0);
  });

  it('keeps the outer shell stable when lines are empty (loading state)', () => {
    const file: PatchFile = {
      path: 'src/loading.ts',
      kind: 'modified',
      additions: 0,
      deletions: 0,
      lines: [],
    };
    const { getByTestId, queryByTestId } = render(DiffFileBlock, {
      props: { file, threadId: 'thread-1', toolName: 'Edit' },
    });
    // Outer shell: header is always rendered with the same testids,
    // regardless of body presence. This pins the "stable transcript
    // rows" invariant: header structure does not change on the
    // summary→exact upgrade.
    expect(getByTestId('diff-file-block')).toBeInTheDocument();
    expect(getByTestId('diff-file-header')).toBeInTheDocument();
    expect(getByTestId('diff-file-label').textContent).toBe('edit');
    expect(getByTestId('diff-file-path').textContent).toBe('src/loading.ts');
    // Body region absent until lines arrive.
    expect(queryByTestId('diff-file-body')).toBeNull();
  });
});
