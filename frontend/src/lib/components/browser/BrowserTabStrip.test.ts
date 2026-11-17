import { tick } from 'svelte';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';
import BrowserTabStrip, { TAB_MAX_WIDTH, TAB_MIN_WIDTH } from './BrowserTabStrip.svelte';
import { setBindingMock } from '../../../test/mocks/bindings-app';

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

  it('marks a tab an agent is using, in its tooltip too, and only that tab', () => {
    const { view } = mount([page('a', { agent: true }), page('b')]);
    expect(tabOf(view, 'a').querySelector('[data-testid="browser-tab-agent"]')).not.toBeNull();
    expect(tabOf(view, 'a').title).toBe('Page a\nhttps://example.test/a\nAn agent is using this page');
    expect(tabOf(view, 'b').querySelector('[data-testid="browser-tab-agent"]')).toBeNull();
  });

  it('says a crashed tab stopped, apart from one that could not be opened', () => {
    const { view } = mount([page('a', { suspended: true, error: 'the page\'s process ended (crashed)' })]);
    expect(tabOf(view, 'a').title).toBe('This page stopped: the page\'s process ended (crashed)');
  });

  describe('tab menu', () => {
    function openMenu(view: { container: HTMLElement }, id: string): void {
      void fireEvent.contextMenu(tabOf(view, id), { clientX: 40, clientY: 20 });
    }
    function item(testId: string): HTMLElement {
      const el = document.querySelector<HTMLElement>(`[data-testid="${testId}"]`);
      if (!el) throw new Error(`no menu item ${testId}`);
      return el;
    }
    function disabled(testId: string): boolean {
      const el = item(testId);
      return el.getAttribute('aria-disabled') === 'true' || (el as HTMLButtonElement).disabled === true;
    }

    it('opens on a right-click and runs each tab action on that tab', async () => {
      const { view, onAct } = mount([page('a'), page('b'), page('c')], 'a');
      for (const [testId, kind] of [
        ['browser-tab-menu-reload', 'reload'],
        ['browser-tab-menu-duplicate', 'duplicate'],
        ['browser-tab-menu-close', 'close'],
        ['browser-tab-menu-close-others', 'close-others'],
        ['browser-tab-menu-close-right', 'close-right'],
      ] as const) {
        openMenu(view, 'b');
        await tick();
        item(testId).click();
        await tick();
        expect(onAct).toHaveBeenLastCalledWith(kind, 'b');
        expect(document.querySelector('[data-testid="browser-tab-menu-reload"]')).toBeNull();
      }
    });

    it('offers only what applies to the tab', async () => {
      const { view } = mount([page('a'), page('new', { creating: true, url: 'about:blank', title: '' }), page('f', { url: 'file:///repo/a.html' })]);
      openMenu(view, 'f');
      await tick();
      expect(disabled('browser-tab-menu-close-right')).toBe(true);
      expect(disabled('browser-tab-menu-reopen')).toBe(true);
      expect(disabled('browser-tab-menu-external')).toBe(true);
      expect(disabled('browser-tab-menu-copy')).toBe(false);
      await fireEvent.keyDown(document, { key: 'Escape' });
      openMenu(view, 'new');
      await tick();
      for (const testId of ['browser-tab-menu-reload', 'browser-tab-menu-duplicate', 'browser-tab-menu-copy', 'browser-tab-menu-external']) {
        expect(disabled(testId), testId).toBe(true);
      }
      expect(disabled('browser-tab-menu-close-others')).toBe(false);
    });

    it('reopens a closed tab when the thread has one', async () => {
      const onAct = vi.fn(async () => {});
      const view = render(BrowserTabStrip, { sessionName: '', pages: [page('a')] as never, activePageId: 'a', closedTabs: 2, onAct, onNewTab: vi.fn() });
      openMenu(view, 'a');
      await tick();
      expect(disabled('browser-tab-menu-reopen')).toBe(false);
      item('browser-tab-menu-reopen').click();
      expect(onAct).toHaveBeenLastCalledWith('reopen');
    });

    it('copies the address and opens it in the system browser', async () => {
      const writeText = vi.fn(async () => {});
      vi.stubGlobal('navigator', { ...navigator, clipboard: { writeText } });
      const open = vi.fn((_url: string) => null);
      vi.stubGlobal('open', open);
      const external = setBindingMock('OpenExternalURL', vi.fn(async (_url: string) => undefined));
      try {
        const { view } = mount([page('a')]);
        openMenu(view, 'a');
        await tick();
        item('browser-tab-menu-copy').click();
        await waitFor(() => expect(writeText).toHaveBeenCalledWith('https://example.test/a'));
        openMenu(view, 'a');
        await tick();
        item('browser-tab-menu-external').click();
        await waitFor(() => expect(external.mock.calls.length + open.mock.calls.length).toBe(1));
        const opened = external.mock.calls[0]?.[0] ?? open.mock.calls[0]?.[0];
        expect(opened).toBe('https://example.test/a');
      } finally {
        vi.unstubAllGlobals();
      }
    });
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
