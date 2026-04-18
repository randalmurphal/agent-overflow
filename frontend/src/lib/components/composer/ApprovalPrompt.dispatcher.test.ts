import { beforeAll, beforeEach, describe, expect, it } from 'vitest';
import { render } from '@testing-library/svelte';
import ApprovalPrompt from './ApprovalPrompt.svelte';
import { createThreadPane } from '../../stores/thread.svelte';
import type { Thread } from '../../types/models';
import type { ApprovalRequest } from '../../types/events';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { installAnimateShim } from '../../../test/integration/_helpers';

// Dispatcher-level coverage: make sure each `kind` value routes to the
// matching per-kind panel. The panels themselves have their own tests
// under approval/*.test.ts for behavior; here we just assert routing.

beforeAll(installAnimateShim);

function seedThread(): Thread {
  return {
    id: 'thread-1',
    title: 'Test',
    provider: 'claude',
    workspacePath: '/tmp',
    projectPath: '/tmp',
    mode: 'chat',
    model: 'claude-opus-4-7',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
  };
}

async function makePane() {
  setBindingMock('SwitchThread', async () => {});
  setBindingMock('ListItems', async () => []);
  setBindingMock('ListPayloadMetas', async () => []);
  const pane = createThreadPane();
  await pane.switchThread(seedThread());
  return pane;
}

function baseApproval(overrides: Partial<ApprovalRequest> = {}): ApprovalRequest {
  return {
    requestId: 'req-1',
    threadId: 'thread-1',
    turnId: 'turn-1',
    toolName: 'Bash',
    description: 'ls',
    input: { command: 'ls' },
    title: 'Run',
    ...overrides,
  };
}

async function mountWith(approval: ApprovalRequest) {
  const pane = await makePane();
  pane.addApproval(approval);
  const result = render(ApprovalPrompt, { pane });
  return { ...result, pane };
}

beforeEach(() => {
  setBindingMock('RespondToApproval', async () => {});
});

describe('<ApprovalPrompt> dispatcher', () => {
  it('does not render the prompt container when there are no approvals', async () => {
    const pane = await makePane();
    const { queryByTestId } = render(ApprovalPrompt, { pane });
    expect(queryByTestId('approval-prompt')).toBeNull();
  });

  it('renders the approval container when an approval is pending', async () => {
    const { getByTestId } = await mountWith(baseApproval());
    expect(getByTestId('approval-prompt')).toBeInTheDocument();
    expect(getByTestId('approval-card')).toBeInTheDocument();
  });

  it('routes kind=user-input to the user-input panel', async () => {
    const { getByTestId, queryByTestId } = await mountWith(
      baseApproval({
        kind: 'user-input',
        questions: [{ id: 'q1', header: '', question: 'Hi' }],
      }),
    );
    expect(getByTestId('user-input-submit')).toBeInTheDocument();
    // Sanity: the default tool panel isn't also mounted.
    expect(queryByTestId('approval-allow')).toBeNull();
  });

  it('routes kind=permission to the permission panel', async () => {
    const { getByTestId, queryByTestId } = await mountWith(
      baseApproval({
        kind: 'permission',
        permissions: { network: { enabled: true } },
      }),
    );
    expect(getByTestId('permission-grant')).toBeInTheDocument();
    expect(queryByTestId('approval-allow')).toBeNull();
  });

  it('routes kind=mcp-elicitation to the elicitation panel', async () => {
    const { getByTestId, queryByTestId } = await mountWith(
      baseApproval({
        kind: 'mcp-elicitation',
        elicitation: {
          mode: 'form',
          message: 'fill it',
          requestedSchema: { type: 'object', properties: { x: { type: 'string' } } },
        },
      }),
    );
    expect(getByTestId('elicitation-accept')).toBeInTheDocument();
    expect(queryByTestId('approval-allow')).toBeNull();
  });

  it('falls back to the default tool panel when kind is absent', async () => {
    const { getByTestId, queryByTestId } = await mountWith(baseApproval());
    expect(getByTestId('approval-allow')).toBeInTheDocument();
    expect(getByTestId('approval-deny')).toBeInTheDocument();
    expect(queryByTestId('permission-grant')).toBeNull();
    expect(queryByTestId('user-input-submit')).toBeNull();
    expect(queryByTestId('elicitation-accept')).toBeNull();
  });

  it('falls back to the default tool panel for kinds like command / file-read / file-change', async () => {
    // `kind` may be set by the backend but still wants the default UI.
    for (const kind of ['command', 'file-read', 'file-change'] as const) {
      const { getByTestId, unmount } = await mountWith(baseApproval({ kind }));
      expect(getByTestId('approval-allow')).toBeInTheDocument();
      unmount();
    }
  });

  it('renders one card per pending approval', async () => {
    const pane = await makePane();
    pane.addApproval(baseApproval({ requestId: 'a' }));
    pane.addApproval(baseApproval({ requestId: 'b' }));
    const { container } = render(ApprovalPrompt, { pane });
    expect(container.querySelectorAll('[data-testid="approval-card"]').length).toBe(2);
  });
});
