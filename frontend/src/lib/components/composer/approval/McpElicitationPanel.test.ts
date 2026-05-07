import { beforeEach, describe, expect, it, vi } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import McpElicitationPanel from './McpElicitationPanel.svelte';
import { ApprovalResponse } from '../../../stores/bindings';
import type { ApprovalRequest } from '../../../types/events';
import { resetBindingMocks, setBindingMock } from '../../../../test/mocks/bindings-app';

// Typed resolver so .mock.calls[0][0] is ApprovalResponse, not undefined.
function makeResolver(
  impl: (r: ApprovalResponse) => Promise<void> = async () => {},
) {
  return vi.fn<(response: ApprovalResponse) => Promise<void>>(impl);
}

// The bulk of MCP elicitation flow is covered in the full-stack
// Composer approval elicitation suite; these tests exercise the panel in
// isolation so regressions here don't silently coast on the dispatcher
// wrapping it.

beforeEach(() => {
  resetBindingMocks();
});

function formApproval(overrides: Partial<ApprovalRequest> = {}): ApprovalRequest {
  return {
    requestId: 'req-el',
    threadId: 'thread-1',
    turnId: 'turn-1',
    toolName: 'mcp_elicitation',
    description: 'Provide a host',
    input: {},
    title: 'Consent',
    kind: 'mcp-elicitation',
    elicitation: {
      mode: 'form',
      message: 'Fill it in',
      serverName: 'db-mcp',
      requestedSchema: {
        type: 'object',
        properties: { host: { type: 'string', title: 'Host' } },
        required: ['host'],
      },
    },
    ...overrides,
  };
}

function urlApproval(overrides: Partial<ApprovalRequest> = {}): ApprovalRequest {
  return {
    requestId: 'req-el-url',
    threadId: 'thread-1',
    turnId: 'turn-1',
    toolName: 'mcp_elicitation',
    description: 'External approval',
    input: {},
    title: 'Consent',
    kind: 'mcp-elicitation',
    elicitation: {
      mode: 'url',
      message: 'Approve in the browser',
      url: 'https://auth.example.com/approve',
    },
    ...overrides,
  };
}

describe('<McpElicitationPanel>', () => {
  it('renders the server + message rows for a form approval', () => {
    const { getByTestId } = render(McpElicitationPanel, {
      props: {
        approval: formApproval(),
        onResolve: makeResolver(),
        onError: vi.fn(),
      },
    });
    expect(getByTestId('elicitation-server').textContent).toContain('db-mcp');
    expect(getByTestId('elicitation-message').textContent).toContain('Fill it in');
  });

  it('Accept sends accept + elicitation content with the collected fields', async () => {
    const onResolve = makeResolver();
    const { getByTestId } = render(McpElicitationPanel, {
      props: {
        approval: formApproval(),
        onResolve,
        onError: vi.fn(),
      },
    });
    await fireEvent.input(getByTestId('el-input-host'), { target: { value: 'db.internal' } });
    await fireEvent.click(getByTestId('elicitation-accept'));
    await waitFor(() => expect(onResolve).toHaveBeenCalled());
    const response = onResolve.mock.calls[0][0] as ApprovalResponse;
    expect(response.decision).toBe('accept');
    expect(response.elicitation).toBeDefined();
    const el = response.elicitation as { action: string; content: unknown };
    expect(el.action).toBe('accept');
    expect(el.content).toEqual({ host: 'db.internal' });
  });

  it('validation failure blocks submit and renders a field error', async () => {
    const onResolve = makeResolver();
    const { getByTestId } = render(McpElicitationPanel, {
      props: {
        approval: formApproval(),
        onResolve,
        onError: vi.fn(),
      },
    });
    // host is required and currently empty — Accept should not call onResolve.
    await fireEvent.click(getByTestId('elicitation-accept'));
    expect(onResolve).not.toHaveBeenCalled();
    expect(getByTestId('el-error-host').textContent).toMatch(/required/i);
  });

  it('Decline sends a decline action with no content', async () => {
    const onResolve = makeResolver();
    const { getByTestId } = render(McpElicitationPanel, {
      props: {
        approval: formApproval(),
        onResolve,
        onError: vi.fn(),
      },
    });
    await fireEvent.click(getByTestId('elicitation-decline'));
    await waitFor(() => expect(onResolve).toHaveBeenCalled());
    const response = onResolve.mock.calls[0][0] as ApprovalResponse;
    const el = response.elicitation as { action: string; content: unknown };
    expect(el.action).toBe('decline');
    expect(el.content).toBeUndefined();
  });

  it('Cancel sends a cancel action with no content', async () => {
    const onResolve = makeResolver();
    const { getByTestId } = render(McpElicitationPanel, {
      props: {
        approval: formApproval(),
        onResolve,
        onError: vi.fn(),
      },
    });
    await fireEvent.click(getByTestId('elicitation-cancel'));
    await waitFor(() => expect(onResolve).toHaveBeenCalled());
    const response = onResolve.mock.calls[0][0] as ApprovalResponse;
    const el = response.elicitation as { action: string; content: unknown };
    expect(el.action).toBe('cancel');
    expect(el.content).toBeUndefined();
  });

  it('URL mode renders the link and routes OpenExternalURL through it', async () => {
    const open = setBindingMock('OpenExternalURL', vi.fn(async () => undefined));
    const { getByTestId, queryByTestId } = render(McpElicitationPanel, {
      props: {
        approval: urlApproval(),
        onResolve: makeResolver(),
        onError: vi.fn(),
      },
    });
    expect(queryByTestId('elicitation-fields')).toBeNull();
    const link = getByTestId('elicitation-url-link') as HTMLAnchorElement;
    expect(link.href).toBe('https://auth.example.com/approve');
    await fireEvent.click(link);
    expect(open).toHaveBeenCalledWith('https://auth.example.com/approve');
  });

  it('blocks unsupported URL schemes in URL mode without rendering the raw URL', () => {
    const open = setBindingMock('OpenExternalURL', vi.fn(async () => undefined));
    const onError = vi.fn();
    const { getByTestId, queryByTestId } = render(McpElicitationPanel, {
      props: {
        approval: urlApproval({
          elicitation: {
            mode: 'url',
            message: 'Approve in the browser',
            url: 'javascript:alert(1)?token=secret-token',
          },
        }),
        onResolve: makeResolver(),
        onError,
      },
    });

    expect(queryByTestId('elicitation-url-link')).toBeNull();
    const blockedText = getByTestId('elicitation-url-blocked').textContent ?? '';
    expect(blockedText).toContain('Unsupported approval URL.');
    expect(blockedText).not.toContain('secret-token');
    expect(blockedText).not.toContain('javascript:alert');
    expect(open).not.toHaveBeenCalled();
    expect(onError).not.toHaveBeenCalled();
  });

  it('URL-mode Accept sends accept with no content (flow completes externally)', async () => {
    const onResolve = makeResolver();
    const { getByTestId } = render(McpElicitationPanel, {
      props: {
        approval: urlApproval(),
        onResolve,
        onError: vi.fn(),
      },
    });
    await fireEvent.click(getByTestId('elicitation-accept'));
    await waitFor(() => expect(onResolve).toHaveBeenCalled());
    const response = onResolve.mock.calls[0][0] as ApprovalResponse;
    const el = response.elicitation as { action: string; content: unknown };
    expect(el.action).toBe('accept');
    expect(el.content).toBeUndefined();
  });

  it('surfaces a resolver rejection via onError', async () => {
    const onResolve = makeResolver(async () => {
      throw new Error('down');
    });
    const onError = vi.fn();
    const { getByTestId } = render(McpElicitationPanel, {
      props: {
        approval: formApproval(),
        onResolve,
        onError,
      },
    });
    await fireEvent.input(getByTestId('el-input-host'), { target: { value: 'x' } });
    await fireEvent.click(getByTestId('elicitation-accept'));
    await waitFor(() => expect(onError).toHaveBeenCalled());
    expect(onError.mock.calls[0][0]).toMatch(/Failed to respond to elicitation/i);
  });
});
