import { describe, expect, it } from 'vitest';
import { render } from '@testing-library/svelte';
import type { SettledTurn } from '../../stores/thread.svelte';
import CompletionDivider from './CompletionDivider.svelte';

function makeTurn(overrides: Partial<SettledTurn> = {}): SettledTurn {
  return {
    turnId: 'turn-1',
    turnIndex: 0,
    startedAt: 0,
    completedAt: 12_000,
    stopReason: 'end_turn',
    assistantMessageId: 'text:0:0',
    tokenUsage: null,
    aborted: false,
    errorMessage: '',
    ...overrides,
  };
}

describe('<CompletionDivider>', () => {
  it('renders the "Response" base label for a normal completed turn', () => {
    const turn = makeTurn({ startedAt: 0, completedAt: 12_000 });
    const { getByTestId } = render(CompletionDivider, { props: { turn } });
    const label = getByTestId('completion-divider-label').textContent ?? '';
    expect(label).toContain('Response');
  });

  it('pins sub-60s elapsed formatting as "Xs"', () => {
    const turn = makeTurn({ startedAt: 0, completedAt: 12_000 });
    const { getByTestId } = render(CompletionDivider, { props: { turn } });
    expect(getByTestId('completion-divider-label').textContent).toContain('Worked for 12s');
  });

  it('pins >=60s elapsed formatting as "Xm Ys"', () => {
    // 90s elapsed -> "1m 30s".
    const turn = makeTurn({ startedAt: 0, completedAt: 90_000 });
    const { getByTestId } = render(CompletionDivider, { props: { turn } });
    expect(getByTestId('completion-divider-label').textContent).toContain('Worked for 1m 30s');
  });

  it('omits the "Worked for ..." suffix when elapsed is zero', () => {
    // startedAt === completedAt; elapsed seconds is 0. The label should
    // fall back to the bare base label rather than rendering "Worked for 0s".
    const turn = makeTurn({ startedAt: 5_000, completedAt: 5_000 });
    const { getByTestId } = render(CompletionDivider, { props: { turn } });
    const label = getByTestId('completion-divider-label').textContent ?? '';
    expect(label).not.toContain('Worked for');
  });

  it('clamps negative elapsed deltas to zero rather than rendering garbage', () => {
    // Backend clock skew could produce completedAt < startedAt. The
    // divider should not leak a negative "Worked for -1s" string.
    const turn = makeTurn({ startedAt: 10_000, completedAt: 9_000 });
    const { getByTestId } = render(CompletionDivider, { props: { turn } });
    expect(getByTestId('completion-divider-label').textContent).not.toContain('-');
  });

  it('appends "· 150 tokens" below the 1k threshold', () => {
    const turn = makeTurn({
      startedAt: 0,
      completedAt: 12_000,
      tokenUsage: { inputTokens: 100, outputTokens: 50 },
    });
    const { getByTestId } = render(CompletionDivider, { props: { turn } });
    const label = getByTestId('completion-divider-label').textContent ?? '';
    expect(label).toContain('150 tokens');
    expect(label).not.toContain('k tokens');
  });

  it('formats >=1000 tokens as "Yk tokens" with two decimals', () => {
    // 1234 total -> "1.23k tokens" per docs/architecture/turn-lifecycle.md.
    const turn = makeTurn({
      startedAt: 0,
      completedAt: 12_000,
      tokenUsage: { inputTokens: 1000, outputTokens: 234 },
    });
    const { getByTestId } = render(CompletionDivider, { props: { turn } });
    expect(getByTestId('completion-divider-label').textContent).toContain('1.23k tokens');
  });

  it('omits the token suffix when tokenUsage is null', () => {
    const turn = makeTurn({ startedAt: 0, completedAt: 12_000, tokenUsage: null });
    const { getByTestId } = render(CompletionDivider, { props: { turn } });
    const label = getByTestId('completion-divider-label').textContent ?? '';
    expect(label).not.toContain('tokens');
  });

  it('omits the token suffix when input + output sum to zero', () => {
    // A zero-token interrupt still carries a tokenUsage object; collapse
    // it to the "no meaningful count" path rather than writing "0 tokens".
    const turn = makeTurn({
      startedAt: 0,
      completedAt: 12_000,
      tokenUsage: { inputTokens: 0, outputTokens: 0 },
    });
    const { getByTestId } = render(CompletionDivider, { props: { turn } });
    const label = getByTestId('completion-divider-label').textContent ?? '';
    expect(label).not.toContain('tokens');
  });

  it('shows "Interrupted" label for an aborted turn', () => {
    const turn = makeTurn({ aborted: true, stopReason: 'interrupted' });
    const { getByTestId } = render(CompletionDivider, { props: { turn } });
    expect(getByTestId('completion-divider-label').textContent).toContain('Interrupted');
  });

  it('does not render the error line on an aborted turn even if errorMessage is set', () => {
    // Aborted wins over error in base-label selection; the error string
    // stays out of the error-colored second row when the label is
    // already "Interrupted".
    const turn = makeTurn({ aborted: true, errorMessage: 'stopped' });
    const { queryByTestId, getByTestId } = render(CompletionDivider, { props: { turn } });
    expect(getByTestId('completion-divider-label').textContent).toContain('Interrupted');
    expect(queryByTestId('completion-divider-error')).toBeNull();
  });

  it('shows "Error" label + renders errorMessage inline for an error turn', () => {
    const turn = makeTurn({
      stopReason: 'error',
      errorMessage: 'rate_limited: try again',
    });
    const { getByTestId } = render(CompletionDivider, { props: { turn } });
    expect(getByTestId('completion-divider-label').textContent).toContain('Error');
    const errLine = getByTestId('completion-divider-error');
    expect(errLine.textContent).toBe('rate_limited: try again');
    // Error rendering must use the error color class so users can pick
    // it out against the quiet surround.
    expect(errLine.className).toContain('text-error');
  });

  it('omits the error line when errorMessage is an empty string', () => {
    const turn = makeTurn({ errorMessage: '' });
    const { queryByTestId } = render(CompletionDivider, { props: { turn } });
    expect(queryByTestId('completion-divider-error')).toBeNull();
  });
});
