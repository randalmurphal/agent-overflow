import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, waitFor } from '@testing-library/svelte';
import CommandOutput from './CommandOutput.svelte';
import { makeItem } from '../../../test/helpers/chat';
import { getBindingMock, resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import {
  DEV_SERVER_PROBE_MAX_DEAD_PROBES,
  DEV_SERVER_PROBE_RETRY_MS,
  DEV_SERVER_PROBE_VERIFY_MS,
  resetDevServerProbeForTest,
} from '../../utils/devServerProbe';
import type { CommandOutputMeta } from '../../types/models';
import {
  REMOTE_BACKEND_UUID,
  resetStagedBackends,
  stageBackend,
} from '../../../test/helpers/backends';
import { emitWailsEvent, resetWailsMocks } from '../../../test/mocks/wailsio-runtime';
import { forgetThread, noteThread } from '../../transport/entityIndex';
import { initDevServers, resetDevServersForTest } from '../../stores/devServers.svelte';
import type { DevServerList } from '../../stores/devServers.svelte';

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

  it('shows a payload protocol error instead of crashing when output data is not text', async () => {
    setBindingMock('GetPayloadPreview', async () => ({
      data: { output: 'object payload' } as unknown as string,
      nextOffset: 1,
      totalSize: 1,
      isComplete: true,
    }));

    const { getByRole, findByRole } = render(CommandOutput, {
      props: {
        item: makeItem({ id: 'tool-cmd', kind: 'tool_call' }),
        meta: commandMeta({ command: 'echo', preview: '', lineCount: 1 }),
        payloadId: 'cmd-payload',
      },
    });

    await fireEvent.click(getByRole('button', { name: /Toggle command output/i }));

    expect(await findByRole('alert')).toHaveTextContent(
      'GetPayloadPreview returned non-string payload data',
    );
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

    expect(getByTestId('row-error-code').textContent).toBe('exit 7');
    expect(getByTestId('row-error-msg').textContent).toBe('first line last failure line');
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

    expect(getByTestId('row-error-code').textContent).toBe('exit 2');
    expect(getByTestId('row-error-msg').textContent).toBe('No such file or directory');
  });

  it('renders no indicator for an exitCode=0 command', () => {
    const { queryByTestId } = render(CommandOutput, {
      props: {
        item: makeItem({ id: 'tool-cmd', kind: 'tool_completion', status: 'completed' }),
        meta: commandMeta({ command: 'ls', exitCode: 0 }),
        payloadId: 'cmd-payload',
      },
    });
    expect(queryByTestId('indicator')).toBeNull();
  });

  it('renders command rows with a terminal icon, bash label, and command text', () => {
    const { getByTestId } = render(CommandOutput, {
      props: {
        item: makeItem({ id: 'tool-cmd', kind: 'tool_call', status: 'running' }),
        meta: commandMeta({ command: 'pnpm test' }),
      },
    });

    expect(getByTestId('command-output-icon')).toBeInTheDocument();
    expect(getByTestId('command-output-label').textContent?.trim()).toBe('bash');
    expect(getByTestId('command-output-command').textContent).toBe('pnpm test');
    expect(getByTestId('command-output-command')).toHaveAttribute('title', 'pnpm test');
  });

  it('keeps long bash commands inside the body column so they cannot overflow into the timestamp', () => {
    // Regression for the case where a long inline command (e.g. a docs
    // grep with a glob argument) ran on under the trailing timestamp
    // because the row's body slot was an inline <span> — Tailwind's
    // `truncate` (overflow:hidden + text-overflow:ellipsis) on an
    // inline element has no effect. The disclosure primitive now
    // marks the body slot itself as a flex container so the inner
    // `flex-1 min-w-0 truncate` resolves into a real truncating
    // flex item. Pin both the slot's flex context and the inner span's
    // truncate class so a future refactor that drops either fails fast.
    const { getByTestId } = render(CommandOutput, {
      props: {
        item: makeItem({ id: 'tool-cmd', kind: 'tool_call', status: 'completed' }),
        meta: commandMeta({
          command: "rg -n --hidden -g '!.git' -g '!node_modules' --type-add 'svelte:*.svelte' --type svelte -e 'docs -g.*node_modules' .",
        }),
      },
    });
    const bodySlot = getByTestId('command-output-toggle-body-slot');
    expect(bodySlot.className).toContain('flex');
    expect(bodySlot.className).toContain('min-w-0');
    const command = getByTestId('command-output-command');
    expect(command.className).toContain('truncate');
    expect(command.className).toContain('min-w-0');
  });

  it('renders foreground running commands in the reserved status slot', () => {
    const { getByTestId } = render(CommandOutput, {
      props: {
        item: makeItem({ id: 'tool-cmd', kind: 'tool_call', status: 'running' }),
        meta: commandMeta({ command: 'pnpm test' }),
      },
    });

    const slot = getByTestId('command-output-status-slot');
    const status = getByTestId('command-output-status');
    expect(slot.className).toContain('min-w-');
    expect(status.querySelector('[data-testid="indicator"]')?.getAttribute('aria-label')).toBe('Running');
  });

  it('keeps the running status slot stable', () => {
    const { getByTestId } = render(CommandOutput, {
      props: {
        item: makeItem({ id: 'tool-cmd', kind: 'tool_call', status: 'running' }),
        meta: commandMeta({ command: 'pnpm test' }),
      },
    });

    expect(getByTestId('command-output-status-slot').className).toContain('min-w-');
    expect(getByTestId('indicator').getAttribute('aria-label')).toBe('Running');
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
    expect(runningSlot.className).toContain('min-w-');
    expect(getByTestId('command-output-status').querySelector('[data-testid="indicator"]')).not.toBeNull();

    await rerender({
      item: completed,
      meta: commandMeta({ command: 'pnpm test', exitCode: 0 }),
    });

    const completedSlot = getByTestId('command-output-status-slot');
    expect(completedSlot.className).toBe(runningSlot.className);
    expect(completedSlot.querySelector('[data-testid="indicator"]')).toBeNull();
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
      expect(slot.className).toContain('justify-center');
      expect(slot.className).toContain('min-w-');
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

  it('places the command indicator slot before the timestamp', () => {
    const { getByTestId } = render(CommandOutput, {
      props: {
        item: makeItem({ id: 'tool-cmd', kind: 'tool_completion', status: 'completed' }),
        meta: commandMeta({ command: 'ls', exitCode: 0 }),
      },
    });

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

  it('renders the failure indicator and row error for a non-zero exit code', () => {
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
    expect(getByTestId('indicator').getAttribute('data-state')).toBe('error');
    expect(getByTestId('row-error-code').textContent).toBe('exit 7');
    expect(queryByText(/error code 7/)).toBeNull();
  });

  it('renders the failure indicator when the parent item was killed even if the command reports exit 0', () => {
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
    expect(getByTestId('indicator').getAttribute('data-state')).toBe('error');
    expect(getByTestId('row-error-msg').textContent).toBe('Tool call stopped');
  });

  it('shows no duration for a completed command under 3 seconds', () => {
    const now = Date.now();
    const { getByTestId } = render(CommandOutput, {
      props: {
        item: makeItem({
          id: 'tool-cmd',
          kind: 'tool_completion',
          status: 'completed',
          createdAt: now - 2_000,
          updatedAt: now,
        }),
        meta: commandMeta({ command: 'ls', exitCode: 0 }),
      },
    });

    expect(getByTestId('command-output-duration').textContent?.trim()).toBe('');
  });

  it('shows duration for a completed command that took >= 3 seconds', () => {
    const now = Date.now();
    const { getByTestId } = render(CommandOutput, {
      props: {
        item: makeItem({
          id: 'tool-cmd',
          kind: 'tool_completion',
          status: 'completed',
          createdAt: now - 5_000,
          updatedAt: now,
        }),
        meta: commandMeta({ command: 'pnpm test', exitCode: 0 }),
      },
    });

    expect(getByTestId('command-output-duration').textContent?.trim()).toBe('5.0s');
  });

  it('prefers caller-provided durationLabel over computed duration', () => {
    const now = Date.now();
    const { getByTestId } = render(CommandOutput, {
      props: {
        item: makeItem({
          id: 'tool-cmd',
          kind: 'tool_completion',
          status: 'completed',
          createdAt: now - 10_000,
          updatedAt: now,
        }),
        meta: commandMeta({ command: 'pnpm test', exitCode: 0 }),
        durationLabel: '12s',
      },
    });

    expect(getByTestId('command-output-duration').textContent?.trim()).toBe('12s');
  });

  it('shows no duration for a backgrounded launch', () => {
    const now = Date.now();
    const { getByTestId } = render(CommandOutput, {
      props: {
        item: makeItem({
          id: 'tool-cmd',
          kind: 'tool_call',
          status: 'running',
          isBackground: true,
          createdAt: now - 10_000,
          updatedAt: now,
        }),
        meta: commandMeta({ command: 'pnpm test' }),
      },
    });

    expect(getByTestId('command-output-duration').textContent?.trim()).toBe('');
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
  // Dev-server affordance. Detection happens in triage (see
  // internal/triage/dev_server_url.go); the row re-validates it as a
  // loopback URL, holds the first detection so the chip survives the
  // per-flush-window meta rewrites while streaming, and renders the chip
  // only after the backend confirms something is listening on the port
  // (utils/devServerProbe.ts) — detection alone only proves the output
  // mentioned the URL. While the command runs the row re-probes: fast
  // while unconfirmed (bounded), slower to re-verify a confirmed URL.
  describe('dev-server chip', () => {
    beforeEach(() => {
      resetDevServerProbeForTest();
    });

    afterEach(() => {
      vi.useRealTimers();
    });

    it('is absent for a command that announced no server', () => {
      const probe = setBindingMock('ProbeDevServerURL', vi.fn(async () => true));
      const { queryByTestId } = render(CommandOutput, {
        props: {
          item: makeItem({ id: 'tool-cmd', kind: 'tool_call' }),
          meta: commandMeta({ command: 'go build ./...' }),
        },
      });

      expect(queryByTestId('dev-server-chip')).toBeNull();
      expect(probe).not.toHaveBeenCalled();
    });

    it('renders the chip once the probe confirms a listener and opens the URL externally', async () => {
      setBindingMock('ProbeDevServerURL', vi.fn(async () => true));
      const open = setBindingMock('OpenExternalURL', vi.fn(async () => undefined));

      const { getByTestId } = render(CommandOutput, {
        props: {
          item: makeItem({ id: 'tool-cmd', kind: 'tool_call' }),
          meta: commandMeta({ command: 'npm run dev', devServerUrl: 'http://localhost:5173/' }),
        },
      });

      const chip = await waitFor(() => getByTestId('dev-server-chip'));
      expect(chip.textContent).toContain('localhost:5173');

      await fireEvent.click(chip);
      expect(open).toHaveBeenCalledWith('http://localhost:5173/');
    });

    it('stays hidden when nothing is listening on a settled row, with no retry', async () => {
      vi.useFakeTimers();
      const probe = setBindingMock('ProbeDevServerURL', vi.fn(async () => false));

      const { queryByTestId } = render(CommandOutput, {
        props: {
          item: makeItem({ id: 'tool-cmd', kind: 'tool_call' }),
          meta: commandMeta({ command: 'cat notes.md', devServerUrl: 'http://localhost:5173/' }),
        },
      });

      await vi.advanceTimersByTimeAsync(0);
      expect(probe).toHaveBeenCalledWith('http://localhost:5173/');
      expect(queryByTestId('dev-server-chip')).toBeNull();

      // Settled row: a dead verdict is final — no retry timer exists.
      await vi.advanceTimersByTimeAsync(DEV_SERVER_PROBE_RETRY_MS * 3);
      expect(probe).toHaveBeenCalledTimes(1);
      expect(queryByTestId('dev-server-chip')).toBeNull();
    });

    it('treats a probe failure as not live', async () => {
      vi.useFakeTimers();
      const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
      const probe = setBindingMock(
        'ProbeDevServerURL',
        vi.fn(async () => {
          throw new Error('wire broke');
        }),
      );

      const { queryByTestId } = render(CommandOutput, {
        props: {
          item: makeItem({ id: 'tool-cmd', kind: 'tool_call' }),
          meta: commandMeta({ command: 'npm run dev', devServerUrl: 'http://localhost:5173/' }),
        },
      });

      await vi.advanceTimersByTimeAsync(0);
      expect(probe).toHaveBeenCalled();
      expect(queryByTestId('dev-server-chip')).toBeNull();
      expect(warn).toHaveBeenCalled();
      warn.mockRestore();
    });

    it('retries while the command is still running until the server answers', async () => {
      vi.useFakeTimers();
      let live = false;
      const probe = setBindingMock('ProbeDevServerURL', vi.fn(async () => live));

      const { queryByTestId } = render(CommandOutput, {
        props: {
          item: makeItem({ id: 'tool-cmd', kind: 'tool_call', status: 'running' }),
          meta: commandMeta({ command: 'npm run dev', devServerUrl: 'http://localhost:5173/' }),
        },
      });

      await vi.advanceTimersByTimeAsync(0);
      expect(probe).toHaveBeenCalledTimes(1);
      expect(queryByTestId('dev-server-chip')).toBeNull();

      live = true;
      await vi.advanceTimersByTimeAsync(DEV_SERVER_PROBE_RETRY_MS);
      expect(probe).toHaveBeenCalledTimes(2);
      expect(queryByTestId('dev-server-chip')).not.toBeNull();
    });

    // The row's thread is on another machine, so `localhost` here is not the
    // port the command announced. The probe binding asks the PAGE's backend
    // and therefore cannot answer for it; the machine's own list can, and
    // does, for both halves of the question the probe was asking.
    describe('when the command ran on another machine', () => {
      const THREAD = 'thread-on-laptop';

      function laptopPane() {
        return {
          paneId: 'pane-1',
          threadId: THREAD,
          expansionStateFor: () => ({
            expanded: false, displayData: '', loading: false, error: '',
            toggle() {}, sizeLabel: '', truncated: false,
            loadedBytes: 0, totalBytes: 0,
          }),
          leaseItemExpansion: () => () => {},
        } as unknown as import('../../stores/thread.svelte').ThreadPane;
      }

      function laptopFrame(overrides: Partial<DevServerList> = {}): DevServerList {
        return {
          servers: [{ port: 5173, allowed: true, source: 'attributed', listening: true }],
          previewHost: 'laptop.tail.ts.net',
          ...overrides,
        };
      }

      beforeEach(() => {
        resetWailsMocks();
        resetDevServersForTest();
        resetStagedBackends();
        noteThread(THREAD, 'laptop');
        stageBackend();
        initDevServers();
      });

      afterEach(() => {
        forgetThread(THREAD);
        resetDevServersForTest();
        resetStagedBackends();
      });

      it('never probes, and offers the chip once that machine says the port is live and shared', async () => {
        const probe = setBindingMock('ProbeDevServerURL', vi.fn(async () => true));
        const mint = setBindingMock(
          'MintPreviewURL',
          vi.fn(async () => 'https://laptop.tail.ts.net/preview/5173/app?t=1'),
        );
        const open = setBindingMock('OpenExternalURL', vi.fn(async () => undefined));

        const { queryByTestId } = render(CommandOutput, {
          props: {
            pane: laptopPane(),
            item: makeItem({ id: 'tool-cmd', kind: 'tool_call', status: 'running' }),
            meta: commandMeta({ command: 'npm run dev', devServerUrl: 'http://localhost:5173/app' }),
          },
        });

        expect(queryByTestId('dev-server-chip')).toBeNull();
        emitWailsEvent('devserver:list', laptopFrame(), REMOTE_BACKEND_UUID);

        const chip = await waitFor(() => {
          const found = queryByTestId('dev-server-chip');
          expect(found).not.toBeNull();
          return found as HTMLElement;
        });
        expect(chip.getAttribute('aria-label')).toBe('Open localhost:5173 on Laptop');
        expect(probe).not.toHaveBeenCalled();

        await fireEvent.click(chip);
        await waitFor(() => expect(mint).toHaveBeenCalledWith(THREAD, 5173, '/app'));
        await waitFor(() =>
          expect(open).toHaveBeenCalledWith('https://laptop.tail.ts.net/preview/5173/app?t=1'),
        );
      });

      it('offers nothing when that machine sees no listener on the port', async () => {
        setBindingMock('ProbeDevServerURL', vi.fn(async () => true));
        const { queryByTestId } = render(CommandOutput, {
          props: {
            pane: laptopPane(),
            item: makeItem({ id: 'tool-cmd', kind: 'tool_call', status: 'running' }),
            meta: commandMeta({ command: 'npm run dev', devServerUrl: 'http://localhost:5173/' }),
          },
        });

        emitWailsEvent(
          'devserver:list',
          laptopFrame({
            servers: [{ port: 5173, allowed: true, source: 'allowed', listening: false }],
          }),
          REMOTE_BACKEND_UUID,
        );

        await waitFor(() => expect(queryByTestId('command-output-command')).not.toBeNull());
        expect(queryByTestId('dev-server-chip')).toBeNull();
      });

      it('offers nothing when the port is live but not shared', async () => {
        setBindingMock('ProbeDevServerURL', vi.fn(async () => true));
        const { queryByTestId } = render(CommandOutput, {
          props: {
            pane: laptopPane(),
            item: makeItem({ id: 'tool-cmd', kind: 'tool_call', status: 'running' }),
            meta: commandMeta({ command: 'npm run dev', devServerUrl: 'http://localhost:5173/' }),
          },
        });

        emitWailsEvent(
          'devserver:list',
          laptopFrame({
            servers: [{ port: 5173, allowed: false, source: 'seen', listening: true }],
          }),
          REMOTE_BACKEND_UUID,
        );

        await waitFor(() => expect(queryByTestId('command-output-command')).not.toBeNull());
        expect(queryByTestId('dev-server-chip')).toBeNull();
      });
    });

    it('stops retrying an unconfirmed candidate after the dead-probe budget', async () => {
      vi.useFakeTimers();
      const probe = setBindingMock('ProbeDevServerURL', vi.fn(async () => false));

      render(CommandOutput, {
        props: {
          item: makeItem({ id: 'tool-cmd', kind: 'tool_call', status: 'running' }),
          meta: commandMeta({ command: 'tail -f app.log', devServerUrl: 'http://localhost:5173/' }),
        },
      });

      await vi.advanceTimersByTimeAsync(DEV_SERVER_PROBE_RETRY_MS * DEV_SERVER_PROBE_MAX_DEAD_PROBES);
      expect(probe).toHaveBeenCalledTimes(DEV_SERVER_PROBE_MAX_DEAD_PROBES);

      await vi.advanceTimersByTimeAsync(DEV_SERVER_PROBE_RETRY_MS * 5);
      expect(probe).toHaveBeenCalledTimes(DEV_SERVER_PROBE_MAX_DEAD_PROBES);
    });

    it('a settle mid-wait still delivers the pending probe as one final attempt', async () => {
      vi.useFakeTimers();
      let live = false;
      const probe = setBindingMock('ProbeDevServerURL', vi.fn(async () => live));
      const item = makeItem({ id: 'tool-cmd', kind: 'tool_call', status: 'running' });
      const devMeta = commandMeta({ command: 'npm run dev', devServerUrl: 'http://localhost:5173/' });

      const { queryByTestId, rerender } = render(CommandOutput, {
        props: { item, meta: devMeta },
      });

      await vi.advanceTimersByTimeAsync(0);
      expect(probe).toHaveBeenCalledTimes(1);

      // The command settles while the retry timer is pending; the server
      // bound in that window. The pending tick must still fire and win.
      live = true;
      await rerender({ item: { ...item, status: 'completed', updatedAt: item.updatedAt + 1 }, meta: devMeta });
      await vi.advanceTimersByTimeAsync(DEV_SERVER_PROBE_RETRY_MS);
      expect(probe).toHaveBeenCalledTimes(2);
      expect(queryByTestId('dev-server-chip')).not.toBeNull();

      // Settled: the loop schedules nothing further.
      await vi.advanceTimersByTimeAsync(DEV_SERVER_PROBE_VERIFY_MS * 2);
      expect(probe).toHaveBeenCalledTimes(2);
    });

    it('retracts a confirmed chip when its server dies mid-run', async () => {
      vi.useFakeTimers();
      let live = true;
      const probe = setBindingMock('ProbeDevServerURL', vi.fn(async () => live));

      const { queryByTestId } = render(CommandOutput, {
        props: {
          item: makeItem({ id: 'tool-cmd', kind: 'tool_call', status: 'running' }),
          meta: commandMeta({ command: 'npm run dev', devServerUrl: 'http://localhost:5173/' }),
        },
      });

      await vi.advanceTimersByTimeAsync(0);
      expect(queryByTestId('dev-server-chip')).not.toBeNull();

      live = false;
      await vi.advanceTimersByTimeAsync(DEV_SERVER_PROBE_VERIFY_MS);
      expect(probe).toHaveBeenCalledTimes(2);
      expect(queryByTestId('dev-server-chip')).toBeNull();

      // A retraction resumes the fast unconfirmed cadence while running.
      await vi.advanceTimersByTimeAsync(DEV_SERVER_PROBE_RETRY_MS);
      expect(probe).toHaveBeenCalledTimes(3);
    });

    it('unmount cancels the probe loop', async () => {
      vi.useFakeTimers();
      const probe = setBindingMock('ProbeDevServerURL', vi.fn(async () => false));

      const { unmount } = render(CommandOutput, {
        props: {
          item: makeItem({ id: 'tool-cmd', kind: 'tool_call', status: 'running' }),
          meta: commandMeta({ command: 'npm run dev', devServerUrl: 'http://localhost:5173/' }),
        },
      });

      await vi.advanceTimersByTimeAsync(0);
      expect(probe).toHaveBeenCalledTimes(1);

      unmount();
      await vi.advanceTimersByTimeAsync(DEV_SERVER_PROBE_RETRY_MS * 3);
      expect(probe).toHaveBeenCalledTimes(1);
    });

    it('opening the chip does not expand the row', async () => {
      setBindingMock('ProbeDevServerURL', vi.fn(async () => true));
      setBindingMock('OpenExternalURL', vi.fn(async () => undefined));

      const { getByTestId } = render(CommandOutput, {
        props: {
          item: makeItem({ id: 'tool-cmd', kind: 'tool_call' }),
          meta: commandMeta({ command: 'npm run dev', devServerUrl: 'http://localhost:5173/' }),
          payloadId: 'cmd-payload',
        },
      });

      await fireEvent.click(await waitFor(() => getByTestId('dev-server-chip')));

      expect(getByTestId('command-output-toggle').getAttribute('aria-expanded')).toBe('false');
    });

    it('falls back to the raw payloadMeta when no normalized meta prop is passed', async () => {
      setBindingMock('ProbeDevServerURL', vi.fn(async () => true));
      const { getByTestId } = render(CommandOutput, {
        props: {
          item: makeItem({
            id: 'tool-cmd',
            kind: 'tool_call',
            payloadMeta: JSON.stringify({ command: 'npm run dev', devServerUrl: 'http://127.0.0.1:3000/' }),
          }),
        },
      });

      await waitFor(() => expect(getByTestId('dev-server-chip').dataset.url).toBe('http://127.0.0.1:3000/'));
    });

    it('ignores a non-loopback URL that reached meta', () => {
      const probe = setBindingMock('ProbeDevServerURL', vi.fn(async () => true));
      const { queryByTestId } = render(CommandOutput, {
        props: {
          item: makeItem({ id: 'tool-cmd', kind: 'tool_call' }),
          meta: commandMeta({ command: 'npm run dev', devServerUrl: 'https://example.com/' }),
        },
      });

      expect(queryByTestId('dev-server-chip')).toBeNull();
      expect(probe).not.toHaveBeenCalled();
    });

    it('keeps the chip when a later streaming window carries no URL', async () => {
      setBindingMock('ProbeDevServerURL', vi.fn(async () => true));
      const item = makeItem({ id: 'tool-cmd', kind: 'tool_call', status: 'running' });
      const { getByTestId, rerender } = render(CommandOutput, {
        props: {
          item,
          meta: commandMeta({ command: 'npm run dev', devServerUrl: 'http://localhost:5173/' }),
        },
      });

      await waitFor(() => expect(getByTestId('dev-server-chip').dataset.url).toBe('http://localhost:5173/'));

      await rerender({
        item: { ...item, updatedAt: item.updatedAt + 1 },
        meta: commandMeta({ command: 'npm run dev' }),
      });

      expect(getByTestId('dev-server-chip').dataset.url).toBe('http://localhost:5173/');
    });

    it('upgrades to a newly detected URL', async () => {
      setBindingMock('ProbeDevServerURL', vi.fn(async () => true));
      const item = makeItem({ id: 'tool-cmd', kind: 'tool_call', status: 'running' });
      const { getByTestId, rerender } = render(CommandOutput, {
        props: {
          item,
          meta: commandMeta({ command: 'npm run dev', devServerUrl: 'http://localhost:5173/' }),
        },
      });

      await rerender({
        item: { ...item, updatedAt: item.updatedAt + 1 },
        meta: commandMeta({ command: 'npm run dev', devServerUrl: 'http://localhost:4173/' }),
      });

      await waitFor(() => expect(getByTestId('dev-server-chip').dataset.url).toBe('http://localhost:4173/'));
    });

    it('retains a confirmed chip when a newly mentioned URL probes dead', async () => {
      vi.useFakeTimers();
      const liveByUrl: Record<string, boolean> = {
        'http://localhost:5173/': true,
        'http://localhost:9999/': false,
      };
      setBindingMock('ProbeDevServerURL', vi.fn(async (url: string) => liveByUrl[url] === true));
      const item = makeItem({ id: 'tool-cmd', kind: 'tool_call', status: 'running' });

      const { getByTestId, rerender } = render(CommandOutput, {
        props: {
          item,
          meta: commandMeta({ command: 'npm run dev', devServerUrl: 'http://localhost:5173/' }),
        },
      });

      await vi.advanceTimersByTimeAsync(0);
      expect(getByTestId('dev-server-chip').dataset.url).toBe('http://localhost:5173/');

      // Later output merely MENTIONS another loopback URL. The verified
      // chip must not blank while the new candidate fails to confirm.
      await rerender({
        item: { ...item, updatedAt: item.updatedAt + 1 },
        meta: commandMeta({ command: 'npm run dev', devServerUrl: 'http://localhost:9999/' }),
      });
      await vi.advanceTimersByTimeAsync(DEV_SERVER_PROBE_RETRY_MS * 3);
      expect(getByTestId('dev-server-chip').dataset.url).toBe('http://localhost:5173/');
    });
  });
});

describe('<CommandOutput> background button (agent-visibility)', () => {
  beforeEach(() => {
    resetBindingMocks();
  });
  afterEach(() => cleanup());

  // Minimal pane: canBackground only asks for presence; the expansion
  // registry is exercised through the pane branch.
  function fakePane() {
    const handles = new Map<string, unknown>();
    return {
      paneId: 'pane-1',
      expansionStateFor(item: { id: string }) {
        let h = handles.get(item.id);
        if (!h) {
          h = { expanded: false, displayData: '', loading: false, error: '', toggle() {}, sizeLabel: '', truncated: false, loadedBytes: 0, totalBytes: 0 };
          handles.set(item.id, h);
        }
        return h;
      },
      leaseItemExpansion() { return () => {}; },
    } as unknown as import('../../stores/thread.svelte').ThreadPane;
  }

  it('shows the button on a running foreground Claude Bash launch and fires BackgroundClaudeTask', async () => {
    const background = vi.fn(async () => {});
    setBindingMock('BackgroundClaudeTask', background);
    const item = makeItem({
      id: 'bash-1',
      kind: 'tool_call',
      status: 'running',
      toolName: 'Bash',
      summary: 'pnpm test',
    });
    const { getByTestId } = render(CommandOutput, {
      props: { pane: fakePane(), item, meta: commandMeta() },
    });
    await fireEvent.click(getByTestId('command-output-background-button'));
    await waitFor(() => expect(background).toHaveBeenCalledWith('thread-1', 'bash-1'));
  });

  it('hides the button once the launch is already backgrounded, settled, or Codex', () => {
    const backgrounded = makeItem({ id: 'b1', kind: 'tool_call', status: 'running', toolName: 'Bash', isBackground: true });
    const settled = makeItem({ id: 'b2', kind: 'tool_call', status: 'completed', toolName: 'Bash' });
    const codex = makeItem({ id: 'b3', kind: 'tool_call', status: 'running', toolName: 'command_execution' });
    for (const item of [backgrounded, settled, codex]) {
      const { queryByTestId, unmount } = render(CommandOutput, {
        props: { pane: fakePane(), item, meta: commandMeta() },
      });
      expect(queryByTestId('command-output-background-button')).toBeNull();
      unmount();
    }
  });

  it('surfaces a failed background request inline', async () => {
    setBindingMock('BackgroundClaudeTask', vi.fn(async () => {
      throw new Error('control request timed out');
    }));
    const item = makeItem({ id: 'bash-9', kind: 'tool_call', status: 'running', toolName: 'Bash' });
    const { getByTestId } = render(CommandOutput, {
      props: { pane: fakePane(), item, meta: commandMeta() },
    });
    await fireEvent.click(getByTestId('command-output-background-button'));
    await waitFor(() =>
      expect(getByTestId('command-output-background-error').textContent).toContain('control request timed out'),
    );
  });
});
