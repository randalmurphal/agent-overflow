import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { page } from 'vitest/browser';
import { render } from '@testing-library/svelte';
import { tick } from 'svelte';
import '../../../app.css';
import { resetBindingMocks } from '../../../test/mocks/bindings-app';
import type { ThreadPane } from '../../stores/thread.svelte';
import { applyShellCommandEvent, resetShellRunsForTest } from '../../stores/shellCommands.svelte';
import ShellRunCard from './ShellRunCard.svelte';

// The pane the card hands focus within: its composer textarea is found by
// pane id and label, as paneComposerFocus finds the real one.
function mountPane() {
  const host = document.createElement('div');
  host.setAttribute('data-pane-id', 'pane-1');
  const cardHost = document.createElement('div');
  const composer = document.createElement('textarea');
  composer.setAttribute('aria-label', 'Message Input');
  host.append(cardHost, composer);
  document.body.appendChild(host);
  const pane = { threadId: 'thread-1', paneId: 'pane-1', setGeneralError: vi.fn() } as unknown as ThreadPane;
  const view = render(ShellRunCard, { target: cardHost, props: { pane } });
  return { host, composer, view };
}

function start(): void {
  applyShellCommandEvent({ phase: 'started', threadId: 'thread-1', runId: 'run-1', command: 'sudo true', startedAt: Date.now() });
}

function ask(terminalSeq: number, text: string): void {
  applyShellCommandEvent({ phase: 'terminal', threadId: 'thread-1', runId: 'run-1', terminalSeq, terminal: text });
}

describe('ShellRunCard prompt focus', () => {
  let cleanup: (() => void) | null = null;

  beforeEach(() => {
    resetBindingMocks();
    resetShellRunsForTest();
  });

  afterEach(() => {
    cleanup?.();
    cleanup = null;
  });

  it('moves focus from the composer to the answer field when the prompt appears, once', async () => {
    const { host, composer, view } = mountPane();
    cleanup = () => host.remove();
    start();
    await tick();
    composer.focus();
    ask(1, '[sudo] password for me: ');
    await tick();
    expect(document.activeElement).toBe(view.getByTestId('shell-run-answer'));

    // Back in the composer by choice: a later frame of the same prompt does
    // not take focus again.
    composer.focus();
    ask(2, '[sudo] password for me: \nSorry, try again.\n[sudo] password for me: ');
    await tick();
    expect(document.activeElement).toBe(composer);
  });

  it('hands focus to the composer when the prompt is answered, and takes it for the next prompt', async () => {
    const { host, composer, view } = mountPane();
    cleanup = () => host.remove();
    start();
    await tick();
    composer.focus();
    ask(1, '[sudo] password for me: ');
    await tick();
    expect(document.activeElement).toBe(view.getByTestId('shell-run-answer'));

    ask(2, '[sudo] password for me: \n');
    await tick();
    expect(view.queryByTestId('shell-run-answer')).toBeNull();
    expect(document.activeElement).toBe(composer);

    ask(3, '[sudo] password for me: \nSorry, try again.\n[sudo] password for me: ');
    await tick();
    expect(document.activeElement).toBe(view.getByTestId('shell-run-answer'));
  });

  it('moves focus from nowhere to the answer field', async () => {
    const { host, view } = mountPane();
    cleanup = () => host.remove();
    start();
    await tick();
    (document.activeElement as HTMLElement | null)?.blur();
    expect(document.activeElement).toBe(document.body);
    ask(1, 'Password: ');
    await tick();
    expect(document.activeElement).toBe(view.getByTestId('shell-run-answer'));
  });

  it('leaves focus that is elsewhere alone', async () => {
    const { host, view } = mountPane();
    const other = document.createElement('input');
    document.body.appendChild(other);
    cleanup = () => { host.remove(); other.remove(); };
    start();
    await tick();
    other.focus();
    ask(1, 'Password: ');
    await tick();
    expect(view.getByTestId('shell-run-answer')).not.toBeNull();
    expect(document.activeElement).toBe(other);
  });

  it('hands focus to the composer when the run ends while the field has it', async () => {
    const { host, composer, view } = mountPane();
    cleanup = () => host.remove();
    start();
    ask(1, 'Password: ');
    await tick();
    view.getByTestId('shell-run-answer').focus();
    applyShellCommandEvent({ phase: 'finished', threadId: 'thread-1', runId: 'run-1', state: 'sent' });
    await tick();
    expect(view.queryByTestId('shell-run-card')).toBeNull();
    expect(document.activeElement).toBe(composer);
  });

  it.each([360, 1280])('keeps the answer row inside the card at %ipx', async (width) => {
    await page.viewport(width, 800);
    const { host, view } = mountPane();
    cleanup = () => host.remove();
    start();
    ask(1, `${'very long prompt text '.repeat(20)}: `);
    await tick();
    const card = view.getByTestId('shell-run-card').getBoundingClientRect();
    for (const id of ['shell-run-answer', 'shell-run-answer-reveal', 'shell-run-answer-send']) {
      const rect = view.getByTestId(id).getBoundingClientRect();
      expect(rect.width, id).toBeGreaterThan(0);
      expect(rect.left, id).toBeGreaterThanOrEqual(card.left);
      expect(rect.right, id).toBeLessThanOrEqual(card.right);
    }
    expect(view.getByTestId('shell-run-answer').getBoundingClientRect().width).toBeGreaterThan(width / 3);
  });
});
