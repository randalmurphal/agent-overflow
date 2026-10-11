import { describe, expect, it, beforeEach, vi } from 'vitest';
import { render } from '@testing-library/svelte';
import AdvisorRow from './AdvisorRow.svelte';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { makeItem } from '../../../test/helpers/chat';

describe('<AdvisorRow>', () => {
  beforeEach(() => {
    resetBindingMocks();
    setBindingMock('GetPayloadPreview', vi.fn(async () => ({ data: '', size: 0, isComplete: true })));
  });

  it('renders the running state with a spinner indicator and the advisor model affix in the header', () => {
    const item = makeItem({
      kind: 'tool_call',
      status: 'running',
      toolName: 'advisor',
      summary: '',
      meta: JSON.stringify({ toolName: 'advisor', advisor_model: 'claude-opus-4-7' }),
    });
    const { getByTestId, queryByTestId } = render(AdvisorRow, { props: { item } });

    // The header is the only visible affordance while running — no payload
    // body yet because payloadId is unset and hasExpandableBody is false.
    expect(queryByTestId('advisor-row-body')).toBeNull();

    const status = getByTestId('advisor-row-status');
    const indicator = status.querySelector('[data-testid="indicator"]');
    expect(indicator?.getAttribute('data-state')).toBe('running');
    expect(indicator?.getAttribute('aria-label')).toBe('Running');

    // Header label always shows the literal "advisor" gutter chip; the
    // body slot carries the model-affixed "Advisor (Opus 4.7)" pair —
    // rendered as two-tone spans (subdued model parenthetical) to
    // match AgentRow, so textContent reads without the literal space
    // (CSS ml-1 supplies the visual gap).
    expect(getByTestId('advisor-row-label').textContent).toBe('advisor');
    const preview = getByTestId('advisor-row-preview');
    expect(preview.textContent).toContain('Advisor(Opus 4.7)');
  });

  it('drops the parenthetical when advisor_model is missing from meta', () => {
    // Defensive: a hostile or pre-flag-shaped envelope might not carry
    // `advisor_model`. The header must still read cleanly without
    // rendering "(Unknown)" or the raw wire id.
    const item = makeItem({
      kind: 'tool_call',
      status: 'running',
      toolName: 'advisor',
      summary: '',
      meta: JSON.stringify({ toolName: 'advisor' }),
    });
    const { getByTestId } = render(AdvisorRow, { props: { item } });
    const previewText = getByTestId('advisor-row-preview').textContent ?? '';
    expect(previewText).toContain('Advisor');
    expect(previewText).not.toContain('(');
    expect(previewText).not.toContain('Unknown');
  });

  it('shows the advisor model when it lands on an already-completed row', async () => {
    // Headless Claude reports the advisor's model on the API message's
    // closing usage, after the advisor result settles the row.
    const completed = {
      kind: 'tool_call',
      status: 'completed',
      toolName: 'advisor',
      summary: 'advisor',
    } as const;
    const { getByTestId, rerender } = render(AdvisorRow, {
      props: { item: makeItem({ ...completed, meta: JSON.stringify({ toolName: 'advisor' }) }) },
    });
    expect(getByTestId('advisor-row-preview').textContent).not.toContain('(');

    await rerender({
      item: makeItem({
        ...completed,
        meta: JSON.stringify({ toolName: 'advisor', advisor_model: 'claude-fable-5-1' }),
      }),
    });
    expect(getByTestId('advisor-row-preview').textContent).toContain('Advisor(Fable 5.1)');
  });

  it('renders the completed state with an expandable body chevron when payloadId is set, sourcing preview text from payloadMeta', () => {
    // The collapsed-row preview pulls from the stored payload header's
    // `preview` field (the 240-char truncation triage writes alongside
    // the tool_call_result payload), NOT from item.summary which for
    // advisor calls is the literal "advisor" gutter label.
    const item = makeItem({
      kind: 'tool_call',
      status: 'completed',
      toolName: 'advisor',
      summary: 'advisor',
      payloadId: 'tool-call-result:srvtoolu_abc',
      payloadKind: 'tool_call_result',
      payloadMeta: JSON.stringify({ preview: 'Reviewer suggested two refactors' }),
      meta: JSON.stringify({ toolName: 'advisor', advisor_model: 'claude-opus-4-7' }),
    });
    const { getByTestId } = render(AdvisorRow, { props: { item } });

    // Toggle is enabled (not aria-disabled) since the row has a payload.
    const toggle = getByTestId('advisor-row-toggle');
    expect(toggle.getAttribute('aria-disabled')).toBe('false');
    expect(toggle.getAttribute('aria-expanded')).toBe('false');

    const preview = getByTestId('advisor-row-preview');
    expect(preview.textContent).toContain('Reviewer suggested two refactors');
    expect(preview.textContent).toContain('Advisor(Opus 4.7)');
    // Pin the negative: the gutter label "advisor" must not also leak
    // into the body slot from a stray item.summary fallback.
    expect(preview.textContent?.match(/advisor/gi)?.length ?? 0).toBeLessThan(3);
  });

  it('truncates a long stored preview with an ellipsis', () => {
    const longPreview =
      'This is a very long advisor response that absolutely exceeds the eighty character preview cap so we expect a trailing ellipsis.';
    const item = makeItem({
      kind: 'tool_call',
      status: 'completed',
      toolName: 'advisor',
      summary: 'advisor',
      payloadId: 'tool-call-result:srvtoolu_long',
      payloadKind: 'tool_call_result',
      payloadMeta: JSON.stringify({ preview: longPreview }),
      meta: JSON.stringify({ toolName: 'advisor', advisor_model: 'claude-opus-4-7' }),
    });
    const { getByTestId } = render(AdvisorRow, { props: { item } });
    const previewText = getByTestId('advisor-row-preview').textContent ?? '';
    expect(previewText).toContain('…');
    expect(previewText).toContain('This is a very long advisor response');
  });

  it('shows a call the API never ran as Not run, settled and without a failure', () => {
    const item = makeItem({
      kind: 'tool_call',
      status: 'declined',
      toolName: 'advisor',
      summary: 'advisor (declined)',
      meta: JSON.stringify({ toolName: 'advisor', item_status: 'declined' }),
    });
    const { getByTestId, queryByText } = render(AdvisorRow, { props: { item } });
    expect(getByTestId('advisor-row').textContent).toContain('Not run');
    expect(queryByText('Advisor call failed')).toBeNull();
    expect(queryByText('Tool call declined')).toBeNull();
    // It never ran, so it shows no elapsed time.
    expect(getByTestId('advisor-row-duration').textContent?.trim() ?? '').toBe('');
    const indicator = getByTestId('advisor-row-status').querySelector('[data-testid="indicator"]');
    expect(indicator?.getAttribute('data-state')).not.toBe('running');
  });

  it.each([
    ['overloaded', 'Advisor overloaded'],
    ['too_many_requests', 'Advisor rate limited'],
  ])('names a failed call by its %s error code', (code, message) => {
    const item = makeItem({
      kind: 'tool_call',
      status: 'errored',
      toolName: 'advisor',
      summary: 'advisor (error)',
      meta: JSON.stringify({ toolName: 'advisor', is_error: true, advisor_error_code: code }),
    });
    const { getByTestId, queryByText, queryByTestId } = render(AdvisorRow, { props: { item } });
    expect(getByTestId('advisor-row').textContent).toContain(message);
    expect(queryByText('Advisor call failed')).toBeNull();
    expect(queryByTestId('row-error-code')).toBeNull();
  });

  it('shows an unrecognised error code beside the generic failure', () => {
    const item = makeItem({
      kind: 'tool_call',
      status: 'errored',
      toolName: 'advisor',
      summary: 'advisor (error)',
      meta: JSON.stringify({ toolName: 'advisor', is_error: true, advisor_error_code: 'unavailable' }),
    });
    const { getByTestId } = render(AdvisorRow, { props: { item } });
    expect(getByTestId('advisor-row').textContent).toContain('Advisor call failed');
    expect(getByTestId('row-error-code').textContent).toBe('unavailable');
  });

  it('keeps the generic failure for a row that ended unresolved', () => {
    const item = makeItem({
      kind: 'tool_call',
      status: 'errored',
      toolName: 'advisor',
      summary: 'advisor — turn ended with tool unresolved',
      meta: JSON.stringify({ toolName: 'advisor' }),
    });
    const { getByTestId } = render(AdvisorRow, { props: { item } });
    expect(getByTestId('advisor-row').textContent).toContain('Advisor call failed');
  });
});
