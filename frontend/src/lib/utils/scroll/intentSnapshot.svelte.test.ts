import { flushSync } from 'svelte';
import { expect, it } from 'vitest';
import { createUseStickToBottomController } from './index.svelte';

it('publishes escape transitions reactively and reads current intent during teardown', () => {
  const controller = createUseStickToBottomController();
  const rendered: boolean[] = [];
  const retired: { escaped: boolean; ownerDriven: boolean }[] = [];
  const dispose = $effect.root(() => {
    $effect(() => {
      rendered.push(controller.escapedFromLock);
      return () => retired.push({
        escaped: controller.escapedFromLock,
        ownerDriven: controller.positionOwnerDriven,
      });
    });
  });
  try {
    flushSync();
    controller.setEscapedFromLock(true);
    flushSync();
    expect(rendered).toEqual([false, true]);
    expect(retired).toEqual([{ escaped: true, ownerDriven: false }]);

    controller.setEscapedFromLock(false);
    flushSync();
    expect(rendered).toEqual([false, true, false]);
    expect(retired.at(-1)).toEqual({ escaped: false, ownerDriven: true });

    controller.setEscapedFromLock(false);
    flushSync();
    expect(rendered).toHaveLength(3);

    controller.setEscapedFromLock(true);
    dispose(); // Destroy in the same batch as the input event.
    expect(retired.at(-1)).toEqual({ escaped: true, ownerDriven: false });
  } finally {
    dispose();
    controller.detach();
  }
});
