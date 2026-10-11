import { afterEach, beforeAll, describe, expect, it } from 'vitest';
// The production stylesheet carries the neo drop-shadow override; the
// test's subject is the rendered SVG under the real cascade, so it runs
// in the `browser` project against real Chromium.
import '../../../../app.css';
import mermaid from 'mermaid';
import { mermaidRenderConfig } from './mermaidRenderConfig';

const FLOWCHART = `flowchart TD
  A[Chat view] -->|send prompt| Q[(Message queue)]
  A -.->|cancel| Q
  Q -->|dequeue| B{Workflow step?}
  B -->|reply| C[Provider session]
  B -->|tool call| D[Tool runner]
  D -->|result| C`;

const CLASS = `classDiagram
  class Provider {
    +Start() error
  }
  Provider <|-- ClaudeProvider
  Store o-- Thread : caches
  Thread "1" *-- "many" Item : contains`;

const SEQUENCE = `sequenceDiagram
  participant A as Client
  participant B as Backend
  A->>B: send prompt
  loop retry
    B-->>A: ack
  end`;

const hosts: HTMLElement[] = [];

// As Mermaid.svelte does on load: the first initialize registers the
// diagram detectors that detectType reads.
beforeAll(() => {
  mermaid.initialize(mermaidRenderConfig(''));
});

afterEach(() => {
  for (const host of hosts.splice(0)) host.remove();
});

// Mirrors Mermaid.svelte: the rendered markup goes inside the page's
// `<svg data-mermaid-svg>` target, whose attributes it also takes.
async function renderInto(code: string): Promise<{ svg: SVGSVGElement; markup: string }> {
  mermaid.initialize(mermaidRenderConfig(mermaid.detectType(code)));
  const { svg: markup } = await mermaid.render(`m${hosts.length}${Date.now()}`, code);
  const host = document.createElement('div');
  host.innerHTML = '<svg data-mermaid-svg></svg>';
  const svg = host.firstElementChild as SVGSVGElement;
  svg.innerHTML = markup;
  const rendered = new DOMParser().parseFromString(markup, 'image/svg+xml').documentElement;
  for (const attribute of Array.from(rendered.attributes)) {
    if (attribute.name !== 'id') svg.setAttribute(attribute.name, attribute.value);
  }
  document.body.appendChild(host);
  hosts.push(host);
  return { svg, markup };
}

function pathCommands(d: string): string {
  return d.replace(/[^A-Za-z]/g, '');
}

function labelBox(svg: SVGSVGElement, text: string): DOMRect {
  const label = Array.from(svg.querySelectorAll('.edgeLabel')).find((el) => el.textContent?.trim() === text);
  if (!label) throw new Error(`no edge label ${text}`);
  return label.getBoundingClientRect();
}

function shadowed(svg: SVGSVGElement): Element[] {
  return Array.from(svg.querySelectorAll('*')).filter((el) => getComputedStyle(el).filter !== 'none');
}

describe('mermaid 12 render config (real render)', () => {
  it('draws flowchart edges as right-angle steps on dagre, labels apart', async () => {
    const { svg } = await renderInto(FLOWCHART);
    const edges = Array.from(svg.querySelectorAll<SVGPathElement>('path.flowchart-link'));
    expect(edges.length).toBe(6);
    for (const edge of edges) {
      // stepAfter is line segments only; basis would emit cubic curves.
      expect(pathCommands(edge.getAttribute('d') ?? '')).toMatch(/^M[LHV]+$/);
    }
    // The two labels leaving "Chat view": ELK places them 4 px apart so
    // they read as one phrase; dagre keeps a visible gap (20 px here,
    // 70 px on the app's larger sample diagram).
    const send = labelBox(svg, 'send prompt');
    const cancel = labelBox(svg, 'cancel');
    const gap = Math.max(cancel.left - send.right, send.left - cancel.right);
    expect(gap).toBeGreaterThanOrEqual(12);
  });

  it('keeps curved edges for class diagrams so their markers stay upright', async () => {
    const { svg } = await renderInto(CLASS);
    const relations = Array.from(svg.querySelectorAll<SVGPathElement>('path.relation'));
    expect(relations.length).toBeGreaterThan(0);
    expect(relations.some((edge) => pathCommands(edge.getAttribute('d') ?? '').includes('C'))).toBe(true);
  });

  it('renders the neo look without its drop shadow under app.css', async () => {
    for (const code of [FLOWCHART, CLASS, SEQUENCE]) {
      const { svg, markup } = await renderInto(code);
      // The rule has work to do: mermaid still emits the shadow.
      expect(markup).toContain('drop-shadow');
      expect(svg.querySelector('[data-look="neo"]')).not.toBeNull();
      expect(shadowed(svg)).toEqual([]);
    }
  });
});
