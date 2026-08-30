/**
 * Re-declare every own property of `props` on `ref`, descriptors intact.
 *
 * The descriptors matter: Svelte's `$props()` object exposes each prop as
 * a getter, so copying VALUES would snapshot them once. Redefining the
 * descriptor keeps the getter, and the context class stays reactive.
 */
export const bind = (ref: object, props: object): void => {
    const descriptors = Object.getOwnPropertyDescriptors(props);
    for (const key in descriptors) {
        Object.defineProperty(ref, key, descriptors[key]);
    }
};
