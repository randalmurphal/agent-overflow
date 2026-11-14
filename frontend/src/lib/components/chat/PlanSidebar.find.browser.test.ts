// The plan pane's find returns focus into the plan pane when the element it
// was opened from is gone: a replacement plan remounts the keyed review
// surface, removing a comment editor the reader was typing in. A real browser,
// because only a real browser refuses focus to an element that cannot take it.
import { afterEach, beforeEach, expect, it } from 'vitest';
import { flushSync, mount, tick, unmount } from 'svelte';
import { userEvent } from 'vitest/browser';
import '../../../app.css';
import PlanSidebar from './PlanSidebar.svelte';
import { buildPane, emitItemEventUpsert, makeItem } from '../../../test/helpers/chat';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { setupEventListeners } from '../../stores/events';
import { resetProposedPlanCacheForTests } from '../../stores/proposedPlans.svelte';
import { makePanelContext } from '../../stores/panelContext.svelte';
import { closePaneFind, getPaneFind, openPaneFind, resetPaneFindForTest } from '../../stores/paneFind.svelte';
import { companionForSource, installCompanionPanes, resetCompanionPanesForTest } from '../../stores/companionPanes.svelte';
import { resetPaneLayoutForTest, setPaneLayoutItemsForTest } from '../../stores/paneLayout.svelte';

let cleanupEvents: (() => void) | null = null;
let component: ReturnType<typeof mount> | null = null;
let host: HTMLElement | null = null;

function plan(id: string, turnIndex: number) {
  return makeItem({
    id, turnIndex, itemIndex: 0, kind: 'tool_call', payloadId: `payload-${id}`, payloadKind: 'proposed_plan',
    payloadMeta: JSON.stringify({ title: id, preview: id, lineCount: 1, charCount: 8 }),
  });
}

beforeEach(() => {
  resetBindingMocks();
  resetProposedPlanCacheForTests();
  resetPaneFindForTest();
  resetCompanionPanesForTest();
  installCompanionPanes();
  cleanupEvents = setupEventListeners();
});

afterEach(() => {
  if (component) unmount(component);
  component = null;
  host?.remove();
  host = null;
  cleanupEvents?.();
  resetProposedPlanCacheForTests();
  resetPaneFindForTest();
  resetCompanionPanesForTest();
  resetPaneLayoutForTest();
});

it('returns focus into the plan pane when a replacement plan removed the comment editor it came from', async () => {
  let plans = [plan('plan-1', 0)];
  setBindingMock('ListThreadProposedPlans', async () => plans);
  setBindingMock('GetPayloadData', async (_thread: string, payloadId: string) => ({
    data: payloadId === 'payload-plan-2' ? '# Second plan' : '# A plan',
  }));
  setBindingMock('ListProposedPlanComments', async () => [{
    id: 'c1', threadId: 't', planItemId: 'plan-1', status: 'draft', startLine: 1, endLine: 1,
    selectedText: '# A plan', body: 'tighten this', createdAt: 1, updatedAt: 1,
  }]);
  const pane = await buildPane();
  setPaneLayoutItemsForTest([{ id: pane.paneId, paneId: pane.paneId, kind: 'thread', widthPx: 600 }]);
  pane.setShowPlanSidebar(true);
  const planPaneId = companionForSource(pane.paneId, 'plan')!.paneId;

  host = document.createElement('div');
  host.style.height = '600px';
  host.style.display = 'flex';
  host.style.flexDirection = 'column';
  document.body.appendChild(host);
  component = mount(PlanSidebar, { target: host, props: { ctx: makePanelContext(pane, () => {}) } });

  // The reader edits a draft comment, then opens find from its editor.
  await expect.poll(() => host!.querySelector('[aria-label="Edit comment"]')).toBeTruthy();
  await userEvent.click(host.querySelector<HTMLElement>('[aria-label="Edit comment"]')!);
  await expect.poll(() => host!.querySelector('[data-testid="plan-comment"] textarea')).toBeTruthy();
  const editor = host.querySelector<HTMLTextAreaElement>('[data-testid="plan-comment"] textarea')!;
  editor.focus();
  expect(document.activeElement).toBe(editor);
  openPaneFind(planPaneId, { returnFocus: editor });
  flushSync();
  await expect.poll(() => document.activeElement?.getAttribute('data-testid')).toBe('pane-find-input');

  // A replacement plan arrives while find stays open: the editor is gone.
  plans = [plan('plan-2', 1), plan('plan-1', 0)];
  emitItemEventUpsert({
    rev: 0, id: 'plan-2', threadId: pane.thread!.id, turnIndex: 1, itemIndex: 0, kind: 'tool_call',
    role: 'assistant', status: 'completed', summary: '', payloadKind: 'proposed_plan', createdAt: 0, updatedAt: 0,
  });
  await expect.poll(() => host!.textContent?.includes('Second plan')).toBe(true);
  expect(editor.isConnected).toBe(false);
  expect(getPaneFind(planPaneId)?.open).toBe(true);

  closePaneFind(planPaneId);
  await tick();
  const scroller = host.querySelector<HTMLElement>('[data-testid="plan-sidebar-scroll"]')!;
  await expect.poll(() => document.activeElement).toBe(scroller);
  // Focusable by script only: no tab stop.
  expect(scroller.tabIndex).toBe(-1);
});
