import { afterEach, beforeAll, beforeEach, describe, expect, it } from 'vitest';
import { cleanup, fireEvent, render, waitFor } from '@testing-library/svelte';
import CommandOutput from './CommandOutput.svelte';
import { makeItem } from '../../../test/helpers/chat';
import { getBindingMock, resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import type { CommandOutputMeta } from '../../types/models';

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
      nextOffset: 24,
      totalSize: 24,
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
    await waitFor(() => {
      expect(container.querySelector('pre')).not.toBeNull();
    });

    const pre = container.querySelector('pre');
    if (!pre) throw new Error('expected <pre> for the command output body');
    expect(pre.parentElement?.className).toContain('max-h-96');
    expect(pre.parentElement?.className).toContain('overflow-auto');
    const styledSpans = pre.querySelectorAll('span.ansi-fg-31');
    expect(styledSpans.length).toBeGreaterThan(0);
    expect(styledSpans[0]?.textContent).toBe('red');
    expect(pre.textContent).toContain(' then plain');
    expect(pre.innerHTML).not.toContain('\u001b[');
  });

  it('escapes raw HTML payloads so script tags never become live nodes', async () => {
    setBindingMock('GetPayloadPreview', async () => ({
      data: '<script>alert(1)</script>',
      nextOffset: 25,
      totalSize: 25,
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
    await waitFor(() => {
      expect(container.querySelector('pre')).not.toBeNull();
    });

    const pre = container.querySelector('pre');
    if (!pre) throw new Error('expected <pre>');
    expect(pre.innerHTML).not.toContain('<script>');
    expect(pre.innerHTML).toContain('&lt;script&gt;');
    expect(pre.querySelector('script')).toBeNull();
  });

  it('loads a command output preview only when expanded', async () => {
    const previewMock = setBindingMock('GetPayloadPreview', async () => ({
      data: '\u001b[32mok\u001b[0m\nfull body',
      nextOffset: 22,
      totalSize: 22,
      isComplete: true,
    }));
    const chunkMock = setBindingMock('GetPayloadChunk', async () => {
      throw new Error('command rows should not fetch chunks for complete previews');
    });
    const dataMock = setBindingMock('GetPayloadData', async () => {
      throw new Error('command rows should not fetch full payloads by default');
    });

    const { getByRole, queryByText, findByText } = render(CommandOutput, {
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
    expect(queryByText('inline preview')).toBeNull();
    expect(previewMock).not.toHaveBeenCalled();
    expect(chunkMock).not.toHaveBeenCalled();
    expect(dataMock).not.toHaveBeenCalled();

    await fireEvent.click(getByRole('button', { name: /Toggle command output/i }));
    expect(await findByText(/full body/)).toBeInTheDocument();
    expect(previewMock).toHaveBeenCalledWith('thread-1', 'pay-3', 32768);
    expect(chunkMock).not.toHaveBeenCalled();
    expect(dataMock).not.toHaveBeenCalled();
  });

  it('loads the rest of a large command output only on explicit request', async () => {
    const previewMock = setBindingMock('GetPayloadPreview', async () => ({
      data: 'first chunk',
      nextOffset: 11,
      totalSize: 22,
      isComplete: false,
    }));
    const chunkMock = setBindingMock('GetPayloadChunk', async () => ({
      data: '\nsecond chunk',
      nextOffset: 24,
      totalSize: 24,
      isComplete: true,
    }));
    const dataMock = setBindingMock('GetPayloadData', async () => {
      throw new Error('command rows should not fetch full payloads by default');
    });

    const { getByRole, getByTestId, findByText, queryByTestId } = render(CommandOutput, {
      props: {
        item: makeItem({ id: 'tool-cmd', kind: 'tool_call' }),
        meta: commandMeta({ command: 'long-command', preview: '', lineCount: 1000 }),
        payloadId: 'pay-large',
      },
    });

    await fireEvent.click(getByRole('button', { name: /Toggle command output/i }));
    expect(await findByText(/first chunk/)).toBeInTheDocument();
    expect(getByTestId('command-output-show-full').textContent).toContain('22 B');
    expect(getByTestId('command-output-show-full').textContent).toContain('Show more output');
    expect(chunkMock).not.toHaveBeenCalled();

    await fireEvent.click(getByTestId('command-output-show-full'));
    expect(await findByText(/second chunk/)).toBeInTheDocument();
    expect(previewMock).toHaveBeenCalledWith('thread-1', 'pay-large', 32768);
    expect(chunkMock).toHaveBeenCalledWith('thread-1', 'pay-large', 11, 262144);
    expect(dataMock).not.toHaveBeenCalled();
    expect(queryByTestId('command-output-show-full')).toBeNull();
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
    expect(previewMock).toHaveBeenCalledWith('thread-1', 'payload-deferred-output', 32768);
  });

  it('shows no collapsed output preview for successful commands', () => {
    setBindingMock('GetPayloadPreview', async () => ({
      data: 'full output',
      nextOffset: 11,
      totalSize: 11,
      isComplete: true,
    }));
    const { queryByText } = render(CommandOutput, {
      props: {
        item: makeItem({ id: 'tool-cmd', kind: 'tool_completion', status: 'completed' }),
        meta: commandMeta({ command: 'ls', preview: 'inline preview', lineCount: 1 }),
        payloadId: 'cmd-payload',
      },
    });

    expect(queryByText('inline preview')).toBeNull();
    expect(getBindingMock('GetPayloadPreview')).not.toHaveBeenCalled();
  });

  it('shows an explicit collapsed preview for wait-owned command completions', async () => {
    const previewMock = setBindingMock('GetPayloadPreview', async () => ({
      data: 'full output',
      nextOffset: 11,
      totalSize: 11,
      isComplete: true,
    }));
    const { getByRole, getByTestId, queryByTestId } = render(CommandOutput, {
      props: {
        item: makeItem({ id: 'tool-cmd', kind: 'tool_completion', status: 'completed' }),
        meta: commandMeta({ command: 'sleep 1; echo done', preview: 'done\n', lineCount: 1 }),
        payloadId: 'cmd-payload',
        collapsedPreview: 'done\n',
      },
    });

    expect(getByTestId('command-output-preview').textContent).toContain('done');
    expect(previewMock).not.toHaveBeenCalled();

    await fireEvent.click(getByRole('button', { name: /Toggle command output/i }));
    await waitFor(() => {
      expect(queryByTestId('command-output-preview')).toBeNull();
    });
  });

  it('truncates explicit collapsed command previews', () => {
    const longPreview = `${'x'.repeat(190)} final text`;
    const { getByTestId } = render(CommandOutput, {
      props: {
        item: makeItem({ id: 'tool-cmd', kind: 'tool_completion', status: 'completed' }),
        meta: commandMeta({ command: 'printf long', preview: longPreview, lineCount: 1 }),
        collapsedPreview: longPreview,
      },
    });

    const preview = getByTestId('command-output-preview').textContent ?? '';
    expect(preview).toContain('...');
    expect(preview).not.toContain('final text');
  });

  it('shows a compact red error line for failed commands without expanding output', () => {
    const previewMock = setBindingMock('GetPayloadPreview', async () => ({
      data: 'full output',
      nextOffset: 11,
      totalSize: 11,
      isComplete: true,
    }));
    const { getByTestId } = render(CommandOutput, {
      props: {
        item: makeItem({
          id: 'tool-cmd',
          kind: 'tool_completion',
          status: 'completed',
          payloadMeta: JSON.stringify({
            command: 'false',
            exitCode: 7,
            errorMessage: 'first line\nlast failure line',
          }),
        }),
        meta: commandMeta({
          command: 'false',
          exitCode: 7,
          errorMessage: 'first line\nlast failure line',
        }),
        payloadId: 'cmd-payload',
      },
    });

    const error = getByTestId('command-output-error');
    expect(error.textContent).toBe('error code 7: first line last failure line');
    expect(error.className).toContain('text-error');
    expect(previewMock).not.toHaveBeenCalled();
  });

  it('uses Claude stderr metadata for legacy Bash failure rows', () => {
    const { getByTestId } = render(CommandOutput, {
      props: {
        item: makeItem({
          id: 'tool-cmd',
          kind: 'tool_completion',
          status: 'errored',
          toolName: 'Bash',
          meta: JSON.stringify({
            exit_code: 2,
            tool_use_result: {
              stderr: '\u001b[31mNo such file or directory\u001b[0m',
            },
          }),
        }),
        meta: commandMeta({ command: 'cat missing.txt', exitCode: 2 }),
      },
    });

    expect(getByTestId('command-output-error').textContent).toBe(
      'error code 2: No such file or directory',
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

  it('renders foreground running commands in the reserved status slot', () => {
    const { getByTestId, queryByTestId } = render(CommandOutput, {
      props: {
        item: makeItem({ id: 'tool-cmd', kind: 'tool_call', status: 'running' }),
        meta: commandMeta({ command: 'pnpm test' }),
      },
    });

    const slot = getByTestId('command-output-status-slot');
    const status = getByTestId('command-output-status');
    expect(slot.className).toContain('min-w-[3.5rem]');
    expect(status.textContent?.trim()).toBe('…');
    expect(status.className).toContain('animate-pulse');
    expect(status.getAttribute('aria-label')).toBe('Running');
    expect(queryByTestId('completion-badge')).toBeNull();
  });

  it('keeps the running status slot when completion badges are suppressed', () => {
    const { getByTestId, queryByTestId } = render(CommandOutput, {
      props: {
        item: makeItem({ id: 'tool-cmd', kind: 'tool_call', status: 'running' }),
        meta: commandMeta({ command: 'pnpm test' }),
        showCompletionBadge: false,
      },
    });

    expect(getByTestId('command-output-status-slot').className).toContain('min-w-[3.5rem]');
    expect(getByTestId('command-output-status').getAttribute('aria-label')).toBe('Running');
    expect(queryByTestId('completion-badge')).toBeNull();
  });

  it('keeps the same status slot when a foreground command completes', async () => {
    const running = makeItem({ id: 'tool-cmd', kind: 'tool_call', status: 'running' });
    const completed = makeItem({ ...running, status: 'completed', updatedAt: running.updatedAt + 1 });
    const { getByTestId, rerender } = render(CommandOutput, {
      props: {
        item: running,
        meta: commandMeta({ command: 'pnpm test', exitCode: 0 }),
      },
    });

    const runningSlot = getByTestId('command-output-status-slot');
    expect(runningSlot.className).toContain('min-w-[3.5rem]');
    expect(getByTestId('command-output-status').textContent?.trim()).toBe('…');

    await rerender({
      item: completed,
      meta: commandMeta({ command: 'pnpm test', exitCode: 0 }),
    });

    const completedSlot = getByTestId('command-output-status-slot');
    expect(completedSlot.className).toBe(runningSlot.className);
    expect(getByTestId('completion-badge').getAttribute('data-status')).toBe('success');
  });

  it('uses the same metadata lane contract as generic tool rows', async () => {
    const command = render(CommandOutput, {
      props: {
        item: makeItem({ id: 'tool-cmd', kind: 'tool_completion', status: 'completed' }),
        meta: commandMeta({ command: 'ls', exitCode: 0 }),
      },
    });
    const { default: GenericToolCallRow } = await import('./GenericToolCallRow.svelte');
    const generic = render(GenericToolCallRow, {
      props: {
        item: makeItem({
          id: 'tool-read',
          kind: 'tool_call',
          status: 'completed',
          toolName: 'Read',
          summary: 'README.md',
        }),
      },
    });

    for (const slot of [
      command.getByTestId('command-output-status-slot'),
      generic.getByTestId('tool-call-card-status-slot'),
    ]) {
      expect(slot.className).toContain('inline-flex');
      expect(slot.className).toContain('justify-end');
      expect(slot.className).toContain('min-w-[3.5rem]');
    }

    for (const duration of [
      command.getByTestId('command-output-duration'),
      generic.getByTestId('tool-call-card-duration'),
    ]) {
      expect(duration.className).toContain('min-w-[3rem]');
      expect(duration.className).toContain('text-right');
      expect(duration.className).toContain('tabular-nums');
    }

    command.unmount();
    generic.unmount();
  });

  it('places the command completion badge before the timestamp', () => {
    const { getByTestId } = render(CommandOutput, {
      props: {
        item: makeItem({ id: 'tool-cmd', kind: 'tool_completion', status: 'completed' }),
        meta: commandMeta({ command: 'ls', exitCode: 0 }),
      },
    });

    expectBefore(getByTestId('completion-badge'), getByTestId('command-output-time'));
    expectBefore(getByTestId('command-output-status-slot'), getByTestId('command-output-time'));
  });

  it('left-aligns expandable command completion headers', () => {
    const { getByTestId } = render(CommandOutput, {
      props: {
        item: makeItem({ id: 'tool-cmd', kind: 'tool_completion', status: 'completed' }),
        meta: commandMeta({ command: 'echo done', exitCode: 0 }),
        payloadId: 'cmd-payload',
      },
    });

    expect(getByTestId('command-output-toggle').className).toContain('text-left');
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
