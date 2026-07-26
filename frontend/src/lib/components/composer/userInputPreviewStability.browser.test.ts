import { describe, it, expect, afterEach, vi } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
// Real production stylesheet: these assertions are cascade-coupled to the
// utilities the panel uses -- `content-start` on the option column and the
// stacked `col-start-1 row-start-1 / invisible` preview layers. Drop either
// and the invariants below fail.
import '../../../app.css';
import ComposerPendingUserInputPanel from './ComposerPendingUserInputPanel.svelte';
import type { UserInputRequest } from '../../types/events';

// happy-dom reports zero geometry, so this invariant -- the side-by-side
// option/preview layout must not change size when the focused option changes
// -- can only be verified in a real layout engine. Runs in the `browser`
// vitest project (real Chromium via Playwright); see frontend/vitest.config.ts.
//
// Why it matters: UserInputOptionButton binds `onmouseenter={onFocus}`, and
// this panel renders inside the bottom-anchored composer overlay. If focus
// changed the layout's height, hovering an option would move the option list
// under a stationary cursor, hand the pointer to a *different* option, fire
// its mouseenter, and oscillate -- forever, for any cursor position in the
// band where the two layouts disagree about the hit target. The pre-fix
// layout moved the panel 59px and stretched every option button 48px -> 68px.

const mounted: HTMLElement[] = [];

afterEach(() => {
  cleanup();
  for (const el of mounted.splice(0)) el.remove();
});

const LONG_PREVIEW = Array.from({ length: 40 }, (_, i) => `line ${i + 1} of a long preview`).join('\n\n');

function previewRequest(): UserInputRequest {
  return {
    requestId: 'req-preview-stability',
    threadId: 'thread-1',
    toolName: 'request_user_input',
    title: 'User Input Required',
    questions: [
      {
        id: 'layout',
        header: 'Layout',
        question: 'Pick one',
        options: [
          { label: 'Short', description: 'one line', preview: 'short.' },
          { label: 'Long', description: 'many lines', preview: LONG_PREVIEW },
          { label: 'None', description: 'no preview at all' },
        ],
      },
    ],
  };
}

async function mountPanel(): Promise<HTMLElement> {
  const host = document.createElement('div');
  // The production parent is a fixed-width bottom-anchored overlay; give the
  // panel a comparable width so the preview wraps the way it really does.
  host.style.cssText = 'width: 900px; position: absolute; bottom: 0; left: 0;';
  document.body.appendChild(host);
  mounted.push(host);
  render(ComposerPendingUserInputPanel, {
    target: host,
    props: {
      request: previewRequest(),
      customAnswer: '',
      submitSignal: 0,
      setCustomAnswerText: vi.fn(),
      onResolve: vi.fn(),
      onResolved: vi.fn(),
      onError: vi.fn(),
    },
  });
  // Let ChatMarkdown mount and lay out every preview layer.
  await new Promise((resolve) => setTimeout(resolve, 100));
  return host;
}

function optionButtons(host: HTMLElement): HTMLButtonElement[] {
  return [...host.querySelectorAll<HTMLButtonElement>('[data-user-input-option]')];
}

/** Focus an option the way the pointer does, then let layout settle. */
async function focusOption(host: HTMLElement, index: number): Promise<void> {
  optionButtons(host)[index].dispatchEvent(new MouseEvent('mouseenter', { bubbles: false }));
  await new Promise((resolve) => setTimeout(resolve, 50));
}

/** Geometry the hover feedback loop would run on. */
function geometry(host: HTMLElement) {
  const panel = host.querySelector('[data-testid="composer-pending-user-input"]')!;
  return {
    panelHeight: Math.round(panel.getBoundingClientRect().height),
    previewHeight: Math.round(
      host.querySelector('[data-testid="user-input-preview"]')!.getBoundingClientRect().height,
    ),
    buttons: optionButtons(host).map((b) => {
      const r = b.getBoundingClientRect();
      return { top: Math.round(r.top), bottom: Math.round(r.bottom) };
    }),
  };
}

describe('user-input option preview layout stability', () => {
  it('keeps panel and option geometry identical across every focused option', async () => {
    const host = await mountPanel();
    const states = [];
    for (const index of [0, 1, 2]) {
      await focusOption(host, index);
      states.push(geometry(host));
    }
    // Sanity: the fixture really does have a preview tall enough to have
    // moved things before the fix (a short one would pass vacuously).
    expect(states[0].previewHeight).toBeGreaterThan(100);
    for (const state of states.slice(1)) expect(state).toEqual(states[0]);
  });

  it('shows exactly one preview layer while laying out all of them', async () => {
    const host = await mountPanel();
    const layers = [...host.querySelectorAll<HTMLElement>('[data-user-input-preview]')];
    expect(layers).toHaveLength(3);

    await focusOption(host, 1);
    const visible = layers.filter((l) => getComputedStyle(l).visibility === 'visible');
    expect(visible).toHaveLength(1);
    expect(visible[0].dataset.active).toBe('true');
    expect(visible[0].textContent).toContain('line 1 of a long preview');
    // The hidden layers still occupy their cell -- that is what holds the
    // height constant. `invisible` must never become `hidden`/`display:none`.
    for (const layer of layers) expect(layer.getBoundingClientRect().height).toBeGreaterThan(0);
  });

  it('sizes the preview cell to the tallest option, capped at max-h-60', async () => {
    const host = await mountPanel();
    const layers = [...host.querySelectorAll<HTMLElement>('[data-user-input-preview]')];
    const heights = layers.map((l) => Math.round(l.getBoundingClientRect().height));
    // Layers share one grid cell and stretch to it, so every layer -- short
    // preview, long preview, no preview -- reports the row height. That
    // uniformity IS the stability property.
    expect(new Set(heights).size).toBe(1);
    // The long preview overflows, so the row sits at the per-layer max-h-60.
    expect(heights[0]).toBe(240);

    const cell = host.querySelector('[data-testid="user-input-preview"]')!;
    const cs = getComputedStyle(cell);
    const chrome = ['paddingTop', 'paddingBottom', 'borderTopWidth', 'borderBottomWidth']
      .reduce((sum, prop) => sum + Number.parseFloat(cs[prop as 'paddingTop']), 0);
    expect(Math.round(cell.getBoundingClientRect().height)).toBe(heights[0] + Math.round(chrome));
  });

  it('keeps option buttons at their natural height beside a tall preview', async () => {
    const host = await mountPanel();
    const buttons = optionButtons(host);
    // Without `content-start` the option column's auto rows stretch to fill
    // the preview's height, inflating every button (48px -> 68px pre-fix).
    for (const button of buttons) {
      expect(Math.round(button.getBoundingClientRect().height)).toBeLessThan(60);
    }
    // The stack as a whole must stay well short of the preview column --
    // the buttons pack at the top rather than spreading down it.
    const span = buttons.at(-1)!.getBoundingClientRect().bottom - buttons[0].getBoundingClientRect().top;
    const preview = host.querySelector('[data-testid="user-input-preview"]')!.getBoundingClientRect();
    expect(span).toBeLessThan(preview.height);
  });
});
