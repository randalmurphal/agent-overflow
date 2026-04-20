import { beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import ApprovalPrompt from './ApprovalPrompt.svelte';
import { createThreadPane } from '../../stores/thread.svelte';
import type { Thread } from '../../types/models';
import type { ApprovalRequest } from '../../types/events';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { installAnimateShim } from '../../../test/integration/_helpers';
import type { Mock } from 'vitest';

beforeAll(installAnimateShim);

interface RespondBody {
  requestId?: string;
  decision?: string;
  updatedInput?: unknown;
  answers?: Record<string, unknown>;
  permissions?: unknown;
  elicitation?: unknown;
  scope?: string;
}

function bodyAt(mock: Mock, i = 0): RespondBody {
  return mock.mock.calls[i]?.[1] as RespondBody;
}

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

function toolApproval(overrides: Partial<ApprovalRequest> = {}): ApprovalRequest {
  return {
    requestId: 'req-edit',
    threadId: 'thread-1',
    turnId: 'turn-1',
    toolName: 'Bash',
    description: 'ls -la',
    input: { command: 'ls -la', timeout: 2000 },
    title: 'Run command',
    // No explicit kind — default-branch behavior.
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

// ---- Visibility of the Edit toggle ----

describe('<ApprovalPrompt> edit-input toggle — visibility', () => {
  it('is shown by default for a tool approval with an input', async () => {
    const { getByTestId } = await mountWith(toolApproval());
    expect(getByTestId('approval-edit-toggle')).toBeInTheDocument();
  });

  it('is hidden for permission kind', async () => {
    const { queryByTestId } = await mountWith(
      toolApproval({ kind: 'permission', permissions: { network: { enabled: true } } }),
    );
    expect(queryByTestId('approval-edit-toggle')).toBeNull();
  });

  it('is hidden for user-input kind', async () => {
    const { queryByTestId } = await mountWith(
      toolApproval({ kind: 'user-input', questions: [{ id: 'q', header: '', question: 'Q' }] }),
    );
    expect(queryByTestId('approval-edit-toggle')).toBeNull();
  });

  it('is hidden for mcp-elicitation kind', async () => {
    const { queryByTestId } = await mountWith(
      toolApproval({
        kind: 'mcp-elicitation',
        elicitation: { mode: 'form', message: 'm', requestedSchema: { type: 'object', properties: {} } },
      }),
    );
    expect(queryByTestId('approval-edit-toggle')).toBeNull();
  });

  it('is hidden when input is null', async () => {
    const { queryByTestId } = await mountWith(toolApproval({ input: null as unknown as object }));
    expect(queryByTestId('approval-edit-toggle')).toBeNull();
  });
});

// ---- Opening and closing the editor ----

describe('<ApprovalPrompt> edit-input toggle — open/close flow', () => {
  it('opens the textarea prefilled with the input serialized as JSON', async () => {
    const { getByTestId } = await mountWith(toolApproval());
    await fireEvent.click(getByTestId('approval-edit-toggle'));
    const ta = getByTestId('approval-edit-textarea') as HTMLTextAreaElement;
    expect(ta.value).toBe(JSON.stringify({ command: 'ls -la', timeout: 2000 }, null, 2));
  });

  it('replaces the plain Allow button with an "Allow with edits" button while editing', async () => {
    const { getByTestId, queryByTestId } = await mountWith(toolApproval());
    await fireEvent.click(getByTestId('approval-edit-toggle'));
    expect(getByTestId('approval-allow-with-edits')).toBeInTheDocument();
    // The unsuffixed "Allow" button is replaced.
    expect(queryByTestId('approval-edit-toggle')).toBeNull();
  });

  it('shows a Cancel-edit control that closes the editor and restores the preview', async () => {
    const { getByTestId, queryByTestId } = await mountWith(toolApproval());
    await fireEvent.click(getByTestId('approval-edit-toggle'));
    await fireEvent.click(getByTestId('approval-edit-cancel'));
    expect(queryByTestId('approval-edit-textarea')).toBeNull();
    // The Edit toggle is visible again.
    expect(getByTestId('approval-edit-toggle')).toBeInTheDocument();
  });

  it('keeps original input intact when the user cancels after edits', async () => {
    const respond = setBindingMock('RespondToApproval', async () => {});
    const { getByTestId } = await mountWith(toolApproval());
    await fireEvent.click(getByTestId('approval-edit-toggle'));
    await fireEvent.input(getByTestId('approval-edit-textarea'), { target: { value: '{"command":"mutated"}' } });
    await fireEvent.click(getByTestId('approval-edit-cancel'));
    // Sending plain Allow now must NOT carry updatedInput — it's the
    // original approval flow, not the edit flow.
    const allowBtn = Array.from(document.querySelectorAll('button')).find((b) => b.textContent?.trim() === 'Allow');
    expect(allowBtn).toBeDefined();
    await fireEvent.click(allowBtn!);
    await waitFor(() => expect(respond).toHaveBeenCalled());
    expect(bodyAt(respond).updatedInput).toBeUndefined();
  });
});

// ---- Validation (invalid JSON) ----

describe('<ApprovalPrompt> edit-input toggle — validation', () => {
  it('shows a parse error when the textarea contents are not valid JSON', async () => {
    const { getByTestId } = await mountWith(toolApproval());
    await fireEvent.click(getByTestId('approval-edit-toggle'));
    await fireEvent.input(getByTestId('approval-edit-textarea'), { target: { value: '{bad' } });
    await fireEvent.click(getByTestId('approval-allow-with-edits'));
    const err = getByTestId('approval-edit-error');
    expect(err.textContent).toMatch(/Invalid JSON/i);
  });

  it('does not call RespondToApproval on parse failure', async () => {
    const respond = setBindingMock('RespondToApproval', async () => {});
    const { getByTestId } = await mountWith(toolApproval());
    await fireEvent.click(getByTestId('approval-edit-toggle'));
    await fireEvent.input(getByTestId('approval-edit-textarea'), { target: { value: 'garbage' } });
    await fireEvent.click(getByTestId('approval-allow-with-edits'));
    expect(respond).not.toHaveBeenCalled();
  });

  it('keeps the editor open after a parse failure so the user can fix', async () => {
    const { getByTestId, queryByTestId } = await mountWith(toolApproval());
    await fireEvent.click(getByTestId('approval-edit-toggle'));
    await fireEvent.input(getByTestId('approval-edit-textarea'), { target: { value: 'bad' } });
    await fireEvent.click(getByTestId('approval-allow-with-edits'));
    expect(queryByTestId('approval-edit-textarea')).toBeInTheDocument();
  });

  it('clears a previous parse error on the next keystroke', async () => {
    const { getByTestId, queryByTestId } = await mountWith(toolApproval());
    await fireEvent.click(getByTestId('approval-edit-toggle'));
    await fireEvent.input(getByTestId('approval-edit-textarea'), { target: { value: 'bad' } });
    await fireEvent.click(getByTestId('approval-allow-with-edits'));
    expect(queryByTestId('approval-edit-error')).not.toBeNull();
    await fireEvent.input(getByTestId('approval-edit-textarea'), { target: { value: '{}' } });
    expect(queryByTestId('approval-edit-error')).toBeNull();
  });
});

// ---- Happy-path submission ----

describe('<ApprovalPrompt> edit-input toggle — submission', () => {
  it('sends allow + parsed updatedInput on Allow with edits', async () => {
    const respond = setBindingMock('RespondToApproval', async () => {});
    const { getByTestId } = await mountWith(toolApproval());
    await fireEvent.click(getByTestId('approval-edit-toggle'));
    await fireEvent.input(getByTestId('approval-edit-textarea'), {
      target: { value: JSON.stringify({ command: 'pwd', timeout: 500 }) },
    });
    await fireEvent.click(getByTestId('approval-allow-with-edits'));
    await waitFor(() => expect(respond).toHaveBeenCalled());
    const body = bodyAt(respond);
    expect(body.decision).toBe('allow');
    expect(body.updatedInput).toEqual({ command: 'pwd', timeout: 500 });
  });

  it('closes the editor on successful Allow-with-edits', async () => {
    const { getByTestId, queryByTestId } = await mountWith(toolApproval());
    await fireEvent.click(getByTestId('approval-edit-toggle'));
    await fireEvent.click(getByTestId('approval-allow-with-edits'));
    // After a successful send, the approval stays in state until the pane
    // removes it — but the editor closes so further clicks use the plain
    // Allow path.
    await waitFor(() => expect(queryByTestId('approval-edit-textarea')).toBeNull());
  });

  it('Allow with edits submits updatedInput without any session-level side effects', async () => {
    const respond = setBindingMock('RespondToApproval', async () => {});
    const { getByTestId } = await mountWith(toolApproval());
    await fireEvent.click(getByTestId('approval-edit-toggle'));
    await fireEvent.input(getByTestId('approval-edit-textarea'), { target: { value: '{"command":"ls"}' } });
    await fireEvent.click(getByTestId('approval-allow-with-edits'));
    await waitFor(() => expect(respond).toHaveBeenCalled());
    expect(bodyAt(respond).updatedInput).toEqual({ command: 'ls' });
  });

  // Each JSON root type gets its own subtest so the test harness mounts+
  // unmounts independently, avoiding "multiple elements found" errors that
  // would hit if we kept several approvals mounted in the same DOM.
  it.each([
    ['{"a":1}', { a: 1 }],
    ['[1, 2, 3]', [1, 2, 3]],
    ['"hi"', 'hi'],
    ['42', 42],
    ['null', null],
    ['true', true],
  ] as const)('passes through JSON root type %s', async (raw, parsed) => {
    const respond = setBindingMock('RespondToApproval', async () => {});
    const { getByTestId } = await mountWith(toolApproval());
    await fireEvent.click(getByTestId('approval-edit-toggle'));
    await fireEvent.input(getByTestId('approval-edit-textarea'), { target: { value: raw } });
    await fireEvent.click(getByTestId('approval-allow-with-edits'));
    await waitFor(() => expect(respond).toHaveBeenCalled());
    expect(bodyAt(respond).updatedInput).toEqual(parsed);
  });

  it('Deny from the edit mode sends decision=deny with no updatedInput', async () => {
    const respond = setBindingMock('RespondToApproval', async () => {});
    const { getByTestId } = await mountWith(toolApproval());
    await fireEvent.click(getByTestId('approval-edit-toggle'));
    await fireEvent.input(getByTestId('approval-edit-textarea'), { target: { value: '{"command":"rm -rf"}' } });
    // The Deny button is always rendered alongside the edit variants.
    const denyBtn = Array.from(document.querySelectorAll('button')).find((b) => b.textContent?.trim() === 'Deny');
    expect(denyBtn).toBeDefined();
    await fireEvent.click(denyBtn!);
    await waitFor(() => expect(respond).toHaveBeenCalled());
    const body = bodyAt(respond);
    expect(body.decision).toBe('deny');
    expect(body.updatedInput).toBeUndefined();
  });
});

// ---- Adversarial content ----

describe('<ApprovalPrompt> edit-input toggle — adversarial', () => {
  it('preserves unicode and nested structures through the edit round-trip', async () => {
    const respond = setBindingMock('RespondToApproval', async () => {});
    const { getByTestId } = await mountWith(toolApproval());
    const value = JSON.stringify({
      label: '日本語',
      nested: { a: [1, 2, { deep: '💥' }] },
      escaped: 'line1\nline2\ttab',
    });
    await fireEvent.click(getByTestId('approval-edit-toggle'));
    await fireEvent.input(getByTestId('approval-edit-textarea'), { target: { value } });
    await fireEvent.click(getByTestId('approval-allow-with-edits'));
    await waitFor(() => expect(respond).toHaveBeenCalled());
    expect(bodyAt(respond).updatedInput).toEqual(JSON.parse(value));
  });

  it('handles a very large edited payload without truncation', async () => {
    const respond = setBindingMock('RespondToApproval', async () => {});
    const { getByTestId } = await mountWith(toolApproval());
    const big = JSON.stringify({ blob: 'x'.repeat(50_000) });
    await fireEvent.click(getByTestId('approval-edit-toggle'));
    await fireEvent.input(getByTestId('approval-edit-textarea'), { target: { value: big } });
    await fireEvent.click(getByTestId('approval-allow-with-edits'));
    await waitFor(() => expect(respond).toHaveBeenCalled());
    const body = bodyAt(respond);
    expect((body.updatedInput as { blob: string }).blob).toHaveLength(50_000);
  });

  it('surfaces a RespondToApproval rejection as a pane error', async () => {
    setBindingMock('RespondToApproval', async () => {
      throw new Error('network is down');
    });
    const { getByTestId, pane } = await mountWith(toolApproval());
    await fireEvent.click(getByTestId('approval-edit-toggle'));
    await fireEvent.click(getByTestId('approval-allow-with-edits'));
    await waitFor(() => {
      expect(pane.generalError).toMatch(/Failed to respond to approval/i);
    });
  });

  it('re-opening the editor after Cancel restores the ORIGINAL input, not the edited text', async () => {
    const { getByTestId } = await mountWith(toolApproval());
    await fireEvent.click(getByTestId('approval-edit-toggle'));
    await fireEvent.input(getByTestId('approval-edit-textarea'), { target: { value: '{"command":"mutated"}' } });
    await fireEvent.click(getByTestId('approval-edit-cancel'));
    await fireEvent.click(getByTestId('approval-edit-toggle'));
    const ta = getByTestId('approval-edit-textarea') as HTMLTextAreaElement;
    expect(ta.value).toBe(JSON.stringify({ command: 'ls -la', timeout: 2000 }, null, 2));
  });
});

void vi;
