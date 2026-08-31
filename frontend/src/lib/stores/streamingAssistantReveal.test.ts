import { describe, expect, it, vi } from 'vitest';
import { expectCleanTransitions } from '../../test/helpers/transitions';
import {
  StreamingAssistantRevealRouter,
  type StreamingAssistantRevealSink,
} from './streamingAssistantReveal';

interface SinkFixture {
  sink: StreamingAssistantRevealSink;
  appended: string[];
  reset: ReturnType<typeof vi.fn>;
  restore: ReturnType<typeof vi.fn>;
}

function makeSink(ready = true, restoreReady = true): SinkFixture {
  const appended: string[] = [];
  const reset = vi.fn();
  const restore = vi.fn(() => restoreReady);
  return {
    appended,
    reset,
    restore,
    sink: {
      canAppendLiteral: () => ready,
      appendLiteral: (_nextSource, delta) => appended.push(delta),
      restoreLiteral: restore,
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
  return appended;
}

const RENDER_CONTEXT = {
  streaming: true,
  volatileTailVisible: true,
  pathLinksInert: false,
  workspacePath: '/workspace',
} as const;

function parserSource(
  router: StreamingAssistantRevealRouter,
  source: string,
): string {
  return router.parserSourceFor('item', source, RENDER_CONTEXT);
}

describe('assistant reveal sink registration transitions', () => {
  // Idempotent release and rejected duplicate registration each have
  // their own test below. The lap those two miss is RE-registration: a
  // row unmounts and remounts (virtualizer recycle, pane switch, a
  // `{#key}` remount on a settings change) and the router must come back
  // to the same resting state every time, never feeding a released sink.
  it('re-registers cleanly and never feeds a released sink', () => {
    const router = new StreamingAssistantRevealRouter();
    const canonical = { value: '' };
    const released: Array<{ fixture: SinkFixture; appendedAtRelease: number }> = [];
    let live: SinkFixture | null = null;

    expectCleanTransitions('assistant reveal sink registration', {
      on() {
        live = makeSink();
        return router.register('item', live.sink, () => {});
      },
      off(handle) {
        if (live) {
          released.push({ fixture: live, appendedAtRelease: live.appended.length });
          live = null;
        }
        (handle as () => void)();
      },
      whileOn() {
        // Arm a checkpoint, then a direct append: a fresh registration
        // has to be reachable, or every lap would pass vacuously.
        const target = live!;
        expect(publish(router, canonical, 'seed ', -1)).toBe(false);
        expect(publish(router, canonical, 'more ')).toBe(true);
        expect(target.appended).toEqual(['more ']);
      },
      onAgain() {
        expect(() => router.register('item', live!.sink, () => {})).toThrow(
          /already registered/,
        );
      },
      read: () => ({
        presentationHeld:
          router.parserSourceFor('item', canonical.value, RENDER_CONTEXT)
            !== canonical.value,
        staleSinksFed: released.filter(
          (entry) => entry.fixture.appended.length > entry.appendedAtRelease,
        ).length,
      }),
    });

    expect(released).not.toHaveLength(0);
    for (const entry of released) {
      expect(entry.fixture.reset).toHaveBeenCalledOnce();
    }
  });
});

describe('streaming assistant reveal bridge', () => {
  it('rejects an empty publish instead of minting ambiguous append lineage', () => {
    const router = new StreamingAssistantRevealRouter();
    const source = { value: 'existing' };

    expect(() => publish(router, source, '')).toThrow(
      'streaming assistant reveal cannot publish an empty delta',
    );
    expect(source.value).toBe('existing');
  });

  it('preflights every representation, commits once, then appends to all', () => {
    const router = new StreamingAssistantRevealRouter();
    const first = makeSink();
    const second = makeSink();
    router.register('item', first.sink, () => {});
    router.register('item', second.sink, () => {});
    const source = { value: '' };

    expect(publish(router, source, 'hello ', -1)).toBe(false);
    expect(publish(router, source, 'world ')).toBe(true);
    expect(source.value).toBe('hello world ');
    expect(parserSource(router, source.value)).toBe('hello ');
    expect(first.appended).toEqual(['world ']);
    expect(second.appended).toEqual(['world ']);
  });

  it('does not arm a checkpoint from spaces before the first literal word', () => {
    const router = new StreamingAssistantRevealRouter();
    const target = makeSink();
    router.register('item', target.sink, () => {});
    const source = { value: '' };

    expect(publish(router, source, '   ', -1)).toBe(false);
    expect(parserSource(router, source.value)).toBe('   ');
    expect(publish(router, source, 'first ')).toBe(false);
    expect(parserSource(router, source.value)).toBe('   first ');
    expect(publish(router, source, 'second ')).toBe(true);
    expect(target.appended).toEqual(['second ']);
  });

  it('falls back before direct mutation when any representation cannot append', () => {
    const router = new StreamingAssistantRevealRouter();
    const first = makeSink();
    const second = makeSink(false);
    router.register('item', first.sink, () => {});
    router.register('item', second.sink, () => {});
    const source = { value: '' };

    expect(publish(router, source, 'hello ', -1)).toBe(false);
    expect(publish(router, source, 'world ')).toBe(false);
    expect(source.value).toBe('hello world ');
    expect(first.appended).toEqual([]);
    expect(second.appended).toEqual([]);
    expect(first.reset).toHaveBeenCalledOnce();
    expect(second.reset).toHaveBeenCalledOnce();
  });

  it('falls back for markdown punctuation and resets prior direct DOM', () => {
    const router = new StreamingAssistantRevealRouter();
    const target = makeSink();
    router.register('item', target.sink, () => {});
    const source = { value: '' };

    expect(publish(router, source, 'hello ', -1)).toBe(false);
    expect(publish(router, source, 'world ')).toBe(true);
    expect(publish(router, source, '**', 'd'.charCodeAt(0))).toBe(false);
    expect(source.value).toBe('hello world **');
    expect(parserSource(router, source.value)).toBe(source.value);
    expect(target.reset).toHaveBeenCalledOnce();
  });

  it('arms from the literal tail of an authoritative markdown unit', () => {
    const router = new StreamingAssistantRevealRouter();
    const target = makeSink();
    router.register('item', target.sink, () => {});
    const source = { value: '' };

    expect(publish(router, source, '**bold ', -1)).toBe(false);
    expect(parserSource(router, source.value)).toBe(source.value);
    expect(publish(router, source, 'continues ')).toBe(true);
    expect(target.appended).toEqual(['continues ']);
    expect(parserSource(router, source.value)).toBe('**bold ');
  });

  it('does not arm from an authoritative unit with no trailing literal leaf', () => {
    const router = new StreamingAssistantRevealRouter();
    const target = makeSink();
    router.register('item', target.sink, () => {});
    const source = { value: '' };

    expect(publish(router, source, '**', -1)).toBe(false);
    expect(publish(router, source, 'bold ', '*'.charCodeAt(0))).toBe(false);
    expect(publish(router, source, 'continues ')).toBe(true);
    expect(target.appended).toEqual(['continues ']);
    expect(parserSource(router, source.value)).toBe('**bold ');
  });

  it('directly appends punctuation whose prose context is complete in the reveal unit', () => {
    const router = new StreamingAssistantRevealRouter();
    const target = makeSink();
    router.register('item', target.sink, () => {});
    const source = { value: '' };

    expect(publish(router, source, 'Opening ', -1)).toBe(false);
    for (const delta of [
      'sentence. ',
      'clause, ',
      'question? ',
      'answer! ',
      'label: ',
      "don't ",
    ]) {
      expect(publish(router, source, delta)).toBe(true);
    }
    expect(target.appended).toEqual([
      'sentence. ',
      'clause, ',
      'question? ',
      'answer! ',
      'label: ',
      "don't ",
    ]);
  });

  it.each([
    ['ordered-list marker', '1. '],
    ['chunk-edge period', 'word.'],
    ['domain-like period', 'www.example '],
    ['leading punctuation', '. '],
    ['entity terminator', 'copy; '],
    ['apostrophe at the chunk edge', "word'"],
  ])('keeps %s on the authoritative parser path', (_name, delta) => {
    const router = new StreamingAssistantRevealRouter();
    const target = makeSink();
    router.register('item', target.sink, () => {});
    const source = { value: '' };

    expect(publish(router, source, 'seed ', -1)).toBe(false);
    expect(publish(router, source, delta)).toBe(false);
    expect(target.appended).toEqual([]);
    expect(parserSource(router, source.value)).toBe(source.value);
  });

  it('owns the canonical write for every fallback outcome', () => {
    const noSinkRouter = new StreamingAssistantRevealRouter();
    const noSinkSource = { value: '' };
    expect(publish(noSinkRouter, noSinkSource, 'unmounted ', -1)).toBe(false);
    expect(noSinkSource.value).toBe('unmounted ');

    const router = new StreamingAssistantRevealRouter();
    const target = makeSink();
    router.register('item', target.sink, () => {});
    const source = { value: '' };
    expect(publish(router, source, 'first ', -1)).toBe(false);
    expect(source.value).toBe('first ');
    expect(publish(router, source, '**')).toBe(false);
    expect(source.value).toBe('first **');
  });

  it('drops an armed checkpoint when its canonical commit fails', () => {
    const router = new StreamingAssistantRevealRouter();
    const target = makeSink();
    router.register('item', target.sink, () => {});

    expect(() => router.publish(
      'item',
      -1,
      '',
      'first ',
      () => { throw new Error('checkpoint commit failed'); },
    )).toThrow('checkpoint commit failed');
    expect(parserSource(router, '')).toBe('');

    const source = { value: '' };
    expect(publish(router, source, 'replacement ', -1)).toBe(false);
    expect(source.value).toBe('replacement ');
  });

  it('falls back when canonical source changed outside the direct path', () => {
    const router = new StreamingAssistantRevealRouter();
    const target = makeSink();
    router.register('item', target.sink, () => {});
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
    const unregister = router.register('item', target.sink, () => {});
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
    firstRouter.register('same-item', first.sink, () => {});
    secondRouter.register('same-item', second.sink, () => {});
    const source = { value: '' };

    expect(firstRouter.publish('same-item', -1, '', 'seed ', () => {})).toBe(false);
    source.value = 'seed ';
    expect(firstRouter.publish(
      'same-item', 32, source.value, 'first pane ', (next) => { source.value = next; },
    )).toBe(true);
    expect(first.appended).toEqual(['first pane ']);
    expect(second.appended).toEqual([]);
  });

  it('restores a new representation from the stable parser checkpoint', () => {
    const router = new StreamingAssistantRevealRouter();
    const first = makeSink();
    router.register('item', first.sink, () => {});
    const source = { value: '' };

    expect(publish(router, source, 'hello ', -1)).toBe(false);
    expect(publish(router, source, 'world ')).toBe(true);

    const remounted = makeSink();
    const requestAuthoritativeRender = vi.fn();
    router.register('item', remounted.sink, requestAuthoritativeRender);
    expect(remounted.restore).toHaveBeenCalledWith(
      'hello ',
      {
        tailSource: 'hello ',
        tailStart: 0,
        tailEnd: 5,
        trailingAsciiSpaces: 1,
      },
      'hello world ',
      ['world '],
    );
    expect(requestAuthoritativeRender).not.toHaveBeenCalled();
    expect(parserSource(router, source.value)).toBe('hello ');
  });

  it('restores every direct unit, including deferred spaces, in source order', () => {
    const router = new StreamingAssistantRevealRouter();
    const first = makeSink();
    router.register('item', first.sink, () => {});
    const source = { value: '' };
    publish(router, source, 'first ', -1);
    publish(router, source, 'second ');
    publish(router, source, '   ');
    publish(router, source, 'third ');

    const remounted = makeSink();
    router.register('item', remounted.sink, () => {});
    expect(remounted.restore).toHaveBeenCalledWith(
      'first ',
      expect.objectContaining({ trailingAsciiSpaces: 1 }),
      'first second    third ',
      ['second ', '   ', 'third '],
    );
  });

  it('drops the checkpoint and requests a full render when restore fails', () => {
    const router = new StreamingAssistantRevealRouter();
    const first = makeSink();
    router.register('item', first.sink, () => {});
    const source = { value: '' };
    publish(router, source, 'hello ', -1);
    publish(router, source, 'world ');

    const remounted = makeSink(true, false);
    const requestAuthoritativeRender = vi.fn();
    router.register('item', remounted.sink, requestAuthoritativeRender);
    expect(requestAuthoritativeRender).toHaveBeenCalledOnce();
    expect(parserSource(router, source.value)).toBe(source.value);
    expect(first.reset).toHaveBeenCalled();
  });

  it('drops the checkpoint before a markdown-meta rewrite', () => {
    const router = new StreamingAssistantRevealRouter();
    const target = makeSink();
    router.register('item', target.sink, () => {});
    const source = { value: '' };
    publish(router, source, 'hello ', -1);
    publish(router, source, 'world ');

    router.reconcileItemWrite(
      { id: 'item', kind: 'assistant_text', summary: source.value, meta: '{}' },
      { id: 'item', kind: 'assistant_text', summary: source.value, meta: '{"pathRefs":[]}' },
    );
    expect(parserSource(router, source.value)).toBe(source.value);
    expect(target.reset).toHaveBeenCalled();
  });

  it('rejects reconciling two different item identities', () => {
    const router = new StreamingAssistantRevealRouter();
    expect(() => router.reconcileItemWrite(
      { id: 'first', kind: 'assistant_text', summary: '' },
      { id: 'second', kind: 'assistant_text', summary: '' },
    )).toThrow(/cannot reconcile first as second/);
  });

  it.each([
    ['streaming mode', { streaming: false }],
    ['volatile-tail visibility', { volatileTailVisible: false }],
    ['view-only mode', { pathLinksInert: true }],
    ['workspace path', { workspacePath: '/other-workspace' }],
  ] as const)('drops the checkpoint when %s changes', (_name, change) => {
    const router = new StreamingAssistantRevealRouter();
    const target = makeSink();
    router.register('item', target.sink, () => {});
    const source = { value: '' };
    publish(router, source, 'first ', -1);
    publish(router, source, 'second ');
    expect(parserSource(router, source.value)).toBe('first ');

    expect(router.parserSourceFor('item', source.value, {
      ...RENDER_CONTEXT,
      ...change,
    })).toBe(source.value);
    expect(target.reset).toHaveBeenCalled();

    expect(publish(router, source, 'third ')).toBe(false);
    expect(publish(router, source, 'fourth ')).toBe(true);
  });

  it('keeps a checkpoint until the last distinct representation unregisters', () => {
    const router = new StreamingAssistantRevealRouter();
    const first = makeSink();
    const second = makeSink();
    const unregisterFirst = router.register('item', first.sink, () => {});
    router.register('item', second.sink, () => {});
    const source = { value: '' };
    publish(router, source, 'first ', -1);
    publish(router, source, 'second ');

    unregisterFirst();

    expect(parserSource(router, source.value)).toBe('first ');
    expect(publish(router, source, 'third ')).toBe(true);
    expect(second.appended).toEqual(['second ', 'third ']);
  });

  it('rejects duplicate registration instead of giving two releases one Set entry', () => {
    const router = new StreamingAssistantRevealRouter();
    const target = makeSink();
    router.register('item', target.sink, () => {});

    expect(() => router.register('item', target.sink, () => {})).toThrow(
      /already registered/,
    );
  });

  it('clears state and removes a representation whose restore throws', () => {
    const router = new StreamingAssistantRevealRouter();
    const first = makeSink();
    router.register('item', first.sink, () => {});
    const source = { value: '' };
    publish(router, source, 'first ', -1);
    publish(router, source, 'second ');

    const failing = makeSink();
    failing.sink.restoreLiteral = () => {
      throw new Error('restore failed');
    };
    const requestAuthoritativeRender = vi.fn();
    expect(() => router.register('item', failing.sink, requestAuthoritativeRender)).toThrow(
      'restore failed',
    );
    expect(requestAuthoritativeRender).toHaveBeenCalledOnce();
    expect(parserSource(router, source.value)).toBe(source.value);
    expect(publish(router, source, 'third ')).toBe(false);
    expect(failing.appended).toEqual([]);
  });

  it('clears state and removes a representation when its full-render request throws', () => {
    const router = new StreamingAssistantRevealRouter();
    const first = makeSink();
    router.register('item', first.sink, () => {});
    const source = { value: '' };
    publish(router, source, 'first ', -1);
    publish(router, source, 'second ');

    const failing = makeSink(true, false);
    expect(() => router.register('item', failing.sink, () => {
      throw new Error('render request failed');
    })).toThrow('render request failed');
    expect(parserSource(router, source.value)).toBe(source.value);
    expect(publish(router, source, 'third ')).toBe(false);
    expect(failing.appended).toEqual([]);
  });

  it('requests the authoritative source even when a failed restore cannot reset cleanly', () => {
    const router = new StreamingAssistantRevealRouter();
    const first = makeSink();
    router.register('item', first.sink, () => {});
    const source = { value: '' };
    publish(router, source, 'first ', -1);
    publish(router, source, 'second ');

    const failing = makeSink(true, false);
    first.reset.mockImplementationOnce(() => {
      throw new Error('reset failed');
    });
    const requestAuthoritativeRender = vi.fn();
    expect(() => router.register(
      'item',
      failing.sink,
      requestAuthoritativeRender,
    )).toThrow(/reset failed/);
    expect(requestAuthoritativeRender).toHaveBeenCalledOnce();
    expect(parserSource(router, source.value)).toBe(source.value);
    expect(publish(router, source, 'third ')).toBe(false);
    expect(failing.appended).toEqual([]);
  });

  it('removes a representation even when its release reset throws', () => {
    const router = new StreamingAssistantRevealRouter();
    const target = makeSink();
    const unregister = router.register('item', target.sink, () => {});
    const source = { value: '' };
    publish(router, source, 'first ', -1);
    publish(router, source, 'second ');
    target.reset.mockImplementationOnce(() => {
      throw new Error('release reset failed');
    });

    expect(unregister).toThrow('release reset failed');
    expect(unregister).not.toThrow();
    expect(publish(router, source, 'third ')).toBe(false);
  });

  it('clears the checkpoint after a summary rewrite and re-arms from the rewrite', () => {
    const router = new StreamingAssistantRevealRouter();
    const target = makeSink();
    router.register('item', target.sink, () => {});
    const source = { value: '' };
    publish(router, source, 'first ', -1);
    publish(router, source, 'second ');

    const rewritten = 'replacement ';
    router.reconcileItemWrite(
      { id: 'item', kind: 'assistant_text', summary: source.value },
      { id: 'item', kind: 'assistant_text', summary: rewritten },
    );
    source.value = rewritten;

    expect(parserSource(router, source.value)).toBe(rewritten);
    expect(publish(router, source, 'after ')).toBe(false);
    expect(publish(router, source, 'rewrite ')).toBe(true);
    expect(parserSource(router, source.value)).toBe('replacement after ');
  });

  it('falls back visibly and keeps the canonical source when a preflight throws', () => {
    const router = new StreamingAssistantRevealRouter();
    const first = makeSink();
    const failing = makeSink();
    failing.sink.canAppendLiteral = () => {
      throw new Error('preflight failed');
    };
    router.register('item', first.sink, () => {});
    router.register('item', failing.sink, () => {});
    const source = { value: '' };
    publish(router, source, 'first ', -1);

    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    expect(publish(router, source, 'second ')).toBe(false);
    expect(source.value).toBe('first second ');
    expect(warn).toHaveBeenCalledWith(
      expect.stringContaining('fell back after a sink failure'),
      expect.stringContaining('phase=preflight'),
    );
    warn.mockRestore();
    expect(parserSource(router, source.value)).toBe(source.value);
    expect(first.reset).toHaveBeenCalledOnce();
    expect(failing.reset).toHaveBeenCalledOnce();
  });

  it('resets the checkpoint when canonical commit throws and recovers a sink append failure', () => {
    const commitRouter = new StreamingAssistantRevealRouter();
    const commitSink = makeSink();
    commitRouter.register('item', commitSink.sink, () => {});
    const committed = { value: '' };
    publish(commitRouter, committed, 'first ', -1);
    expect(() => commitRouter.publish(
      'item',
      32,
      committed.value,
      'second ',
      () => { throw new Error('commit failed'); },
    )).toThrow('commit failed');
    expect(parserSource(commitRouter, committed.value)).toBe(committed.value);

    const appendRouter = new StreamingAssistantRevealRouter();
    const appendSink = makeSink();
    appendSink.sink.appendLiteral = () => {
      throw new Error('append failed');
    };
    appendRouter.register('item', appendSink.sink, () => {});
    const appended = { value: '' };
    publish(appendRouter, appended, 'first ', -1);
    const modes: string[] = [];
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    expect(appendRouter.publish(
      'item',
      32,
      appended.value,
      'second ',
      (nextSource, mode) => {
        appended.value = nextSource;
        modes.push(mode);
      },
    )).toBe(false);
    expect(modes).toEqual(['direct', 'authoritative']);
    expect(warn).toHaveBeenCalledWith(
      expect.stringContaining('fell back after a sink failure'),
      expect.stringContaining('phase=append'),
    );
    warn.mockRestore();
    expect(parserSource(appendRouter, appended.value)).toBe(appended.value);
  });

  it('reports both the sink failure and an authoritative recovery failure', () => {
    const router = new StreamingAssistantRevealRouter();
    const target = makeSink();
    target.sink.appendLiteral = () => {
      throw new Error('append failed');
    };
    router.register('item', target.sink, () => {});
    const source = { value: '' };
    publish(router, source, 'first ', -1);

    let commits = 0;
    expect(() => router.publish(
      'item',
      32,
      source.value,
      'second ',
      (nextSource, mode) => {
        commits++;
        if (mode === 'authoritative') throw new Error('recovery commit failed');
        source.value = nextSource;
      },
    )).toThrowError(expect.objectContaining({
      message: 'streaming assistant reveal append recovery failed for item',
      errors: expect.arrayContaining([
        expect.objectContaining({ message: 'append failed' }),
        expect.objectContaining({ message: 'recovery commit failed' }),
      ]),
    }));
    expect(commits).toBe(2);
    expect(parserSource(router, source.value)).toBe(source.value);
  });

  it('does not reset a sink before the first authoritative checkpoint', () => {
    const router = new StreamingAssistantRevealRouter();
    const target = makeSink();
    target.reset.mockImplementationOnce(() => {
      throw new Error('reset failed');
    });
    router.register('item', target.sink, () => {});
    const source = { value: '' };

    expect(publish(router, source, 'first ', -1)).toBe(false);
    expect(source.value).toBe('first ');
    expect(parserSource(router, source.value)).toBe('first ');
    expect(target.reset).not.toHaveBeenCalled();
  });

  it('commits an unsafe reveal after a checkpoint reset fails', () => {
    const router = new StreamingAssistantRevealRouter();
    const target = makeSink();
    router.register('item', target.sink, () => {});
    const source = { value: '' };
    publish(router, source, 'first ', -1);
    publish(router, source, 'second ');
    target.reset.mockImplementationOnce(() => {
      throw new Error('reset failed');
    });

    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    expect(publish(router, source, '**')).toBe(false);
    expect(source.value).toBe('first second **');
    expect(parserSource(router, source.value)).toBe(source.value);
    expect(warn).toHaveBeenCalledWith(
      expect.stringContaining('fell back after a sink failure'),
      expect.stringContaining('phase=reset'),
    );
    warn.mockRestore();
  });

  it('returns the canonical parser source when a context-change reset fails', () => {
    const router = new StreamingAssistantRevealRouter();
    const target = makeSink();
    router.register('item', target.sink, () => {});
    const source = { value: '' };
    publish(router, source, 'first ', -1);
    publish(router, source, 'second ');
    expect(parserSource(router, source.value)).toBe('first ');
    target.reset.mockImplementationOnce(() => {
      throw new Error('context reset failed');
    });

    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    expect(router.parserSourceFor('item', source.value, {
      ...RENDER_CONTEXT,
      workspacePath: '/other-workspace',
    })).toBe(source.value);
    expect(warn).toHaveBeenCalledWith(
      expect.stringContaining('fell back after a sink failure'),
      expect.stringContaining('phase=reset'),
    );
    warn.mockRestore();
  });

  it('reports both reset and authoritative commit failures', () => {
    const router = new StreamingAssistantRevealRouter();
    const target = makeSink();
    router.register('item', target.sink, () => {});
    const source = { value: '' };
    publish(router, source, 'first ', -1);
    publish(router, source, 'second ');
    target.reset.mockImplementationOnce(() => {
      throw new Error('reset failed');
    });

    expect(() => router.publish(
      'item',
      32,
      source.value,
      '**',
      () => { throw new Error('authoritative commit failed'); },
    )).toThrowError(expect.objectContaining({
      message: 'streaming assistant reveal reset recovery failed for item',
      errors: expect.arrayContaining([
        expect.objectContaining({
          message: 'streaming assistant reveal reset failed for item',
          errors: expect.arrayContaining([
            expect.objectContaining({ message: 'reset failed' }),
          ]),
        }),
        expect.objectContaining({ message: 'authoritative commit failed' }),
      ]),
    }));
  });

  it('disposes every representation and clears maps even when resets fail', () => {
    const router = new StreamingAssistantRevealRouter();
    const first = makeSink();
    const second = makeSink();
    router.register('item', first.sink, () => {});
    router.register('item', second.sink, () => {});
    first.reset.mockImplementation(() => {
      throw new Error('dispose reset failed');
    });

    expect(() => router.dispose()).toThrow(/disposal failed/);
    expect(second.reset).toHaveBeenCalled();

    const replacement = makeSink();
    router.register('item', replacement.sink, () => {});
    const source = { value: '' };
    expect(publish(router, source, 'new ', -1)).toBe(false);
  });
});
