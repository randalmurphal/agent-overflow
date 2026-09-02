// DiffFileBlock is the unified per-file inline diff renderer used by
// both Claude (single-file tool calls) and Codex (multi-file
// apply_patch). Tests cover the header contract, the expanded
// body render, the collapseDiffPreviews default + per-card disclosure
// toggle, the capped-file preview fallback, and the review promote
// affordances.

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { tick } from 'svelte';
import { fireEvent, render, waitFor, within } from '@testing-library/svelte';
import DiffFileBlock from './DiffFileBlock.svelte';
import { requestFileSpans } from '../../utils/diffSpanCache.svelte';
import type { PatchFile, PatchLine } from '../../utils/patchFiles';
import type { ThreadPane } from '../../stores/thread.svelte';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { loadSettings, resetSettingsForTest } from '../../stores/settings.svelte';
import { makeSettings } from '../../../test/helpers/settings';
import { buildPane, makeItem } from '../../../test/helpers/chat';
import { formatTimeOfDay } from '../../utils/format';
import { openReviewCompanion } from '../../stores/reviewPane.svelte';

// Mocked so the collapse tests can assert that collapsed cards skip
// the span request; line-tint rendering does not depend on it.
vi.mock('../../utils/diffSpanCache.svelte', () => ({
  requestFileSpans: vi.fn(async () => {}),
  getSpansForLine: vi.fn(() => null),
  diffSpanCacheGeneration: vi.fn(() => 0),
}));

// The subject derivation stays real: what the card hands the companion is
// part of what these tests assert.
vi.mock('../../stores/reviewPane.svelte', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../../stores/reviewPane.svelte')>()),
  openReviewCompanion: vi.fn(async () => null),
}));

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

const PANE_THREAD = {
  id: 'thread-1',
  projectId: 'project-1',
  workspacePath: '/tmp/workspace',
} as ThreadPane['thread'];

/** What `openReviewForItem` derives from `fakePane()`. */
const PANE_SUBJECT = {
  identity: 'thread-1',
  threadId: 'thread-1',
  workspace: { projectId: 'project-1', workspacePath: '/tmp/workspace' },
  thread: PANE_THREAD,
};

function fakePane(): Partial<ThreadPane> {
  return {
    paneId: 'pane-1',
    threadId: 'thread-1',
    thread: PANE_THREAD,
    workspace: { projectId: 'project-1', workspacePath: '/tmp/workspace' },
  } as Partial<ThreadPane>;
}

function expectBefore(left: Element, right: Element) {
  expect(left.compareDocumentPosition(right) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
}

describe('<DiffFileBlock>', () => {
  // The body-render cases are about what an EXPANDED card draws, so they
  // seed collapseDiffPreviews off rather than riding the shipped default
  // (which collapses). The collapse cases below seed it on for themselves.
  beforeEach(async () => {
    vi.restoreAllMocks();
    vi.mocked(openReviewCompanion).mockClear();
    resetSettingsForTest();
    setBindingMock('GetSettings', async () => makeSettings({ collapseDiffPreviews: false }));
    await loadSettings();
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

  it('renders the diff body inline with the setting off (no expand needed)', () => {
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

  it('renders a fade + review CTA when the file exceeds the inline preview cap', async () => {
    const pane = fakePane() as ThreadPane;
    const file = makeLongPatchFile(16);
    const { getByTestId } = render(DiffFileBlock, {
      props: { pane, file, payloadId: 'p-long', threadId: 'thread-1' },
    });
    expect(getByTestId('diff-file-fade')).toBeInTheDocument();
    const cta = getByTestId('diff-file-show-full');
    expect(cta).toBeInTheDocument();
    expect(cta.textContent ?? '').toContain('Open in review pane');
    const bodyText = getByTestId('diff-file-body').textContent ?? '';
    expect(bodyText).toContain('line 15;');
    expect(bodyText).not.toContain('line 16;');
    await fireEvent.click(cta);
    expect(openReviewCompanion).toHaveBeenCalledWith('pane-1', PANE_SUBJECT, {
      scope: 'workspace',
      filePath: 'src/big.ts',
    });
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

  it('hides the review trigger and CTA when no pane is provided', () => {
    const file = makePatchFile();
    const { queryByTestId } = render(DiffFileBlock, {
      props: { file, threadId: 'thread-1' },
    });
    expect(queryByTestId('diff-file-open-sidebar')).toBeNull();
  });

  it('does not render an inert long-file CTA when review promotion is unavailable', () => {
    const file = makeLongPatchFile(16);
    const { queryByTestId } = render(DiffFileBlock, {
      props: { file, threadId: 'thread-1' },
    });
    expect(queryByTestId('diff-file-show-full')).toBeNull();
  });

  it('clicking the review trigger opens workspace review with filePath', async () => {
    const pane = fakePane() as ThreadPane;
    const file = makePatchFile();
    const { getByTestId } = render(DiffFileBlock, {
      props: { pane, file, payloadId: 'p-1', threadId: 'thread-1' },
    });
    await fireEvent.click(getByTestId('diff-file-open-sidebar'));
    expect(openReviewCompanion).toHaveBeenCalledWith('pane-1', PANE_SUBJECT, {
      scope: 'workspace',
      filePath: 'src/foo.ts',
    });
  });

  it('clicking the long-file CTA opens review with filePath', async () => {
    const pane = fakePane() as ThreadPane;
    const file = makeLongPatchFile(500, 'src/long.ts');
    const { getByTestId } = render(DiffFileBlock, {
      props: { pane, file, payloadId: 'p-2', threadId: 'thread-1' },
    });
    await fireEvent.click(getByTestId('diff-file-show-full'));
    expect(openReviewCompanion).toHaveBeenCalledWith('pane-1', PANE_SUBJECT, {
      scope: 'workspace',
      filePath: 'src/long.ts',
    });
  });

  it('mod-click on the header promotes to review', async () => {
    const pane = fakePane() as ThreadPane;
    const file = makePatchFile();
    const { getByTestId } = render(DiffFileBlock, {
      props: { pane, file, payloadId: 'p-3', threadId: 'thread-1' },
    });
    await fireEvent.click(getByTestId('diff-file-header'), { metaKey: true });
    expect(openReviewCompanion).toHaveBeenCalledWith('pane-1', PANE_SUBJECT, {
      scope: 'workspace',
      filePath: 'src/foo.ts',
    });
  });

  it('plain click on the header does NOT promote (mod-only contract)', async () => {
    const pane = fakePane() as ThreadPane;
    const file = makePatchFile();
    const { getByTestId } = render(DiffFileBlock, {
      props: { pane, file, payloadId: 'p-4', threadId: 'thread-1' },
    });
    await fireEvent.click(getByTestId('diff-file-header'));
    expect(openReviewCompanion).not.toHaveBeenCalled();
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

  it('clicking the file path opens the file in the editor without promoting to review', async () => {
    const openEditor = setBindingMock('OpenInEditor', vi.fn(async () => undefined));
    const pane = fakePane() as ThreadPane;
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
    expect(openEditor.mock.calls[0]).toEqual(['src/foo.ts', 0, 0, '/tmp/workspace', '']);
    expect(openReviewCompanion).not.toHaveBeenCalled();
  });

  it('renders the body with line-tint backgrounds even before tokens land', () => {
    const file = makePatchFile();
    const { getByTestId } = render(DiffFileBlock, {
      props: { file, threadId: 'thread-1' },
    });
    const body = getByTestId('diff-file-body');
    // The line-tint classes are applied per-row regardless of
    // tokenization status — that's the "always usable" pre-render
    // pattern shared with the review diff rows.
    expect(body.querySelectorAll('.bg-success\\/12').length).toBeGreaterThan(0);
    expect(body.querySelectorAll('.bg-error\\/12').length).toBeGreaterThan(0);
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

  it('renders the clock time last in the header actions when createdAt is provided', () => {
    const createdAt = new Date(2026, 5, 10, 20, 5, 0).getTime();
    const pane = fakePane() as ThreadPane;
    const { getByTestId } = render(DiffFileBlock, {
      props: {
        pane,
        file: makePatchFile(),
        payloadId: 'p-time',
        threadId: 'thread-1',
        toolName: 'Edit',
        createdAt,
      },
    });

    const time = getByTestId('diff-file-time');
    expect(time.getAttribute('datetime')).toBe(new Date(createdAt).toISOString());
    expect(time.textContent?.trim()).toBe(formatTimeOfDay(createdAt));
    // Clock sits after counts and the review button so it column-aligns
    // with the right-edge timestamps of every other tool row.
    expectBefore(getByTestId('diff-file-counts'), time);
    expectBefore(getByTestId('diff-file-open-sidebar'), time);
  });

  it('reserves the shared status slot and renders the running indicator when statusItem is provided', () => {
    const item = makeItem({
      kind: 'tool_call',
      status: 'running',
      toolName: 'Edit',
    });
    const { getByTestId } = render(DiffFileBlock, {
      props: {
        file: makePatchFile({ lines: [] }),
        threadId: 'thread-1',
        toolName: 'Edit',
        statusItem: item,
      },
    });

    expect(getByTestId('diff-file-status-slot')).toBeInTheDocument();
    expect(getByTestId('diff-file-status').getAttribute('data-state')).toBe('running');
  });

  it('shows the shared error row for failed edit placeholders', () => {
    const item = makeItem({
      kind: 'tool_call',
      status: 'errored',
      toolName: 'Edit',
    });
    const { getByTestId } = render(DiffFileBlock, {
      props: {
        file: makePatchFile({ lines: [] }),
        threadId: 'thread-1',
        toolName: 'Edit',
        statusItem: item,
      },
    });

    expect(getByTestId('diff-file-status').getAttribute('data-state')).toBe('error');
    expect(getByTestId('row-error').textContent).toContain('File edit failed');
  });

  it('omits the timestamp and its meta strip when createdAt is not provided', () => {
    const { queryByTestId } = render(DiffFileBlock, {
      props: { file: makePatchFile(), threadId: 'thread-1', toolName: 'Edit' },
    });
    expect(queryByTestId('diff-file-time')).toBeNull();
    expect(queryByTestId('diff-file-status-slot')).toBeNull();
  });

  it('drops the time element instead of crashing when createdAt is corrupt (NaN)', () => {
    const { getByTestId, queryByTestId } = render(DiffFileBlock, {
      props: { file: makePatchFile(), threadId: 'thread-1', toolName: 'Edit', createdAt: Number.NaN },
    });
    // Row still renders; ToolHeaderMeta suppresses the <time> rather than
    // throwing on Invalid Date toISOString().
    expect(getByTestId('diff-file-label').textContent).toBe('edit');
    expect(queryByTestId('diff-file-time')).toBeNull();
  });

  describe('collapse setting + per-card toggle', () => {
    afterEach(() => {
      resetSettingsForTest();
    });

    async function enableCollapseSetting(): Promise<void> {
      setBindingMock('GetSettings', async () => makeSettings({ collapseDiffPreviews: true }));
      await loadSettings();
    }

    it('starts collapsed when collapseDiffPreviews is on (header only)', async () => {
      await enableCollapseSetting();
      const pane = fakePane() as ThreadPane;
      const file = { ...makeLongPatchFile(16), additions: 5, deletions: 2 };
      const { getByTestId, queryByTestId } = render(DiffFileBlock, {
        props: { pane, file, payloadId: 'p-collapsed', threadId: 'thread-1', toolName: 'Edit' },
      });

      // Header survives intact: label, path, and counts stay visible.
      expect(getByTestId('diff-file-label').textContent).toBe('edit');
      expect(getByTestId('diff-file-path').textContent).toBe('src/big.ts');
      const counts = getByTestId('diff-file-counts').textContent ?? '';
      expect(counts).toContain('+5');
      expect(counts).toContain('-2');
      // Preview body, fade, and CTA are all withheld until expand.
      expect(queryByTestId('diff-file-body')).toBeNull();
      expect(queryByTestId('diff-file-fade')).toBeNull();
      expect(queryByTestId('diff-file-show-full')).toBeNull();
      expect(getByTestId('diff-file-toggle').getAttribute('aria-expanded')).toBe('false');
    });

    it('keeps the timestamp visible on a collapsed header', async () => {
      await enableCollapseSetting();
      const createdAt = new Date(2026, 5, 10, 20, 5, 0).getTime();
      const { getByTestId, queryByTestId } = render(DiffFileBlock, {
        props: { file: makePatchFile(), threadId: 'thread-1', toolName: 'Edit', createdAt },
      });

      expect(queryByTestId('diff-file-body')).toBeNull();
      expect(getByTestId('diff-file-time').getAttribute('datetime')).toBe(
        new Date(createdAt).toISOString(),
      );
    });

    it('clicking the toggle expands a default-collapsed card to the full preview', async () => {
      await enableCollapseSetting();
      const pane = fakePane() as ThreadPane;
      const file = makeLongPatchFile(16);
      const { getByTestId, queryByTestId } = render(DiffFileBlock, {
        props: { pane, file, payloadId: 'p-expand', threadId: 'thread-1' },
      });
      expect(queryByTestId('diff-file-body')).toBeNull();

      const toggle = getByTestId('diff-file-toggle');
      await fireEvent.click(toggle);

      // Expanded view matches the always-inline render: body, fade,
      // and review CTA all present, aria wired to the region.
      expect(getByTestId('diff-file-body').textContent).toContain('line 15;');
      expect(getByTestId('diff-file-fade')).toBeInTheDocument();
      expect(getByTestId('diff-file-show-full')).toBeInTheDocument();
      expect(toggle.getAttribute('aria-expanded')).toBe('true');
      const controls = toggle.getAttribute('aria-controls');
      expect(controls).toBeTruthy();
      expect(document.getElementById(controls!)).not.toBeNull();
    });

    it('toggle collapses and re-expands with the setting off (default expanded)', async () => {
      const file = makePatchFile();
      const { getByTestId, queryByTestId } = render(DiffFileBlock, {
        props: { file, threadId: 'thread-1' },
      });
      const toggle = getByTestId('diff-file-toggle');
      expect(toggle.getAttribute('aria-expanded')).toBe('true');
      expect(getByTestId('diff-file-body')).toBeInTheDocument();

      await fireEvent.click(toggle);
      expect(queryByTestId('diff-file-body')).toBeNull();
      expect(toggle.getAttribute('aria-expanded')).toBe('false');

      await fireEvent.click(toggle);
      expect(getByTestId('diff-file-body')).toBeInTheDocument();
      expect(toggle.getAttribute('aria-expanded')).toBe('true');
    });

    it('mod-click on the toggle promotes to review exactly once and does not collapse', async () => {
      const pane = fakePane() as ThreadPane;
      const file = makePatchFile();
      const { getByTestId } = render(DiffFileBlock, {
        props: { pane, file, payloadId: 'p-mod-toggle', threadId: 'thread-1' },
      });

      await fireEvent.click(getByTestId('diff-file-toggle'), { metaKey: true });

      // One promote (the toggle bails and lets the wrapper handle it),
      // and the card stays expanded.
      expect(openReviewCompanion).toHaveBeenCalledExactlyOnceWith('pane-1', PANE_SUBJECT, {
        scope: 'workspace',
        filePath: 'src/foo.ts',
      });
      expect(getByTestId('diff-file-body')).toBeInTheDocument();
    });

    it('pane-backed override survives unmount and re-render', async () => {
      const item = makeItem({
        id: 'item-diff-override',
        kind: 'tool_completion',
        toolName: 'Edit',
      });
      const pane = await buildPane(undefined, [item]);
      const file = makePatchFile();
      const props = { pane, file, threadId: 'thread-1', itemId: item.id, toolName: 'Edit' };

      const first = render(DiffFileBlock, { props });
      expect(first.getByTestId('diff-file-body')).toBeInTheDocument();
      await fireEvent.click(first.getByTestId('diff-file-toggle'));
      expect(first.queryByTestId('diff-file-body')).toBeNull();
      first.unmount();

      // Re-render with the same pane + itemId — the collapse choice is
      // remembered in the per-pane registry, not row-local state.
      const second = render(DiffFileBlock, { props });
      expect(second.queryByTestId('diff-file-body')).toBeNull();
      expect(second.getByTestId('diff-file-toggle').getAttribute('aria-expanded')).toBe('false');
    });

    it('skips the span request while collapsed and requests on expand', async () => {
      await enableCollapseSetting();
      const request = vi.mocked(requestFileSpans);
      request.mockClear();
      const file = makePatchFile();
      const { getByTestId } = render(DiffFileBlock, {
        props: { file, threadId: 'thread-1' },
      });

      await tick();
      expect(request).not.toHaveBeenCalled();

      await fireEvent.click(getByTestId('diff-file-toggle'));
      await waitFor(() => expect(request).toHaveBeenCalledWith(file, 'thread-1'));
    });

    it('follows live setting flips for untouched cards while user-expanded cards stay pinned', async () => {
      await enableCollapseSetting();
      const item = makeItem({ id: 'item-flip', kind: 'tool_completion', toolName: 'Edit' });
      const pane = await buildPane(undefined, [item]);
      // Two renders share one document — scope queries per container.
      const untouched = within(
        render(DiffFileBlock, {
          props: { pane, file: makePatchFile(), threadId: 'thread-1', itemId: item.id },
        }).container,
      );
      const pinned = within(
        render(DiffFileBlock, {
          props: {
            pane,
            file: makePatchFile({ path: 'src/other.ts' }),
            threadId: 'thread-1',
            itemId: item.id,
          },
        }).container,
      );

      // Both start collapsed; expand one explicitly (override stored).
      expect(untouched.queryByTestId('diff-file-body')).toBeNull();
      await fireEvent.click(pinned.getByTestId('diff-file-toggle'));
      expect(pinned.getByTestId('diff-file-body')).toBeInTheDocument();

      // Flip the setting off: the untouched card follows the new
      // default live; the explicitly-expanded card keeps its override.
      setBindingMock('GetSettings', async () => makeSettings({ collapseDiffPreviews: false }));
      await loadSettings();
      await waitFor(() => expect(untouched.getByTestId('diff-file-body')).toBeInTheDocument());
      expect(pinned.getByTestId('diff-file-body')).toBeInTheDocument();

      // Flip back on: untouched follows again, pinned stays expanded.
      setBindingMock('GetSettings', async () => makeSettings({ collapseDiffPreviews: true }));
      await loadSettings();
      await waitFor(() => expect(untouched.queryByTestId('diff-file-body')).toBeNull());
      expect(pinned.getByTestId('diff-file-body')).toBeInTheDocument();
    });

    it('re-follows the setting after a card is toggled back to the default', async () => {
      const item = makeItem({ id: 'item-refollow', kind: 'tool_completion', toolName: 'Edit' });
      const pane = await buildPane(undefined, [item]);
      const { getByTestId, queryByTestId } = render(DiffFileBlock, {
        props: { pane, file: makePatchFile(), threadId: 'thread-1', itemId: item.id },
      });

      // Collapse, then expand back to the (setting-off) default — the
      // override clears instead of pinning "expanded".
      await fireEvent.click(getByTestId('diff-file-toggle'));
      expect(queryByTestId('diff-file-body')).toBeNull();
      await fireEvent.click(getByTestId('diff-file-toggle'));
      expect(getByTestId('diff-file-body')).toBeInTheDocument();
      expect(pane.diffCardExpandedOverride(item.id, 'src/foo.ts')).toBeUndefined();

      // A later setting flip therefore applies to this card too.
      setBindingMock('GetSettings', async () => makeSettings({ collapseDiffPreviews: true }));
      await loadSettings();
      await waitFor(() => expect(queryByTestId('diff-file-body')).toBeNull());
    });

    it('renders an inert chevron when there is nothing to toggle (header-only row)', () => {
      const file: PatchFile = {
        path: 'src/loading.ts',
        kind: 'modified',
        additions: 0,
        deletions: 0,
        lines: [],
      };
      const { getByTestId } = render(DiffFileBlock, {
        props: { file, threadId: 'thread-1', toolName: 'Edit' },
      });
      const toggle = getByTestId('diff-file-toggle');
      expect(toggle.getAttribute('aria-disabled')).toBe('true');
      expect(toggle.getAttribute('aria-expanded')).toBe('false');
    });
  });
});
