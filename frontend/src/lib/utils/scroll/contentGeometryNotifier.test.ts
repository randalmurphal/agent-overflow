import { describe, expect, it, vi } from 'vitest';
import { createContentGeometryNotifier } from './contentGeometryNotifier';

describe('createContentGeometryNotifier', () => {
  it('delivers while subscribed and stops after an idempotent release', () => {
    const notifier = createContentGeometryNotifier();
    const listener = vi.fn();
    const release = notifier.subscribe(listener);

    notifier.notify(true);
    release();
    release();
    notifier.notify(false);

    expect(listener).toHaveBeenCalledTimes(1);
    expect(listener).toHaveBeenCalledWith(true);
  });

  it('keeps duplicate subscriptions independent', () => {
    const notifier = createContentGeometryNotifier();
    const listener = vi.fn();
    const releaseFirst = notifier.subscribe(listener);
    notifier.subscribe(listener);

    releaseFirst();
    notifier.notify(true);

    expect(listener).toHaveBeenCalledTimes(1);
  });

  it('continues to the next listener when the current listener releases itself', () => {
    const notifier = createContentGeometryNotifier();
    const calls: string[] = [];
    let releaseFirst = (): void => {};
    releaseFirst = notifier.subscribe(() => {
      calls.push('first');
      releaseFirst();
    });
    notifier.subscribe(() => calls.push('second'));

    notifier.notify(true);
    notifier.notify(false);

    expect(calls).toEqual(['first', 'second', 'second']);
  });

  it('defers a subscription added during delivery until the next notification', () => {
    const notifier = createContentGeometryNotifier();
    const calls: string[] = [];
    let subscribed = false;
    notifier.subscribe(() => {
      calls.push('first');
      if (subscribed) return;
      subscribed = true;
      notifier.subscribe(() => calls.push('late'));
    });

    notifier.notify(true);
    notifier.notify(false);

    expect(calls).toEqual(['first', 'first', 'late']);
  });

  it('delivers to every listener before surfacing callback failures', () => {
    const notifier = createContentGeometryNotifier();
    const reached = vi.fn();
    notifier.subscribe(() => {
      throw new Error('first listener failed');
    });
    notifier.subscribe(reached);

    expect(() => notifier.notify(true)).toThrow('content geometry delivery failed');
    expect(reached).toHaveBeenCalledOnce();
  });
});
