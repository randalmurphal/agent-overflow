import { afterEach, beforeAll, beforeEach, describe, expect, it } from 'vitest';
import { cleanup, fireEvent, render } from '@testing-library/svelte';
import CommandOutput from './CommandOutput.svelte';
import { makeItem } from '../../../test/helpers/chat';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import type { CommandOutputMeta } from '../../types/models';

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

  it('renders ANSI escapes in the preview as styled spans, not raw escape text', async () => {
    // The body carries the \x1b[31m…\x1b[0m sequence as raw bytes.
    // ansiToHtml converts it to <span class="text-red-400">red</span>;
    // if the conversion path broke, the literal escape string would
    // survive into the DOM and the user would see garbage.
    //
    // Empty payloadId short-circuits the lazy payload-preview fetch
    // (loadPreview bails on falsy id), so displayData stays null and
    // the template falls through to the inline meta.preview — which is
    // the path we want to pin through ansiToHtml.
    const ansiPreview = '\x1b[31mred\x1b[0m then plain';
    const { getByRole, container } = render(CommandOutput, {
      props: {
        item: makeItem({ id: 'tool-cmd', kind: 'tool_call' }),
        meta: commandMeta({ command: 'ls', preview: ansiPreview, lineCount: 1 }),
        payloadId: '',
      },
    });

    // Expand so the <pre> containing the preview mounts.
    await fireEvent.click(getByRole('button', { name: /Toggle command output/i }));
    await Promise.resolve();
    await Promise.resolve();

    const pre = container.querySelector('pre');
    if (!pre) throw new Error('expected <pre> for the command output body');

    const styledSpans = pre.querySelectorAll('span.text-red-400');
    expect(styledSpans.length).toBeGreaterThan(0);
    expect(styledSpans[0]?.textContent).toBe('red');

    // The raw escape code must NOT survive into the rendered DOM.
    expect(pre.textContent).toContain('red');
    expect(pre.textContent).toContain(' then plain');
    expect(pre.innerHTML).not.toContain('\u001b[');
    expect(pre.textContent).not.toContain('\u001b[');
  });

  it('escapes HTML-looking text even when no ANSI codes are present', async () => {
    // Defensive pair: ansiToHtml's "no escapes" fast path still has to
    // escape raw HTML. A regression that printed the preview verbatim
    // via {@html} would render the script tag as a real node.
    const { getByRole, container } = render(CommandOutput, {
      props: {
        item: makeItem({ id: 'tool-cmd', kind: 'tool_call' }),
        meta: commandMeta({
          command: 'echo',
          preview: '<script>alert(1)</script>',
          lineCount: 1,
        }),
        payloadId: '',
      },
    });
    await fireEvent.click(getByRole('button', { name: /Toggle command output/i }));
    await Promise.resolve();
    await Promise.resolve();

    const pre = container.querySelector('pre');
    if (!pre) throw new Error('expected <pre>');
    // jsdom normalizes innerHTML; we check the HTML source directly
    // so a literal <script> tag would still fail the assertion.
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
      totalSize: 2048,
      isComplete: false,
    }));
    const dataMock = setBindingMock('GetPayloadData', async () => 'full body');

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
    expect(previewMock).toHaveBeenCalledWith('pay-3', 32768);
    expect(dataMock).not.toHaveBeenCalled();
  });
});
