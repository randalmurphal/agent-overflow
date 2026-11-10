import { expect, it, vi } from 'vitest';
import { render } from '@testing-library/svelte';
import '../../../../app.css';
import SendButton from './SendButton.svelte';

// The pending state replaces what the button shows, not what it measures:
// a button that changed size when a message was sent would move the
// toolbar controls beside it.
it.each([
  { name: 'icon-only', props: {}, split: false },
  { name: 'labeled', props: { action: 'refine' as const, label: 'Refine proposed plan' }, split: false },
  { name: 'labeled with a comment count', props: { action: 'send-comments' as const, label: 'Send comments', planCommentCount: 12 }, split: false },
  { name: 'implement', props: { action: 'implement' as const, label: 'Implement' }, split: false },
  { name: 'implement split', props: { action: 'implement' as const, label: 'Implement', onSendInNewThread: vi.fn() }, split: true },
  {
    name: 'send-comments split',
    props: { action: 'send-comments' as const, label: 'Send comments', planCommentCount: 3, onSendWithoutPlanComments: vi.fn() },
    split: true,
  },
])('keeps the $name button the same size while its send is pending', async ({ props, split }) => {
  const base = { canSend: false, isTurnActive: false, onSend: vi.fn(), onInterrupt: vi.fn(), ...props };
  const view = render(SendButton, { props: { ...base, sendPending: false } });
  const box = (testId: string) => view.getByTestId(testId).getBoundingClientRect();
  const idle = box('composer-send');
  const idleMenu = split ? box('composer-send-menu') : null;
  expect(idle.width).toBeGreaterThan(0);

  await view.rerender({ ...base, sendPending: true });
  expect(view.getByTestId('composer-send').hasAttribute('data-pending')).toBe(true);
  const pending = box('composer-send');
  expect(pending.width).toBe(idle.width);
  expect(pending.height).toBe(idle.height);
  expect(pending.left).toBe(idle.left);
  if (idleMenu) expect(box('composer-send-menu').left).toBe(idleMenu.left);

  // The spinner sits inside the button, centered on it.
  const spinner = view.getByTestId('composer-send-spinner').querySelector('[data-testid="stepped-spinner"]')!.getBoundingClientRect();
  expect(spinner.width).toBeGreaterThan(0);
  expect(Math.abs((spinner.left + spinner.right) / 2 - (pending.left + pending.right) / 2)).toBeLessThan(0.5);
  expect(Math.abs((spinner.top + spinner.bottom) / 2 - (pending.top + pending.bottom) / 2)).toBeLessThan(0.5);
  // The content it covers paints nothing.
  expect(getComputedStyle(view.getByTestId('composer-send-content')).visibility).toBe('hidden');
});
