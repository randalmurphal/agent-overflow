import { afterEach, beforeAll, beforeEach, describe, expect, it } from 'vitest';
import { cleanup, fireEvent, render } from '@testing-library/svelte';
import CommandOutput from './CommandOutput.svelte';
import { makeItem } from '../../../test/helpers/chat';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import type { CommandOutputMeta } from '../../types/models';
import { DEFAULT_PAYLOAD_PREVIEW_BYTES } from './payloadExpansion.svelte';

// Some Svelte transitions call Element.prototype.animate; jsdom doesn't
// implement it. Stub it the same way ToolResultDropdown.test.ts does so
// expand/collapse doesn't throw.
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
    command: 'npm test',
    exitCode: 0,
    lineCount: 1,
    preview: '',
    ...overrides,
  };
}

describe('<CommandOutput>', () => {
  beforeEach(() => {
    resetBindingMocks();
  });

  afterEach(() => {
    cleanup();
  });

  it('paints server-rendered ANSI html in the expanded <pre>', async () => {
    // The Go-side ANSI renderer emits stable `term-*` classes around
    // colored segments. CommandOutput paints the HTML via {@html}, so we
    // stub GetPayloadPreview to hand back the span the server would
    // produce, then verify it lands in the DOM under the expected class.
    setBindingMock('GetPayloadPreview', async () => ({
      data: '\x1b[31mred\x1b[0m then plain',
      html: '<span class="term-fg31">red</span> then plain',
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
    const styledSpans = pre.querySelectorAll('span.term-fg31');
    expect(styledSpans.length).toBeGreaterThan(0);
    expect(styledSpans[0]?.textContent).toBe('red');
    expect(pre.textContent).toContain(' then plain');
    // The raw escape must not land in the DOM — server strips it.
    expect(pre.innerHTML).not.toContain('\u001b[');
  });

  it('paints server-escaped HTML so script tags never become live nodes', async () => {
    // The Go renderer escapes HTML before handing bytes to the frontend,
    // so the html channel is safe to drop through {@html}. This test
    // pins that contract by stubbing what the backend would return for
    // a dangerous payload and verifying no live <script> ends up in
    // the DOM.
    setBindingMock('GetPayloadPreview', async () => ({
      data: '<script>alert(1)</script>',
      html: '&lt;script&gt;alert(1)&lt;/script&gt;',
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
    // The preview is rendered inline from meta; GetPayloadData should
    // only fire on explicit "Show full output" click. This pins the
    // lazy-content guarantee that keeps memory bounded.
    const previewMock = setBindingMock('GetPayloadPreview', async () => ({
      data: '\u001b[32mok\u001b[0m',
      html: '<span class="term-fg32">ok</span>',
      totalSize: 2048,
      isComplete: false,
    }));
    const dataMock = setBindingMock('GetPayloadData', async () => ({ data: 'full body', html: 'full body' }));

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
    expect(dataMock).not.toHaveBeenCalled();

    await fireEvent.click(getByRole('button', { name: /Toggle command output/i }));
    await Promise.resolve();
    await Promise.resolve();
    // Expand loads the preview but NOT the full body.
    expect(previewMock).toHaveBeenCalledWith('pay-3', DEFAULT_PAYLOAD_PREVIEW_BYTES);
    expect(dataMock).not.toHaveBeenCalled();
  });
});
