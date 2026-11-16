import { fireEvent, render } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';
import BrowserTabStrip, { TAB_MAX_WIDTH, TAB_MIN_WIDTH } from './BrowserTabStrip.svelte';

function page(id: string, extra: Record<string, unknown> = {}) {
  return { id, url: `https://example.test/${id}`, title: `Page ${id}`, canGoBack: false, canGoForward: false, ...extra };
}

function mount(pages: ReturnType<typeof page>[], activePageId = pages[0]?.id ?? '') {
  const onAct = vi.fn(async () => {});
  const onNewTab = vi.fn();
  const onEmptyDoubleClick = vi.fn();
  const view = render(BrowserTabStrip, {
    sessionName: '',
    pages: pages as never,
    activePageId,
    onAct,
    onNewTab,
    onEmptyDoubleClick,
  });
  return { view, onAct, onNewTab, onEmptyDoubleClick };
}

function tabOf(view: { container: HTMLElement }, id: string): HTMLElement {
  const el = view.container.querySelector<HTMLElement>(`[data-tab-id="${id}"]`);
  if (!el) throw new Error(`no tab ${id}`);
  return el;
}

describe('<BrowserTabStrip>', () => {
  it('keeps the new-tab button outside the scrolling tab row, after it', () => {
    const { view, onNewTab } = mount([page('a'), page('b')]);
    const scroller = view.getByTestId('browser-tab-scroller');
    const newTab = view.getByRole('button', { name: 'New tab' });
    expect(scroller.contains(newTab)).toBe(false);
    expect(scroller.compareDocumentPosition(newTab) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(scroller.getAttribute('role')).toBe('tablist');
    newTab.click();
    expect(onNewTab).toHaveBeenCalledTimes(1);
  });

  it('bounds every tab between the minimum and maximum width', () => {
    const { view } = mount([page('a'), page('b'), page('c')]);
    for (const id of ['a', 'b', 'c']) {
      const tab = tabOf(view, id);
      expect(tab.style.minWidth).toBe(`${TAB_MIN_WIDTH}px`);
      expect(tab.style.maxWidth).toBe(`${TAB_MAX_WIDTH}px`);
    }
    expect(TAB_MIN_WIDTH).toBeLessThan(TAB_MAX_WIDTH);
  });

  it('shows each state in the favicon slot: alert, stepped spinner, favicon, globe', () => {
    const { view } = mount([
      page('err', { error: 'profile is locked' }),
      page('new', { creating: true, title: '', url: 'about:blank' }),
      page('back', { restoring: true }),
      page('load', { loading: true, favicon: 'data:image/png;base64,AAAA' }),
      page('fav', { favicon: 'data:image/png;base64,AAAA' }),
      page('bare'),
    ]);
    const icons = view.getAllByTestId('browser-tab-icon').map((icon) => icon.dataset.icon);
    expect(icons).toEqual(['error', 'spinner', 'spinner', 'spinner', 'favicon', 'globe']);
    expect(view.getAllByTestId('browser-tab').map((tab) => tab.dataset.tabState))
      .toEqual(['error', 'creating', 'restoring', 'loading', 'ready', 'ready']);
    expect(tabOf(view, 'fav').querySelector('img')?.getAttribute('src')).toBe('data:image/png;base64,AAAA');
    expect(tabOf(view, 'new').textContent).toContain('Opening…');
  });

  it('gives every tab a tooltip with its title and address, and a failed tab its error', () => {
    const { view } = mount([page('a'), page('b', { error: 'profile is locked' }), page('c', { title: '', url: 'about:blank' })]);
    expect(tabOf(view, 'a').title).toBe('Page a\nhttps://example.test/a');
    expect(tabOf(view, 'b').title).toBe('Could not open this tab: profile is locked');
    expect(tabOf(view, 'c').title).toBe('New tab');
  });

  it('shows the active tab\'s close button at rest, others on hover, and every one under compact', () => {
    const { view, onAct } = mount([page('a'), page('b')], 'b');
    const close = (id: string) => tabOf(view, id).querySelector<HTMLElement>('[data-testid="browser-tab-close"]')!;
    expect(close('b').className).not.toContain('opacity-0');
    expect(close('a').className).toContain('opacity-0');
    expect(close('a').className).toContain('group-hover:opacity-100');
    for (const id of ['a', 'b']) expect(close(id).className).toContain('compact:opacity-100');
    expect(tabOf(view, 'b').dataset.active).toBe('true');
    expect(view.getAllByRole('tab').map((tab) => tab.getAttribute('aria-selected'))).toEqual(['false', 'true']);
    close('a').click();
    expect(onAct).toHaveBeenCalledWith('close', 'a');
  });

  it('closes a tab on a middle click', async () => {
    const { view, onAct } = mount([page('a'), page('b')]);
    await fireEvent(tabOf(view, 'b'), new MouseEvent('auxclick', { button: 1, bubbles: true }));
    expect(onAct).toHaveBeenCalledWith('close', 'b');
  });

  it('activates a tab on click', () => {
    const { view, onAct } = mount([page('a'), page('b')]);
    tabOf(view, 'b').querySelector<HTMLElement>('[data-testid="browser-tab"]')!.click();
    expect(onAct).toHaveBeenCalledWith('activate', 'b');
  });

  it('toggles on a double-click of the strip\'s empty space, never of a tab or a button', async () => {
    const { view, onEmptyDoubleClick } = mount([page('a')]);
    await fireEvent.dblClick(tabOf(view, 'a').querySelector('[data-testid="browser-tab"]')!);
    await fireEvent.dblClick(view.getByRole('button', { name: 'New tab' }));
    expect(onEmptyDoubleClick).not.toHaveBeenCalled();
    await fireEvent.dblClick(view.getByTestId('browser-tab-strip-space'));
    await fireEvent.dblClick(view.getByTestId('browser-tab-strip'));
    expect(onEmptyDoubleClick).toHaveBeenCalledTimes(2);
  });

  it('marks the insertion point while a tab is dragged and moves it on drop', async () => {
    const { view, onAct } = mount([page('a'), page('b'), page('c')]);
    const transfer = {
      data: new Map<string, string>(),
      types: [] as string[],
      setData(type: string, value: string) { this.data.set(type, value); this.types.push(type); },
      getData(type: string) { return this.data.get(type) ?? ''; },
      effectAllowed: '', dropEffect: '',
    };
    await fireEvent.dragStart(tabOf(view, 'a'), { dataTransfer: transfer });
    const target = tabOf(view, 'c');
    target.getBoundingClientRect = () => ({ left: 200, right: 300, width: 100, top: 0, bottom: 30, height: 30, x: 200, y: 0, toJSON() {} });
    await fireEvent.dragOver(target, { dataTransfer: transfer, clientX: 290 });
    const indicators = view.getAllByTestId('browser-tab-drop-indicator');
    expect(indicators).toHaveLength(1);
    expect(target.contains(indicators[0])).toBe(true);
    await fireEvent.drop(target, { dataTransfer: transfer });
    expect(onAct).toHaveBeenCalledWith('move', 'a', '', 2);
    expect(view.queryByTestId('browser-tab-drop-indicator')).toBeNull();
  });
});
