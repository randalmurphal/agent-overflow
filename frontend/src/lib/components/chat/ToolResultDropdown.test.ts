import { afterEach, beforeAll, describe, expect, it } from 'vitest';
import { render, fireEvent, cleanup } from '@testing-library/svelte';
import { tick } from 'svelte';
import ToolResultDropdown from './ToolResultDropdown.svelte';
import type { Item, PayloadMeta } from '../../types/models';
import { getBindingMock, setBindingMock } from '../../../test/mocks/bindings-app';

// happy-dom doesn't implement Element.animate, which Svelte's `slide`
// transition hits when the dropdown body appears. Install the same
// stub the DiffPanelDrawer tests use so the first expand doesn't
// throw out of the rendering microtask.
beforeAll(() => {
  if (typeof (Element.prototype as unknown as { animate?: unknown }).animate !== 'function') {
    (Element.prototype as unknown as { animate: (...args: unknown[]) => unknown }).animate =
      function fakeAnimate() {
        let onfinish: (() => void) | null = null;
        return {
          finished: Promise.resolve(),
          currentTime: 0,
          playState: 'finished' as const,
          cancel() {},
          finish() { onfinish?.(); },
          play() {},
          pause() {},
          reverse() {},
          addEventListener(type: string, cb: EventListener) {
            if (type === 'finish') onfinish = cb as unknown as () => void;
          },
          removeEventListener() {},
          get onfinish() { return onfinish; },
          set onfinish(cb: (() => void) | null) {
            onfinish = cb;
            if (cb) queueMicrotask(cb);
          },
        };
      };
  }
});

/**
 * Item is being extended by the schema agent to carry a `status`
 * field. The component reads it defensively, so stubs here don't need
 * to set it unless a specific behaviour (running vs failed badge)
 * matters.
 */
type DropdownItem = Item & { status?: 'running' | 'completed' | 'failed' };

function item(overrides: Partial<DropdownItem> & { id: string }): DropdownItem {
  return {
    threadId: 't1',
    turnIndex: 0,
    itemIndex: 0,
    kind: 'tool_result',
    role: 'assistant',
    summary: 'Bash',
    payloadId: 'p1',
    createdAt: 0,
    ...overrides,
  } as DropdownItem;
}

function payloadMeta(overrides: Partial<PayloadMeta> & { meta: string }): PayloadMeta {
  return {
    id: 'p1',
    kind: 'tool_result',
    createdAt: 0,
    ...overrides,
  };
}

async function flush(): Promise<void> {
  await tick();
  // Let any queued microtasks (the GetPayloadData await chain) drain.
  await Promise.resolve();
  await Promise.resolve();
  await tick();
}

describe('<ToolResultDropdown>', () => {
  afterEach(() => {
    cleanup();
  });

  it('renders the summary row and no body on initial mount', () => {
    const { getByTestId, queryByTestId } = render(ToolResultDropdown, {
      props: { item: item({ id: 'i1' }) },
    });
    const toggle = getByTestId('tool-result-dropdown-toggle');
    expect(toggle.getAttribute('aria-expanded')).toBe('false');
    expect(toggle.textContent).toContain('Bash');
    // Body is hidden until the caret is clicked.
    expect(queryByTestId('tool-result-dropdown-body')).toBeNull();
  });

  it('shows exit code from payload meta in the summary row', () => {
    const { getByTestId } = render(ToolResultDropdown, {
      props: {
        item: item({ id: 'i1' }),
        payloadMeta: payloadMeta({
          id: 'p1',
          meta: JSON.stringify({ exitCode: 1 }),
        }),
      },
    });
    expect(getByTestId('tool-result-dropdown-exit').textContent).toContain('exit 1');
  });

  it('fetches the payload on first expand and renders the body', async () => {
    const body = 'line 1\nline 2\nline 3';
    // Gate the fetch behind a deferred promise so we can assert that the
    // loading state actually renders before the body arrives.
    let resolveFetch: (value: string) => void = () => {};
    const pending = new Promise<string>((resolve) => {
      resolveFetch = resolve;
    });
    setBindingMock('GetPayloadData', () => pending);

    const { getByTestId } = render(ToolResultDropdown, {
      props: { item: item({ id: 'i1', payloadId: 'p1' }) },
    });

    const toggle = getByTestId('tool-result-dropdown-toggle');
    await fireEvent.click(toggle);
    // With the fetch still pending the body should advertise loading.
    expect(getByTestId('tool-result-dropdown-loading')).toBeInTheDocument();

    resolveFetch(body);
    await flush();

    expect(toggle.getAttribute('aria-expanded')).toBe('true');
    expect(getByTestId('tool-result-dropdown-output').textContent).toBe(body);
    const mock = getBindingMock('GetPayloadData');
    expect(mock).toHaveBeenCalledTimes(1);
    expect(mock?.mock.calls[0]).toEqual(['p1']);
  });

  it('collapses on second click and does not refetch when reopened', async () => {
    const body = 'CACHED';
    setBindingMock('GetPayloadData', async () => body);

    const { getByTestId, queryByTestId } = render(ToolResultDropdown, {
      props: { item: item({ id: 'i1', payloadId: 'p1' }) },
    });

    const toggle = getByTestId('tool-result-dropdown-toggle');
    await fireEvent.click(toggle);
    await flush();
    expect(getByTestId('tool-result-dropdown-output').textContent).toBe(body);

    // Collapse.
    await fireEvent.click(toggle);
    expect(toggle.getAttribute('aria-expanded')).toBe('false');
    expect(queryByTestId('tool-result-dropdown-body')).toBeNull();

    // Re-expand. The cached body should render again without a second fetch.
    await fireEvent.click(toggle);
    await flush();
    expect(getByTestId('tool-result-dropdown-output').textContent).toBe(body);
    expect(getBindingMock('GetPayloadData')).toHaveBeenCalledTimes(1);
  });

  it('surfaces a fetch failure inline without retrying automatically', async () => {
    setBindingMock('GetPayloadData', async () => {
      throw new Error('network boom');
    });
    const { getByTestId } = render(ToolResultDropdown, {
      props: { item: item({ id: 'i1', payloadId: 'p1' }) },
    });

    await fireEvent.click(getByTestId('tool-result-dropdown-toggle'));
    await flush();

    const errorNode = getByTestId('tool-result-dropdown-error');
    expect(errorNode.getAttribute('role')).toBe('alert');
    expect(errorNode.textContent).toContain('network boom');
    expect(getBindingMock('GetPayloadData')).toHaveBeenCalledTimes(1);
  });
});
