import { fireEvent, render } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';
import type { ThreadPane } from '../../stores/thread.svelte';
import type { PayloadExpansionHandle } from '../../utils/payloadExpansion.svelte';
import ExpandablePayloadBody from './ExpandablePayloadBody.svelte';

function expansionHandle(overrides: Partial<PayloadExpansionHandle> = {}): PayloadExpansionHandle {
  return {
    expanded: true,
    loading: false,
    error: null,
    previewData: 'visible output',
    fullData: null,
    totalSize: 128 * 1024,
    isComplete: false,
    hasMore: true,
    payloadVersion: 'version-a',
    displayData: 'visible output',
    toggle: vi.fn(async () => {}),
    expand: vi.fn(async () => {}),
    ensureLoaded: vi.fn(async () => true),
    collapse: vi.fn(() => {}),
    showFull: vi.fn(async () => {}),
    retry: vi.fn(async () => {}),
    reset: vi.fn(() => {}),
    setPayloadVersion: vi.fn(() => {}),
    ...overrides,
  };
}

describe('<ExpandablePayloadBody>', () => {
  it('preserves the clicked show-more anchor while loading the rest of a payload', async () => {
    const expansion = expansionHandle();
    const preserveScrollAnchor = vi.fn(async (_anchor: HTMLElement, action: () => void | Promise<void>) => {
      await action();
    });
    const pane = {
      scrollController: { preserveScrollAnchor },
    } as unknown as ThreadPane;

    const { getByTestId } = render(ExpandablePayloadBody, {
      props: {
        pane,
        expansion,
        id: 'payload-body',
        testPrefix: 'payload-body',
        emptyMessage: 'No output.',
      },
    });

    const button = getByTestId('payload-body-show-full');
    await fireEvent.click(button);

    expect(preserveScrollAnchor).toHaveBeenCalledWith(button, expect.any(Function));
    expect(expansion.showFull).toHaveBeenCalledTimes(1);
  });

  it('renders the default AnsiText output when no renderContent snippet is provided', () => {
    const expansion = expansionHandle({
      displayData: 'plain ansi line',
      hasMore: false,
    });
    const { getByTestId } = render(ExpandablePayloadBody, {
      props: {
        expansion,
        id: 'payload-body',
        testPrefix: 'payload-body',
        emptyMessage: 'No output.',
      },
    });

    const output = getByTestId('payload-body-output');
    // The default branch wraps AnsiText in an `ansi-body` shell; pin
    // the class so a consumer that needs to skin the default branch
    // (rather than override it) has a stable hook.
    expect(output.className).toContain('ansi-body');
    expect(output.textContent).toContain('plain ansi line');
  });
});
