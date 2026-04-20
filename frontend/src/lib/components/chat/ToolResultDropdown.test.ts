import { afterEach, beforeAll, beforeEach, describe, expect, it } from 'vitest';
import { cleanup, fireEvent, render } from '@testing-library/svelte';
import ToolResultDropdown from './ToolResultDropdown.svelte';
import { makeItem } from '../../../test/helpers/chat';
import { getBindingMock, resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { DEFAULT_PAYLOAD_PREVIEW_BYTES } from './payloadExpansion.svelte';

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

describe('<ToolResultDropdown>', () => {
  beforeEach(() => {
    resetBindingMocks();
  });

  afterEach(() => {
    cleanup();
  });

  it('renders the summary row from the item', () => {
    const { getByTestId } = render(ToolResultDropdown, {
      props: {
        item: makeItem({
          id: 'tool-1',
          kind: 'tool_call',
          summary: 'Bash: ls -la',
          status: 'running',
        }),
      },
    });

    expect(getByTestId('tool-result-dropdown-toggle').textContent).toContain('Bash: ls -la');
  });

  it('loads payload data on expand and shows the body', async () => {
    setBindingMock('GetPayloadPreview', async () => ({
      data: 'line 1\nline 2',
      totalSize: 20,
      isComplete: true,
    }));
    setBindingMock('GetPayloadData', async () => 'unused');
    const { getByTestId } = render(ToolResultDropdown, {
      props: {
        item: makeItem({
          id: 'tool-1',
          kind: 'tool_call',
          payloadId: 'payload-1',
          payloadMeta: JSON.stringify({ exitCode: 1 }),
        }),
      },
    });

    await fireEvent.click(getByTestId('tool-result-dropdown-toggle'));

    expect(getBindingMock('GetPayloadPreview')).toHaveBeenCalledWith('payload-1', DEFAULT_PAYLOAD_PREVIEW_BYTES);
    expect(getBindingMock('GetPayloadData')).not.toHaveBeenCalled();
    expect(getByTestId('tool-result-dropdown-output').textContent).toContain('line 1');
    expect(getByTestId('tool-result-dropdown-exit').textContent).toContain('exit 1');
  });

  it('shows an inline error when payload loading fails', async () => {
    setBindingMock('GetPayloadPreview', async () => {
      throw new Error('boom');
    });
    const { getByTestId } = render(ToolResultDropdown, {
      props: {
        item: makeItem({
          id: 'tool-1',
          kind: 'tool_call',
          payloadId: 'payload-1',
        }),
      },
    });

    await fireEvent.click(getByTestId('tool-result-dropdown-toggle'));

    expect(getByTestId('tool-result-dropdown-error').textContent).toContain('boom');
  });

  it('discards expanded payload data on collapse and refetches preview on re-open', async () => {
    setBindingMock('GetPayloadPreview', async () => ({
      data: 'preview body',
      totalSize: 128 * 1024,
      isComplete: false,
    }));
    setBindingMock('GetPayloadData', async () => 'full body');
    const { getByTestId } = render(ToolResultDropdown, {
      props: {
        item: makeItem({
          id: 'tool-1',
          kind: 'tool_call',
          payloadId: 'payload-1',
        }),
      },
    });

    await fireEvent.click(getByTestId('tool-result-dropdown-toggle'));
    await Promise.resolve();
    await Promise.resolve();
    expect(getByTestId('tool-result-dropdown-show-full').textContent).toContain('128.0 KB');

    await fireEvent.click(getByTestId('tool-result-dropdown-show-full'));
    await Promise.resolve();
    await Promise.resolve();
    expect(getBindingMock('GetPayloadData')).toHaveBeenCalledTimes(1);
    expect(getByTestId('tool-result-dropdown-output').textContent).toContain('full body');

    await fireEvent.click(getByTestId('tool-result-dropdown-toggle'));
    await fireEvent.click(getByTestId('tool-result-dropdown-toggle'));
    await Promise.resolve();
    await Promise.resolve();

    expect(getBindingMock('GetPayloadPreview')).toHaveBeenCalledTimes(2);
  });

  it('renders the approval decision chip when present', () => {
    const { getByTestId } = render(ToolResultDropdown, {
      props: {
        item: makeItem({
          id: 'tool-1',
          kind: 'tool_call',
          summary: 'Bash: rm -rf tmp',
          decision: 'declined',
        }),
      },
    });

    expect(getByTestId('tool-decision-chip').textContent).toContain('Declined');
  });
});
