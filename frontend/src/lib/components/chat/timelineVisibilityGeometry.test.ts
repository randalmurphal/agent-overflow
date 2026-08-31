import { afterEach, describe, expect, it } from 'vitest';
import { createTimelineVisibilityGeometry } from './timelineVisibilityGeometry';

let originalVisibilityState: PropertyDescriptor | undefined;
let visibilityState: DocumentVisibilityState = 'visible';

function setVisibility(next: DocumentVisibilityState): void {
  visibilityState = next;
  document.dispatchEvent(new Event('visibilitychange'));
}

describe('timelineVisibilityGeometry', () => {
  afterEach(() => {
    if (originalVisibilityState) {
      Object.defineProperty(document, 'visibilityState', originalVisibilityState);
    } else {
      delete (document as unknown as Record<string, unknown>).visibilityState;
    }
    originalVisibilityState = undefined;
    visibilityState = 'visible';
  });

  function installVisibilityState(initial: DocumentVisibilityState): void {
    originalVisibilityState = Object.getOwnPropertyDescriptor(document, 'visibilityState');
    visibilityState = initial;
    Object.defineProperty(document, 'visibilityState', {
      configurable: true,
      get: () => visibilityState,
    });
  }

  it('requires a visible geometry sample after every hidden transition', () => {
    installVisibilityState('visible');
    const geometry = createTimelineVisibilityGeometry();
    const stop = geometry.install();

    expect(geometry.ready()).toBe(true);

    setVisibility('hidden');
    expect(geometry.ready()).toBe(false);
    // Background deliveries cannot clear the barrier.
    expect(geometry.noteGeometrySample()).toBe(false);

    setVisibility('visible');
    expect(geometry.ready()).toBe(false);
    expect(geometry.noteGeometrySample()).toBe(true);
    expect(geometry.ready()).toBe(true);
    // Normal visible streaming samples add no quiet-work trigger.
    expect(geometry.noteGeometrySample()).toBe(false);

    // The second lap proves the barrier is re-armed rather than consumed
    // permanently by the first resume.
    setVisibility('hidden');
    setVisibility('visible');
    expect(geometry.ready()).toBe(false);
    expect(geometry.noteGeometrySample()).toBe(true);
    expect(geometry.ready()).toBe(true);

    stop();
    stop();
    // Listener teardown is real: a later hidden -> visible event pair with
    // no intervening read cannot re-arm this retired instance.
    setVisibility('hidden');
    setVisibility('visible');
    expect(geometry.ready()).toBe(true);
  });

  it('starts blocked when the timeline mounts while hidden', () => {
    installVisibilityState('hidden');
    const geometry = createTimelineVisibilityGeometry();

    expect(geometry.ready()).toBe(false);
    setVisibility('visible');
    expect(geometry.ready()).toBe(false);
    expect(geometry.noteGeometrySample()).toBe(true);
    expect(geometry.ready()).toBe(true);
  });
});
