import { describe, expect, it, vi } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import PermissionPanel from './PermissionPanel.svelte';
import { ApprovalResponse } from '../../../stores/bindings';
import type { ApprovalRequest } from '../../../types/events';

// Typed resolver so .mock.calls[0][0] is ApprovalResponse, not undefined.
function makeResolver(
  impl: (r: ApprovalResponse) => Promise<void> = async () => {},
) {
  return vi.fn<(response: ApprovalResponse) => Promise<void>>(impl);
}

function baseApproval(overrides: Partial<ApprovalRequest> = {}): ApprovalRequest {
  return {
    requestId: 'req-perm',
    threadId: 'thread-1',
    turnId: 'turn-1',
    toolName: 'permission_request',
    description: 'Grant network + write',
    input: {},
    title: 'Permission',
    kind: 'permission',
    permissions: {
      network: { enabled: true },
      fileSystem: { read: ['/tmp'], write: ['/tmp'] },
    },
    ...overrides,
  };
}

describe('<PermissionPanel>', () => {
  it('renders the permission summary lines for network + fileSystem', () => {
    const { getByTestId, getByText } = render(PermissionPanel, {
      props: {
        approval: baseApproval(),
        onResolve: makeResolver(),
        onError: vi.fn(),
      },
    });
    expect(getByTestId('permission-summary')).toBeInTheDocument();
    expect(getByText(/Network: Enabled/)).toBeInTheDocument();
    expect(getByText(/Read: \/tmp/)).toBeInTheDocument();
    expect(getByText(/Write: \/tmp/)).toBeInTheDocument();
  });

  it('Approve once sends { decision: accept, scope: turn } with the permissions forwarded', async () => {
    const onResolve = makeResolver();
    const { getByTestId } = render(PermissionPanel, {
      props: {
        approval: baseApproval(),
        onResolve,
        onError: vi.fn(),
      },
    });
    await fireEvent.click(getByTestId('permission-grant'));
    const response = onResolve.mock.calls[0][0] as ApprovalResponse;
    expect(response.decision).toBe('accept');
    expect(response.scope).toBe('turn');
    // The permissions object is wrapped into a PermissionProfile but carries
    // the same shape.
    expect(response.permissions).toMatchObject({
      network: { enabled: true },
      fileSystem: { read: ['/tmp'], write: ['/tmp'] },
    });
  });

  it('Always allow this session sends scope=session', async () => {
    const onResolve = makeResolver();
    const { getByTestId } = render(PermissionPanel, {
      props: {
        approval: baseApproval(),
        onResolve,
        onError: vi.fn(),
      },
    });
    await fireEvent.click(getByTestId('permission-grant-session'));
    const response = onResolve.mock.calls[0][0] as ApprovalResponse;
    expect(response.decision).toBe('acceptForSession');
    expect(response.scope).toBe('session');
  });

  it('Decline sends decision=decline with no permissions payload', async () => {
    const onResolve = makeResolver();
    const { getByTestId } = render(PermissionPanel, {
      props: {
        approval: baseApproval(),
        onResolve,
        onError: vi.fn(),
      },
    });
    await fireEvent.click(getByTestId('permission-deny'));
    const response = onResolve.mock.calls[0][0] as ApprovalResponse;
    expect(response.decision).toBe('decline');
    expect(response.permissions).toBeUndefined();
    expect(response.scope).toBeUndefined();
  });

  it('handles an empty permissions profile without crashing', () => {
    const { getByTestId } = render(PermissionPanel, {
      props: {
        approval: baseApproval({ permissions: undefined }),
        onResolve: makeResolver(),
        onError: vi.fn(),
      },
    });
    // Summary row is conditional on approval.permissions — absent here.
    // Buttons still render so the panel is functional.
    expect(getByTestId('permission-grant')).toBeInTheDocument();
  });

  it('surfaces a resolver rejection via onError', async () => {
    const onResolve = makeResolver(async () => {
      throw new Error('nope');
    });
    const onError = vi.fn();
    const { getByTestId } = render(PermissionPanel, {
      props: {
        approval: baseApproval(),
        onResolve,
        onError,
      },
    });
    await fireEvent.click(getByTestId('permission-grant'));
    await Promise.resolve();
    await Promise.resolve();
    expect(onError).toHaveBeenCalled();
    expect(onError.mock.calls[0][0]).toMatch(/Failed to grant permission/i);
  });

  it('moves focus across actions with j/k and arrow keys', async () => {
    const { getByTestId } = render(PermissionPanel, {
      props: {
        approval: baseApproval(),
        onResolve: makeResolver(),
        onError: vi.fn(),
      },
    });
    const cancel = getByTestId('permission-cancel') as HTMLButtonElement;
    const deny = getByTestId('permission-deny') as HTMLButtonElement;
    const session = getByTestId('permission-grant-session') as HTMLButtonElement;

    cancel.focus();
    await fireEvent.keyDown(cancel, { key: 'j' });
    expect(document.activeElement).toBe(deny);
    await fireEvent.keyDown(deny, { key: 'ArrowRight' });
    expect(document.activeElement).toBe(session);
    await fireEvent.keyDown(session, { key: 'k' });
    expect(document.activeElement).toBe(deny);
  });

  it('focuses the action row on mount without activating an approval action', async () => {
    const root = document.createElement('div');
    root.setAttribute('data-testid', 'composer-root');
    const textarea = document.createElement('textarea');
    textarea.setAttribute('aria-label', 'Message Input');
    const target = document.createElement('div');
    root.appendChild(textarea);
    root.appendChild(target);
    document.body.appendChild(root);
    textarea.focus();

    const onResolve = makeResolver();
    const { getByRole, getByTestId, unmount } = render(PermissionPanel, {
      target,
      props: {
        approval: baseApproval(),
        onResolve,
        onError: vi.fn(),
      },
    });

    const toolbar = getByRole('toolbar', { name: 'Permission actions' });
    await waitFor(() => expect(document.activeElement).toBe(toolbar));

    await fireEvent.keyDown(toolbar, { key: 'Enter' });
    await fireEvent.keyDown(toolbar, { key: ' ' });
    expect(onResolve).not.toHaveBeenCalled();

    await fireEvent.keyDown(toolbar, { key: 'j' });
    expect(document.activeElement).toBe(getByTestId('permission-cancel'));

    unmount();
    root.remove();
  });
});
