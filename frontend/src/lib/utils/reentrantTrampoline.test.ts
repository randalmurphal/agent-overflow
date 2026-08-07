import { describe, expect, it } from 'vitest';
import {
  createReentrantTrampoline,
  MAX_TRAMPOLINE_LAPS,
} from './reentrantTrampoline';
import { installDiagnosticsCapture } from '../../test/helpers/diagnostics';

// The trampoline `threadStreamingReveal.svelte.ts#recomputeReveal` runs on.
// Its production driver is a synchronous `onReveal` re-entering the pass, and
// there is no seam to fire one at will through the real smoother — so the loop
// is tested here, at the layer that owns it, with a pass that re-enters on
// purpose.
//
// Diagnostics go through the REAL capture pipeline (dedupe -> serialize ->
// batch -> RPC): the claim is that an abandoned loop lands in
// `ui-trace/frontend-errors.jsonl`, which is the only trace an aborted flush
// leaves.

describe('createReentrantTrampoline', () => {
  const diagnostics = installDiagnosticsCapture();

  it('runs the pass once when nothing re-enters', async () => {
    let runs = 0;
    const enter = createReentrantTrampoline('probe', () => {
      runs += 1;
    });

    enter();

    expect(runs).toBe(1);
    expect(await diagnostics.messages()).toEqual([]);
  });

  it('re-runs the pass instead of nesting it, and converges', async () => {
    // A nested pass is the corruption mode this shape exists to prevent: the
    // outer pass would finish afterwards and overwrite the state the nested
    // one computed from fresher input. So a re-entry raises a flag and the
    // OUTER call re-runs — observable as depth staying 1 while runs climbs.
    let runs = 0;
    let depth = 0;
    let maxDepth = 0;
    let reentriesLeft = 3;
    // Self-reference through a holder: the pass has to be able to call the
    // trampoline that wraps it.
    let loop: (() => void) | null = null;
    const enter = createReentrantTrampoline('probe', () => {
      depth += 1;
      maxDepth = Math.max(maxDepth, depth);
      runs += 1;
      if (reentriesLeft-- > 0) loop?.();
      depth -= 1;
    });
    loop = enter;

    enter();

    expect(runs).toBe(4);
    expect(maxDepth).toBe(1);
    expect(await diagnostics.messages()).toEqual([]);
  });

  it('abandons a pass that re-enters on every lap, and reports', async () => {
    // Unbounded, this is the renderer-freeze signature: one core pegged, no
    // paint, no error, nothing in any log. (Without the cap this test hangs.)
    let runs = 0;
    let loop: (() => void) | null = null;
    const enter = createReentrantTrampoline('probe.loop', () => {
      runs += 1;
      loop?.();
    });
    loop = enter;

    enter();

    expect(runs).toBe(MAX_TRAMPOLINE_LAPS);

    const records = await diagnostics.all();
    expect(records).toHaveLength(1);
    expect(records[0].message).toContain('reentrantTrampoline');
    // Constant message; the loop name and lap count ride in the detail, or
    // every call site and every count would mint its own dedupe signature.
    expect(records[0].message).not.toContain('probe.loop');
    expect(records[0].detail).toContain('probe.loop');
    // Console fallback: a remote session cannot persist the record at all.
    expect(diagnostics.warnings().join('\n')).toContain('probe.loop');
  });

  it('gives the next entry a full budget after an abandoned one', async () => {
    // Per-ENTRY, deliberately: the driver is external (wire events), so a
    // sticky stand-down would disable the pass for the session on the strength
    // of one bad input. Contrast `timelineQuietWork`, whose own timer drives it
    // and whose stand-down therefore must be sticky.
    let runs = 0;
    let looping = true;
    let loop: (() => void) | null = null;
    const enter = createReentrantTrampoline('probe.loop', () => {
      runs += 1;
      if (looping) loop?.();
    });
    loop = enter;

    enter();
    expect(runs).toBe(MAX_TRAMPOLINE_LAPS);

    looping = false;
    enter();

    // One clean pass, not an immediate re-abandon: `again` did not survive.
    expect(runs).toBe(MAX_TRAMPOLINE_LAPS + 1);
    expect(await diagnostics.messages()).toHaveLength(1);
  });

  it('honours a custom cap', async () => {
    let runs = 0;
    let loop: (() => void) | null = null;
    const enter = createReentrantTrampoline('probe.loop', () => {
      runs += 1;
      loop?.();
    }, 4);
    loop = enter;

    enter();

    expect(runs).toBe(4);
    expect(await diagnostics.messages()).toHaveLength(1);
  });
});
