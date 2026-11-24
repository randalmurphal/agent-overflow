// Both sides read every frame shape to the same refusal, or to none: Go
// runs the same cases (internal/dispatchrefusal's
// TestTheFrontendReadsEveryFrameAsThisPackageDoes).

import { describe, expect, it } from 'vitest';
import frames from '../../test/fixtures/dispatchRefusalFrames.json';
import { dispatchRefusalKind, parseDispatchMark } from './dispatchRefusal';
import { TransportError } from './wsClient';

describe('who refused, from a frame', () => {
  it.each(frames.map((frame) => [frame.name, frame] as const))('reads %s as Go does', (_name, frame) => {
    const dispatch = 'dispatch' in frame ? frame.dispatch : undefined;
    expect(dispatchRefusalKind(frame.code, frame.message, parseDispatchMark(dispatch))).toBe(frame.kind);
    const err = new TransportError(frame.code, frame.message, { dispatch });
    expect(err.dispatchRefusal ?? null).toBe(frame.kind);
    if (frame.kind === null) expect(err.message).toBe(frame.message);
  });
});
