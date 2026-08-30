import { render } from '@testing-library/svelte';
import { beforeEach, describe, expect, it } from 'vitest';
import DirectMarkdownDifferentialHarness from './DirectMarkdownDifferentialHarness.svelte';
import {
  describeViolations,
  recordVisibleText,
} from './markdown/visibleTextMonotonicity';
import { setBindingMock } from '../../../test/mocks/bindings-app';

type Harness = {
  append(delta: string): Promise<boolean>;
  complete(finalSource: string): Promise<void>;
};

// The reported item, verbatim (thread 01f857b5, item text:9:2). Its reveal
// rolled the visible string back three times in one paragraph: to
// "The hour-long", to "… main-thread", and to "… pane-local".
const INCIDENT_PARAGRAPH =
  'The hour-long monitor rules out a process crash and a multi-minute ' +
  'main-thread lock. Backend items kept persisting. Renderer animation ' +
  'callbacks continued near 165 Hz. The page remained partly interactive. ' +
  'Frontend exceptions explain the pane-local failure.';

// The completion patch: the provider's final body is not what the reveal
// painted. This one is shorter, so the controller cannot reach it by
// extending — it is the genuine-divergence branch.
const INCIDENT_COMPLETION =
  'The hour-long monitor rules out a process crash and a multi-minute ' +
  'main-thread lock. Backend items kept persisting. The page remained ' +
  'partly interactive.';

const SECOND_PARAGRAPH =
  'Frontend exceptions explain the pane-local failure without a renderer ' +
  'stall, so the recovery spring is a consequence rather than a physics bug.';

const MULTI_PARAGRAPH_COMPLETION =
  `${INCIDENT_PARAGRAPH}\n\n` +
  'Frontend exceptions explain the pane-local failure without a renderer ' +
  'stall.';

/** Word-sized reveal units, the shape the smoother emits. */
function revealUnits(source: string): string[] {
  const units: string[] = [];
  let index = 0;
  while (index < source.length) {
    let end = source.indexOf(' ', index);
    end = end < 0 ? source.length : end + 1;
    units.push(source.slice(index, end));
    index = end;
  }
  return units;
}

beforeEach(() => {
  setBindingMock('HighlightClassNames', async () => []);
  setBindingMock('HighlightCode', async ({ lang }: { lang: string }) => ({
    lang,
    lines: [],
    truncated: false,
  }));
});

describe('direct markdown reveal visible-text monotonicity', () => {
  it('extends the visible string at every mutation record of a paragraph reveal', async () => {
    const view = render(DirectMarkdownDifferentialHarness);
    const harness = view.component as unknown as Harness;
    const root = view.container.querySelector<HTMLElement>('[data-differential-direct]');
    if (!root) throw new Error('direct markdown root did not mount');

    const recorder = recordVisibleText(root);
    try {
      for (const unit of revealUnits(INCIDENT_PARAGRAPH)) await harness.append(unit);
      recorder.drain();
      expect(recorder.matchesDom(), 'replay model diverged from the live tree').toBe(true);

      const violations = recorder.violations();
      expect(
        violations.length,
        `visible text stopped extending:\n${describeViolations(violations)}`,
      ).toBe(0);
      expect(recorder.checkpointViolations()).toEqual([]);
      expect(recorder.visible().trim()).toBe(INCIDENT_PARAGRAPH);
      expect(recorder.steps().length).toBeGreaterThan(0);
    } finally {
      recorder.stop();
    }
  });

  it('replaces a diverging completion body in one atomic mutation record', async () => {
    const view = render(DirectMarkdownDifferentialHarness);
    const harness = view.component as unknown as Harness;
    const root = view.container.querySelector<HTMLElement>('[data-differential-direct]');
    if (!root) throw new Error('direct markdown root did not mount');

    const recorder = recordVisibleText(root);
    try {
      for (const unit of revealUnits(INCIDENT_PARAGRAPH)) await harness.append(unit);
      await harness.complete(INCIDENT_COMPLETION);
      recorder.drain();
      expect(recorder.matchesDom(), 'replay model diverged from the live tree').toBe(true);

      const violations = recorder.violations();
      expect(
        violations.length,
        `visible text stopped extending:\n${describeViolations(violations)}`,
      ).toBe(0);
      expect(recorder.checkpointViolations()).toEqual([]);
      expect(recorder.visible().trim()).toBe(INCIDENT_COMPLETION);

      const baseline = view.container.querySelector('[data-differential-baseline] .markdown-body');
      const direct = view.container.querySelector('[data-differential-direct] .markdown-body');
      expect(direct?.textContent).toBe(baseline?.textContent);
    } finally {
      recorder.stop();
    }
  });

  it('extends the visible string at every mutation record of a tail reveal below a committed paragraph', async () => {
    const view = render(DirectMarkdownDifferentialHarness);
    const harness = view.component as unknown as Harness;
    const root = view.container.querySelector<HTMLElement>('[data-differential-direct]');
    if (!root) throw new Error('direct markdown root did not mount');

    // Settle the first paragraph into the committed container BEFORE
    // recording. Promoting a block out of the volatile tail is Streamdown's
    // own committed/volatile handoff, not the reveal, and
    // it is deliberately outside this window.
    for (const unit of revealUnits(INCIDENT_PARAGRAPH)) await harness.append(unit);
    await harness.append('\n\n');
    await harness.append('Frontend ');

    const recorder = recordVisibleText(root);
    try {
      for (const unit of revealUnits(SECOND_PARAGRAPH.slice('Frontend '.length))) {
        await harness.append(unit);
      }
      await harness.complete(MULTI_PARAGRAPH_COMPLETION);
      recorder.drain();
      expect(recorder.matchesDom(), 'replay model diverged from the live tree').toBe(true);

      const violations = recorder.violations();
      expect(
        violations.length,
        `visible text stopped extending:\n${describeViolations(violations)}`,
      ).toBe(0);
      expect(recorder.checkpointViolations()).toEqual([]);

      const baseline = view.container.querySelector('[data-differential-baseline] .markdown-body');
      const direct = view.container.querySelector('[data-differential-direct] .markdown-body');
      expect(direct?.textContent).toBe(baseline?.textContent);
    } finally {
      recorder.stop();
    }
  });
});
