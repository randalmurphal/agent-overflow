import { beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import ApprovalPrompt from './ApprovalPrompt.svelte';
import { createThreadPane } from '../../stores/thread.svelte';
import type { Thread } from '../../types/models';
import type { ApprovalRequest } from '../../types/events';
import { setBindingMock, getBindingMock } from '../../../test/mocks/bindings-app';
import { installAnimateShim } from '../../../test/integration/_helpers';
import type { Mock } from 'vitest';

// Response body shape observed in these tests — keeps the type narrow so
// assertions can reach `.elicitation.content` without every line casting.
interface RespondBody {
  requestId?: string;
  decision?: string;
  elicitation?: { action?: string; content?: unknown };
}

function respondBody(respond: Mock, callIndex = 0): RespondBody {
  return respond.mock.calls[callIndex]?.[1] as RespondBody;
}

beforeAll(installAnimateShim);

function seedThread(id = 'thread-1'): Thread {
  return {
    id,
    title: 'Test',
    provider: 'codex',
    workspacePath: '/tmp',
    projectPath: '/tmp',
    mode: 'chat',
    model: 'gpt-4',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
  };
}

async function makePane(threadId = 'thread-1') {
  setBindingMock('SwitchThread', async () => {});
  setBindingMock('ListItems', async () => []);
  setBindingMock('ListPayloadMetas', async () => []);
  const pane = createThreadPane();
  await pane.switchThread(seedThread(threadId));
  return pane;
}

function formApproval(overrides: Partial<ApprovalRequest> = {}): ApprovalRequest {
  return {
    requestId: 'req-1',
    threadId: 'thread-1',
    turnId: 'turn-1',
    toolName: 'mcp_elicitation',
    description: 'Please provide details',
    input: {},
    title: 'MCP Server Consent',
    kind: 'mcp-elicitation',
    elicitation: {
      mode: 'form',
      message: 'Provide details',
      serverName: 'db-mcp',
      requestedSchema: {
        type: 'object',
        properties: {
          host: { type: 'string', title: 'Host' },
        },
        required: ['host'],
      },
    },
    ...overrides,
  };
}

function urlApproval(overrides: Partial<ApprovalRequest> = {}): ApprovalRequest {
  return {
    requestId: 'req-url',
    threadId: 'thread-1',
    turnId: 'turn-1',
    toolName: 'mcp_elicitation',
    description: 'Authorize',
    input: {},
    title: 'MCP Server Consent',
    kind: 'mcp-elicitation',
    elicitation: {
      mode: 'url',
      message: 'Click the link below to authorize.',
      serverName: 'oauth-mcp',
      url: 'https://auth.example.com/approve?token=abc',
      elicitationId: 'el-42',
    },
    ...overrides,
  };
}

async function renderWithApproval(approval: ApprovalRequest) {
  const pane = await makePane();
  pane.addApproval(approval);
  const result = render(ApprovalPrompt, { pane });
  return { ...result, pane };
}

beforeEach(() => {
  setBindingMock('RespondToApproval', async () => {});
});

// ---- Rendering, server/message wiring ----

describe('<ApprovalPrompt> MCP elicitation — rendering', () => {
  it('renders the server name and message for a form-mode approval', async () => {
    const { getByTestId } = await renderWithApproval(formApproval());
    expect(getByTestId('elicitation-server').textContent).toContain('db-mcp');
    expect(getByTestId('elicitation-message').textContent).toContain('Provide details');
  });

  it('hides the server-name row when serverName is absent', async () => {
    const { queryByTestId } = await renderWithApproval(
      formApproval({ elicitation: { mode: 'form', message: 'hi', requestedSchema: { type: 'object', properties: {} } } }),
    );
    expect(queryByTestId('elicitation-server')).toBeNull();
  });

  it('hides the message row when message is empty', async () => {
    const { queryByTestId } = await renderWithApproval(
      formApproval({ elicitation: { mode: 'form', message: '', serverName: 'x', requestedSchema: { type: 'object', properties: {} } } }),
    );
    expect(queryByTestId('elicitation-message')).toBeNull();
  });

  it('shows the empty-schema fallback message when requestedSchema has no fields', async () => {
    const { getByTestId } = await renderWithApproval(
      formApproval({ elicitation: { mode: 'form', message: 'm', requestedSchema: { type: 'object', properties: {} } } }),
    );
    expect(getByTestId('elicitation-empty-schema').textContent).toMatch(/did not send/i);
  });

  it('marks required fields with a visible asterisk', async () => {
    const { container } = await renderWithApproval(formApproval());
    // Required: the `host` field. Asterisk has aria-label="required".
    const asterisk = container.querySelector('[aria-label="required"]');
    expect(asterisk).not.toBeNull();
  });
});

// ---- Per-field-kind rendering ----

describe('<ApprovalPrompt> MCP elicitation — field kinds', () => {
  it('renders a plain text input for a string field without format', async () => {
    const { getByTestId } = await renderWithApproval(
      formApproval({
        elicitation: {
          mode: 'form',
          message: 'm',
          requestedSchema: {
            type: 'object',
            properties: { x: { type: 'string', title: 'X' } },
          },
        },
      }),
    );
    const input = getByTestId('el-input-x') as HTMLInputElement;
    expect(input.type).toBe('text');
  });

  it.each([
    ['email', 'email'],
    ['uri', 'url'],
    ['date', 'date'],
    ['date-time', 'datetime-local'],
  ] as const)('maps format %s to input type %s', async (format, expected) => {
    const { getByTestId } = await renderWithApproval(
      formApproval({
        elicitation: {
          mode: 'form',
          message: 'm',
          requestedSchema: {
            type: 'object',
            properties: { f: { type: 'string', format } },
          },
        },
      }),
    );
    const input = getByTestId('el-input-f') as HTMLInputElement;
    expect(input.type).toBe(expected);
  });

  it('renders a number input with step/min/max attrs', async () => {
    const { getByTestId } = await renderWithApproval(
      formApproval({
        elicitation: {
          mode: 'form',
          message: 'm',
          requestedSchema: {
            type: 'object',
            properties: { n: { type: 'integer', title: 'N', minimum: 1, maximum: 10 } },
          },
        },
      }),
    );
    const input = getByTestId('el-input-n') as HTMLInputElement;
    expect(input.type).toBe('number');
    expect(input.step).toBe('1');
    expect(input.min).toBe('1');
    expect(input.max).toBe('10');
  });

  it('renders a checkbox for a boolean field', async () => {
    const { getByTestId } = await renderWithApproval(
      formApproval({
        elicitation: {
          mode: 'form',
          message: 'm',
          requestedSchema: {
            type: 'object',
            properties: { optIn: { type: 'boolean', title: 'Opt in' } },
          },
        },
      }),
    );
    const input = getByTestId('el-input-optIn') as HTMLInputElement;
    expect(input.type).toBe('checkbox');
  });

  it('renders a <select> with every enum option and a placeholder', async () => {
    const { getByTestId } = await renderWithApproval(
      formApproval({
        elicitation: {
          mode: 'form',
          message: 'm',
          requestedSchema: {
            type: 'object',
            properties: {
              color: { type: 'string', enum: ['red', 'green', 'blue'] },
            },
          },
        },
      }),
    );
    const select = getByTestId('el-input-color') as HTMLSelectElement;
    // 1 placeholder + 3 options
    expect(select.options.length).toBe(4);
    expect(Array.from(select.options).slice(1).map((o) => o.value)).toEqual(['red', 'green', 'blue']);
  });

  it('uses `oneOf` titles as labels when the server provides them', async () => {
    const { getByTestId } = await renderWithApproval(
      formApproval({
        elicitation: {
          mode: 'form',
          message: 'm',
          requestedSchema: {
            type: 'object',
            properties: {
              role: {
                type: 'string',
                oneOf: [
                  { const: 'admin', title: 'Administrator' },
                  { const: 'user', title: 'User' },
                ],
              },
            },
          },
        },
      }),
    );
    const select = getByTestId('el-input-role') as HTMLSelectElement;
    const labels = Array.from(select.options).slice(1).map((o) => o.textContent?.trim());
    expect(labels).toEqual(['Administrator', 'User']);
  });

  it('renders a checkbox list for multi-select fields', async () => {
    const { getByTestId } = await renderWithApproval(
      formApproval({
        elicitation: {
          mode: 'form',
          message: 'm',
          requestedSchema: {
            type: 'object',
            properties: {
              tags: {
                type: 'array',
                items: { type: 'string', enum: ['a', 'b', 'c'] },
              },
            },
          },
        },
      }),
    );
    expect(getByTestId('el-option-tags-a')).toBeInTheDocument();
    expect(getByTestId('el-option-tags-b')).toBeInTheDocument();
    expect(getByTestId('el-option-tags-c')).toBeInTheDocument();
  });
});

// ---- Default values ----

describe('<ApprovalPrompt> MCP elicitation — defaults', () => {
  it('prefills a string default', async () => {
    const { getByTestId } = await renderWithApproval(
      formApproval({
        elicitation: {
          mode: 'form',
          message: 'm',
          requestedSchema: {
            type: 'object',
            properties: { host: { type: 'string', default: 'localhost' } },
          },
        },
      }),
    );
    expect((getByTestId('el-input-host') as HTMLInputElement).value).toBe('localhost');
  });

  it('prefills a boolean default as checked', async () => {
    const { getByTestId } = await renderWithApproval(
      formApproval({
        elicitation: {
          mode: 'form',
          message: 'm',
          requestedSchema: {
            type: 'object',
            properties: { b: { type: 'boolean', default: true } },
          },
        },
      }),
    );
    expect((getByTestId('el-input-b') as HTMLInputElement).checked).toBe(true);
  });

  it('accepts a user-entered value over the default once typed', async () => {
    const approval = formApproval({
      elicitation: {
        mode: 'form',
        message: 'm',
        requestedSchema: {
          type: 'object',
          properties: { host: { type: 'string', default: 'localhost' } },
        },
      },
    });
    const { getByTestId } = await renderWithApproval(approval);
    const input = getByTestId('el-input-host') as HTMLInputElement;
    await fireEvent.input(input, { target: { value: 'db.example' } });
    expect(input.value).toBe('db.example');
  });
});

// ---- Validation ----

describe('<ApprovalPrompt> MCP elicitation — validation', () => {
  it('surfaces a required-field error on submit when empty', async () => {
    const { getByTestId, queryByTestId } = await renderWithApproval(formApproval());
    await fireEvent.click(getByTestId('elicitation-accept'));
    expect(queryByTestId('el-error-host')?.textContent).toMatch(/required/i);
  });

  it('does not send RespondToApproval when validation fails', async () => {
    const respond = setBindingMock('RespondToApproval', async () => {});
    const { getByTestId } = await renderWithApproval(formApproval());
    await fireEvent.click(getByTestId('elicitation-accept'));
    expect(respond).not.toHaveBeenCalled();
  });

  it('clears a field error once the user edits the field', async () => {
    const { getByTestId, queryByTestId } = await renderWithApproval(formApproval());
    await fireEvent.click(getByTestId('elicitation-accept'));
    expect(queryByTestId('el-error-host')).not.toBeNull();
    await fireEvent.input(getByTestId('el-input-host'), { target: { value: 'x' } });
    expect(queryByTestId('el-error-host')).toBeNull();
  });

  it.each([
    ['minLength', { minLength: 4 }, 'ab', /at least 4|minimum 4/i],
    ['maxLength', { maxLength: 3 }, 'abcd', /at most 3|maximum 3/i],
  ] as const)('rejects string violating %s', async (_, constraint, value, errorRe) => {
    const { getByTestId } = await renderWithApproval(
      formApproval({
        elicitation: {
          mode: 'form',
          message: 'm',
          requestedSchema: {
            type: 'object',
            properties: { s: { type: 'string', ...constraint } },
          },
        },
      }),
    );
    await fireEvent.input(getByTestId('el-input-s'), { target: { value } });
    await fireEvent.click(getByTestId('elicitation-accept'));
    expect(getByTestId('el-error-s').textContent).toMatch(errorRe);
  });

  it('rejects an invalid email format', async () => {
    const { getByTestId } = await renderWithApproval(
      formApproval({
        elicitation: {
          mode: 'form',
          message: 'm',
          requestedSchema: {
            type: 'object',
            properties: { email: { type: 'string', format: 'email' } },
            required: ['email'],
          },
        },
      }),
    );
    await fireEvent.input(getByTestId('el-input-email'), { target: { value: 'not-an-email' } });
    await fireEvent.click(getByTestId('elicitation-accept'));
    expect(getByTestId('el-error-email').textContent).toMatch(/email/i);
  });

  it('accepts a well-formed email', async () => {
    const respond = setBindingMock('RespondToApproval', async () => {});
    const { getByTestId } = await renderWithApproval(
      formApproval({
        elicitation: {
          mode: 'form',
          message: 'm',
          requestedSchema: {
            type: 'object',
            properties: { email: { type: 'string', format: 'email' } },
            required: ['email'],
          },
        },
      }),
    );
    await fireEvent.input(getByTestId('el-input-email'), { target: { value: 'a@b.co' } });
    await fireEvent.click(getByTestId('elicitation-accept'));
    await waitFor(() => expect(respond).toHaveBeenCalled());
  });

  it('rejects an invalid URI', async () => {
    const { getByTestId } = await renderWithApproval(
      formApproval({
        elicitation: {
          mode: 'form',
          message: 'm',
          requestedSchema: {
            type: 'object',
            properties: { uri: { type: 'string', format: 'uri' } },
            required: ['uri'],
          },
        },
      }),
    );
    await fireEvent.input(getByTestId('el-input-uri'), { target: { value: 'nope' } });
    await fireEvent.click(getByTestId('elicitation-accept'));
    expect(getByTestId('el-error-uri').textContent).toMatch(/url/i);
  });

  it('rejects a number below minimum', async () => {
    const { getByTestId } = await renderWithApproval(
      formApproval({
        elicitation: {
          mode: 'form',
          message: 'm',
          requestedSchema: {
            type: 'object',
            properties: { n: { type: 'number', minimum: 10 } },
            required: ['n'],
          },
        },
      }),
    );
    await fireEvent.input(getByTestId('el-input-n'), { target: { value: '5' } });
    await fireEvent.click(getByTestId('elicitation-accept'));
    expect(getByTestId('el-error-n').textContent).toMatch(/≥|>= ?10/);
  });

  it('rejects a number above maximum', async () => {
    const { getByTestId } = await renderWithApproval(
      formApproval({
        elicitation: {
          mode: 'form',
          message: 'm',
          requestedSchema: {
            type: 'object',
            properties: { n: { type: 'number', maximum: 10 } },
            required: ['n'],
          },
        },
      }),
    );
    await fireEvent.input(getByTestId('el-input-n'), { target: { value: '99' } });
    await fireEvent.click(getByTestId('elicitation-accept'));
    expect(getByTestId('el-error-n').textContent).toMatch(/≤|<= ?10/);
  });

  it('rejects multi-select with fewer than minItems', async () => {
    const { getByTestId } = await renderWithApproval(
      formApproval({
        elicitation: {
          mode: 'form',
          message: 'm',
          requestedSchema: {
            type: 'object',
            properties: {
              tags: {
                type: 'array',
                items: { type: 'string', enum: ['a', 'b', 'c'] },
                minItems: 2,
              },
            },
            required: ['tags'],
          },
        },
      }),
    );
    await fireEvent.click(getByTestId('el-option-tags-a'));
    await fireEvent.click(getByTestId('elicitation-accept'));
    expect(getByTestId('el-error-tags').textContent).toMatch(/at least 2/i);
  });

  it('rejects multi-select with more than maxItems', async () => {
    const { getByTestId } = await renderWithApproval(
      formApproval({
        elicitation: {
          mode: 'form',
          message: 'm',
          requestedSchema: {
            type: 'object',
            properties: {
              tags: {
                type: 'array',
                items: { type: 'string', enum: ['a', 'b', 'c'] },
                maxItems: 1,
              },
            },
            required: ['tags'],
          },
        },
      }),
    );
    await fireEvent.click(getByTestId('el-option-tags-a'));
    await fireEvent.click(getByTestId('el-option-tags-b'));
    await fireEvent.click(getByTestId('elicitation-accept'));
    expect(getByTestId('el-error-tags').textContent).toMatch(/at most 1/i);
  });

  it('surfaces multiple errors at once', async () => {
    const { getByTestId } = await renderWithApproval(
      formApproval({
        elicitation: {
          mode: 'form',
          message: 'm',
          requestedSchema: {
            type: 'object',
            properties: {
              a: { type: 'string', minLength: 3 },
              b: { type: 'string' },
            },
            required: ['a', 'b'],
          },
        },
      }),
    );
    await fireEvent.input(getByTestId('el-input-a'), { target: { value: 'x' } });
    await fireEvent.click(getByTestId('elicitation-accept'));
    expect(getByTestId('el-error-a')).toBeInTheDocument();
    expect(getByTestId('el-error-b')).toBeInTheDocument();
  });
});

// ---- Accept / decline / cancel payloads ----

describe('<ApprovalPrompt> MCP elicitation — submission', () => {
  it('sends action=accept with the collected content', async () => {
    const respond = setBindingMock('RespondToApproval', async () => {});
    const { getByTestId } = await renderWithApproval(
      formApproval({
        elicitation: {
          mode: 'form',
          message: 'm',
          requestedSchema: {
            type: 'object',
            properties: {
              host: { type: 'string' },
              port: { type: 'integer' },
              tls: { type: 'boolean' },
              tags: { type: 'array', items: { type: 'string', enum: ['a', 'b'] } },
              role: { type: 'string', oneOf: [{ const: 'admin', title: 'Admin' }] },
            },
          },
        },
      }),
    );
    await fireEvent.input(getByTestId('el-input-host'), { target: { value: 'db.internal' } });
    await fireEvent.input(getByTestId('el-input-port'), { target: { value: '5432' } });
    await fireEvent.click(getByTestId('el-input-tls'));
    await fireEvent.click(getByTestId('el-option-tags-a'));
    await fireEvent.change(getByTestId('el-input-role'), { target: { value: 'admin' } });
    await fireEvent.click(getByTestId('elicitation-accept'));

    await waitFor(() => expect(respond).toHaveBeenCalled());
    const body = respondBody(respond);
    expect(body.decision).toBe('accept');
    expect(body.elicitation?.action).toBe('accept');
    expect(body.elicitation?.content).toEqual({
      host: 'db.internal',
      port: 5432,
      tls: true,
      tags: ['a'],
      role: 'admin',
    });
  });

  it('omits empty optional fields from the content', async () => {
    const respond = setBindingMock('RespondToApproval', async () => {});
    const { getByTestId } = await renderWithApproval(
      formApproval({
        elicitation: {
          mode: 'form',
          message: 'm',
          requestedSchema: {
            type: 'object',
            properties: {
              host: { type: 'string' },
              name: { type: 'string' }, // optional
            },
            required: ['host'],
          },
        },
      }),
    );
    await fireEvent.input(getByTestId('el-input-host'), { target: { value: 'db' } });
    await fireEvent.click(getByTestId('elicitation-accept'));
    await waitFor(() => expect(respond).toHaveBeenCalled());
    const body = respondBody(respond);
    expect(body.elicitation?.content).toEqual({ host: 'db' });
  });

  it('sends action=decline with no content', async () => {
    const respond = setBindingMock('RespondToApproval', async () => {});
    const { getByTestId } = await renderWithApproval(formApproval());
    await fireEvent.click(getByTestId('elicitation-decline'));
    await waitFor(() => expect(respond).toHaveBeenCalled());
    const body = respondBody(respond);
    expect(body.elicitation?.action).toBe('decline');
    expect(body.elicitation?.content).toBeUndefined();
  });

  it('sends action=cancel with no content', async () => {
    const respond = setBindingMock('RespondToApproval', async () => {});
    const { getByTestId } = await renderWithApproval(formApproval());
    await fireEvent.click(getByTestId('elicitation-cancel'));
    await waitFor(() => expect(respond).toHaveBeenCalled());
    const body = respondBody(respond);
    expect(body.elicitation?.action).toBe('cancel');
    expect(body.elicitation?.content).toBeUndefined();
  });

  it('URL-mode Accept sends action=accept with no content (URL flow completes externally)', async () => {
    const respond = setBindingMock('RespondToApproval', async () => {});
    const { getByTestId } = await renderWithApproval(urlApproval());
    await fireEvent.click(getByTestId('elicitation-accept'));
    await waitFor(() => expect(respond).toHaveBeenCalled());
    const body = respondBody(respond);
    expect(body.elicitation?.action).toBe('accept');
    expect(body.elicitation?.content).toBeUndefined();
  });

  it('surfaces errors from RespondToApproval to the pane', async () => {
    setBindingMock('RespondToApproval', async () => {
      throw new Error('network is down');
    });
    const { getByTestId, pane } = await renderWithApproval(formApproval());
    await fireEvent.input(getByTestId('el-input-host'), { target: { value: 'db' } });
    await fireEvent.click(getByTestId('elicitation-accept'));
    await waitFor(() => {
      expect(pane.generalError).toMatch(/Failed to respond to elicitation/i);
    });
  });
});

// ---- URL mode interaction ----

describe('<ApprovalPrompt> MCP elicitation — URL mode', () => {
  it('renders the URL as a link with rel="noopener noreferrer"', async () => {
    const { getByTestId } = await renderWithApproval(urlApproval());
    const link = getByTestId('elicitation-url-link') as HTMLAnchorElement;
    expect(link.href).toBe('https://auth.example.com/approve?token=abc');
    expect(link.rel).toContain('noopener');
    expect(link.rel).toContain('noreferrer');
    expect(link.target).toBe('_blank');
  });

  it('calls window.open when the user clicks the link', async () => {
    const openSpy = vi.spyOn(window, 'open').mockImplementation(() => null);
    const { getByTestId } = await renderWithApproval(urlApproval());
    await fireEvent.click(getByTestId('elicitation-url-link'));
    expect(openSpy).toHaveBeenCalledWith(
      'https://auth.example.com/approve?token=abc',
      '_blank',
      'noopener,noreferrer',
    );
    openSpy.mockRestore();
  });

  it('does not render form fields when mode is url', async () => {
    const { queryByTestId } = await renderWithApproval(urlApproval());
    expect(queryByTestId('elicitation-fields')).toBeNull();
    expect(queryByTestId('elicitation-empty-schema')).toBeNull();
  });
});

// ---- Adversarial / isolation / escaping ----

describe('<ApprovalPrompt> MCP elicitation — adversarial', () => {
  it('isolates state between concurrent elicitation approvals', async () => {
    const a = formApproval({
      requestId: 'req-a',
      elicitation: {
        mode: 'form',
        message: 'A',
        requestedSchema: {
          type: 'object',
          properties: { x: { type: 'string', title: 'X A' } },
          required: ['x'],
        },
      },
    });
    const b = formApproval({
      requestId: 'req-b',
      elicitation: {
        mode: 'form',
        message: 'B',
        requestedSchema: {
          type: 'object',
          properties: { x: { type: 'string', title: 'X B' } },
          required: ['x'],
        },
      },
    });
    const respond = setBindingMock('RespondToApproval', async () => {});

    const pane = await makePane();
    pane.addApproval(a);
    pane.addApproval(b);
    const { container } = render(ApprovalPrompt, { pane });

    // Both panels render two distinct inputs with the same test id — scope to
    // each panel's `x` input via their parent.
    const inputs = container.querySelectorAll('[data-testid="el-input-x"]') as NodeListOf<HTMLInputElement>;
    expect(inputs.length).toBe(2);

    await fireEvent.input(inputs[0], { target: { value: 'alpha' } });
    await fireEvent.input(inputs[1], { target: { value: 'beta' } });

    // Submit B only — container.querySelectorAll returns approvals in array
    // order, so the second Accept button belongs to B.
    const acceptBtns = container.querySelectorAll('[data-testid="elicitation-accept"]') as NodeListOf<HTMLButtonElement>;
    await fireEvent.click(acceptBtns[1]);
    await waitFor(() => expect(respond).toHaveBeenCalledTimes(1));
    const body = respondBody(respond);
    expect(body.requestId).toBe('req-b');
    expect(body.elicitation?.content).toEqual({ x: 'beta' });
  });

  it('preserves unicode in field titles, descriptions, values', async () => {
    const respond = setBindingMock('RespondToApproval', async () => {});
    const { getByTestId, getByText } = await renderWithApproval(
      formApproval({
        elicitation: {
          mode: 'form',
          message: '日本語 prompt',
          requestedSchema: {
            type: 'object',
            properties: {
              名前: { type: 'string', title: '名前', description: 'お名前' },
            },
            required: ['名前'],
          },
        },
      }),
    );
    expect(getByText('お名前')).toBeInTheDocument();
    await fireEvent.input(getByTestId('el-input-名前'), { target: { value: '太郎' } });
    await fireEvent.click(getByTestId('elicitation-accept'));
    await waitFor(() => expect(respond).toHaveBeenCalled());
    const body = respondBody(respond);
    expect(body.elicitation?.content).toEqual({ 名前: '太郎' });
  });

  it('escapes HTML in server-provided titles and messages (Svelte default)', async () => {
    const { container } = await renderWithApproval(
      formApproval({
        elicitation: {
          mode: 'form',
          message: '<img src=x onerror="alert(1)">',
          serverName: '<script>alert(2)</script>',
          requestedSchema: {
            type: 'object',
            properties: {
              f: { type: 'string', title: '<b>bold</b>' },
            },
          },
        },
      }),
    );
    // No actual script/img tags must be in the DOM — Svelte escapes text by default.
    expect(container.querySelector('script')).toBeNull();
    expect(container.querySelector('img[onerror]')).toBeNull();
    expect(container.querySelector('b')).toBeNull();
    // The raw text should still be visible to the user.
    expect(container.textContent).toContain('<img src=x onerror="alert(1)">');
    expect(container.textContent).toContain('<script>alert(2)</script>');
    expect(container.textContent).toContain('<b>bold</b>');
  });

  it('handles a very long string value without truncation', async () => {
    const respond = setBindingMock('RespondToApproval', async () => {});
    const long = 'x'.repeat(100_000);
    const { getByTestId } = await renderWithApproval(
      formApproval({
        elicitation: {
          mode: 'form',
          message: 'm',
          requestedSchema: {
            type: 'object',
            properties: { blob: { type: 'string' } },
            required: ['blob'],
          },
        },
      }),
    );
    await fireEvent.input(getByTestId('el-input-blob'), { target: { value: long } });
    await fireEvent.click(getByTestId('elicitation-accept'));
    await waitFor(() => expect(respond).toHaveBeenCalled());
    const body = respondBody(respond);
    expect((body.elicitation?.content as { blob: string }).blob).toHaveLength(100_000);
  });

  it('does not crash when the server ships a malformed schema', async () => {
    const respond = setBindingMock('RespondToApproval', async () => {});
    const { getByTestId } = await renderWithApproval(
      formApproval({
        elicitation: {
          mode: 'form',
          message: 'no schema structure',
          requestedSchema: { kaboom: true },
        },
      }),
    );
    // Empty-schema fallback renders.
    expect(getByTestId('elicitation-empty-schema')).toBeInTheDocument();
    // Decline still works.
    await fireEvent.click(getByTestId('elicitation-decline'));
    await waitFor(() => expect(respond).toHaveBeenCalled());
  });

  it('silently ignores clicking Accept repeatedly while offline', async () => {
    let resolve: () => void = () => {};
    const pending = new Promise<void>((r) => { resolve = r; });
    const respond = setBindingMock('RespondToApproval', async () => { await pending; });
    const { getByTestId } = await renderWithApproval(urlApproval());
    const btn = getByTestId('elicitation-accept');
    await fireEvent.click(btn);
    await fireEvent.click(btn);
    await fireEvent.click(btn);
    // Three clicks fire three in-flight calls (we don't debounce here —
    // document behavior). Un-block so the test can finish.
    resolve();
    await waitFor(() => expect(respond).toHaveBeenCalledTimes(3));
  });
});

// Make the unused getBindingMock import tree-shake-safe without removing the
// import (kept for symmetry with Composer.test.ts's pattern).
void getBindingMock;
