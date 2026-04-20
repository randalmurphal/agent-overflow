import { describe, expect, it, vi } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import ToolApprovalPanel from './ToolApprovalPanel.svelte';
import { ApprovalResponse } from '../../../stores/bindings';
import type { ApprovalRequest } from '../../../types/events';

// Typed resolver so .mock.calls[0][0] is ApprovalResponse, not undefined.
function makeResolver(
  impl: (r: ApprovalResponse) => Promise<void> = async () => {},
) {
  return vi.fn<(response: ApprovalResponse) => Promise<void>>(impl);
}

function bashApproval(overrides: Partial<ApprovalRequest> = {}): ApprovalRequest {
  return {
    requestId: 'req-tool',
    threadId: 'thread-1',
    turnId: 'turn-1',
    toolName: 'Bash',
    description: 'ls -la',
    input: { command: 'ls -la', timeout: 2000 },
    title: 'Run command',
    ...overrides,
  };
}

describe('<ToolApprovalPanel>', () => {
  it('renders the command preview for a Bash approval', () => {
    const { getByText } = render(ToolApprovalPanel, {
      props: {
        approval: bashApproval(),
        onResolve: makeResolver(),
        onError: vi.fn(),
      },
    });
    expect(getByText('ls -la')).toBeInTheDocument();
    expect(getByText('Command')).toBeInTheDocument();
  });

  it('renders a file path preview for a Read approval', () => {
    const { getByText } = render(ToolApprovalPanel, {
      props: {
        approval: bashApproval({
          toolName: 'Read',
          input: { file_path: '/etc/hosts' },
        }),
        onResolve: makeResolver(),
        onError: vi.fn(),
      },
    });
    expect(getByText('/etc/hosts')).toBeInTheDocument();
    expect(getByText('File')).toBeInTheDocument();
  });

  it('Allow sends decision=allow with no updatedInput', async () => {
    const onResolve = makeResolver();
    const { getByTestId } = render(ToolApprovalPanel, {
      props: {
        approval: bashApproval(),
        onResolve,
        onError: vi.fn(),
      },
    });
    await fireEvent.click(getByTestId('approval-allow'));
    await waitFor(() => expect(onResolve).toHaveBeenCalled());
    const response = onResolve.mock.calls[0][0] as ApprovalResponse;
    expect(response.decision).toBe('allow');
    expect(response.updatedInput).toBeUndefined();
  });

  it('Deny sends decision=deny', async () => {
    const onResolve = makeResolver();
    const { getByTestId } = render(ToolApprovalPanel, {
      props: {
        approval: bashApproval(),
        onResolve,
        onError: vi.fn(),
      },
    });
    await fireEvent.click(getByTestId('approval-deny'));
    await waitFor(() => expect(onResolve).toHaveBeenCalled());
    expect((onResolve.mock.calls[0][0] as ApprovalResponse).decision).toBe('deny');
  });

  it('Edit toggle is hidden when input is null', () => {
    const { queryByTestId } = render(ToolApprovalPanel, {
      props: {
        approval: bashApproval({ input: null as unknown as object }),
        onResolve: makeResolver(),
        onError: vi.fn(),
      },
    });
    expect(queryByTestId('approval-edit-toggle')).toBeNull();
  });

  it('Edit open prefills textarea with pretty-printed JSON', async () => {
    const { getByTestId } = render(ToolApprovalPanel, {
      props: {
        approval: bashApproval(),
        onResolve: makeResolver(),
        onError: vi.fn(),
      },
    });
    await fireEvent.click(getByTestId('approval-edit-toggle'));
    const ta = getByTestId('approval-edit-textarea') as HTMLTextAreaElement;
    expect(ta.value).toBe(JSON.stringify({ command: 'ls -la', timeout: 2000 }, null, 2));
  });

  it('Allow with edits sends parsed JSON as updatedInput', async () => {
    const onResolve = makeResolver();
    const { getByTestId } = render(ToolApprovalPanel, {
      props: {
        approval: bashApproval(),
        onResolve,
        onError: vi.fn(),
      },
    });
    await fireEvent.click(getByTestId('approval-edit-toggle'));
    await fireEvent.input(getByTestId('approval-edit-textarea'), {
      target: { value: '{"command":"pwd"}' },
    });
    await fireEvent.click(getByTestId('approval-allow-with-edits'));
    await waitFor(() => expect(onResolve).toHaveBeenCalled());
    const response = onResolve.mock.calls[0][0] as ApprovalResponse;
    expect(response.decision).toBe('allow');
    expect(response.updatedInput).toEqual({ command: 'pwd' });
  });

  it('Malformed JSON in edit mode does not call onResolve and shows an error', async () => {
    const onResolve = makeResolver();
    const { getByTestId } = render(ToolApprovalPanel, {
      props: {
        approval: bashApproval(),
        onResolve,
        onError: vi.fn(),
      },
    });
    await fireEvent.click(getByTestId('approval-edit-toggle'));
    await fireEvent.input(getByTestId('approval-edit-textarea'), {
      target: { value: '{bad' },
    });
    await fireEvent.click(getByTestId('approval-allow-with-edits'));
    expect(onResolve).not.toHaveBeenCalled();
    expect(getByTestId('approval-edit-error').textContent).toMatch(/Invalid JSON/i);
  });

  it('Cancel edit restores the preview and clears any edit error', async () => {
    const { getByTestId, queryByTestId } = render(ToolApprovalPanel, {
      props: {
        approval: bashApproval(),
        onResolve: makeResolver(),
        onError: vi.fn(),
      },
    });
    await fireEvent.click(getByTestId('approval-edit-toggle'));
    await fireEvent.input(getByTestId('approval-edit-textarea'), { target: { value: 'bad' } });
    await fireEvent.click(getByTestId('approval-allow-with-edits'));
    expect(queryByTestId('approval-edit-error')).not.toBeNull();
    await fireEvent.click(getByTestId('approval-edit-cancel'));
    expect(queryByTestId('approval-edit-textarea')).toBeNull();
    expect(queryByTestId('approval-edit-error')).toBeNull();
    expect(getByTestId('approval-edit-toggle')).toBeInTheDocument();
  });

  it('surfaces a resolver rejection via onError', async () => {
    const onResolve = makeResolver(async () => {
      throw new Error('network is down');
    });
    const onError = vi.fn();
    const { getByTestId } = render(ToolApprovalPanel, {
      props: {
        approval: bashApproval(),
        onResolve,
        onError,
      },
    });
    await fireEvent.click(getByTestId('approval-allow'));
    await waitFor(() => expect(onError).toHaveBeenCalled());
    expect(onError.mock.calls[0][0]).toMatch(/Failed to respond to approval/i);
  });
});
