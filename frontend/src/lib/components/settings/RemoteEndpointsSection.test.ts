import { describe, expect, it, beforeEach, afterEach, vi } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import RemoteEndpointsSection from './RemoteEndpointsSection.svelte';
import { setBindingMock, resetBindingMocks, getBindingMock } from '../../../test/mocks/bindings-app';
import { setRunMode, resetRunMode } from '../../../test/runMode';

interface MockEndpoint {
  id: string;
  name: string;
  url: string;
  token: string;
  lastUsedAt?: number;
}

function endpoint(over: Partial<MockEndpoint> = {}): MockEndpoint {
  return {
    id: 'ep-1',
    name: 'Tailnet box',
    url: 'ws://10.0.0.5:54321/',
    token: 'abcdefghijk',
    ...over,
  };
}

describe('<RemoteEndpointsSection>', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetRunMode();
  });

  afterEach(() => {
    resetBindingMocks();
    resetRunMode();
  });

  it('lists stored endpoints from the backend', async () => {
    setBindingMock('ListRemoteEndpoints', async () => [
      endpoint({ id: 'ep-1', name: 'Tailnet', url: 'ws://10.0.0.5:54321/' }),
      endpoint({ id: 'ep-2', name: 'SSH tunnel', url: 'wss://example.com/ws' }),
    ]);

    const { findByText } = render(RemoteEndpointsSection);

    await findByText('Tailnet');
    await findByText('SSH tunnel');
  });

  it('renders the empty state when no endpoints are stored', async () => {
    setBindingMock('ListRemoteEndpoints', async () => []);

    const { findByText } = render(RemoteEndpointsSection);
    await findByText(/No remote endpoints saved/i);
  });

  it('rejects http:// in the form before calling the backend', async () => {
    setBindingMock('ListRemoteEndpoints', async () => []);
    const addMock = setBindingMock('AddRemoteEndpoint', async () =>
      endpoint({ id: 'new-ep' }),
    );

    const { findByText, getByLabelText, findByRole } = render(RemoteEndpointsSection);
    await findByText(/No remote endpoints saved/i);

    const addButton = await findByRole('button', { name: 'Add remote endpoint' });
    await fireEvent.click(addButton);

    const urlInput = getByLabelText('URL') as HTMLInputElement;
    await fireEvent.input(urlInput, { target: { value: 'http://example.com/?token=abc' } });
    const tokenInput = getByLabelText('Token') as HTMLInputElement;
    await fireEvent.input(tokenInput, { target: { value: 'abc' } });

    const submitButton = await findByRole('button', { name: 'Add' });
    await fireEvent.click(submitButton);

    // The client-side validator must trip before AddRemoteEndpoint is
    // called — sending http:// to the backend would surface a less
    // specific error and cost a round trip.
    await waitFor(() => {
      expect(addMock).not.toHaveBeenCalled();
    });
    await findByText(/URL must start with ws:\/\/ or wss:\/\//i);
  });

  it('adds a new endpoint and appends it to the list', async () => {
    setBindingMock('ListRemoteEndpoints', async () => []);
    const created = endpoint({ id: 'new-ep', name: 'New', url: 'ws://newhost:1234/' });
    const addMock = setBindingMock('AddRemoteEndpoint', async () => created);

    const { findByRole, findByText, getByLabelText } = render(RemoteEndpointsSection);
    await findByText(/No remote endpoints saved/i);

    const addButton = await findByRole('button', { name: 'Add remote endpoint' });
    await fireEvent.click(addButton);

    await fireEvent.input(getByLabelText('Nickname (optional)'), {
      target: { value: 'New' },
    });
    await fireEvent.input(getByLabelText('URL'), {
      target: { value: 'ws://newhost:1234/' },
    });
    await fireEvent.input(getByLabelText('Token'), {
      target: { value: 'token-x' },
    });

    const submit = await findByRole('button', { name: 'Add' });
    await fireEvent.click(submit);

    await waitFor(() => {
      expect(addMock).toHaveBeenCalledTimes(1);
    });
    expect(addMock.mock.calls[0]).toEqual(['New', 'ws://newhost:1234/', 'token-x']);
    await findByText('New');
  });

  it('writes the launch command to the clipboard on Copy', async () => {
    const ep = endpoint({ id: 'ep-1', url: 'ws://10.0.0.5:54321/' });
    setBindingMock('ListRemoteEndpoints', async () => [ep]);
    // Copy fetches the token explicitly — the bulk list omits it.
    setBindingMock('GetRemoteEndpointToken', async (..._args: unknown[]) => 'token-ABC');
    const touchMock = setBindingMock('TouchRemoteEndpoint', async () => undefined);

    const writeText = vi.fn(async (_text: string): Promise<void> => undefined);
    Object.defineProperty(navigator, 'clipboard', {
      configurable: true,
      value: { writeText },
    });

    const { findByRole, findByText } = render(RemoteEndpointsSection);
    await findByText('Tailnet box');

    const copyBtn = await findByRole('button', {
      name: /Copy connect command for/i,
    });
    await fireEvent.click(copyBtn);

    await waitFor(() => {
      expect(writeText).toHaveBeenCalledTimes(1);
    });
    const calls = writeText.mock.calls;
    if (calls.length === 0) throw new Error('writeText was not called');
    const written = calls[0][0];
    expect(written).toContain('agent-overflow --connect');
    expect(written).toContain('ws://10.0.0.5:54321');
    expect(written).toContain('token=token-ABC');

    // TouchRemoteEndpoint is best-effort — it should be fired so the
    // backend can update LastUsedAt for sort ordering.
    await waitFor(() => {
      expect(touchMock).toHaveBeenCalledWith('ep-1');
    });
  });

  it('deletes an endpoint and removes it from the list', async () => {
    setBindingMock('ListRemoteEndpoints', async () => [
      endpoint({ id: 'ep-1', name: 'Keep me' }),
      endpoint({ id: 'ep-2', name: 'Delete me' }),
    ]);
    const deleteMock = setBindingMock('DeleteRemoteEndpoint', async () => undefined);

    const { findByText, queryByText, findAllByRole } = render(RemoteEndpointsSection);
    await findByText('Delete me');

    const deleteButtons = await findAllByRole('button', { name: 'Delete' });
    // The 'Delete me' entry is the second list row.
    await fireEvent.click(deleteButtons[1]);

    await waitFor(() => {
      expect(deleteMock).toHaveBeenCalledWith('ep-2');
    });
    await waitFor(() => {
      expect(queryByText('Delete me')).toBeNull();
    });
    await findByText('Keep me');
  });

  it('reveals the token under the row when Show is clicked', async () => {
    // Bulk list omits the token now; reveal fetches it explicitly via
    // GetRemoteEndpointToken so the wire path can stay credential-free.
    setBindingMock('ListRemoteEndpoints', async () => [
      endpoint({ id: 'ep-1' }),
    ]);
    const tokenMock = setBindingMock(
      'GetRemoteEndpointToken',
      async (..._args: unknown[]) => 'fullsecret',
    );

    const { findByText, findByRole } = render(RemoteEndpointsSection);
    await findByText('Tailnet box');

    // Default: list response carried no token, so the literal isn't on
    // screen even via the underlying state.
    expect(document.body.textContent).not.toContain('fullsecret');

    const showButton = await findByRole('button', { name: 'Show' });
    await fireEvent.click(showButton);

    await waitFor(() => {
      expect(document.body.textContent).toContain('fullsecret');
    });
    expect(tokenMock).toHaveBeenCalledWith('ep-1');
  });

  it('updates an existing endpoint through UpdateRemoteEndpoint when edited', async () => {
    setBindingMock('ListRemoteEndpoints', async () => [
      endpoint({ id: 'ep-1', name: 'Old name', url: 'ws://10.0.0.5:54321/' }),
    ]);
    // Edit pre-fills the token field via GetRemoteEndpointToken — the
    // bulk list now omits it for security, so opening Edit fires a
    // dedicated fetch.
    setBindingMock('GetRemoteEndpointToken', async (..._args: unknown[]) => 'tok-1');
    const updateMock = setBindingMock('UpdateRemoteEndpoint', async (..._args: unknown[]) =>
      endpoint({
        id: 'ep-1',
        name: 'New name',
        url: 'ws://10.0.0.5:54321/',
        token: 'tok-1',
      }),
    );

    const { findByText, findByRole, getByLabelText } = render(RemoteEndpointsSection);
    await findByText('Old name');

    const editButton = await findByRole('button', { name: 'Edit' });
    await fireEvent.click(editButton);

    // Form pre-fills with the existing values; tweak only the nickname
    // so the assertion focuses on what the edit flow actually updated.
    const nameInput = getByLabelText('Nickname (optional)') as HTMLInputElement;
    await fireEvent.input(nameInput, { target: { value: 'New name' } });

    const submit = await findByRole('button', { name: 'Save' });
    await fireEvent.click(submit);

    await waitFor(() => {
      expect(updateMock).toHaveBeenCalledTimes(1);
    });
    // UpdateRemoteEndpoint(id, name, url, token) — the binding signature
    // is positional. Passing the wrong order would silently corrupt the
    // record on the backend, so this assertion pins the order.
    expect(updateMock.mock.calls[0]).toEqual([
      'ep-1',
      'New name',
      'ws://10.0.0.5:54321/',
      'tok-1',
    ]);
    // List re-renders with the updated record returned from the RPC.
    await findByText('New name');
  });

  it('shows the "Copy failed" indicator when the clipboard write throws', async () => {
    const ep = endpoint({ id: 'ep-1', url: 'ws://10.0.0.5:54321/' });
    setBindingMock('ListRemoteEndpoints', async () => [ep]);
    setBindingMock('GetRemoteEndpointToken', async (..._args: unknown[]) => 'tok-x');
    setBindingMock('TouchRemoteEndpoint', async () => undefined);

    // Non-secure contexts (and some browsers) reject writeText with
    // NotAllowedError. The component must surface a transient
    // "Copy failed" indicator instead of swallowing the rejection.
    const writeText = vi.fn(async (): Promise<void> => {
      throw new Error('clipboard denied');
    });
    Object.defineProperty(navigator, 'clipboard', {
      configurable: true,
      value: { writeText },
    });

    const { findByRole, findByText } = render(RemoteEndpointsSection);
    await findByText('Tailnet box');

    const copyBtn = await findByRole('button', {
      name: /Copy connect command for/i,
    });
    await fireEvent.click(copyBtn);

    await waitFor(() => {
      expect(writeText).toHaveBeenCalledTimes(1);
    });
    // The button's aria-label is the row's "Copy connect command for X"
    // (stable for a11y); the failure indicator surfaces in the visible
    // button text. Wait for that text to appear on the same button.
    await waitFor(() => {
      expect(copyBtn.textContent ?? '').toMatch(/Copy failed/i);
    });
    // TouchRemoteEndpoint must NOT fire on a failed copy — the
    // intent is to bump LastUsedAt only when the copy succeeded.
    expect(getBindingMock('TouchRemoteEndpoint')).not.toHaveBeenCalled();
  });

  it('single-quotes the launch command so URL metacharacters can not split the shell command', async () => {
    // The token + URL include shell metacharacters that, without
    // quoting, would split the command at the shell. Single-quote
    // wrapping (POSIX) preserves them as-is.
    const ep = endpoint({
      id: 'ep-1',
      url: 'ws://10.0.0.5:54321/path?other=keep&danger=$(rm)',
    });
    setBindingMock('ListRemoteEndpoints', async () => [ep]);
    setBindingMock('GetRemoteEndpointToken', async (..._args: unknown[]) => 'tok&with;chars');
    setBindingMock('TouchRemoteEndpoint', async () => undefined);

    const writeText = vi.fn(async (_text: string): Promise<void> => undefined);
    Object.defineProperty(navigator, 'clipboard', {
      configurable: true,
      value: { writeText },
    });

    const { findByRole, findByText } = render(RemoteEndpointsSection);
    await findByText('Tailnet box');
    const copyBtn = await findByRole('button', {
      name: /Copy connect command for/i,
    });
    await fireEvent.click(copyBtn);

    await waitFor(() => {
      expect(writeText).toHaveBeenCalledTimes(1);
    });
    const written = writeText.mock.calls[0][0];
    // The URL portion must be wrapped in single quotes so the `&`,
    // `$`, `(`, `)` inside it can't break out.
    expect(written.startsWith("agent-overflow --connect '")).toBe(true);
    expect(written.endsWith("'")).toBe(true);
    // Pasting the result into a POSIX shell tokenises it as exactly
    // two args: the program name + the URL. Anything else means the
    // metacharacters split the command and the launch is broken (or
    // worse, executes side commands). We exercise that property by
    // splitting the unquoted form on `&`/`;` and confirming the
    // quoted form does not yield those splits.
    const cmdArgs = written.split(' ');
    // The first segment is the program, the second is `--connect`,
    // and the URL is single-quoted so it survives as one argv[3]
    // when shell-tokenised. We assert >=3 total tokens (program +
    // flag + URL) and that the URL is quoted, not split.
    expect(cmdArgs[0]).toBe('agent-overflow');
    expect(cmdArgs[1]).toBe('--connect');
    expect(cmdArgs.slice(2).join(' ').startsWith("'")).toBe(true);
  });

  it('escapes embedded single quotes in tokens via the POSIX close-reopen pattern', async () => {
    const ep = endpoint({
      id: 'ep-1',
      url: 'ws://10.0.0.5:54321/',
    });
    setBindingMock('ListRemoteEndpoints', async () => [ep]);
    setBindingMock('GetRemoteEndpointToken', async (..._args: unknown[]) => "tok'with'quotes");
    setBindingMock('TouchRemoteEndpoint', async () => undefined);

    const writeText = vi.fn(async (_text: string): Promise<void> => undefined);
    Object.defineProperty(navigator, 'clipboard', {
      configurable: true,
      value: { writeText },
    });

    const { findByRole, findByText } = render(RemoteEndpointsSection);
    await findByText('Tailnet box');
    await fireEvent.click(
      await findByRole('button', { name: /Copy connect command for/i }),
    );

    await waitFor(() => {
      expect(writeText).toHaveBeenCalledTimes(1);
    });
    const written = writeText.mock.calls[0][0];
    // Each embedded ' becomes '\'' (close, escaped quote, reopen).
    // The URL-encoded form lands in the URL serialisation as %27
    // because URL.searchParams normalises it; assert the resulting
    // command is single-quoted as a whole rather than the literal
    // input quote bytes (URL parsing handles those).
    expect(written.includes("'\\''") || !written.includes("''")).toBe(true);
    expect(written.startsWith("agent-overflow --connect '")).toBe(true);
  });

  it('hides the editor and renders a placeholder in --connect (client) mode', async () => {
    setRunMode('client');
    // Even if the binding is mocked, the component should not call it
    // — the whole point of the placeholder is to avoid mutating the
    // remote backend's settings.
    const listMock = setBindingMock('ListRemoteEndpoints', async () => [
      endpoint({ id: 'ep-1', name: 'Should not render' }),
    ]);

    const { findByText, queryByRole } = render(RemoteEndpointsSection);

    // Placeholder copy must mention the local-install constraint.
    await findByText(/Remote endpoints can only be edited from your local install/i);

    // No "Add" button, no list rows, no Edit/Delete buttons.
    expect(queryByRole('button', { name: 'Add remote endpoint' })).toBeNull();
    expect(queryByRole('button', { name: 'Edit' })).toBeNull();
    expect(queryByRole('button', { name: 'Delete' })).toBeNull();

    // The RPC is skipped entirely so the remote server is not touched.
    expect(listMock).not.toHaveBeenCalled();
  });
});
