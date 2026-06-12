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
