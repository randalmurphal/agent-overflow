import { describe, expect, it, vi } from 'vitest';
import {
  StreamingAssistantRevealRouter,
  type StreamingAssistantRevealSink,
} from './streamingAssistantReveal';

interface SinkFixture {
  sink: StreamingAssistantRevealSink;
  appended: string[];
  reset: ReturnType<typeof vi.fn>;
}

function makeSink(ready = true): SinkFixture {
  const appended: string[] = [];
  const reset = vi.fn();
  return {
    appended,
    reset,
    sink: {
      canAppendLiteral: () => ready,
      appendLiteral: (_nextSource, delta) => appended.push(delta),
      reset,
    },
  };
}

function publish(
  router: StreamingAssistantRevealRouter,
  source: { value: string },
  delta: string,
  previousCodeUnit = 32,
): boolean {
  const previous = source.value;
  const appended = router.publish(
    'item',
    previousCodeUnit,
    previous,
    delta,
    (nextSource) => { source.value = nextSource; },
  );
  if (!appended) source.value = previous + delta;
  return appended;
}

describe('streaming assistant reveal bridge', () => {
  it('preflights every representation, commits once, then appends to all', () => {
    const router = new StreamingAssistantRevealRouter();
    const first = makeSink();
    const second = makeSink();
    router.register('item', first.sink);
    router.register('item', second.sink);
    const source = { value: '' };

    expect(publish(router, source, 'hello ', -1)).toBe(false);
    expect(publish(router, source, 'world ')).toBe(true);
    expect(source.value).toBe('hello world ');
    expect(first.appended).toEqual(['world ']);
    expect(second.appended).toEqual(['world ']);
  });

  it('falls back before direct mutation when any representation cannot append', () => {
    const router = new StreamingAssistantRevealRouter();
    const first = makeSink();
    const second = makeSink(false);
    router.register('item', first.sink);
    router.register('item', second.sink);
    const source = { value: '' };

    expect(publish(router, source, 'hello ', -1)).toBe(false);
    expect(publish(router, source, 'world ')).toBe(false);
    expect(source.value).toBe('hello world ');
    expect(first.appended).toEqual([]);
    expect(second.appended).toEqual([]);
    expect(first.reset).toHaveBeenCalledTimes(2);
    expect(second.reset).toHaveBeenCalledTimes(2);
  });

  it('falls back for markdown punctuation and resets prior direct DOM', () => {
    const router = new StreamingAssistantRevealRouter();
    const target = makeSink();
    router.register('item', target.sink);
    const source = { value: '' };

    expect(publish(router, source, 'hello ', -1)).toBe(false);
    expect(publish(router, source, 'world ')).toBe(true);
    expect(publish(router, source, '**', 'd'.charCodeAt(0))).toBe(false);
    expect(source.value).toBe('hello world **');
    expect(target.reset).toHaveBeenCalledTimes(2);
  });

  it('falls back when canonical source changed outside the direct path', () => {
    const router = new StreamingAssistantRevealRouter();
    const target = makeSink();
    router.register('item', target.sink);
    const source = { value: '' };

    expect(publish(router, source, 'hello ', -1)).toBe(false);
    expect(publish(router, source, 'world ')).toBe(true);
    source.value = 'authoritative rewrite ';
    expect(publish(router, source, 'again ')).toBe(false);
    expect(source.value).toBe('authoritative rewrite again ');
  });

  it('unregisters idempotently and never routes to a stale sink', () => {
    const router = new StreamingAssistantRevealRouter();
    const target = makeSink();
    const unregister = router.register('item', target.sink);
    unregister();
    unregister();
    const source = { value: '' };

    expect(publish(router, source, 'world ', -1)).toBe(false);
    expect(target.appended).toEqual([]);
    expect(target.reset).toHaveBeenCalledOnce();
  });

  it('keeps duplicate thread panes isolated even when item ids match', () => {
    const firstRouter = new StreamingAssistantRevealRouter();
    const secondRouter = new StreamingAssistantRevealRouter();
    const first = makeSink();
    const second = makeSink();
    firstRouter.register('same-item', first.sink);
    secondRouter.register('same-item', second.sink);
    const source = { value: '' };

    expect(firstRouter.publish('same-item', -1, '', 'seed ', () => {})).toBe(false);
    source.value = 'seed ';
    expect(firstRouter.publish(
      'same-item', 32, source.value, 'first pane ', (next) => { source.value = next; },
    )).toBe(true);
    expect(first.appended).toEqual(['first pane ']);
    expect(second.appended).toEqual([]);
  });
});
