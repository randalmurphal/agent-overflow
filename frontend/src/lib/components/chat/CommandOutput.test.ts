import { afterEach, beforeAll, beforeEach, describe, expect, it } from 'vitest';
import { cleanup, fireEvent, render } from '@testing-library/svelte';
import CommandOutput from './CommandOutput.svelte';
import { makeItem } from '../../../test/helpers/chat';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import type { CommandOutputMeta } from '../../types/models';
import { DEFAULT_PAYLOAD_PREVIEW_BYTES } from './payloadExpansion.svelte';

// Some Svelte transitions call Element.prototype.animate; jsdom doesn't
// implement it. Stub it so expand/collapse doesn't throw.
beforeAll(() => {
  if (typeof (Element.prototype as unknown as { animate?: unknown }).animate !== 'function') {
    (Element.prototype as unknown as { animate: (...args: unknown[]) => unknown }).animate =
      function fakeAnimate() {
        return {
          finished: Promise.resolve(),
          currentTime: 0,
          playState: 'finished' as const,
          cancel() {}, finish() {}, play() {}, pause() {}, reverse() {},
          addEventListener() {}, removeEventListener() {},
          onfinish: null, oncancel: null,
        };
      };
  }
});

function commandMeta(overrides: Partial<CommandOutputMeta> = {}): CommandOutputMeta {
  return {
    command: 'pnpm test',
    exitCode: 0,
    lineCount: 1,
    preview: '',
    ...overrides,
  };
}

function expectBefore(left: Element, right: Element) {
  expect(left.compareDocumentPosition(right) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
}

describe('<CommandOutput>', () => {
  beforeEach(() => {
    resetBindingMocks();
  });

  afterEach(() => {
    cleanup();
  });

  it('renders raw ANSI payloads in the expanded output', async () => {
    setBindingMock('GetPayloadPreview', async () => ({
      data: '\x1b[31mred\x1b[0m then plain',
      totalSize: 32,
      isComplete: true,
    }));

    const { getByRole, container } = render(CommandOutput, {
      props: {
        item: makeItem({ id: 'tool-cmd', kind: 'tool_call' }),
        meta: commandMeta({ command: 'ls', preview: '', lineCount: 1 }),
        payloadId: 'cmd-payload',
      },
    });

    await fireEvent.click(getByRole('button', { name: /Toggle command output/i }));
    await Promise.resolve();
    await Promise.resolve();

    const pre = container.querySelector('pre');
    if (!pre) throw new Error('expected <pre> for the command output body');
    const styledSpans = pre.querySelectorAll('span.ansi-fg-31');
    expect(styledSpans.length).toBeGreaterThan(0);
    expect(styledSpans[0]?.textContent).toBe('red');
    expect(pre.textContent).toContain(' then plain');
    expect(pre.innerHTML).not.toContain('\u001b[');
  });

  it('escapes raw HTML payloads so script tags never become live nodes', async () => {
    setBindingMock('GetPayloadPreview', async () => ({
      data: '<script>alert(1)</script>',
      totalSize: 30,
      isComplete: true,
    }));

    const { getByRole, container } = render(CommandOutput, {
      props: {
        item: makeItem({ id: 'tool-cmd', kind: 'tool_call' }),
        meta: commandMeta({ command: 'echo', preview: '', lineCount: 1 }),
        payloadId: 'cmd-payload',
      },
    });
    await fireEvent.click(getByRole('button', { name: /Toggle command output/i }));
    await Promise.resolve();
    await Promise.resolve();

    const pre = container.querySelector('pre');
    if (!pre) throw new Error('expected <pre>');
    expect(pre.innerHTML).not.toContain('<script>');
    expect(pre.innerHTML).toContain('&lt;script&gt;');
    expect(pre.querySelector('script')).toBeNull();
  });

  it('does not load full payload until the user asks for it', async () => {
    // The preview is rendered inline from meta; GetPayloadChunk should
    // only fire on explicit "Show full output" click. This pins the
    // lazy-content guarantee that keeps memory bounded.
    const previewMock = setBindingMock('GetPayloadPreview', async () => ({
      data: '\u001b[32mok\u001b[0m',
      nextOffset: DEFAULT_PAYLOAD_PREVIEW_BYTES,
      totalSize: 2048,
      isComplete: false,
    }));
    const chunkMock = setBindingMock('GetPayloadChunk', async () => ({
      data: 'full body',
      offset: DEFAULT_PAYLOAD_PREVIEW_BYTES,
      nextOffset: DEFAULT_PAYLOAD_PREVIEW_BYTES + 9,
      totalSize: 2048,
      isComplete: true,
    }));

    const { getByRole } = render(CommandOutput, {
      props: {
        item: makeItem({ id: 'tool-cmd', kind: 'tool_call' }),
        meta: commandMeta({
          command: 'ls',
          preview: 'inline preview',
          lineCount: 1,
        }),
        payloadId: 'pay-3',
      },
    });
    // Before expand: neither binding fires.
    expect(previewMock).not.toHaveBeenCalled();
    expect(chunkMock).not.toHaveBeenCalled();

    await fireEvent.click(getByRole('button', { name: /Toggle command output/i }));
    await Promise.resolve();
    await Promise.resolve();
    // Expand loads the preview but NOT the full body.
    expect(previewMock).toHaveBeenCalledWith('thread-1', 'pay-3', DEFAULT_PAYLOAD_PREVIEW_BYTES);
    expect(chunkMock).not.toHaveBeenCalled();
  });

  it('loads output when an already-expanded deferred row gains a payload id', async () => {
    const previewMock = setBindingMock('GetPayloadPreview', async () => ({
      data: 'arrived later',
      nextOffset: 13,
      totalSize: 13,
      isComplete: true,
    }));
    const itemWithoutPayload = makeItem({
      id: 'tool-deferred-output',
      kind: 'tool_completion',
      status: 'completed',
      meta: JSON.stringify({ output_file_state: 'loading' }),
    });

    const { getByRole, findByText, rerender } = render(CommandOutput, {
      props: {
        item: itemWithoutPayload,
        meta: commandMeta({ command: 'git log' }),
        payloadId: undefined,
      },
    });

    await fireEvent.click(getByRole('button', { name: /Toggle command output/i }));
    expect(await findByText('Loading…')).toBeInTheDocument();
    expect(previewMock).not.toHaveBeenCalled();

    const itemWithPayload = {
      ...itemWithoutPayload,
      payloadId: 'payload-deferred-output',
      payloadKind: 'command_output',
      updatedAt: itemWithoutPayload.updatedAt + 1,
    };
    await rerender({
      item: itemWithPayload,
      meta: commandMeta({ command: 'git log' }),
      payloadId: 'payload-deferred-output',
    });

    expect(await findByText('arrived later')).toBeInTheDocument();
    expect(previewMock).toHaveBeenCalledWith(
      'thread-1',
      'payload-deferred-output',
      DEFAULT_PAYLOAD_PREVIEW_BYTES,
    );
  });

  it('renders the success badge for an exitCode=0 command', () => {
    const { getByTestId } = render(CommandOutput, {
      props: {
        item: makeItem({ id: 'tool-cmd', kind: 'tool_completion', status: 'completed' }),
        meta: commandMeta({ command: 'ls', exitCode: 0 }),
        payloadId: 'cmd-payload',
      },
    });
    const badge = getByTestId('completion-badge');
    expect(badge.getAttribute('data-status')).toBe('success');
    expect(badge.className).toContain('text-success');
  });

  it('renders command rows with a terminal icon, Bash label, and command text', () => {
    const { getByTestId } = render(CommandOutput, {
      props: {
        item: makeItem({ id: 'tool-cmd', kind: 'tool_call', status: 'running' }),
        meta: commandMeta({ command: 'pnpm test' }),
      },
    });

    expect(getByTestId('command-output-icon')).toBeInTheDocument();
    expect(getByTestId('command-output-label').textContent?.trim()).toBe('Bash');
    expect(getByTestId('command-output-command').textContent).toBe('pnpm test');
  });

  it('places the command completion badge before the timestamp', () => {
    const { getByTestId } = render(CommandOutput, {
      props: {
        item: makeItem({ id: 'tool-cmd', kind: 'tool_completion', status: 'completed' }),
        meta: commandMeta({ command: 'ls', exitCode: 0 }),
      },
    });

    expectBefore(getByTestId('completion-badge'), getByTestId('command-output-time'));
  });

  it('renders the failure badge for a non-zero exit code', () => {
    // Pre-unification this row carried an `exit 7` rose pill. Now the
    // failure verdict is conveyed by the unified badge alone — no
    // exit-code text in the chat.
    const { getByTestId, queryByText } = render(CommandOutput, {
      props: {
        item: makeItem({ id: 'tool-cmd', kind: 'tool_completion', status: 'completed' }),
        meta: commandMeta({ command: 'false', exitCode: 7 }),
        payloadId: 'cmd-payload',
      },
    });
    const badge = getByTestId('completion-badge');
    expect(badge.getAttribute('data-status')).toBe('failure');
    expect(badge.className).toContain('text-error');
    // The old `exit 7` text must not return.
    expect(queryByText(/exit 7/)).toBeNull();
  });

  it('renders the failure badge when the parent item was killed even if the command reports exit 0', () => {
    // Codex reports "command finished" before the kill signal lands;
    // the parent item carries `status='killed'` and the unified
    // helper must collapse that to failure regardless of the exit
    // code in meta.
    const { getByTestId } = render(CommandOutput, {
      props: {
        item: makeItem({ id: 'tool-cmd', kind: 'tool_completion', status: 'killed' }),
        meta: commandMeta({ command: 'sleep 60', exitCode: 0 }),
        payloadId: 'cmd-payload',
      },
    });
    expect(getByTestId('completion-badge').getAttribute('data-status')).toBe('failure');
  });

  it('shows the item timestamp in the header instead of the line count', () => {
    const createdAt = Date.UTC(2026, 0, 2, 15, 4);
    const { getByTestId, queryByText } = render(CommandOutput, {
      props: {
        item: makeItem({ id: 'tool-cmd', kind: 'tool_call', createdAt }),
        meta: commandMeta({ command: 'pnpm test', lineCount: 87 }),
        payloadId: 'cmd-payload',
      },
    });

    expect(queryByText('87 lines')).toBeNull();
    expect(getByTestId('command-output-time').getAttribute('datetime')).toBe(new Date(createdAt).toISOString());
  });
});
