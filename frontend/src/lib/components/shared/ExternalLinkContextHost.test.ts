import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { render, cleanup, fireEvent } from '@testing-library/svelte';
import ExternalLinkContextHost from './ExternalLinkContextHost.svelte';
import { buildPathLinkHref } from '../../utils/pathLinkExtension';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { copyToClipboard } from '../../utils/clipboard';
import { addToast } from '../../stores/toast.svelte';

vi.mock('../../utils/clipboard', () => ({ copyToClipboard: vi.fn(async () => true) }));
vi.mock('../../stores/toast.svelte', () => ({ addToast: vi.fn() }));

const copyMock = vi.mocked(copyToClipboard);
const toastMock = vi.mocked(addToast);

function anchor(href: string): HTMLAnchorElement {
  const link = document.createElement('a');
  link.setAttribute('href', href);
  link.textContent = 'link';
  document.body.appendChild(link);
  return link;
}

function menuItem(label: string): HTMLElement {
  const found = Array.from(document.querySelectorAll<HTMLElement>('[role="menuitem"]'))
    .find((el) => el.textContent?.trim() === label);
  if (!found) throw new Error(`no menu item labelled ${label}`);
  return found;
}

function rightClick(el: Element): MouseEvent {
  const event = new MouseEvent('contextmenu', { bubbles: true, cancelable: true });
  el.dispatchEvent(event);
  return event;
}

describe('ExternalLinkContextHost', () => {
  beforeEach(() => {
    resetBindingMocks();
    copyMock.mockClear();
    copyMock.mockResolvedValue(true);
    toastMock.mockClear();
  });

  afterEach(() => {
    cleanup();
    document.body.innerHTML = '';
  });

  it('opens on an outbound link and suppresses the native menu', async () => {
    render(ExternalLinkContextHost);
    const event = rightClick(anchor('https://example.com/docs'));
    await Promise.resolve();

    expect(event.defaultPrevented).toBe(true);
    expect(document.querySelector('[role="menu"]')).not.toBeNull();
  });

  // The menu must never appear on something the click delegate would not
  // open — both resolve the target through externalURLForEventTarget.
  it('leaves path links, relative hrefs and plain text to the platform menu', async () => {
    render(ExternalLinkContextHost);
    for (const href of [
      buildPathLinkHref('src/foo.ts', undefined, undefined, ''),
      '/docs/local',
      'javascript:alert(1)',
    ]) {
      const event = rightClick(anchor(href));
      await Promise.resolve();
      expect(event.defaultPrevented).toBe(false);
      expect(document.querySelector('[role="menu"]')).toBeNull();
    }

    const plain = document.createElement('span');
    plain.textContent = 'no link here';
    document.body.appendChild(plain);
    const event = rightClick(plain);
    await Promise.resolve();
    expect(event.defaultPrevented).toBe(false);
    expect(document.querySelector('[role="menu"]')).toBeNull();
  });

  it('routes Open Link through the host open binding and closes', async () => {
    const open = setBindingMock('OpenExternalURL', vi.fn(async () => undefined));
    render(ExternalLinkContextHost);
    rightClick(anchor('https://example.com/docs'));
    await Promise.resolve();

    await fireEvent.click(menuItem('Open Link'));

    expect(open).toHaveBeenCalledExactlyOnceWith('https://example.com/docs');
    expect(document.querySelector('[role="menu"]')).toBeNull();
  });

  it('copies the resolved URL and reports the outcome', async () => {
    render(ExternalLinkContextHost);
    rightClick(anchor('https://example.com/docs?q=1'));
    await Promise.resolve();

    await fireEvent.click(menuItem('Copy Link Address'));

    expect(copyMock).toHaveBeenCalledExactlyOnceWith('https://example.com/docs?q=1');
    expect(toastMock).toHaveBeenCalledWith('info', 'Copied link address.');
    expect(document.querySelector('[role="menu"]')).toBeNull();
  });

  it('surfaces a failed copy instead of swallowing it', async () => {
    copyMock.mockResolvedValue(false);
    render(ExternalLinkContextHost);
    rightClick(anchor('https://example.com/docs'));
    await Promise.resolve();

    await fireEvent.click(menuItem('Copy Link Address'));

    expect(toastMock).toHaveBeenCalledWith('error', 'Copy failed.');
  });

  it('dismisses on Escape', async () => {
    render(ExternalLinkContextHost);
    rightClick(anchor('https://example.com/docs'));
    await Promise.resolve();
    expect(document.querySelector('[role="menu"]')).not.toBeNull();

    await fireEvent.keyDown(document, { key: 'Escape' });
    expect(document.querySelector('[role="menu"]')).toBeNull();
  });

  it('stops listening once unmounted', async () => {
    const { unmount } = render(ExternalLinkContextHost);
    unmount();

    const event = rightClick(anchor('https://example.com/docs'));
    await Promise.resolve();

    expect(event.defaultPrevented).toBe(false);
    expect(document.querySelector('[role="menu"]')).toBeNull();
  });
});
