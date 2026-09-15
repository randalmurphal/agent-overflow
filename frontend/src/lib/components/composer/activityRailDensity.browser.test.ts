import { afterEach, expect, it } from 'vitest';
import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import '../../../app.css';
import { tick } from 'svelte';
import ActivityRailHost from '../../../test/mocks/ActivityRailTestHost.svelte';
import { buildPane, makeItem } from '../../../test/helpers/chat';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { UsageBucket } from '../../stores/bindings';
import { resetSettingsForTest } from '../../stores/settings.svelte';
import { setCompactLayoutForTest } from '../../stores/layoutMode.svelte';
import { __resetCustomSpinnersForTest } from '../../stores/spinners.svelte';
import { updateFrontendPreferences } from '../../stores/frontendPreferences.svelte';
import { BUILTIN_SPRITES } from '../../spinners/catalog';
import { raf } from '../../../test/helpers/browserFrames';

afterEach(() => {
  setCompactLayoutForTest(false);
  resetSettingsForTest();
  __resetCustomSpinnersForTest();
});

it('protects both controls and holds the row height across sprites, widths and layout changes', async () => {
  setCompactLayoutForTest(true);
  const pane = await buildPane();
  setBindingMock('ListLiveBackgroundTasks', async () => Array.from({ length: 4 }, (_, i) => makeItem({
    id: `background-${i}`, threadId: pane.threadId!, kind: 'tool_call', toolName: 'Bash',
    status: 'running', isBackground: true, summary: 'sleep 30',
  })));
  setBindingMock('GetUsageStats', async () => [new UsageBucket({
    bucket: '', outputTokens: 30800, costUsd: 5.49, turnCount: 1, unpricedRows: 0,
  })]);
  pane.setLiveTodo(Array.from({ length: 6 }, (_, i) => ({ step: `Research source ${i}`, status: i < 2 ? 'completed' : 'inProgress' })));
  pane.setActiveTurn({ turnId: 'rail-layout-turn', turnIndex: 0, startedAt: Date.now() - 453000 });
  const target = document.createElement('div');
  target.style.width = '800px';
  document.body.append(target);
  const view = render(ActivityRailHost, { target, props: { pane } });
  try {
    await view.findByTestId('activity-rail-background-toggle');
    let usage = await view.findByTestId('usage-chip-trigger');
    const row = view.getByTestId('activity-rail').querySelector<HTMLElement>('[data-activity-rail-row]')!;
    const todos = view.getByTestId('activity-rail-todos-toggle');
    const background = view.getByTestId('activity-rail-background-toggle');
    expect(todos.querySelector('.lucide-list-todo')).not.toBeNull();
    expect(background.querySelector('.lucide-send-to-back')).not.toBeNull();
    for (const compact of [false, true, false]) {
      setCompactLayoutForTest(compact);
      updateFrontendPreferences({ spinnerAnimationsEnabled: false });
      target.style.width = '800px';
      await raf();
      await raf();
      if (compact) usage = await view.findByTestId('usage-chip-trigger');
      const height = row.getBoundingClientRect().height;
      for (const width of [800, 412, 360, 288, 272, 800]) {
        target.style.width = `${width}px`;
        for (const sprite of BUILTIN_SPRITES) {
          updateFrontendPreferences({ spinnerAnimationsEnabled: true,
            spinnerDisabledAnimations: BUILTIN_SPRITES.filter(s => s.id !== sprite.id).map(s => s.id) });
          await tick();
          await raf();
          await waitFor(() => {
            expect(row.querySelector('.working-sprite')).toHaveAttribute('data-sprite-id', sprite.id);
            expect(row.scrollWidth, `${compact}/${width}/${sprite.id}`).toBeLessThanOrEqual(row.clientWidth + 1);
            for (const control of [todos, background]) {
              expect(control.scrollWidth, `${width}/${sprite.id} control`).toBeLessThanOrEqual(control.clientWidth + 1);
            }
            expect(row.getBoundingClientRect().height).toBeCloseTo(height, 1);
          });
          if (compact) {
            const art = row.querySelector<HTMLElement>('.working-sprite')!;
            expect(art.getBoundingClientRect().width).toBeLessThanOrEqual(32);
            expect(usage.textContent?.trim()).toBe('30.8k');
            const rects = [view.getByTestId('activity-rail-working'), todos, background, usage].map(el => el.getBoundingClientRect());
            for (let i = 1; i < rects.length; i++) expect(rects[i].left).toBeGreaterThanOrEqual(rects[i - 1].right);
          }
        }
      }
      updateFrontendPreferences({ spinnerAnimationsEnabled: false });
      await raf();
      expect(view.queryByTestId('activity-rail-sprite')).toBeNull();
      expect(row.getBoundingClientRect().height).toBeCloseTo(height, 1);
    }
    await fireEvent.click(todos);
    expect(todos).toHaveAttribute('aria-expanded', 'true');
    await fireEvent.click(background);
    expect(background).toHaveAttribute('aria-expanded', 'true');
    setCompactLayoutForTest(true);
    usage = await view.findByTestId('usage-chip-trigger');
    await fireEvent.click(usage);
    expect(await screen.findByTestId('usage-chip-cost')).toHaveTextContent('$5.49');
  } finally {
    view.unmount();
    target.remove();
  }
}, 15000);
