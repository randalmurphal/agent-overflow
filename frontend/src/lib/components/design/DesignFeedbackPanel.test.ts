import { describe, expect, it } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import DesignFeedbackPanel from './DesignFeedbackPanel.svelte';
import { createThreadPane } from '../../stores/thread.svelte';
import type { Thread } from '../../types/models';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import type { SliderControl } from '../../types/design';

// Stub Element.animate (Toast subtree depends on it for transitions).
if (typeof Element !== 'undefined' && !('animate' in Element.prototype)) {
  (Element.prototype as unknown as { animate: unknown }).animate = function () {
    return {
      cancel() {},
      finish() {},
      play() {},
      pause() {},
      reverse() {},
      addEventListener() {},
      removeEventListener() {},
      onfinish: null,
      oncancel: null,
      finished: Promise.resolve(),
      effect: null,
      startTime: 0,
      currentTime: 0,
      playState: 'finished',
      playbackRate: 1,
    };
  };
}

function makeThread(overrides: Partial<Thread> = {}): Thread {
  return {
    id: 'thread-1',
    title: 'Design thread',
    provider: 'claude',
    workspacePath: '/tmp',
    projectPath: '/tmp',
    projectId: 'proj-design',
    mode: 'design',
    model: 'claude-sonnet-4-6',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
    ...overrides,
  };
}

async function buildPane() {
  setBindingMock('SwitchThread', async () => {});
  setBindingMock('ListItems', async () => []);
  setBindingMock('ListPayloadMetas', async () => []);
  setBindingMock('SendMessage', async () => 'msg-id');
  const pane = createThreadPane();
  await pane.switchThread(makeThread());
  return pane;
}

describe('<DesignFeedbackPanel>', () => {
  // TestSliderEffectDoesNotInfiniteLoop pins the load-bearing fix for
  // the freeze the user hit when the agent first published an
  // `expose_controls` set. The seeding effect reads `sliderValues` and
  // writes `sliderValues = next`; without `untrack`, the read
  // subscribes the effect to the very signal the write invalidates,
  // and Svelte 5's identity-equality on $state objects re-fires the
  // effect on every assignment. The result is a synchronous reactive
  // loop that wedges the main thread, starves the screenshot
  // postMessage round-trip, and surfaces as a `read_screenshot`
  // timeout error on the agent's tool call before the user can
  // diagnose anything.
  //
  // If this test ever times out, the loop is back. Don't relax the
  // timeout — find whatever in DesignFeedbackPanel reads + writes the
  // same $state without untrack.
  it('does not infinite-loop when exposed controls land', async () => {
    const pane = await buildPane();
    const controls: SliderControl[] = [
      { id: 'header-density', label: 'Header density', min: 0.5, max: 1.5, step: 0.05, value: 1.0 },
      { id: 'accent-saturation', label: 'Accent saturation', min: 0, max: 1.4, step: 0.05, value: 1.0 },
    ];
    pane.setExposedControls(controls);

    const { container } = render(DesignFeedbackPanel, { props: { pane } });

    // Both sliders rendered? The component never gets here if the
    // effect deadlocks the render pass.
    const sliders = container.querySelectorAll('input[type="range"]');
    expect(sliders.length).toBe(2);
    expect((sliders[0] as HTMLInputElement).value).toBe('1');
    expect((sliders[1] as HTMLInputElement).value).toBe('1');
  }, 2000);

  it('preserves a user-touched slider value across re-emissions of the same control set', async () => {
    const pane = await buildPane();
    const initialControls: SliderControl[] = [
      { id: 'density', label: 'Density', min: 0, max: 2, step: 0.1, value: 1.0 },
    ];
    pane.setExposedControls(initialControls);

    const { container } = render(DesignFeedbackPanel, { props: { pane } });

    const slider = container.querySelector(
      'input[type="range"]',
    ) as HTMLInputElement | null;
    expect(slider).not.toBeNull();
    if (!slider) return;

    // User drags the slider to 1.5.
    await fireEvent.input(slider, { target: { value: '1.5' } });
    expect(slider.value).toBe('1.5');

    // Agent re-emits the same control id — wholesale-replacement
    // wire contract, but with the user mid-tweak. Without the seed
    // preserving touched values, the slider would yank back to the
    // agent default.
    pane.setExposedControls([
      { id: 'density', label: 'Density', min: 0, max: 2, step: 0.1, value: 1.0 },
    ]);
    // Re-query because the binding might not auto-update the
    // reflected attribute on the same node; either reference works.
    const refreshed = container.querySelector(
      'input[type="range"]',
    ) as HTMLInputElement;
    expect(refreshed.value).toBe('1.5');
  }, 2000);
});
