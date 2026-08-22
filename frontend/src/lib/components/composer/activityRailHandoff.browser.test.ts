import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { cleanup, render } from '@testing-library/svelte';
import { tick } from 'svelte';
import '../../../app.css';
import ActivityRailHost from '../../../test/mocks/ActivityRailTestHost.svelte';
import { buildPane } from '../../../test/helpers/chat';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import {
  projectSendStarted,
  resetForTest as resetThreadStatuses,
} from '../../stores/threadStatuses.svelte';

function nextFrame(): Promise<void> {
  return new Promise((resolve) => requestAnimationFrame(() => resolve()));
}

describe('activity rail pending-send handoff', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetThreadStatuses();
    setBindingMock('ListLiveBackgroundTasks', async () => []);
  });

  afterEach(() => {
    cleanup();
    resetThreadStatuses();
  });

  it('keeps the working chip width and label nodes stable when the provider starts', async () => {
    const pane = await buildPane();
    projectSendStarted(pane.threadId!);
    const { findByTestId } = render(ActivityRailHost, { props: { pane } });
    await tick();
    await nextFrame();

    const working = await findByTestId('activity-rail-working');
    const label = await findByTestId('activity-rail-working-label');
    const elapsed = await findByTestId('activity-rail-working-elapsed');
    const pendingWidth = working.getBoundingClientRect().width;
    expect(getComputedStyle(elapsed).visibility).toBe('hidden');

    pane.setActiveTurn({ turnId: 'turn-1', turnIndex: 0, startedAt: Date.now() });
    await tick();
    await nextFrame();

    expect(await findByTestId('activity-rail-working-label')).toBe(label);
    expect(await findByTestId('activity-rail-working-elapsed')).toBe(elapsed);
    expect(getComputedStyle(elapsed).visibility).toBe('visible');
    expect(working.getBoundingClientRect().width).toBeCloseTo(pendingWidth, 1);
  });
});
