import type { Component } from 'svelte';

export interface CachedComponentLoader<TProps extends Record<string, unknown>> {
  /** Returns the loaded component synchronously once resolved, or null before first load. */
  peek(): Component<TProps> | null;
  /** Loads (or joins an in-flight load of) the component and caches its default export. */
  load(): Promise<Component<TProps>>;
}

export function createEagerComponentLoader<
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  TProps extends Record<string, any> = Record<string, any>,
>(
  component: Component<TProps>,
): CachedComponentLoader<TProps> {
  let resolved: Promise<Component<TProps>> | null = null;
  return {
    peek(): Component<TProps> {
      return component;
    },
    load(): Promise<Component<TProps>> {
      resolved ??= Promise.resolve(component);
      return resolved;
    },
  };
}

export function createCachedComponentLoader<
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  TProps extends Record<string, any> = Record<string, any>,
>(
  importer: () => Promise<{ default: Component<TProps> }>,
): CachedComponentLoader<TProps> {
  let cached: Component<TProps> | null = null;
  let pending: Promise<Component<TProps>> | null = null;

  return {
    peek(): Component<TProps> | null {
      return cached;
    },
    load(): Promise<Component<TProps>> {
      if (cached) return Promise.resolve(cached);
      pending ??= importer()
        .then((mod) => {
          cached = mod.default;
          return cached;
        })
        .catch((err) => {
          pending = null;
          throw err;
        });
      return pending;
    },
  };
}
