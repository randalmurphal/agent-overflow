import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  createUserMessageOverflowCoordinator,
  type UserMessageOverflowProbe,
} from './userMessageOverflowMeasurement';

const mounted: HTMLElement[] = [];

function elementWithGeometry(scrollHeight: number, clientHeight: number): HTMLElement {
  const element = document.createElement('p');
  Object.defineProperties(element, {
    scrollHeight: { configurable: true, get: () => scrollHeight },
    clientHeight: { configurable: true, get: () => clientHeight },
  });
  document.body.appendChild(element);
  mounted.push(element);
  return element;
}

afterEach(() => {
  for (const element of mounted.splice(0)) element.remove();
});

describe('user-message overflow coordinator', () => {
  it('measures every active probe before applying any result', () => {
    const coordinator = createUserMessageOverflowCoordinator();
    const first = elementWithGeometry(300, 240);
    const second = elementWithGeometry(200, 240);
    const order: string[] = [];
    Object.defineProperties(second, {
      scrollHeight: {
        configurable: true,
        get: () => {
          order.push('read-second-scroll');
          return 200;
        },
      },
      clientHeight: {
        configurable: true,
        get: () => {
          order.push('read-second-client');
          return 240;
        },
      },
    });
    const firstProbe: UserMessageOverflowProbe = {
      element: () => first,
      active: () => true,
      apply: (overflows) => order.push(`apply-first:${overflows}`),
    };
    const secondProbe: UserMessageOverflowProbe = {
      element: () => second,
      active: () => true,
      apply: (overflows) => order.push(`apply-second:${overflows}`),
    };
    coordinator.register(firstProbe);
    coordinator.register(secondProbe);

    coordinator.measureAll();

    expect(order).toEqual([
      'read-second-scroll',
      'read-second-client',
      'apply-first:true',
      'apply-second:false',
    ]);
  });

  it('handles active, inactive, unregister, and repeated-request transitions', async () => {
    const coordinator = createUserMessageOverflowCoordinator();
    const element = elementWithGeometry(300, 240);
    let active = false;
    const apply = vi.fn();
    const probe: UserMessageOverflowProbe = {
      element: () => element,
      active: () => active,
      apply,
    };
    const unregister = coordinator.register(probe);

    coordinator.request(probe);
    await Promise.resolve();
    expect(apply).not.toHaveBeenCalled();

    active = true;
    coordinator.request(probe);
    coordinator.request(probe);
    await Promise.resolve();
    expect(apply).toHaveBeenCalledOnce();
    expect(apply).toHaveBeenLastCalledWith(true);

    unregister();
    coordinator.request(probe);
    coordinator.measureAll();
    await Promise.resolve();
    expect(apply).toHaveBeenCalledOnce();
  });

  it('rejects duplicate registration and makes stale releases harmless', () => {
    const coordinator = createUserMessageOverflowCoordinator();
    const element = elementWithGeometry(300, 240);
    const apply = vi.fn();
    const probe: UserMessageOverflowProbe = {
      element: () => element,
      active: () => true,
      apply,
    };
    const releaseFirst = coordinator.register(probe);

    expect(() => coordinator.register(probe)).toThrow(/already registered/);
    releaseFirst();
    const releaseSecond = coordinator.register(probe);
    releaseFirst();
    coordinator.measureAll();

    expect(apply).toHaveBeenCalledOnce();
    releaseSecond();
  });

  it('remeasures every registered probe after typography changes', async () => {
    const coordinator = createUserMessageOverflowCoordinator();
    const first = elementWithGeometry(300, 240);
    const second = elementWithGeometry(200, 240);
    const firstApply = vi.fn();
    const secondApply = vi.fn();
    coordinator.register({
      element: () => first,
      active: () => true,
      apply: firstApply,
    });
    coordinator.register({
      element: () => second,
      active: () => true,
      apply: secondApply,
    });

    coordinator.requestAll();
    await Promise.resolve();

    expect(firstApply).toHaveBeenCalledOnce();
    expect(firstApply).toHaveBeenCalledWith(true);
    expect(secondApply).toHaveBeenCalledOnce();
    expect(secondApply).toHaveBeenCalledWith(false);
  });

  it('applies every measured result before surfacing a probe failure', () => {
    const coordinator = createUserMessageOverflowCoordinator();
    const first = elementWithGeometry(300, 240);
    const second = elementWithGeometry(200, 240);
    const secondApply = vi.fn();
    coordinator.register({
      element: () => first,
      active: () => true,
      apply: () => { throw new Error('first apply failed'); },
    });
    coordinator.register({
      element: () => second,
      active: () => true,
      apply: secondApply,
    });

    expect(() => coordinator.measureAll()).toThrow('first apply failed');
    expect(secondApply).toHaveBeenCalledOnce();
    expect(secondApply).toHaveBeenCalledWith(false);
  });
});
