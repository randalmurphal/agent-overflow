// Regression test for the "event-slot-release" hunk of
// frontend/patches/svelte@5.56.3.patch.
//
// Pristine svelte (through 5.56.3 and upstream main as of 2026-07)
// stores every delegated event in a module-level slot
// (`last_propagated_event` in internal/client/dom/elements/events.js) as
// a deliberate Firefox workaround: if the event wrapper is GC'd
// mid-propagation, its `__root` expando is lost and the event is
// processed twice. The slot is never cleared, so after the dispatch it
// keeps retaining the LAST delegated event — and through `event.target`
// the entire detached subtree of whatever component the user last
// clicked in — until the next delegated event happens to overwrite it.
// In the live app that meant a just-closed pane's whole DOM tree
// survived GC indefinitely on an idle window (2026-07-20 heap
// analysis).
//
// The patch hunk schedules a macrotask after each dispatch that nulls
// the slot: strictly after propagation and its trailing microtasks
// settle, so the Firefox workaround window is fully preserved.
//
// This test clicks a delegated handler, unmounts the component, and
// asserts the clicked element becomes collectable WITHOUT any further
// events. On pristine svelte it fails (the slot pins the button). Drop
// the hunk when this passes on an unpatched release.
//
// Needs --expose-gc; skips loudly without it:
//   NODE_OPTIONS=--expose-gc pnpm exec vitest run --project unit \
//     src/test/integration/svelte-patch-event-slot.test.ts
import { describe, expect, it } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import EventSlotTarget from './svelte-patch-fixtures/EventSlotTarget.svelte';

const gc = (globalThis as { gc?: () => void }).gc;

const settle = () => new Promise((r) => setTimeout(r, 25));

async function collectHard(): Promise<void> {
  for (let i = 0; i < 5; i += 1) {
    gc!();
    await settle();
  }
}

describe.runIf(gc)('svelte patch: delegated-event slot releases after dispatch', () => {
  it('a clicked element is collectable after unmount with no further events', async () => {
    // Element refs stay confined to this inner scope — stack locals in
    // the test frame would themselves be GC roots.
    function mountClickUnmount(): WeakRef<Element> {
      let clicks = 0;
      const target = document.body.appendChild(document.createElement('div'));
      const app = mount(EventSlotTarget, {
        target,
        props: { onClicked: () => { clicks += 1; } },
      });
      flushSync();
      const button = target.querySelector('[data-testid="event-slot-target"]')!;
      button.dispatchEvent(new MouseEvent('click', { bubbles: true }));
      // Guards against passing vacuously: the click must have actually
      // routed through svelte's delegation (and thus through the slot).
      expect(clicks, 'probe precondition: delegated handler ran').toBe(1);
      const ref = new WeakRef(button);
      unmount(app);
      flushSync();
      target.remove();
      return ref;
    }

    const ref = mountClickUnmount();

    // Phase 1 — before the clear macrotask has had a chance to run, the
    // slot still holds the event strongly, so the element MUST survive
    // GC. This proves the probe actually observes the slot (the final
    // assertion cannot pass vacuously) and demonstrates the pristine
    // behavior: without the patch this pin lasts forever.
    gc!();
    gc!();
    expect(ref.deref(), 'probe precondition: slot should pin the element until the clear fires').toBeDefined();

    // Phase 2 — after macrotasks run, the patch's scheduled clear has
    // nulled the slot and the element must be collectable.
    await collectHard();
    expect(ref.deref(), 'clicked element still retained — is the event-slot-release hunk applied?').toBeUndefined();
  });
});

describe.runIf(!gc)('svelte patch: delegated-event slot releases after dispatch (SKIPPED)', () => {
  it('requires --expose-gc (run with NODE_OPTIONS=--expose-gc)', () => {
    expect(true).toBe(true);
  });
});
