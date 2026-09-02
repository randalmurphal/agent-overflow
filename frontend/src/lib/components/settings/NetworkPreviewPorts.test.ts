import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import NetworkPreviewPorts from './NetworkPreviewPorts.svelte';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { emitWailsEvent, resetWailsMocks } from '../../../test/mocks/wailsio-runtime';
import { pairViewOnly, resetToLocalPage } from '../../../test/helpers/scopes';
import { __resetScopesForTest } from '../../transport/scopes';
import { initDevServers, resetDevServersForTest } from '../../stores/devServers.svelte';
import type { DevServerList } from '../../stores/devServers.svelte';

// Nothing here is applied optimistically. Sharing a port is the machine's
// decision — it is the one that has to open a listener — so every case
// presses a control, asserts the call, and then lets the backend's next
// frame be what moves the list.

function frame(ports: number[], previewHost = 'desk.tail.ts.net'): DevServerList {
  return {
    previewHost,
    servers: ports.map((port) => ({
      port,
      allowed: true,
      source: 'allowed' as const,
      listening: true,
    })),
  };
}

// The pane's own read resolves a microtask after mount, so a pushed frame
// stated before it lands would be overwritten by the answer already in
// flight. Every case that pushes one mounts through here first.
async function mounted(initial: DevServerList = frame([])) {
  const read = setBindingMock('GetDevServers', vi.fn(async () => initial));
  const view = render(NetworkPreviewPorts);
  await waitFor(() => expect(read).toHaveBeenCalled());
  await new Promise((resolve) => setTimeout(resolve, 0));
  return view;
}

describe('<NetworkPreviewPorts>', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetWailsMocks();
    resetDevServersForTest();
    resetToLocalPage();
    setBindingMock('GetDevServers', async () => frame([]));
  });

  afterEach(() => {
    resetDevServersForTest();
    resetToLocalPage();
    __resetScopesForTest();
  });

  it('reads the list on mount and says so while nothing is shared', async () => {
    const read = setBindingMock('GetDevServers', vi.fn(async () => frame([])));
    const { getByTestId, queryByTestId } = render(NetworkPreviewPorts);

    await waitFor(() => expect(read).toHaveBeenCalled());
    expect(getByTestId('preview-ports-empty')).toBeTruthy();
    expect(queryByTestId('preview-ports-list')).toBeNull();
  });

  it('issues nothing from a session that was not granted the list', async () => {
    const read = setBindingMock('GetDevServers', vi.fn(async () => frame([])));
    await pairViewOnly();
    render(NetworkPreviewPorts);
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(read).not.toHaveBeenCalled();
  });

  it('renders a row per shared port, in the order the machine sent them', async () => {
    initDevServers();
    const { getAllByTestId } = await mounted();
    emitWailsEvent('devserver:list', frame([5173, 3000]));

    await waitFor(() => expect(getAllByTestId('preview-port-row')).toHaveLength(2));
    expect(getAllByTestId('preview-port-row').map((row) => row.dataset.port)).toEqual([
      '5173',
      '3000',
    ]);
  });

  it('shares a typed port and clears the field, leaving the list to the next frame', async () => {
    initDevServers();
    const allow = setBindingMock('AllowPreviewPort', vi.fn(async () => undefined));
    const { getByTestId, queryByTestId } = await mounted();

    const input = getByTestId('preview-port-input') as HTMLInputElement;
    await fireEvent.input(input, { target: { value: '5173' } });
    await fireEvent.click(getByTestId('preview-port-add'));

    expect(allow).toHaveBeenCalledWith(5173);
    await waitFor(() => expect(input.value).toBe(''));
    expect(queryByTestId('preview-port-row')).toBeNull();

    emitWailsEvent('devserver:list', frame([5173]));
    await waitFor(() => expect(getByTestId('preview-port-row')).toBeTruthy());
  });

  it('stops sharing a port from its own row', async () => {
    initDevServers();
    const disallow = setBindingMock('DisallowPreviewPort', vi.fn(async () => undefined));
    const { getByTestId } = await mounted();
    emitWailsEvent('devserver:list', frame([5173]));

    await waitFor(() => expect(getByTestId('preview-port-row')).toBeTruthy());
    await fireEvent.click(getByTestId('preview-port-remove'));
    expect(disallow).toHaveBeenCalledWith(5173);
  });

  it.each([
    ['not a number', 'abc', 'Enter a number.'],
    ['zero', '0', 'Enter a port between 1 and 65535.'],
    ['past the top of the range', '65536', 'Enter a port between 1 and 65535.'],
  ])('refuses %s before calling anything', async (_name, typed, message) => {
    const allow = setBindingMock('AllowPreviewPort', vi.fn(async () => undefined));
    const { getByTestId } = render(NetworkPreviewPorts);

    await fireEvent.input(getByTestId('preview-port-input'), { target: { value: typed } });
    await waitFor(() => expect(getByTestId('preview-port-error').textContent?.trim()).toBe(message));
    expect(getByTestId('preview-port-add')).toBeDisabled();
    expect(allow).not.toHaveBeenCalled();
  });

  it('refuses a port that is already shared, rather than calling to no effect', async () => {
    initDevServers();
    const { getByTestId } = await mounted();
    emitWailsEvent('devserver:list', frame([5173]));

    await fireEvent.input(getByTestId('preview-port-input'), { target: { value: '5173' } });
    await waitFor(() =>
      expect(getByTestId('preview-port-error').textContent?.trim()).toBe(
        'That port is already shared.',
      ),
    );
    expect(getByTestId('preview-port-add')).toBeDisabled();
  });

  it('says when the machine has no address to serve any of them on', async () => {
    initDevServers();
    const { getByTestId, queryByTestId } = await mounted();

    emitWailsEvent('devserver:list', frame([5173], ''));
    await waitFor(() => expect(getByTestId('preview-ports-no-address')).toBeTruthy());

    emitWailsEvent('devserver:list', frame([5173]));
    await waitFor(() => expect(queryByTestId('preview-ports-no-address')).toBeNull());
  });

  it('shows a refused change as a sentence rather than losing it', async () => {
    initDevServers();
    setBindingMock('AllowPreviewPort', async () => {
      throw new Error('port already in use');
    });
    const { getByTestId } = await mounted();

    await fireEvent.input(getByTestId('preview-port-input'), { target: { value: '5173' } });
    await fireEvent.click(getByTestId('preview-port-add'));

    await waitFor(() =>
      expect(getByTestId('preview-port-action-error').textContent?.trim()).toBe(
        'Port already in use.',
      ),
    );
  });
});
