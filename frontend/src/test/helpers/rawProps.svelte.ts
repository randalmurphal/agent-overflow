// Component props a test can change after mount. Each field is its own
// raw signal: values are neither proxied nor copied, so class instances
// and identity-keyed caches see exactly the objects the test passed.

export function rawProps<T extends object>(initial: T): T {
  const props = {} as T;
  for (const key of Object.keys(initial) as (keyof T)[]) {
    let value = $state.raw(initial[key]);
    Object.defineProperty(props, key, {
      enumerable: true,
      get: () => value,
      set: (next: T[keyof T]) => {
        value = next;
      },
    });
  }
  return props;
}
