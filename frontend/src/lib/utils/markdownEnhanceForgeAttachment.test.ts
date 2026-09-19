import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  ensureForgeAttachmentClickDelegate,
  __resetForgeAttachmentDelegateForTest,
} from './markdownEnhance';
import { buildForgeAttachmentHref } from './forgeAttachments';
import { PATH_LINK_HREF_PREFIX } from './pathLinkExtension';

const open = vi.hoisted(() => vi.fn());
vi.mock('./forgeAttachmentActions', () => ({
  openForgeAttachment: (...args: unknown[]) => open(...args),
}));

const HEX = '0123456789abcdef0123456789abcdef';
const HREF = buildForgeAttachmentHref({
  href: `/uploads/${HEX}/report.pdf`,
  pr: { forge: 'gitlab', namespace: 'group', repo: 'widget', number: 3 },
  backend: 'gpu',
  webBase: 'https://gitlab.example.test/group/widget/-/merge_requests/3',
});

function mountAnchor(href: string): HTMLAnchorElement {
  const a = document.createElement('a');
  a.setAttribute('href', href);
  a.textContent = 'the report';
  document.body.appendChild(a);
  return a;
}

describe('ensureForgeAttachmentClickDelegate', () => {
  beforeEach(() => {
    open.mockReset();
    __resetForgeAttachmentDelegateForTest();
  });
  afterEach(() => {
    document.body.innerHTML = '';
    __resetForgeAttachmentDelegateForTest();
  });

  it('runs the attachment action instead of navigating to the custom scheme', () => {
    ensureForgeAttachmentClickDelegate();
    const link = mountAnchor(HREF);

    const event = new MouseEvent('click', { bubbles: true, cancelable: true, button: 0 });
    link.dispatchEvent(event);

    expect(event.defaultPrevented).toBe(true);
    expect(open).toHaveBeenCalledTimes(1);
    expect(open.mock.calls[0][0]).toMatchObject({
      href: `/uploads/${HEX}/report.pdf`,
      backend: 'gpu',
    });
  });

  it('resolves the enclosing anchor, not just a direct hit', () => {
    ensureForgeAttachmentClickDelegate();
    const link = mountAnchor(HREF);
    link.innerHTML = '<span id="inner">the report</span>';

    document.getElementById('inner')!
      .dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true, button: 0 }));

    expect(open).toHaveBeenCalledTimes(1);
  });

  // The href is an unregistered custom scheme: a middle-click "open in new
  // tab" would become an external-protocol-handler request carrying the PR
  // reference if anything on the host ever claimed `agent-overflow:`.
  it('suppresses middle and right button flows on the anchor', () => {
    ensureForgeAttachmentClickDelegate();
    const link = mountAnchor(HREF);

    const aux = new MouseEvent('auxclick', { bubbles: true, cancelable: true, button: 1 });
    link.dispatchEvent(aux);
    expect(aux.defaultPrevented).toBe(true);

    const secondary = new MouseEvent('click', { bubbles: true, cancelable: true, button: 1 });
    link.dispatchEvent(secondary);
    expect(open).not.toHaveBeenCalled();
  });

  it.each([
    `${PATH_LINK_HREF_PREFIX}path=%2Frepo%2Fx.md`,
    'agent-overflow:forge?nonce=deadbeef&href=x&forge=github&repo=x&n=1',
    'https://example.test/x',
  ])('leaves another scheme, and a forged nonce, to their own owners: %s', (href) => {
    ensureForgeAttachmentClickDelegate();
    const link = mountAnchor(href);

    const event = new MouseEvent('click', { bubbles: true, cancelable: true, button: 0 });
    link.dispatchEvent(event);

    expect(open).not.toHaveBeenCalled();
    expect(event.defaultPrevented).toBe(false);
  });

  it('installs only one listener however many surfaces ask', () => {
    ensureForgeAttachmentClickDelegate();
    ensureForgeAttachmentClickDelegate();
    const link = mountAnchor(HREF);

    link.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true, button: 0 }));

    expect(open).toHaveBeenCalledTimes(1);
  });
});
