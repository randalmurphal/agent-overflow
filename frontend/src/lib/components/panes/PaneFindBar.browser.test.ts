// The find bar's focus return in a real browser, which refuses focus to an
// element that cannot take it: the return ends on an element that took
// focus, and a fallback that cannot is reported.
import { afterEach, beforeEach, expect, it } from 'vitest';
import { flushSync, mount, tick, unmount } from 'svelte';
import '../../../app.css';
import PaneFindBar from './PaneFindBar.svelte';
import { closePaneFind, openPaneFind, registerPaneFindProvider, resetPaneFindForTest } from '../../stores/paneFind.svelte';
import { resetPaneLayoutForTest, setPaneLayoutItemsForTest } from '../../stores/paneLayout.svelte';
import { fakeFindProvider } from '../../../test/helpers/paneFind.svelte';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { frontendErrorCaptureStateForTest, resetFrontendErrorCaptureForTest } from '../../utils/frontendErrorCapture';

let component: ReturnType<typeof mount> | null = null;

beforeEach(() => {
  resetPaneFindForTest();
  setPaneLayoutItemsForTest([{ id: 'a', paneId: 'a', kind: 'thread', widthPx: 600 }]);
  registerPaneFindProvider('a', fakeFindProvider());
  setBindingMock('ReportFrontendErrorBatch', async () => '');
});

afterEach(() => {
  if (component) unmount(component);
  component = null;
  resetFrontendErrorCaptureForTest();
  resetPaneFindForTest();
  resetPaneLayoutForTest();
  document.body.replaceChildren();
});

async function closeAfterOriginRemoved(fallback: HTMLElement): Promise<void> {
  const origin = document.createElement('textarea');
  document.body.append(origin, fallback);
  const target = document.createElement('div');
  document.body.appendChild(target);
  component = mount(PaneFindBar, { target, props: { paneId: 'a', title: 'A', fallbackFocus: () => fallback } });
  origin.focus();
  openPaneFind('a', { returnFocus: origin });
  flushSync();
  await expect.poll(() => document.activeElement?.getAttribute('data-testid')).toBe('pane-find-input');
  origin.remove();
  closePaneFind('a');
  await tick();
  await tick();
}

it('focuses a script-focusable fallback when the origin is gone', async () => {
  const fallback = document.createElement('div');
  fallback.tabIndex = -1;
  await closeAfterOriginRemoved(fallback);
  await expect.poll(() => document.activeElement).toBe(fallback);
  expect(frontendErrorCaptureStateForTest().pendingCount).toBe(0);
});

it('reports a fallback that cannot take focus', async () => {
  const fallback = document.createElement('div');
  await closeAfterOriginRemoved(fallback);
  await expect.poll(() => frontendErrorCaptureStateForTest().pendingCount).toBe(1);
  expect(document.activeElement).toBe(document.body);
});
