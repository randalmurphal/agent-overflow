import { describe, expect, it, beforeEach, afterEach, vi } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import RemoteEndpointRow from './RemoteEndpointRow.svelte';
import { setBindingMock, resetBindingMocks, getBindingMock } from '../../../test/mocks/bindings-app';

interface EndpointSummary {
  id: string;
  name: string;
  url: string;
  lastUsedAt?: number;
}

function endpoint(over: Partial<EndpointSummary> = {}): EndpointSummary {
  return {
    id: 'ep-1',
    name: 'Tailnet',
    url: 'ws://10.0.0.5:54321/',
    ...over,
  };
}

describe('<RemoteEndpointRow>', () => {
  beforeEach(() => {
    resetBindingMocks();
  });

  afterEach(() => {
    resetBindingMocks();
  });

  it('renders the nickname and URL', async () => {
    const { findByText } = render(RemoteEndpointRow, {
      props: {
        endpoint: endpoint({ name: 'My machine', url: 'ws://10.0.0.5:54321/' }),
        saving: false,
        onEdit: () => {},
        onDelete: () => {},
      },
    });
    await findByText('My machine');
    await findByText('ws://10.0.0.5:54321/');
  });

  it('falls back to the URL when no nickname is set', async () => {
    const { findAllByText } = render(RemoteEndpointRow, {
      props: {
        endpoint: endpoint({ name: '', url: 'ws://10.0.0.5:54321/' }),
        saving: false,
        onEdit: () => {},
        onDelete: () => {},
      },
    });
    // Both the title and the URL line render the URL when name is
    // empty — confirm the URL shows up at least once.
    const matches = await findAllByText('ws://10.0.0.5:54321/');
    expect(matches.length).toBeGreaterThanOrEqual(1);
  });

  it('reveals the token via GetRemoteEndpointToken when Show is clicked', async () => {
    const tokenMock = setBindingMock(
      'GetRemoteEndpointToken',
      async (..._args: unknown[]) => 'fullsecret',
    );
    const { findByRole } = render(RemoteEndpointRow, {
      props: {
        endpoint: endpoint(),
        saving: false,
        onEdit: () => {},
        onDelete: () => {},
      },
    });
    expect(document.body.textContent).not.toContain('fullsecret');
    const showButton = await findByRole('button', { name: 'Show' });
    await fireEvent.click(showButton);
    await waitFor(() => {
      expect(document.body.textContent).toContain('fullsecret');
    });
    expect(tokenMock).toHaveBeenCalledWith('ep-1');
  });

  it('writes the launch command to the clipboard on Copy', async () => {
    setBindingMock('GetRemoteEndpointToken', async (..._args: unknown[]) => 'token-ABC');
    const touchMock = setBindingMock('TouchRemoteEndpoint', async () => undefined);
    const writeText = vi.fn(async (_text: string): Promise<void> => undefined);
    Object.defineProperty(navigator, 'clipboard', {
      configurable: true,
      value: { writeText },
    });
    const { findByRole } = render(RemoteEndpointRow, {
      props: {
        endpoint: endpoint({ url: 'ws://10.0.0.5:54321/' }),
        saving: false,
        onEdit: () => {},
        onDelete: () => {},
      },
    });
    const copyBtn = await findByRole('button', { name: /Copy connect command for/i });
    await fireEvent.click(copyBtn);
    await waitFor(() => {
      expect(writeText).toHaveBeenCalledTimes(1);
    });
    const written = writeText.mock.calls[0][0] as string;
    expect(written).toContain('agent-overflow --connect');
    expect(written).toContain('ws://10.0.0.5:54321');
    expect(written).toContain('token=token-ABC');
    await waitFor(() => {
      expect(touchMock).toHaveBeenCalledWith('ep-1');
    });
  });

  it('shows "Copy failed" when the clipboard write throws', async () => {
    setBindingMock('GetRemoteEndpointToken', async (..._args: unknown[]) => 'tok-x');
    setBindingMock('TouchRemoteEndpoint', async () => undefined);
    const writeText = vi.fn(async (): Promise<void> => {
      throw new Error('clipboard denied');
    });
    Object.defineProperty(navigator, 'clipboard', {
      configurable: true,
      value: { writeText },
    });
    const { findByRole } = render(RemoteEndpointRow, {
      props: {
        endpoint: endpoint(),
        saving: false,
        onEdit: () => {},
        onDelete: () => {},
      },
    });
    const copyBtn = await findByRole('button', { name: /Copy connect command for/i });
    await fireEvent.click(copyBtn);
    await waitFor(() => {
      expect(writeText).toHaveBeenCalledTimes(1);
    });
    await waitFor(() => {
      expect(copyBtn.textContent ?? '').toMatch(/Copy failed/i);
    });
    // TouchRemoteEndpoint must NOT fire on a failed copy — the bump
    // is only meaningful when the user actually has the launch
    // command in their clipboard.
    expect(getBindingMock('TouchRemoteEndpoint')).not.toHaveBeenCalled();
  });

  it('invokes onEdit when the Edit button is clicked', async () => {
    const onEdit = vi.fn();
    const { findByRole } = render(RemoteEndpointRow, {
      props: {
        endpoint: endpoint(),
        saving: false,
        onEdit,
        onDelete: () => {},
      },
    });
    await fireEvent.click(await findByRole('button', { name: 'Edit' }));
    expect(onEdit).toHaveBeenCalledTimes(1);
  });

  it('invokes onDelete when the Delete button is clicked', async () => {
    const onDelete = vi.fn();
    const { findByRole } = render(RemoteEndpointRow, {
      props: {
        endpoint: endpoint(),
        saving: false,
        onEdit: () => {},
        onDelete,
      },
    });
    await fireEvent.click(await findByRole('button', { name: 'Delete' }));
    expect(onDelete).toHaveBeenCalledTimes(1);
  });

  it('disables the Delete button while saving', async () => {
    const { findByRole } = render(RemoteEndpointRow, {
      props: {
        endpoint: endpoint(),
        saving: true,
        onEdit: () => {},
        onDelete: () => {},
      },
    });
    const deleteBtn = (await findByRole('button', { name: 'Delete' })) as HTMLButtonElement;
    expect(deleteBtn.disabled).toBe(true);
  });
});
