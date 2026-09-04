export interface PaneLike {
  thread: { workspacePath: string };
}

// Which child a row renders:
// - 'default':      defaulted prop + init-time read (leak shape 1)
// - 'no-default':   plain prop + init-time read (leak shape 2)
// - 'template-read': defaulted prop read from the template (healthy path)
export type FixtureVariant = 'default' | 'no-default' | 'template-read';

export interface FixtureItem {
  key: string;
  title: string;
  variant: FixtureVariant;
}

// A row for DuplicateKeyHost.svelte. `key` is what the keyed {#each} keys
// on, deliberately typed loose so a test can feed it a repeat of any type;
// `label` is what the row renders, and stays unique so a repaired row is
// still identifiable in the DOM.
export interface DuplicateKeyItem {
  key: unknown;
  label: string;
}
