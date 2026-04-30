import { describe, expect, it } from 'vitest';
import { render } from '@testing-library/svelte';
import SessionDiedNotification from './SessionDiedNotification.svelte';
import type { Item } from '../../types/models';

function makeNotification(meta: Record<string, unknown>, summary = ''): Item {
  return {
    id: 'notification:0:0',
    threadId: 'thread-1',
    turnIndex: 0,
    itemIndex: 0,
    kind: 'notification',
    role: 'system',
    status: 'completed',
    summary: summary || 'Provider session ended',
    meta: JSON.stringify({ kind: 'session_died', ...meta }),
    createdAt: 0,
    updatedAt: 0,
  } as Item;
}

describe('<SessionDiedNotification>', () => {
  it('shows the wire summary as the primary line', () => {
    const { getByTestId } = render(SessionDiedNotification, {
      props: { item: makeNotification({ exitCode: 1 }, 'Provider session exited with code 1') },
    });
    expect(getByTestId('session-died-notification').textContent).toContain(
      'Provider session exited with code 1',
    );
  });

  it('renders the signal-killed detail line when meta.signal is present', () => {
    const { getByText } = render(SessionDiedNotification, {
      props: { item: makeNotification({ signal: 'SIGKILL' }) },
    });
    expect(getByText(/Killed by signal SIGKILL/)).toBeInTheDocument();
  });

  it('renders the exit-code detail line when meta.exitCode is present', () => {
    const { getByText } = render(SessionDiedNotification, {
      props: { item: makeNotification({ exitCode: 137 }) },
    });
    expect(getByText(/Exited with code 137/)).toBeInTheDocument();
  });

  it('forwards meta.reason as the wrapper title for hover', () => {
    const { getByTestId } = render(SessionDiedNotification, {
      props: { item: makeNotification({ reason: 'killed by host', exitCode: 0 }) },
    });
    expect(getByTestId('session-died-notification').getAttribute('title')).toBe(
      'killed by host',
    );
  });
});
