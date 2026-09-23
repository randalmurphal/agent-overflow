// The delegated `contextmenu` hosts (diagram, attachment image, external
// link) share one contract: a host yields on an event something earlier
// already claimed (`defaultPrevented`), and App.svelte mounts them most
// specific first, so their document listeners run in that order and exactly
// one menu opens for any press.

import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render } from '@testing-library/svelte';
import { tick } from 'svelte';
import { SRC_ROOT } from '../../../test/sourceScan';
import { attachmentImageMenuTag } from '../../utils/imageMenuActions';
import DiagramInteractionHost from '../chat/DiagramInteractionHost.svelte';
import ImageMenuHost from '../chat/ImageMenuHost.svelte';
import ExternalLinkContextHost from './ExternalLinkContextHost.svelte';

vi.mock('../../utils/diagramClipboard', () => ({
  copyAsPNG: vi.fn(async () => {}),
  copyAsSVG: vi.fn(async () => {}),
  copySource: vi.fn(async () => {}),
}));

const HOSTS_IN_ORDER = ['DiagramInteractionHost', 'ImageMenuHost', 'ExternalLinkContextHost'];

function mountHostsInAppOrder(): void {
  render(DiagramInteractionHost);
  render(ImageMenuHost);
  render(ExternalLinkContextHost);
}

function menus(): string[] {
  return Array.from(document.querySelectorAll('[role="menu"]')).map(
    (menu) => menu.getAttribute('aria-label') ?? '',
  );
}

function diagram(): { host: HTMLElement; svg: SVGSVGElement } {
  const host = document.createElement('div');
  host.setAttribute('data-mermaid-source', 'graph TD; A-->B');
  const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg') as SVGSVGElement;
  svg.setAttribute('data-mermaid-svg', '');
  host.appendChild(svg);
  return { host, svg };
}

function image(): HTMLImageElement {
  const img = document.createElement('img');
  for (const [name, value] of Object.entries(
    attachmentImageMenuTag({ id: 'att-1', threadId: 'thread-1', filename: 'hero.png' }),
  )) {
    img.setAttribute(name, value);
  }
  return img;
}

function link(): HTMLAnchorElement {
  const anchor = document.createElement('a');
  anchor.href = 'https://example.com/docs';
  return anchor;
}

async function rightClick(target: Element): Promise<MouseEvent> {
  const event = new MouseEvent('contextmenu', { bubbles: true, cancelable: true, clientX: 10, clientY: 10 });
  await fireEvent(target, event);
  await tick();
  return event;
}

describe('delegated context menu hosts', () => {
  afterEach(() => {
    cleanup();
    document.body.innerHTML = '';
  });

  it('are mounted in App.svelte most specific first', () => {
    const app = readFileSync(join(SRC_ROOT, 'App.svelte'), 'utf8');
    const positions = HOSTS_IN_ORDER.map((name) => app.indexOf(`<${name} />`));
    expect(positions.every((at) => at >= 0), 'every host is mounted in App.svelte').toBe(true);
    expect([...positions].sort((a, b) => a - b)).toEqual(positions);
  });

  it('open exactly one menu, the most specific, for a press inside a link', async () => {
    mountHostsInAppOrder();
    const wrapped = link();
    const img = image();
    wrapped.appendChild(img);
    document.body.appendChild(wrapped);
    await rightClick(img);
    expect(menus()).toEqual(['Image Actions']);
  });

  it('open the link menu for a plain link', async () => {
    mountHostsInAppOrder();
    const plain = link();
    plain.textContent = 'docs';
    document.body.appendChild(plain);
    await rightClick(plain);
    expect(menus()).toEqual(['Link Actions']);
  });

  it.each([
    ['diagram', () => { const { host, svg } = diagram(); return { root: host, target: svg as Element }; }],
    ['attachment image', () => { const img = image(); return { root: img, target: img as Element }; }],
    ['external link', () => { const anchor = link(); anchor.textContent = 'docs'; return { root: anchor, target: anchor as Element }; }],
  ])('each yields a %s press an inner handler already claimed', async (_name, build) => {
    mountHostsInAppOrder();
    const { root, target } = build();
    const owner = document.createElement('div');
    owner.addEventListener('contextmenu', (event) => event.preventDefault());
    owner.appendChild(root);
    document.body.appendChild(owner);
    await rightClick(target);
    expect(menus()).toEqual([]);
  });
});
