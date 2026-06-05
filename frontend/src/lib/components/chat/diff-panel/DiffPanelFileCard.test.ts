import { describe, expect, it, beforeEach, vi } from 'vitest';
import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import DiffPanelFileCard from './DiffPanelFileCard.svelte';
import { parsePatchFiles } from '../../../utils/patchFiles';
import { resetSharedTokenCacheForTest } from '../../../utils/tokenCacheReactive.svelte';

// Stub the Shiki worker pool — happy-dom's Web Worker support is
// incomplete and the real pool would never resolve. The spy lets us
// assert that the card's $effect actually posts a tokenize batch
// once the user opens a file with a known-language extension.
const tokenizeSpy = vi.fn();
vi.mock('../../../utils/diffHighlighterPool', async (importOriginal) => {
  const actual =
    await importOriginal<typeof import('../../../utils/diffHighlighterPool')>();
  return {
    ...actual,
    getSharedDiffHighlighterPool: () => ({
      tokenize: tokenizeSpy,
      terminate: vi.fn(),
      get isActive() {
        return true;
      },
    }),
  };
});

describe('<DiffPanelFileCard>', () => {
  beforeEach(() => {
    resetSharedTokenCacheForTest();
    tokenizeSpy.mockReset();
    tokenizeSpy.mockResolvedValue([]);
  });

  it('hides git diff metadata from the expanded body', () => {
    const [file] = parsePatchFiles(`diff --git a/test-ui.txt b/test-ui.txt
new file mode 100644
index 0000000..572d5d9
--- /dev/null
+++ b/test-ui.txt
@@ -0,0 +1,2 @@
+Line 1
+Line 2
`);

    const { container } = render(DiffPanelFileCard, {
      props: {
        file,
        open: true,
        threadId: 'thread-test',
        workspacePath: '/tmp/project',
        viewMode: 'stacked',
        wordWrap: false,
        onToggle: vi.fn(),
      },
    });

    expect(container.textContent).toContain('+Line 1');
    expect(container.textContent).toContain('+Line 2');
    expect(container.textContent).not.toContain('diff --git');
    expect(container.textContent).not.toContain('new file mode');
    expect(container.textContent).not.toContain('index 0000000');
    expect(container.textContent).not.toContain('--- /dev/null');
    expect(container.textContent).not.toContain('+++ b/test-ui.txt');
    expect(container.textContent).not.toContain('@@ -0,0 +1,2 @@');
  });

  it('sizes stacked no-wrap rows to the horizontal scroll content', () => {
    const longLine = 'const value = "' + 'x'.repeat(180) + '";';
    const [file] = parsePatchFiles(`diff --git a/src/long.ts b/src/long.ts
--- a/src/long.ts
+++ b/src/long.ts
@@ -1 +1 @@
-${longLine}
+${longLine.replace('value', 'nextValue')}
`);

    const { getByTestId, getAllByTestId } = render(DiffPanelFileCard, {
      props: {
        file,
        open: true,
        threadId: 'thread-test',
        workspacePath: '/tmp/project',
        viewMode: 'stacked',
        wordWrap: false,
        onToggle: vi.fn(),
      },
    });

    expect(getByTestId('diff-panel-stacked-content').className).toContain('w-max');
    expect(getByTestId('diff-panel-stacked-content').className).toContain('min-w-full');
    expect(getAllByTestId('diff-panel-line-row').every((row) => row.className.includes('min-w-full'))).toBe(true);
    expect(getAllByTestId('diff-panel-line-content').every((line) => line.className.includes('min-w-max'))).toBe(true);
    expect(getAllByTestId('diff-panel-line-content').every((line) => line.className.includes('whitespace-pre'))).toBe(true);
  });

  it('keeps stacked word-wrap rows constrained to the viewport width', () => {
    const longLine = 'const value = "' + 'x'.repeat(180) + '";';
    const [file] = parsePatchFiles(`diff --git a/src/long.ts b/src/long.ts
--- a/src/long.ts
+++ b/src/long.ts
@@ -1 +1 @@
-${longLine}
+${longLine.replace('value', 'nextValue')}
`);

    const { getAllByTestId, getByTestId } = render(DiffPanelFileCard, {
      props: {
        file,
        open: true,
        threadId: 'thread-test',
        workspacePath: '/tmp/project',
        viewMode: 'stacked',
        wordWrap: true,
        onToggle: vi.fn(),
      },
    });

    expect(getByTestId('diff-panel-stacked-content').className).toContain('w-full');
    expect(getByTestId('diff-panel-stacked-content').className).toContain('min-w-full');
    expect(getByTestId('diff-panel-stacked-content').className).not.toContain('w-max');
    const contents = getAllByTestId('diff-panel-line-content');
    expect(contents.every((line) => line.className.includes('min-w-0'))).toBe(true);
    expect(contents.every((line) => line.className.includes('whitespace-pre-wrap'))).toBe(true);
    expect(contents.every((line) => line.className.includes('break-all'))).toBe(true);
    expect(contents.every((line) => !line.className.includes('min-w-max'))).toBe(true);
  });

  it('sizes split no-wrap rows to the horizontal scroll content', () => {
    const longLine = 'const value = "' + 'x'.repeat(180) + '";';
    const [file] = parsePatchFiles(`diff --git a/src/long.ts b/src/long.ts
--- a/src/long.ts
+++ b/src/long.ts
@@ -1 +1 @@
-${longLine}
+${longLine.replace('value', 'nextValue')}
`);

    const { getByTestId, getAllByTestId } = render(DiffPanelFileCard, {
      props: {
        file,
        open: true,
        threadId: 'thread-test',
        workspacePath: '/tmp/project',
        viewMode: 'split',
        wordWrap: false,
        onToggle: vi.fn(),
      },
    });

    expect(getByTestId('diff-panel-split-content').className).toContain('w-max');
    expect(getByTestId('diff-panel-split-content').className).toContain('min-w-full');
    expect(getAllByTestId('diff-panel-line-row').every((row) => row.className.includes('min-w-full'))).toBe(true);
    expect(getAllByTestId('diff-panel-line-content').every((line) => line.className.includes('min-w-max'))).toBe(true);
  });

  it('keeps split word-wrap rows constrained to the viewport width', () => {
    const longLine = 'const value = "' + 'x'.repeat(180) + '";';
    const [file] = parsePatchFiles(`diff --git a/src/long.ts b/src/long.ts
--- a/src/long.ts
+++ b/src/long.ts
@@ -1 +1 @@
-${longLine}
+${longLine.replace('value', 'nextValue')}
`);

    const { getByTestId, getAllByTestId } = render(DiffPanelFileCard, {
      props: {
        file,
        open: true,
        threadId: 'thread-test',
        workspacePath: '/tmp/project',
        viewMode: 'split',
        wordWrap: true,
        onToggle: vi.fn(),
      },
    });

    expect(getByTestId('diff-panel-split-content').className).toContain('w-full');
    expect(getByTestId('diff-panel-split-content').className).toContain('min-w-full');
    expect(getByTestId('diff-panel-split-content').className).not.toContain('w-max');

    const contents = getAllByTestId('diff-panel-line-content');
    expect(contents.every((line) => line.className.includes('min-w-0'))).toBe(true);
    expect(contents.every((line) => line.className.includes('whitespace-pre-wrap'))).toBe(true);
    expect(contents.every((line) => line.className.includes('break-all'))).toBe(true);
    expect(contents.every((line) => !line.className.includes('min-w-max'))).toBe(true);
  });

  it('shows line and file comment affordances only for commentable diffs', async () => {
    const [file] = parsePatchFiles(`diff --git a/test-ui.txt b/test-ui.txt
--- a/test-ui.txt
+++ b/test-ui.txt
@@ -1 +1 @@
-old
+new
`);
    const onCreateComment = vi.fn();

    const { rerender } = render(DiffPanelFileCard, {
      props: {
        file,
        open: true,
        threadId: 'thread-test',
        workspacePath: '/tmp/project',
        viewMode: 'stacked',
        wordWrap: false,
        commentable: false,
        reviewScope: null,
        onToggle: vi.fn(),
        onCreateComment,
      },
    });

    expect(screen.queryByLabelText('Comment on file')).not.toBeInTheDocument();
    expect(screen.queryAllByLabelText('Add comment')).toHaveLength(0);

    await rerender({
      file,
      open: true,
      threadId: 'thread-test',
      workspacePath: '/tmp/project',
      viewMode: 'stacked',
      wordWrap: false,
      commentable: true,
      reviewScope: 'workspace',
      sourceKey: 'diff-1',
      onToggle: vi.fn(),
      onCreateComment,
    });

    await fireEvent.click(screen.getAllByLabelText('Add comment')[0]);
    expect(screen.getByPlaceholderText('Comment on this line')).toBeInTheDocument();
  });

  it('dispatches syntax-highlight tokens for a non-plaintext file when expanded', async () => {
    const [file] = parsePatchFiles(`diff --git a/foo.ts b/foo.ts
--- a/foo.ts
+++ b/foo.ts
@@ -1 +1 @@
-const x = 1;
+const x = 2;
`);

    render(DiffPanelFileCard, {
      props: {
        file,
        open: true,
        threadId: 'thread-test',
        workspacePath: '/tmp/project',
        viewMode: 'stacked',
        wordWrap: false,
        onToggle: vi.fn(),
      },
    });

    await waitFor(() => {
      expect(tokenizeSpy).toHaveBeenCalled();
    });
    const callArg = tokenizeSpy.mock.calls[0]?.[0] as {
      lines: string[];
      lang: string;
      theme: string;
    };
    expect(callArg.lang).toBe('typescript');
    // Lines are passed with the +/- prefix stripped (the worker
    // tokenizes source text, not diff syntax).
    expect(callArg.lines).toEqual(expect.arrayContaining(['const x = 1;', 'const x = 2;']));
  });

  it('does not dispatch tokens for a closed card', async () => {
    const [file] = parsePatchFiles(`diff --git a/foo.ts b/foo.ts
--- a/foo.ts
+++ b/foo.ts
@@ -1 +1 @@
-const x = 1;
+const x = 2;
`);

    render(DiffPanelFileCard, {
      props: {
        file,
        open: false,
        threadId: 'thread-test',
        workspacePath: '/tmp/project',
        viewMode: 'stacked',
        wordWrap: false,
        onToggle: vi.fn(),
      },
    });

    // Give the effect microtask a beat to settle — if dispatch were
    // unconditional, it would have fired by now.
    await Promise.resolve();
    await Promise.resolve();
    expect(tokenizeSpy).not.toHaveBeenCalled();
  });

  it('skips dispatch for plaintext file extensions', async () => {
    const [file] = parsePatchFiles(`diff --git a/notes.txt b/notes.txt
--- a/notes.txt
+++ b/notes.txt
@@ -1 +1 @@
-hello
+world
`);

    render(DiffPanelFileCard, {
      props: {
        file,
        open: true,
        threadId: 'thread-test',
        workspacePath: '/tmp/project',
        viewMode: 'stacked',
        wordWrap: false,
        onToggle: vi.fn(),
      },
    });

    await Promise.resolve();
    await Promise.resolve();
    expect(tokenizeSpy).not.toHaveBeenCalled();
  });
});
